package scan_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"websitemapper/internal/cache"
	"websitemapper/internal/discovery"
	"websitemapper/internal/discovery/dnsresolve"
	"websitemapper/internal/discovery/htmlcrawl"
	"websitemapper/internal/discovery/httpprobe"
	"websitemapper/internal/discovery/subdomains"
	"websitemapper/internal/fetch"
	"websitemapper/internal/resource"
	"websitemapper/internal/results"
	"websitemapper/internal/scan"
)

// fakeNet is a small public website spread over three hosts. It counts
// every network operation the pipeline makes and the bytes it serves, and
// takes real slots from the shared pools, like the production client and
// resolver.
type fakeNet struct {
	httpPool, dnsPool *resource.Pool

	ctCalls, dnsLookups, httpRequests, bytes atomic.Int64
	ctGate                                   chan struct{} // optional: hold certificate lookups
}

var fakePages = map[string]string{
	"https://example.com/":            `<title>Home</title><a href="/about">About</a><a href="/docs">Docs</a><a href="https://blog.example.com/">Blog</a><script src="/app.js"></script><img src="/logo.png">`,
	"https://example.com/about":       `<title>About</title><a href="/team">Team</a>`,
	"https://example.com/docs":        `<title>Docs</title><a href="/api/v1/items">API</a>`,
	"https://example.com/team":        `<title>Team</title>`,
	"https://www.example.com/":        `<title>WWW</title><a href="/about">About</a>`,
	"https://www.example.com/about":   `<title>WWW About</title>`,
	"https://blog.example.com/":       `<title>Blog</title><a href="/post-1">Post</a>` + strings.Repeat("<p>long blog text</p>", 500),
	"https://blog.example.com/post-1": `<title>Post 1</title>`,
}

func (n *fakeNet) Name() string                 { return "crt.sh" }
func (n *fakeNet) Provenance() discovery.Source { return discovery.SourceCT }
func (n *fakeNet) Discover(ctx context.Context, domain string) ([]string, error) {
	n.ctCalls.Add(1)
	if n.ctGate != nil {
		<-n.ctGate
	}
	return []string{"www." + domain, "blog." + domain, "old." + domain}, nil
}

func (n *fakeNet) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	n.dnsLookups.Add(1)
	if strings.HasPrefix(host, "old.") {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
}
func (n *fakeNet) LookupCNAME(context.Context, string) (string, error) { return "", nil }

func (n *fakeNet) Get(ctx context.Context, u string) (*fetch.Response, error) {
	if err := resource.FromContext(ctx).TakeRequest(); err != nil {
		return nil, err
	}
	release, _, err := n.httpPool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	n.httpRequests.Add(1)
	body, ok := fakePages[u]
	if !ok {
		return &fetch.Response{URL: u, StatusCode: 404, ContentType: "text/html", Header: http.Header{}}, nil
	}
	n.bytes.Add(int64(len(body)))
	return &fetch.Response{URL: u, StatusCode: 200, ContentType: "text/html", Header: http.Header{"Server": {"nginx"}}, Body: []byte(body)}, nil
}

type testClock struct{ now atomic.Int64 }

func (c *testClock) Now() time.Time          { return time.Unix(1_800_000_000, c.now.Load()) }
func (c *testClock) Advance(d time.Duration) { c.now.Add(int64(d)) }

type cachedPipeline struct {
	net    *fakeNet
	clk    *testClock
	certC  *subdomains.Cache
	dnsC   *dnsresolve.Cache
	probeC *httpprobe.Cache
	pageC  *htmlcrawl.Cache
	stages []scan.Stage
}

func newCachedPipeline() *cachedPipeline {
	p := &cachedPipeline{clk: &testClock{}}
	p.net = &fakeNet{httpPool: resource.NewPool("http", 4), dnsPool: resource.NewPool("dns", 4)}
	opts := func(name string) cache.Options { return cache.Options{Name: name, MaxCost: 4 << 20, Now: p.clk.Now} }
	p.certC = cache.New[subdomains.Answer](opts("certificate"))
	p.dnsC = cache.New[discovery.DNSInfo](opts("dns"))
	p.probeC = cache.New[discovery.HTTPInfo](opts("probe"))
	p.pageC = cache.New[htmlcrawl.Page](opts("page"))

	ct := subdomains.New([]subdomains.Source{p.net}, time.Minute, quiet).
		WithCache(subdomains.CacheOptions{Cache: p.certC, TTL: 6 * time.Hour, RateLimitTTL: 15 * time.Minute, FailureTTL: 5 * time.Minute})
	dns := dnsresolve.New(p.net, dnsresolve.Options{MaxHosts: 100, Concurrency: 4, Pool: p.net.dnsPool,
		Cache: p.dnsC, TTL: 5 * time.Minute, NegativeTTL: time.Minute})
	probe := httpprobe.New(p.net, httpprobe.Options{MaxHosts: 100, Concurrency: 4, Cache: p.probeC, TTL: 2 * time.Minute})
	crawl := htmlcrawl.New(p.net, htmlcrawl.Options{MaxHosts: 10, MaxPagesPerHost: 20, MaxDepth: 2, Concurrency: 2,
		HostConcurrency: 2, Cache: p.pageC, CacheTTL: 15 * time.Minute}, quiet)
	p.stages = []scan.Stage{
		stage("subdomains", ct), stage("resolve", dns), stage("probe", probe),
		stage("crawl", crawl), stage("follow-up", dns, probe, crawl),
	}
	return p
}

type netUsage struct{ ct, dns, http, bytes, httpSlots, dnsSlots int64 }

func (p *cachedPipeline) usage() netUsage {
	return netUsage{p.net.ctCalls.Load(), p.net.dnsLookups.Load(), p.net.httpRequests.Load(), p.net.bytes.Load(),
		p.net.httpPool.Stats().Acquired, p.net.dnsPool.Stats().Acquired}
}

func (u netUsage) minus(o netUsage) netUsage {
	return netUsage{u.ct - o.ct, u.dns - o.dns, u.http - o.http, u.bytes - o.bytes, u.httpSlots - o.httpSlots, u.dnsSlots - o.dnsSlots}
}

// fingerprint is what a user sees: hosts with their state and URLs with
// type, state and title (not timestamps or cache markers).
func fingerprint(t *testing.T, svc *scan.Service, id string) string {
	res, err := svc.Result(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, h := range res.Hosts {
		lines = append(lines, fmt.Sprintf("host %s %s %v", h.Hostname, h.State, h.Sources))
		for _, u := range h.URLs {
			lines = append(lines, fmt.Sprintf("  %s %s %s %d %q", u.URL, u.Type, u.State, u.Status, u.Title))
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func runScan(t *testing.T, svc *scan.Service, target string) scan.Scan {
	t.Helper()
	return waitStatus(t, svc, mustCreate(t, svc, target).ID, scan.StatusCompleted)
}

// TestRepeatScanReusesCache runs the same scan three times: cold, warm
// (everything cached) and after the probe TTL (probes redone, pages and
// DNS still cached). Results must be identical; network work must not be.
func TestRepeatScanReusesCache(t *testing.T) {
	p := newCachedPipeline()
	svc := startService(t, p.stages, scan.Options{MaxRunning: 2})

	before := p.usage()
	cold := runScan(t, svc, "example.com")
	coldUse := p.usage().minus(before)

	warm := runScan(t, svc, "example.com")
	warmUse := p.usage().minus(before).minus(coldUse)

	p.clk.Advance(3 * time.Minute) // probe TTL (2 min) passed; DNS (5 min) and pages (15 min) fresh
	later := runScan(t, svc, "example.com")
	laterUse := p.usage().minus(before).minus(coldUse).minus(warmUse)

	t.Logf("cold scan:  %d cert lookups, %d DNS lookups, %d HTTP requests, %d bytes, %d HTTP slots, %d DNS slots",
		coldUse.ct, coldUse.dns, coldUse.http, coldUse.bytes, coldUse.httpSlots, coldUse.dnsSlots)
	t.Logf("warm scan:  %d cert lookups, %d DNS lookups, %d HTTP requests, %d bytes, %d HTTP slots, %d DNS slots",
		warmUse.ct, warmUse.dns, warmUse.http, warmUse.bytes, warmUse.httpSlots, warmUse.dnsSlots)
	t.Logf("+3 min:     %d cert lookups, %d DNS lookups, %d HTTP requests, %d bytes",
		laterUse.ct, laterUse.dns, laterUse.http, laterUse.bytes)

	if coldUse.ct != 1 || coldUse.dns == 0 || coldUse.http == 0 || coldUse.bytes == 0 {
		t.Fatalf("cold scan did no network work: %+v", coldUse)
	}
	if warmUse != (netUsage{}) {
		t.Errorf("warm scan used the network or the pools: %+v", warmUse)
	}
	// Only the probes (one request per reachable host) are repeated, plus
	// the one negative DNS answer (NXDOMAIN, cached for 1 minute).
	if laterUse.ct != 0 || laterUse.dns != 1 || laterUse.http != int64(cold.Counts.HostsReachable) || laterUse.bytes == 0 {
		t.Errorf("after the probe TTL: %+v (reachable hosts %d)", laterUse, cold.Counts.HostsReachable)
	}

	// Same map every time.
	fp := fingerprint(t, svc, cold.ID)
	for _, sc := range []scan.Scan{warm, later} {
		if got := fingerprint(t, svc, sc.ID); got != fp {
			t.Errorf("scan %s differs from the cold scan:\n%s\n---\n%s", sc.ID, got, fp)
		}
	}
	// Provenance: the warm scan knows what was reused.
	if c := warm.Counts.Cache; c.Hosts != 3 || c.DNS != cold.Counts.Hosts || c.Probes != cold.Counts.HostsProbed || c.Pages != cold.Counts.URLsFetched {
		t.Errorf("warm cache counts = %+v, cold counts = %+v", c, cold.Counts)
	}
	if c := cold.Counts.Cache; c != (results.CacheCounts{}) {
		t.Errorf("cold scan reports cache use: %+v", c)
	}
	if warm.Resources.Requests != 0 {
		t.Errorf("warm scan charged %d requests to its budget", warm.Resources.Requests)
	}
}

// TestConcurrentDifferentScansShareLookups: two scans that are not
// equivalent (apex and www) but overlap run at the same time; shared
// lookups are made once and reused by the other scan.
func TestConcurrentDifferentScansShareLookups(t *testing.T) {
	p := newCachedPipeline()
	p.net.ctGate = make(chan struct{})
	svc := startService(t, p.stages, scan.Options{MaxRunning: 2})
	a := mustCreate(t, svc, "example.com")
	b := mustCreate(t, svc, "www.example.com")
	if a.ID == b.ID {
		t.Fatal("different start URLs must not coalesce")
	}
	waitFor(t, func() bool { return p.certC.Stats().Coalesced >= 1 })
	close(p.net.ctGate)
	waitStatus(t, svc, a.ID, scan.StatusCompleted)
	waitStatus(t, svc, b.ID, scan.StatusCompleted)
	if n := p.net.ctCalls.Load(); n != 1 {
		t.Errorf("certificate provider called %d times, want 1", n)
	}
	// Each host is looked up once across both scans.
	if n, hosts := p.net.dnsLookups.Load(), int64(4); n != hosts {
		t.Errorf("DNS lookups = %d, want %d (one per host)", n, hosts)
	}
}

// TestEquivalentScansMakeOneSetOfRequests: many identical requests at once
// produce exactly the network work of one scan.
func TestEquivalentScansMakeOneSetOfRequests(t *testing.T) {
	solo := newCachedPipeline()
	svc1 := startService(t, solo.stages, scan.Options{MaxRunning: 4})
	runScan(t, svc1, "example.com")
	one := solo.usage()

	p := newCachedPipeline()
	p.net.ctGate = make(chan struct{})
	svc := startService(t, p.stages, scan.Options{MaxRunning: 4})
	var ids []string
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sc := mustCreate(t, svc, "example.com")
			mu.Lock()
			ids = append(ids, sc.ID)
			mu.Unlock()
		}()
	}
	wg.Wait()
	close(p.net.ctGate)
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("requests did not share one scan: %v", ids)
		}
	}
	waitStatus(t, svc, ids[0], scan.StatusCompleted)
	if got := p.usage(); got != one {
		t.Errorf("5 identical requests used %+v; one scan uses %+v", got, one)
	}
	if svc.Stats().CoalescedRequests != 4 {
		t.Errorf("coalesced = %d", svc.Stats().CoalescedRequests)
	}
}
