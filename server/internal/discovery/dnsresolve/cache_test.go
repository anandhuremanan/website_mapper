package dnsresolve

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"websitemapper/internal/cache"
	"websitemapper/internal/discovery"
	"websitemapper/internal/resource"
)

type clock struct{ now atomic.Int64 }

func (c *clock) Now() time.Time          { return time.Unix(0, c.now.Load()) }
func (c *clock) Advance(d time.Duration) { c.now.Add(int64(d)) }

// scriptedResolver answers per host and counts lookups.
type scriptedResolver struct {
	calls   atomic.Int32
	answers map[string]error // nil error: resolves to a public address
	gate    chan struct{}
}

func (r *scriptedResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	r.calls.Add(1)
	if r.gate != nil {
		<-r.gate
	}
	if err := r.answers[host]; err != nil {
		return nil, err
	}
	return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
}
func (r *scriptedResolver) LookupCNAME(context.Context, string) (string, error) { return "", nil }

func cachedEngine(r Resolver, clk *clock, pool *resource.Pool) *Engine {
	c := cache.New[discovery.DNSInfo](cache.Options{Name: "dns", MaxCost: 1 << 20, Now: clk.Now})
	return New(r, Options{MaxHosts: 100, Concurrency: 8, Pool: pool, Cache: c, TTL: 5 * time.Minute, NegativeTTL: time.Minute})
}

func resolveAll(t *testing.T, e *Engine, hosts ...string) map[string]*discovery.DNSInfo {
	t.Helper()
	var views []discovery.HostView
	for _, h := range hosts {
		views = append(views, discovery.HostView{Name: h})
	}
	tgt, _ := discovery.ParseTarget("example.com")
	var mu sync.Mutex
	got := map[string]*discovery.DNSInfo{}
	if err := e.Discover(context.Background(), discovery.Input{Target: tgt, State: state{views}}, func(f discovery.Finding) {
		mu.Lock()
		got[f.Host] = f.DNS
		mu.Unlock()
	}); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestDNSCacheHitAvoidsLookupAndPoolSlot(t *testing.T) {
	clk := &clock{}
	r := &scriptedResolver{}
	pool := resource.NewPool("dns", 4)
	e := cachedEngine(r, clk, pool)

	first := resolveAll(t, e, "a.example.com", "b.example.com")
	if r.calls.Load() != 2 || pool.Stats().Acquired != 2 || first["a.example.com"].CachedAt != nil {
		t.Fatalf("first: calls %d, pool %d", r.calls.Load(), pool.Stats().Acquired)
	}
	clk.Advance(4 * time.Minute)
	second := resolveAll(t, e, "a.example.com", "b.example.com")
	if r.calls.Load() != 2 || pool.Stats().Acquired != 2 {
		t.Errorf("hits made lookups (%d) or took pool slots (%d)", r.calls.Load(), pool.Stats().Acquired)
	}
	if a := second["a.example.com"]; a == nil || !a.Resolved || a.CachedAt == nil || len(a.Addresses) != 1 {
		t.Errorf("cached answer = %+v", a)
	}
	clk.Advance(2 * time.Minute) // past the 5 min TTL
	resolveAll(t, e, "a.example.com")
	if r.calls.Load() != 3 || pool.Stats().Acquired != 3 {
		t.Errorf("expired entry: calls %d, pool %d", r.calls.Load(), pool.Stats().Acquired)
	}
}

func TestDNSCacheFailures(t *testing.T) {
	clk := &clock{}
	r := &scriptedResolver{answers: map[string]error{
		"gone.example.com": &net.DNSError{Err: "no such host", Name: "gone.example.com", IsNotFound: true},
		"slow.example.com": &net.DNSError{Err: "i/o timeout", Name: "slow.example.com", IsTimeout: true},
	}}
	e := cachedEngine(r, clk, nil)

	resolveAll(t, e, "gone.example.com", "slow.example.com")
	got := resolveAll(t, e, "gone.example.com", "slow.example.com")
	// NXDOMAIN is reused briefly; a timeout is a transient failure and is
	// looked up again.
	if r.calls.Load() != 3 {
		t.Errorf("lookups = %d, want 3 (gone cached, slow retried)", r.calls.Load())
	}
	if g := got["gone.example.com"]; g.Resolved || g.Error != "no such host" || g.CachedAt == nil {
		t.Errorf("gone = %+v", g)
	}
	if s := got["slow.example.com"]; s.Resolved || s.Error != "lookup timed out" || s.CachedAt != nil {
		t.Errorf("slow = %+v", s)
	}
	clk.Advance(61 * time.Second) // past the 1 min negative TTL
	resolveAll(t, e, "gone.example.com")
	if r.calls.Load() != 4 {
		t.Errorf("negative answer outlived its TTL: lookups = %d", r.calls.Load())
	}
}

func TestDNSCacheConcurrentMissesShareOneLookup(t *testing.T) {
	clk := &clock{}
	r := &scriptedResolver{gate: make(chan struct{})}
	e := cachedEngine(r, clk, resource.NewPool("dns", 4))
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ { // five scans resolving the same host at once
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := resolveAll(t, e, "api.example.com"); !got["api.example.com"].Resolved {
				t.Error("not resolved")
			}
		}()
	}
	for e.opts.Cache.Stats().Coalesced < 4 {
		time.Sleep(time.Millisecond)
	}
	close(r.gate)
	wg.Wait()
	if r.calls.Load() != 1 {
		t.Errorf("lookups = %d, want 1", r.calls.Load())
	}
}
