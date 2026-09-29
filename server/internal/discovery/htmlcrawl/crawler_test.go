package htmlcrawl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"websitemapper/internal/discovery"
	"websitemapper/internal/fetch"
	"websitemapper/internal/resource"
)

// fakeSite serves canned responses keyed by exact URL, across any number of hosts.
type fakeSite struct {
	mu        sync.Mutex
	pages     map[string]*fetch.Response
	requested []string
}

func (s *fakeSite) Get(_ context.Context, u string) (*fetch.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requested = append(s.requested, u)
	if r, ok := s.pages[u]; ok {
		r.URL = u
		return r, nil
	}
	return nil, errors.New("connection refused")
}

func (s *fakeSite) requestedSorted() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]string(nil), s.requested...)
	sort.Strings(out)
	return out
}

func htmlPage(body string) *fetch.Response {
	return &fetch.Response{StatusCode: 200, ContentType: "text/html", Header: http.Header{"Server": {"nginx"}}, Body: []byte(body)}
}

func redirect(to string) *fetch.Response {
	return &fetch.Response{StatusCode: 301, Location: to, Header: http.Header{}}
}

type collector struct {
	mu       sync.Mutex
	findings []discovery.Finding
}

func (c *collector) emit(f discovery.Finding) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.findings = append(c.findings, f)
}

func (c *collector) byURL(u string) []discovery.Finding {
	var out []discovery.Finding
	for _, f := range c.findings {
		if f.URL == u {
			out = append(out, f)
		}
	}
	return out
}

func (c *collector) fetched(u string) *discovery.Response {
	for _, f := range c.byURL(u) {
		if f.Response != nil {
			return f.Response
		}
	}
	return nil
}

func (c *collector) crawlInfo(host string) *discovery.CrawlInfo {
	for _, f := range c.findings {
		if f.Host == host && f.Crawl != nil {
			return f.Crawl
		}
	}
	return nil
}

// state is a fixed discovery.State for tests.
type state struct {
	hosts []discovery.HostView
	pages []string
}

func (s state) Hosts() []discovery.HostView { return append([]discovery.HostView(nil), s.hosts...) }
func (s state) PageURLs() []string          { return s.pages }

// reachable is a host whose probe answered on https.
func reachable(name string) discovery.HostView {
	return discovery.HostView{Name: name, HTTP: &discovery.HTTPInfo{Reachable: true, Scheme: "https", URL: "https://" + name + "/"}}
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func target(t *testing.T, in string) discovery.Target {
	t.Helper()
	tgt, err := discovery.ParseTarget(in)
	if err != nil {
		t.Fatal(err)
	}
	return tgt
}

func input(t *testing.T, tgt string, hosts ...discovery.HostView) discovery.Input {
	return discovery.Input{Target: target(t, tgt), State: state{hosts: hosts}}
}

func TestCrawlDiscoversLinksAndResources(t *testing.T) {
	site := &fakeSite{pages: map[string]*fetch.Response{
		"https://example.com/": htmlPage(`<html><head>
			<title> Example
			Home </title>
			<meta name="generator" content="Hugo 0.120">
			<link rel="stylesheet" href="/css/site.css">
			<link rel="icon" href="/favicon.ico">
			<script src="/_app/immutable/start.js"></script>
			<script src="https://cdn.other.net/lib.js"></script>
			</head><body>
			<a href="/about/">About</a>
			<a href="/ABOUT">About again</a>
			<a href="https://blog.example.com/post">Blog</a>
			<a href="https://twitter.com/example">Twitter</a>
			<a href="mailto:hi@example.com">Mail</a>
			<a href="/files/report.pdf">Report</a>
			<img src="/img/logo.png" srcset="/img/logo@2x.png 2x">
			<form action="/search" method="post"></form>
			</body></html>`),
		"https://example.com/about": htmlPage(`<title>About</title><a href="/team">Team</a>`),
		"https://example.com/ABOUT": htmlPage(`<title>About caps</title>`),
		"https://example.com/team":  htmlPage(`<title>Team</title>`),
	}}
	c := New(site, Options{MaxHosts: 5, MaxDepth: 2, MaxPagesPerHost: 20, Concurrency: 2}, quiet)
	col := &collector{}
	if err := c.Discover(context.Background(), input(t, "example.com", reachable("example.com")), col.emit); err != nil {
		t.Fatal(err)
	}

	home := col.fetched("https://example.com/")
	if home == nil || home.Title != "Example Home" || home.Generator != "Hugo 0.120" || home.Server != "nginx" {
		t.Fatalf("home metadata = %+v", home)
	}
	if fs := col.byURL("https://example.com/"); fs[0].Source != discovery.SourceTarget || fs[0].Hint != discovery.HintEntry {
		t.Errorf("start URL finding = %+v", fs[0])
	}
	for _, want := range []string{
		"https://example.com/about", "https://blog.example.com/post", "https://example.com/css/site.css",
		"https://example.com/favicon.ico", "https://example.com/_app/immutable/start.js",
		"https://example.com/files/report.pdf", "https://example.com/img/logo.png",
		"https://example.com/img/logo@2x.png", "https://example.com/search", "https://example.com/team",
	} {
		if len(col.byURL(want)) == 0 {
			t.Errorf("missing finding for %s", want)
		}
	}
	for _, notWant := range []string{"https://cdn.other.net/lib.js", "https://twitter.com/example"} {
		if len(col.byURL(notWant)) > 0 {
			t.Errorf("out-of-scope URL reported: %s", notWant)
		}
	}
	if fs := col.byURL("https://example.com/search"); fs[0].Method != "POST" || fs[0].Hint != discovery.HintForm {
		t.Errorf("form finding = %+v", fs[0])
	}

	// Assets, forms, out-of-scope links and other hosts are not fetched by
	// this host's crawl. /about/ and /about are the same page; /ABOUT is not.
	want := []string{"https://example.com/", "https://example.com/ABOUT", "https://example.com/about", "https://example.com/team"}
	if got := site.requestedSorted(); !equal(got, want) {
		t.Fatalf("requested = %v, want %v", got, want)
	}
	if info := col.crawlInfo("example.com"); info == nil || info.Requests != 4 || info.LimitReached {
		t.Errorf("crawl info = %+v", info)
	}
}

func TestCrawlMultipleHosts(t *testing.T) {
	site := &fakeSite{pages: map[string]*fetch.Response{
		"https://example.com/":                    htmlPage(`<a href="/a1">A1</a><a href="https://b.example.com/linked">B</a>`),
		"https://example.com/a1":                  htmlPage(`<title>A1</title>`),
		"https://b.example.com/":                  htmlPage(`<a href="/b1">B1</a>`),
		"https://b.example.com/b1":                htmlPage(`<title>B1</title>`),
		"https://b.example.com/linked":            htmlPage(`<title>Linked</title>`),
		"https://stories.example.com/":            htmlPage(`<a href="/stories/foo">foo</a>`),
		"https://stories.example.com/stories/foo": htmlPage(`<title>Foo</title>`),
	}}
	ct := func(h discovery.HostView) discovery.HostView {
		h.Sources = []discovery.Source{discovery.SourceCT}
		return h
	}
	col := &collector{}
	c := New(site, Options{MaxHosts: 10, MaxDepth: 2, MaxPagesPerHost: 20, Concurrency: 2, HostConcurrency: 2}, quiet)
	err := c.Discover(context.Background(), input(t, "example.com",
		reachable("example.com"),
		ct(reachable("b.example.com")),
		ct(reachable("stories.example.com")),
		discovery.HostView{Name: "old.example.com", Sources: []discovery.Source{discovery.SourceCT}},
	), col.emit)
	if err != nil {
		t.Fatal(err)
	}

	for u, title := range map[string]string{
		"https://example.com/a1":                  "A1",
		"https://b.example.com/b1":                "B1",
		"https://stories.example.com/stories/foo": "Foo",
	} {
		if r := col.fetched(u); r == nil || r.Title != title {
			t.Errorf("%s = %+v, want title %q", u, r, title)
		}
	}
	// A discovered host's root is reported with host provenance.
	if fs := col.byURL("https://stories.example.com/"); len(fs) == 0 || fs[0].Source != discovery.SourceHost {
		t.Errorf("stories root findings = %+v", fs)
	}
	// The cross-host link is reported by example.com's crawl but not fetched
	// by it; b.example.com's own crawl does not link to it either.
	if fs := col.byURL("https://b.example.com/linked"); len(fs) == 0 || fs[0].From != "https://example.com/" {
		t.Errorf("cross-host link findings = %+v", fs)
	}
	for _, u := range site.requested {
		if u == "https://b.example.com/linked" {
			t.Error("cross-host link fetched by the wrong host's crawl")
		}
	}
	for _, h := range []string{"example.com", "b.example.com", "stories.example.com"} {
		if col.crawlInfo(h) == nil {
			t.Errorf("no crawl info for %s", h)
		}
	}
	if col.crawlInfo("old.example.com") != nil {
		t.Error("unreachable host should not be crawled")
	}
}

func TestCrawlPerHostBudgetAndHostLimit(t *testing.T) {
	site := &fakeSite{pages: map[string]*fetch.Response{
		"https://example.com/":    htmlPage(`<a href="/1">1</a><a href="/2">2</a><a href="/3">3</a><a href="/4">4</a><a href="/5">5</a>`),
		"https://a.example.com/":  htmlPage(`<a href="/x">x</a>`),
		"https://a.example.com/x": htmlPage(``),
		"https://b.example.com/":  htmlPage(``),
	}}
	for i := 1; i <= 5; i++ {
		site.pages[fmt.Sprintf("https://example.com/%d", i)] = htmlPage(``)
	}
	already := reachable("done.example.com")
	already.Crawl = &discovery.CrawlInfo{Requests: 1}

	col := &collector{}
	c := New(site, Options{MaxHosts: 3, MaxDepth: 3, MaxPagesPerHost: 3, Concurrency: 1}, quiet)
	err := c.Discover(context.Background(), input(t, "example.com",
		reachable("example.com"), already, reachable("a.example.com"), reachable("b.example.com")), col.emit)
	if err != nil {
		t.Fatal(err)
	}

	// The big host is capped at its own budget; a.example.com still gets crawled.
	if info := col.crawlInfo("example.com"); info == nil || info.Requests != 3 || !info.LimitReached {
		t.Errorf("example.com crawl = %+v", info)
	}
	if info := col.crawlInfo("a.example.com"); info == nil || info.Requests != 2 || info.LimitReached {
		t.Errorf("a.example.com crawl = %+v", info)
	}
	// MaxHosts counts the already-crawled host, so only 2 more fit.
	if info := col.crawlInfo("b.example.com"); info == nil || info.Skipped != "host limit reached" {
		t.Errorf("b.example.com = %+v", info)
	}
	if col.crawlInfo("done.example.com") != nil {
		t.Error("already-crawled host crawled again")
	}
}

func TestCrawlRespectsDepth(t *testing.T) {
	site := &fakeSite{pages: map[string]*fetch.Response{
		"https://example.com/":  htmlPage(`<a href="/a">a</a><a href="/b">b</a>`),
		"https://example.com/a": htmlPage(`<a href="/deep">deep</a>`),
		"https://example.com/b": htmlPage(``),
	}}
	col := &collector{}
	c := New(site, Options{MaxHosts: 1, MaxDepth: 1, MaxPagesPerHost: 100, Concurrency: 1}, quiet)
	if err := c.Discover(context.Background(), input(t, "example.com", reachable("example.com")), col.emit); err != nil {
		t.Fatal(err)
	}
	if len(site.requested) != 3 {
		t.Errorf("depth 1: requested %v", site.requested)
	}
	// /deep is discovered (reported) but not fetched.
	if len(col.byURL("https://example.com/deep")) != 1 || col.fetched("https://example.com/deep") != nil {
		t.Error("/deep should be reported but not fetched")
	}
}

func TestCrawlRedirects(t *testing.T) {
	site := &fakeSite{pages: map[string]*fetch.Response{
		"https://example.com/":          redirect("https://www.example.com/"),
		"https://www.example.com/":      htmlPage(`<a href="/docs">docs</a><a href="/out">out</a>`),
		"https://www.example.com/docs":  redirect("https://www.example.com/docs/"),
		"https://www.example.com/docs/": htmlPage(`<title>Docs</title>`),
		"https://www.example.com/out":   redirect("https://elsewhere.org/"),
	}}
	col := &collector{}
	c := New(site, Options{MaxHosts: 5, MaxDepth: 2, MaxPagesPerHost: 20, Concurrency: 2}, quiet)
	err := c.Discover(context.Background(), input(t, "example.com", reachable("example.com"), reachable("www.example.com")), col.emit)
	if err != nil {
		t.Fatal(err)
	}
	if r := col.fetched("https://example.com/"); r == nil || r.Status != 301 || r.Redirect != "https://www.example.com/" {
		t.Errorf("apex redirect metadata = %+v", r)
	}
	var fromRedirect bool
	for _, f := range col.byURL("https://www.example.com/") {
		fromRedirect = fromRedirect || f.Source == discovery.SourceRedirect
	}
	if !fromRedirect {
		t.Error("www should carry the redirect source")
	}
	// A trailing-slash redirect to the same canonical URL is followed.
	if r := col.fetched("https://www.example.com/docs/"); r == nil || r.Title != "Docs" {
		t.Errorf("docs/ = %+v", r)
	}
	// Cross-host redirect: www is fetched once, by its own crawl.
	n := 0
	for _, u := range site.requested {
		if u == "https://www.example.com/" {
			n++
		}
		if u == "https://elsewhere.org/" {
			t.Error("followed redirect out of scope")
		}
	}
	if n != 1 {
		t.Errorf("www root requested %d times, want 1", n)
	}
}

func TestCrawlUsesEnteredPathAndProbedScheme(t *testing.T) {
	site := &fakeSite{pages: map[string]*fetch.Response{
		"http://www.example.com/docs": htmlPage(`<title>Docs</title>`),
	}}
	h := discovery.HostView{Name: "www.example.com", HTTP: &discovery.HTTPInfo{Reachable: true, Scheme: "http", URL: "http://www.example.com/"}}
	col := &collector{}
	c := New(site, Options{MaxHosts: 1, MaxDepth: 0, MaxPagesPerHost: 5, Concurrency: 1}, quiet)
	if err := c.Discover(context.Background(), input(t, "https://www.example.com/docs", h), col.emit); err != nil {
		t.Fatal(err)
	}
	if r := col.fetched("http://www.example.com/docs"); r == nil || r.Title != "Docs" {
		t.Errorf("requested = %v", site.requested)
	}
}

func TestCrawlUsesSeedsForTheirHost(t *testing.T) {
	site := &fakeSite{pages: map[string]*fetch.Response{
		"https://example.com/":         htmlPage(``),
		"https://example.com/unlinked": htmlPage(`<title>From sitemap</title>`),
	}}
	col := &collector{}
	c := New(site, Options{MaxHosts: 1, MaxDepth: 1, MaxPagesPerHost: 5, Concurrency: 1}, quiet)
	in := discovery.Input{Target: target(t, "example.com"), State: state{
		hosts: []discovery.HostView{reachable("example.com")},
		pages: []string{"https://example.com/unlinked", "https://other.example.com/x"},
	}}
	if err := c.Discover(context.Background(), in, col.emit); err != nil {
		t.Fatal(err)
	}
	if r := col.fetched("https://example.com/unlinked"); r == nil {
		t.Error("seed not crawled")
	}
	for _, u := range site.requested {
		if u == "https://other.example.com/x" {
			t.Error("seed for another host crawled")
		}
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestCrawlRecordsRedirectOnlyHostsWithoutCrawling(t *testing.T) {
	site := &fakeSite{pages: map[string]*fetch.Response{
		"https://example.com/":      htmlPage(``),
		"https://docs.example.com/": htmlPage(``),
	}}
	blog := discovery.HostView{Name: "blog.example.com", HTTP: &discovery.HTTPInfo{
		Reachable: true, Scheme: "https", URL: "https://blog.example.com/", Status: 301,
		Redirect: "https://example.com/blog", FinalURL: "https://example.com/blog", FinalStatus: 200,
	}}
	gone := discovery.HostView{Name: "gone.example.com", HTTP: &discovery.HTTPInfo{
		Reachable: true, Scheme: "https", URL: "https://gone.example.com/", Status: 404, FinalStatus: 404,
	}}
	col := &collector{}
	// MaxHosts 2: the redirect-only host must not use a slot, and the 2xx
	// host must be preferred over the 404 one.
	c := New(site, Options{MaxHosts: 2, MaxDepth: 1, MaxPagesPerHost: 5, Concurrency: 1}, quiet)
	err := c.Discover(context.Background(), input(t, "example.com",
		reachable("example.com"), blog, gone, reachable("docs.example.com")), col.emit)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range site.requested {
		if u == "https://blog.example.com/" {
			t.Error("redirect-only host was requested again")
		}
	}
	if r := col.fetched("https://blog.example.com/"); r == nil || r.Status != 301 || r.Redirect != "https://example.com/blog" {
		t.Errorf("blog root = %+v", r)
	}
	if fs := col.byURL("https://example.com/blog"); len(fs) == 0 || fs[0].Source != discovery.SourceRedirect {
		t.Errorf("redirect target findings = %+v", fs)
	}
	if info := col.crawlInfo("blog.example.com"); info == nil || info.Skipped == "" {
		t.Errorf("blog crawl = %+v", info)
	}
	if info := col.crawlInfo("docs.example.com"); info == nil || info.Skipped != "" {
		t.Errorf("docs should be crawled before the 404 host: %+v", info)
	}
	if info := col.crawlInfo("gone.example.com"); info == nil || info.Skipped != "host limit reached" {
		t.Errorf("gone = %+v", info)
	}
}

// linkFarm serves an HTML page linking n pages under every path, so the
// crawl frontier explodes unless it is bounded.
type linkFarm struct {
	n         int
	requests  atomic.Int64
	peakGorou atomic.Int64
	delay     time.Duration
	started   chan struct{}
	once      sync.Once
}

func (f *linkFarm) Get(ctx context.Context, u string) (*fetch.Response, error) {
	if err := resource.FromContext(ctx).TakeRequest(); err != nil {
		return nil, err
	}
	f.requests.Add(1)
	if f.started != nil {
		f.once.Do(func() { close(f.started) })
	}
	if g := int64(runtime.NumGoroutine()); g > f.peakGorou.Load() {
		f.peakGorou.Store(g)
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	var b strings.Builder
	for i := 0; i < f.n; i++ {
		fmt.Fprintf(&b, `<a href="/p%d">p</a>`, i)
	}
	return htmlPage(b.String()), nil
}

func TestCrawlFrontierIsBoundedByBudget(t *testing.T) {
	farm := &linkFarm{n: 5000}
	col := &collector{}
	base := runtime.NumGoroutine()
	c := New(farm, Options{MaxHosts: 1, MaxDepth: 3, MaxPagesPerHost: 10, Concurrency: 4}, quiet)
	if err := c.Discover(context.Background(), input(t, "example.com", reachable("example.com")), col.emit); err != nil {
		t.Fatal(err)
	}
	if got := farm.requests.Load(); got != 10 {
		t.Errorf("requests = %d, want the per-host budget of 10", got)
	}
	// Workers are bounded by Concurrency, not by the 5000 links per page.
	if extra := farm.peakGorou.Load() - int64(base); extra > 20 {
		t.Errorf("goroutines grew by %d during the crawl", extra)
	}
	if info := col.crawlInfo("example.com"); info == nil || !info.LimitReached || info.Requests != 10 {
		t.Errorf("crawl info = %+v", info)
	}
	// Every link is still reported as discovered.
	if len(col.byURL("https://example.com/p4999")) == 0 {
		t.Error("links beyond the budget were not reported")
	}
}

func TestCrawlStopsAtScanRequestBudget(t *testing.T) {
	farm := &linkFarm{n: 50}
	col := &collector{}
	acct := resource.NewAccount("scan", 7)
	ctx := resource.WithAccount(context.Background(), acct)
	c := New(farm, Options{MaxHosts: 5, MaxDepth: 3, MaxPagesPerHost: 100, Concurrency: 2}, quiet)
	err := c.Discover(ctx, input(t, "example.com", reachable("example.com"), reachable("a.example.com"), reachable("b.example.com")), col.emit)
	if err != nil {
		t.Fatal(err)
	}
	if got := farm.requests.Load(); got != 7 {
		t.Errorf("requests = %d, want the scan budget of 7", got)
	}
	// Refused requests are not recorded as failed URLs.
	for _, f := range col.findings {
		if f.Error != "" {
			t.Errorf("budget refusal recorded as failure: %+v", f)
		}
	}
	skipped := 0
	for _, h := range []string{"a.example.com", "b.example.com"} {
		if info := col.crawlInfo(h); info != nil && info.Skipped == discovery.SkipRequestLimit {
			skipped++
		}
	}
	if info := col.crawlInfo("example.com"); info == nil || !info.LimitReached {
		t.Errorf("example.com crawl = %+v", info)
	}
	if skipped == 0 {
		t.Error("hosts after the budget ran out should be skipped with the request-limit reason")
	}
}

func TestCrawlStopsPromptlyOnCancel(t *testing.T) {
	farm := &linkFarm{n: 200, delay: 20 * time.Millisecond, started: make(chan struct{})}
	col := &collector{}
	ctx, cancel := context.WithCancel(context.Background())
	c := New(farm, Options{MaxHosts: 3, MaxDepth: 5, MaxPagesPerHost: 10000, Concurrency: 4, HostConcurrency: 3}, quiet)
	done := make(chan error)
	go func() {
		done <- c.Discover(ctx, input(t, "example.com", reachable("example.com"), reachable("a.example.com")), col.emit)
	}()
	<-farm.started
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("crawl did not stop after cancel")
	}
	after := farm.requests.Load()
	time.Sleep(100 * time.Millisecond)
	if farm.requests.Load() != after {
		t.Error("requests continued after Discover returned")
	}
}
