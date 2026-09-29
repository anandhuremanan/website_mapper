package results

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"websitemapper/internal/classify"
	"websitemapper/internal/discovery"
)

func newAgg(t *testing.T) *Aggregator {
	t.Helper()
	tgt, err := discovery.ParseTarget("example.com")
	if err != nil {
		t.Fatal(err)
	}
	return NewAggregator(tgt, Limits{})
}

func findHost(t *testing.T, r Result, name string) Host {
	t.Helper()
	for _, h := range r.Hosts {
		if h.Hostname == name {
			return h
		}
	}
	t.Fatalf("host %s not in result", name)
	return Host{}
}

func TestAggregatorDeduplicatesURLsAndKeepsProvenance(t *testing.T) {
	a := newAgg(t)
	a.Add(discovery.Finding{URL: "https://example.com/about", Source: discovery.SourceHTML, Hint: discovery.HintLink, From: "https://example.com/"})
	a.Add(discovery.Finding{URL: "https://EXAMPLE.com/about/", Source: discovery.SourceSitemap})
	a.Add(discovery.Finding{URL: "https://example.com/about#team", Source: discovery.SourceJavaScript, From: "https://example.com/app.js"})
	a.Add(discovery.Finding{URL: "https://example.com/about", Source: discovery.SourceHTML, Response: &discovery.Response{Status: 200, ContentType: "text/html", Title: "About"}})

	res := a.Result()
	if len(res.Hosts) != 1 || len(res.Hosts[0].URLs) != 1 {
		t.Fatalf("result = %+v", res)
	}
	u := res.Hosts[0].URLs[0]
	wantSources := []discovery.Source{discovery.SourceHTML, discovery.SourceJavaScript, discovery.SourceSitemap}
	if u.URL != "https://example.com/about" || u.Hostname != "example.com" || u.Path != "/about" || !reflect.DeepEqual(u.Sources, wantSources) {
		t.Errorf("url = %+v", u)
	}
	if u.Status != 200 || u.Title != "About" || u.Type != classify.TypePage || !u.Fetched {
		t.Errorf("metadata = %+v", u)
	}
	if len(u.DiscoveredFrom) != 2 {
		t.Errorf("discoveredFrom = %v", u.DiscoveredFrom)
	}
}

func TestAggregatorMergesHostSources(t *testing.T) {
	a := newAgg(t)
	a.Add(discovery.Finding{Host: "*.Stories.Example.com", Source: discovery.SourceCT})
	a.Add(discovery.Finding{Host: "stories.example.com.", Source: discovery.SourceCT})
	a.Add(discovery.Finding{URL: "https://stories.example.com/stories/foo", Source: discovery.SourceHTML, From: "https://example.com/"})
	// A host root requested because the host was discovered is not host provenance.
	a.Add(discovery.Finding{URL: "https://stories.example.com/", Source: discovery.SourceHost, Hint: discovery.HintEntry})

	res := a.Result()
	if len(res.Hosts) != 1 {
		t.Fatalf("hosts = %+v", res.Hosts)
	}
	h := res.Hosts[0]
	if h.Hostname != "stories.example.com" || !reflect.DeepEqual(h.Sources, []discovery.Source{discovery.SourceCT, discovery.SourceHTML}) {
		t.Errorf("host = %+v", h)
	}
	if h.Counts.URLs != 2 || len(h.URLs) != 2 || h.URLs[1].Path != "/stories/foo" {
		t.Errorf("host urls = %+v", h.URLs)
	}
	if !reflect.DeepEqual(h.URLs[0].Sources, []discovery.Source{discovery.SourceHost}) {
		t.Errorf("root url sources = %v", h.URLs[0].Sources)
	}
}

func TestAggregatorHostObservationsAndStates(t *testing.T) {
	a := newAgg(t)
	for _, h := range []string{"example.com", "api.example.com", "old.example.com", "mail.example.com", "unchecked.example.com"} {
		a.Add(discovery.Finding{Host: h, Source: discovery.SourceCT})
	}
	a.Add(discovery.Finding{Host: "example.com", DNS: &discovery.DNSInfo{Resolved: true, Addresses: []string{"93.184.216.34"}}})
	a.Add(discovery.Finding{Host: "example.com", HTTP: &discovery.HTTPInfo{Reachable: true, Status: 200, Server: "nginx/1.25"}})
	a.Add(discovery.Finding{Host: "example.com", Crawl: &discovery.CrawlInfo{Requests: 4}})
	a.Add(discovery.Finding{Host: "api.example.com", DNS: &discovery.DNSInfo{Resolved: true}})
	a.Add(discovery.Finding{Host: "api.example.com", HTTP: &discovery.HTTPInfo{Reachable: true, Status: 404}})
	a.Add(discovery.Finding{Host: "old.example.com", DNS: &discovery.DNSInfo{Resolved: false, Error: "no such host"}})
	a.Add(discovery.Finding{Host: "mail.example.com", DNS: &discovery.DNSInfo{Resolved: true}})
	a.Add(discovery.Finding{Host: "mail.example.com", HTTP: &discovery.HTTPInfo{Reachable: false, Error: "connection refused"}})

	res := a.Result()
	var order []string
	for _, h := range res.Hosts {
		order = append(order, h.Hostname)
	}
	want := []string{"example.com", "api.example.com", "mail.example.com", "old.example.com", "unchecked.example.com"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("host order = %v, want %v", order, want)
	}
	states := map[string]HostState{
		"example.com": HostReachable, "api.example.com": HostReachable, "mail.example.com": HostResolved,
		"old.example.com": HostDiscovered, "unchecked.example.com": HostDiscovered,
	}
	for name, st := range states {
		if h := findHost(t, res, name); h.State != st {
			t.Errorf("%s state = %s, want %s", name, h.State, st)
		}
	}
	// A host that no longer resolves stays visible, with its DNS result.
	if old := findHost(t, res, "old.example.com"); old.DNS == nil || old.DNS.Error != "no such host" {
		t.Errorf("old = %+v", old)
	}
	if h := findHost(t, res, "unchecked.example.com"); h.URLs == nil {
		t.Error("urls must be an empty list, not null")
	}

	c := a.Counts()
	if c.Hosts != 5 || c.HostsResolved != 3 || c.HostsReachable != 2 || c.HostsCrawled != 1 {
		t.Errorf("counts = %+v", c)
	}
	if rc := res.Counts(); rc != c {
		t.Errorf("result counts %+v != live counts %+v", rc, c)
	}

	// The State view exposes the same observations to engines.
	for _, v := range a.Hosts() {
		if v.Name == "api.example.com" && (!v.Reachable() || v.DNS == nil) {
			t.Errorf("view = %+v", v)
		}
	}
}

func TestAggregatorScope(t *testing.T) {
	a := newAgg(t)
	for _, h := range []string{"example.org", "evil-example.com", "example.com.evil.io", "not a host", "10.0.0.1"} {
		a.Add(discovery.Finding{Host: h, Source: discovery.SourceCT})
	}
	a.Add(discovery.Finding{URL: "https://cdn.other.net/x.js", Source: discovery.SourceHTML})
	a.Add(discovery.Finding{URL: "https://evil-example.com/", Source: discovery.SourceHTML})
	a.Add(discovery.Finding{Host: "api.example.com", Source: discovery.SourceCT})
	a.Add(discovery.Finding{URL: "https://docs.example.com/foo", Source: discovery.SourceHTML})

	var names []string
	for _, h := range a.Result().Hosts {
		names = append(names, h.Hostname)
	}
	if !reflect.DeepEqual(names, []string{"api.example.com", "docs.example.com"}) {
		t.Errorf("hosts = %v", names)
	}
}

func TestAggregatorPageURLs(t *testing.T) {
	a := newAgg(t)
	a.Add(discovery.Finding{URL: "https://example.com/linked", Source: discovery.SourceHTML, Hint: discovery.HintLink})
	a.Add(discovery.Finding{URL: "https://example.com/done", Source: discovery.SourceHTML, Hint: discovery.HintLink,
		Response: &discovery.Response{Status: 200, ContentType: "text/html"}})
	a.Add(discovery.Finding{URL: "https://example.com/app.js", Source: discovery.SourceHTML, Hint: discovery.HintScript})
	if got := a.PageURLs(); !reflect.DeepEqual(got, []string{"https://example.com/linked"}) {
		t.Errorf("PageURLs = %v", got)
	}
}

func TestAggregatorPrefersSuccessfulResponse(t *testing.T) {
	a := newAgg(t)
	a.Add(discovery.Finding{URL: "https://example.com/docs", Source: discovery.SourceHTML, Response: &discovery.Response{Status: 301, Redirect: "https://example.com/docs/"}})
	a.Add(discovery.Finding{URL: "https://example.com/docs/", Source: discovery.SourceRedirect, Response: &discovery.Response{Status: 200, ContentType: "text/html"}})
	if u := a.Result().Hosts[0].URLs[0]; u.Status != 200 {
		t.Errorf("status = %d, want 200", u.Status)
	}
}

func TestTechnologyDetection(t *testing.T) {
	a := newAgg(t)
	a.Add(discovery.Finding{URL: "https://example.com/_app/immutable/start.js", Source: discovery.SourceHTML, Hint: discovery.HintScript})
	a.Add(discovery.Finding{URL: "https://example.com/_app/immutable/app.js", Source: discovery.SourceHTML, Hint: discovery.HintScript})
	a.Add(discovery.Finding{URL: "https://example.com/", Source: discovery.SourceTarget, Response: &discovery.Response{
		Status: 200, ContentType: "text/html", Server: "nginx/1.25.3", PoweredBy: "Express", Generator: "WordPress 6.4",
	}})
	a.Add(discovery.Finding{Host: "status.example.com", Source: discovery.SourceCT})
	a.Add(discovery.Finding{Host: "status.example.com", HTTP: &discovery.HTTPInfo{Reachable: true, Server: "cloudflare"}})
	got := a.Result().Technologies
	want := []Technology{
		{Name: "Express", Evidence: []string{"X-Powered-By: Express"}},
		{Name: "SvelteKit", Evidence: []string{"URL path contains /_app/immutable/"}},
		{Name: "WordPress", Evidence: []string{`<meta name="generator" content="WordPress 6.4">`}},
		{Name: "cloudflare", Evidence: []string{"Server: cloudflare"}},
		{Name: "nginx", Evidence: []string{"Server: nginx/1.25.3"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("technologies =\n%+v\nwant\n%+v", got, want)
	}
}

func TestHeaderProduct(t *testing.T) {
	for in, want := range map[string]string{
		"nginx/1.25.3":          "nginx",
		"Apache/2.4.1 (Ubuntu)": "Apache",
		"Google Frontend":       "Google Frontend",
		"WordPress 6.4.2":       "WordPress",
		"Hugo v0.120.0":         "Hugo",
		"Express":               "Express",
		"Microsoft-IIS/10.0":    "Microsoft-IIS",
		"Discourse 3.1.1 - https://github.com/discourse/discourse":            "Discourse",
		"Discourse 2026.10.0-latest - https://github.com/discourse/discourse": "Discourse",
	} {
		if got := headerProduct(in); got != want {
			t.Errorf("headerProduct(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAggregatorCapsURLsPerHost(t *testing.T) {
	tgt, _ := discovery.ParseTarget("example.com")
	a := NewAggregator(tgt, Limits{MaxURLsPerHost: 2})
	for _, p := range []string{"/a", "/b", "/c", "/d", "/a/"} {
		a.Add(discovery.Finding{URL: "https://example.com" + p, Source: discovery.SourceHTML, Hint: discovery.HintLink})
	}
	// A URL the scanner fetched is always recorded, even over the limit.
	a.Add(discovery.Finding{URL: "https://example.com/fetched", Source: discovery.SourceHTML,
		Response: &discovery.Response{Status: 200, ContentType: "text/html"}})
	// Other hosts have their own limit.
	a.Add(discovery.Finding{URL: "https://api.example.com/x", Source: discovery.SourceHTML})

	res := a.Result()
	apex := findHost(t, res, "example.com")
	var paths []string
	for _, u := range apex.URLs {
		paths = append(paths, u.Path)
	}
	if !reflect.DeepEqual(paths, []string{"/a", "/b", "/fetched"}) || apex.Omitted != 2 {
		t.Errorf("paths = %v omitted = %d", paths, apex.Omitted)
	}
	if api := findHost(t, res, "api.example.com"); len(api.URLs) != 1 {
		t.Errorf("api urls = %v", api.URLs)
	}
}

func TestAggregatorCapsDiscoveredHosts(t *testing.T) {
	tgt, _ := discovery.ParseTarget("example.com")
	a := NewAggregator(tgt, Limits{MaxHosts: 100})
	for i := 0; i < 20000; i++ {
		a.Add(discovery.Finding{Host: fmt.Sprintf("h%d.example.com", i), Source: discovery.SourceCT})
	}
	// The target's own host is always recorded, even over the limit.
	a.Add(discovery.Finding{Host: "example.com", Source: discovery.SourceTarget})
	// URLs and observations for omitted hosts are dropped, not recorded.
	a.Add(discovery.Finding{URL: "https://h19999.example.com/x", Source: discovery.SourceHTML})

	c := a.Counts()
	if c.Hosts != 101 || c.Limits.HostsOmitted != 20000-100+1 || c.URLs != 0 {
		t.Errorf("counts = %+v", c)
	}
	res := a.Result()
	if len(res.Hosts) != 101 || res.HostsOmitted != c.Limits.HostsOmitted || res.Counts() != c {
		t.Errorf("result hosts = %d omitted = %d", len(res.Hosts), res.HostsOmitted)
	}
}

func TestAggregatorCapsTotalURLs(t *testing.T) {
	tgt, _ := discovery.ParseTarget("example.com")
	a := NewAggregator(tgt, Limits{MaxURLs: 3, MaxURLsPerHost: 100})
	for i := 0; i < 5; i++ {
		a.Add(discovery.Finding{URL: fmt.Sprintf("https://a%d.example.com/p", i), Source: discovery.SourceHTML})
	}
	c := a.Counts()
	if c.URLs != 3 || c.Limits.URLsOmitted != 2 || c.Hosts != 5 {
		t.Errorf("counts = %+v", c)
	}
}

func TestAggregatorProgressCounters(t *testing.T) {
	a := newAgg(t)
	add := func(h string, f discovery.Finding) {
		f.Host = h
		a.Add(f)
	}
	for _, h := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		add(h+".example.com", discovery.Finding{Source: discovery.SourceCT})
	}
	// a: fully processed and crawled until its request limit.
	add("a.example.com", discovery.Finding{DNS: &discovery.DNSInfo{Resolved: true}})
	add("a.example.com", discovery.Finding{HTTP: &discovery.HTTPInfo{Reachable: true, Status: 200}})
	add("a.example.com", discovery.Finding{Crawl: &discovery.CrawlInfo{Requests: 5, LimitReached: true}})
	// b: reachable, waiting to be crawled.
	add("b.example.com", discovery.Finding{DNS: &discovery.DNSInfo{Resolved: true}})
	add("b.example.com", discovery.Finding{HTTP: &discovery.HTTPInfo{Reachable: true, Status: 200}})
	// c: resolved, waiting to be probed.
	add("c.example.com", discovery.Finding{DNS: &discovery.DNSInfo{Resolved: true}})
	// d: resolved, probe failed.
	add("d.example.com", discovery.Finding{DNS: &discovery.DNSInfo{Resolved: true}})
	add("d.example.com", discovery.Finding{HTTP: &discovery.HTTPInfo{Error: "connection refused"}})
	// e: does not resolve. f: skipped by the resolve budget. g: pending.
	add("e.example.com", discovery.Finding{DNS: &discovery.DNSInfo{Error: "no such host"}})
	add("f.example.com", discovery.Finding{DNS: &discovery.DNSInfo{Skipped: discovery.SkipHostLimit}})
	// A reachable host skipped by the crawl budget; a redirect-only one is not a budget skip.
	add("b.example.com", discovery.Finding{})
	for _, h := range []string{"x", "y"} {
		add(h+".example.com", discovery.Finding{DNS: &discovery.DNSInfo{Resolved: true}})
		add(h+".example.com", discovery.Finding{HTTP: &discovery.HTTPInfo{Reachable: true, Status: 301}})
	}
	add("x.example.com", discovery.Finding{Crawl: &discovery.CrawlInfo{Skipped: discovery.SkipHostLimit}})
	add("y.example.com", discovery.Finding{Crawl: &discovery.CrawlInfo{Skipped: "root redirects to another host"}})
	// URLs: one fetched, one failed, one only discovered.
	a.Add(discovery.Finding{URL: "https://a.example.com/", Source: discovery.SourceHost, Response: &discovery.Response{Status: 200, ContentType: "text/html"}})
	a.Add(discovery.Finding{URL: "https://a.example.com/down", Source: discovery.SourceHTML, Error: "timeout"})
	a.Add(discovery.Finding{URL: "https://a.example.com/later", Source: discovery.SourceHTML, Hint: discovery.HintLink})

	c := a.Counts()
	want := Counts{
		Hosts: 9, HostsResolved: 6, HostsReachable: 4, HostsCrawled: 1,
		HostsUnresolved: 1, HostsResolvePending: 1, HostsProbed: 5, HostsUnreachable: 1,
		HostsProbePending: 1, HostsCrawlPending: 1,
		URLs: 3, Pages: 2, URLsFetched: 1, URLsFailed: 1, // /down has no hint: unknown
		Limits: LimitCounts{ResolveSkipped: 1, CrawlSkipped: 1, CrawlLimited: 1},
	}
	if c != want {
		t.Errorf("counts =\n%+v\nwant\n%+v", c, want)
	}
	if rc := a.Result().Counts(); rc != c {
		t.Errorf("result counts differ:\n%+v\n%+v", rc, c)
	}
}

func TestAggregatorIncrementalCountsFollowReclassification(t *testing.T) {
	a := newAgg(t)
	// Linked from HTML: a page. Then fetched and found to return JSON: an API.
	a.Add(discovery.Finding{URL: "https://example.com/data", Source: discovery.SourceHTML, Hint: discovery.HintLink})
	if c := a.Counts(); c.Pages != 1 || c.APIs != 0 {
		t.Fatalf("before fetch: %+v", c)
	}
	a.Add(discovery.Finding{URL: "https://example.com/data", Source: discovery.SourceHTML,
		Response: &discovery.Response{Status: 200, ContentType: "application/json"}})
	c := a.Counts()
	if c.Pages != 0 || c.APIs != 1 || c.URLs != 1 || c.URLsFetched != 1 {
		t.Errorf("after fetch: %+v", c)
	}
	if rc := a.Result().Counts(); rc != c {
		t.Errorf("result counts %+v != live %+v", rc, c)
	}
}

func TestAggregatorCacheProvenance(t *testing.T) {
	a := newAgg(t)
	past := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// cached-only host, host seen both ways, host seen only live.
	a.Add(discovery.Finding{Host: "old.example.com", Source: discovery.SourceCT, CachedAt: past})
	a.Add(discovery.Finding{Host: "both.example.com", Source: discovery.SourceCT, CachedAt: past})
	a.Add(discovery.Finding{URL: "https://both.example.com/x", Source: discovery.SourceHTML, Hint: discovery.HintLink})
	a.Add(discovery.Finding{Host: "new.example.com", Source: discovery.SourceCT})
	a.Add(discovery.Finding{Host: "old.example.com", DNS: &discovery.DNSInfo{Resolved: true, CachedAt: &past}})
	a.Add(discovery.Finding{Host: "old.example.com", HTTP: &discovery.HTTPInfo{Reachable: true, Status: 200, CachedAt: &past}})

	// URL states: an asset only referenced, a failed request, a verified
	// page from the cache, and a cached response replaced by a live one.
	a.Add(discovery.Finding{URL: "https://new.example.com/app.js", Source: discovery.SourceHTML, Hint: discovery.HintScript})
	a.Add(discovery.Finding{URL: "https://new.example.com/down", Source: discovery.SourceHTML, Error: "timeout"})
	a.Add(discovery.Finding{URL: "https://new.example.com/cached", Source: discovery.SourceHTML,
		Response: &discovery.Response{Status: 200, ContentType: "text/html", CachedAt: &past}})
	a.Add(discovery.Finding{URL: "https://new.example.com/both", Source: discovery.SourceHTML,
		Response: &discovery.Response{Status: 200, ContentType: "text/html", CachedAt: &past}})
	a.Add(discovery.Finding{URL: "https://new.example.com/both", Source: discovery.SourceHTML,
		Response: &discovery.Response{Status: 200, ContentType: "text/html", Title: "live"}})

	res := a.Result()
	if h := findHost(t, res, "old.example.com"); !h.FromCache || h.DNS.CachedAt == nil || h.HTTP.CachedAt == nil {
		t.Errorf("old = %+v", h)
	}
	if findHost(t, res, "both.example.com").FromCache || findHost(t, res, "new.example.com").FromCache {
		t.Error("hosts seen by this scan must not be marked as from cache")
	}
	states := map[string]URLState{}
	cachedAt := map[string]bool{}
	for _, u := range findHost(t, res, "new.example.com").URLs {
		states[u.Path], cachedAt[u.Path] = u.State, u.CachedAt != nil
	}
	want := map[string]URLState{"/app.js": URLDiscovered, "/down": URLFetched, "/cached": URLVerified, "/both": URLVerified}
	if !reflect.DeepEqual(states, want) || !cachedAt["/cached"] || cachedAt["/both"] {
		t.Errorf("states = %v, cachedAt = %v", states, cachedAt)
	}

	c := a.Counts()
	if c.Cache != (CacheCounts{Hosts: 1, DNS: 1, Probes: 1, Pages: 1}) || c.URLsFetched != 2 || c.URLsFailed != 1 {
		t.Errorf("counts = %+v", c)
	}
	if rc := res.Counts(); rc != c {
		t.Errorf("result counts %+v != live %+v", rc, c)
	}
}
