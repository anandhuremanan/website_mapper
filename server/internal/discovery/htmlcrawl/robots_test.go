package htmlcrawl

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"websitemapper/internal/discovery"
	"websitemapper/internal/fetch"
	"websitemapper/internal/results"
)

func robotsFile(body string) *fetch.Response {
	return &fetch.Response{StatusCode: 200, ContentType: "text/plain", Header: http.Header{}, Body: []byte(body)}
}

func robotsOptions() Options {
	return Options{MaxHosts: 5, MaxDepth: 2, MaxPagesPerHost: 20, Concurrency: 1, RespectRobots: true, RobotsAgent: "WebsiteMapperBot"}
}

func requestedSet(s *fakeSite) map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := map[string]bool{}
	for _, u := range s.requested {
		m[u] = true
	}
	return m
}

func TestCrawlRespectsRobotsByDefault(t *testing.T) {
	site := &fakeSite{pages: map[string]*fetch.Response{
		"https://example.com/robots.txt":     robotsFile("User-agent: *\nDisallow: /private\nDisallow: /secret-admin-panel\n\nUser-agent: OtherBot\nDisallow: /\n"),
		"https://example.com/":               htmlPage(`<a href="/public">p</a><a href="/private/report">r</a><a href="/private">r</a>`),
		"https://example.com/public":         htmlPage(`<title>Public</title>`),
		"https://example.com/private/report": htmlPage(`<title>Should not be fetched</title>`),
	}}
	col := &collector{}
	c := New(site, robotsOptions(), quiet)
	if err := c.Discover(context.Background(), input(t, "example.com", reachable("example.com")), col.emit); err != nil {
		t.Fatal(err)
	}

	req := requestedSet(site)
	if !req["https://example.com/robots.txt"] || !req["https://example.com/public"] {
		t.Errorf("requested = %v", site.requested)
	}
	// Disallowed paths are never requested...
	for _, u := range []string{"https://example.com/private/report", "https://example.com/private"} {
		if req[u] {
			t.Errorf("disallowed %s was requested", u)
		}
		// ...but the links themselves are still reported, flagged.
		fs := col.byURL(u)
		if len(fs) == 0 || !fs[len(fs)-1].RobotsDisallowed {
			t.Errorf("%s findings = %+v", u, fs)
		}
	}
	// Disallow entries are not treated as discovered routes.
	if fs := col.byURL("https://example.com/secret-admin-panel"); len(fs) != 0 {
		t.Errorf("robots.txt Disallow entry reported as a route: %+v", fs)
	}
	info := col.crawlInfo("example.com")
	if info == nil || info.Robots != "respected" || info.RobotsDisallowed != 2 || info.Requests != 2 {
		t.Errorf("crawl info = %+v", info)
	}
}

func TestCrawlBlockedEntirelyByRobots(t *testing.T) {
	site := &fakeSite{pages: map[string]*fetch.Response{
		"https://example.com/robots.txt": robotsFile("User-agent: WebsiteMapperBot\nDisallow: /\n"),
		"https://example.com/":           htmlPage(`<a href="/a">a</a>`),
	}}
	col := &collector{}
	c := New(site, robotsOptions(), quiet)
	if err := c.Discover(context.Background(), input(t, "example.com", reachable("example.com")), col.emit); err != nil {
		t.Fatal(err)
	}
	if len(site.requested) != 1 || site.requested[0] != "https://example.com/robots.txt" {
		t.Errorf("requested = %v, want only robots.txt", site.requested)
	}
	if info := col.crawlInfo("example.com"); info == nil || info.Requests != 0 || info.RobotsDisallowed != 1 {
		t.Errorf("crawl info = %+v", info)
	}
}

func TestCrawlContinuesWhenRobotsUnavailable(t *testing.T) {
	for name, robots := range map[string]*fetch.Response{
		"missing (connection error)": nil,
		"server error":               {StatusCode: 503, Header: http.Header{}},
		"not found":                  {StatusCode: 404, Header: http.Header{}},
	} {
		t.Run(name, func(t *testing.T) {
			site := &fakeSite{pages: map[string]*fetch.Response{
				"https://example.com/":  htmlPage(`<a href="/a">a</a>`),
				"https://example.com/a": htmlPage(``),
			}}
			if robots != nil {
				site.pages["https://example.com/robots.txt"] = robots
			}
			col := &collector{}
			c := New(site, robotsOptions(), quiet)
			if err := c.Discover(context.Background(), input(t, "example.com", reachable("example.com")), col.emit); err != nil {
				t.Fatal(err)
			}
			if !requestedSet(site)["https://example.com/a"] {
				t.Errorf("crawl did not continue: %v", site.requested)
			}
			if info := col.crawlInfo("example.com"); info == nil || info.Robots == "respected" || info.Robots == "" {
				t.Errorf("crawl info = %+v", info)
			}
		})
	}
}

func TestCrawlCanIgnoreRobotsWhenConfigured(t *testing.T) {
	site := &fakeSite{pages: map[string]*fetch.Response{
		"https://example.com/robots.txt": robotsFile("User-agent: *\nDisallow: /\n"),
		"https://example.com/":           htmlPage(``),
	}}
	opts := robotsOptions()
	opts.RespectRobots = false
	col := &collector{}
	if err := New(site, opts, quiet).Discover(context.Background(), input(t, "example.com", reachable("example.com")), col.emit); err != nil {
		t.Fatal(err)
	}
	req := requestedSet(site)
	if req["https://example.com/robots.txt"] || !req["https://example.com/"] {
		t.Errorf("requested = %v", site.requested)
	}
	if info := col.crawlInfo("example.com"); info == nil || info.Robots != "ignored" {
		t.Errorf("crawl info = %+v", info)
	}
}

// TestCrawlStopsAtScanRequestLimit checks that the scan-wide budget stops
// the crawl without recording failed URLs.
func TestCrawlStopsAtScanRequestLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<a href="/1">1</a><a href="/2">2</a><a href="/3">3</a><a href="/4">4</a>`))
	}))
	defer srv.Close()

	client := fetch.New(fetch.Options{AllowPrivate: true, Timeout: 2 * time.Second})
	ctx := fetch.WithBudget(context.Background(), fetch.NewBudget(2, 0))
	host := discovery.HostView{Name: "127.0.0.1", HTTP: &discovery.HTTPInfo{Reachable: true, Scheme: "http", URL: srv.URL + "/"}}
	in := discovery.Input{Target: discovery.Target{Domain: "127.0.0.1", StartURL: srv.URL + "/"}, State: state{hosts: []discovery.HostView{host}}}

	col := &collector{}
	opts := robotsOptions()
	if err := New(client, opts, quiet).Discover(ctx, in, col.emit); err != nil {
		t.Fatal(err)
	}
	for _, f := range col.findings {
		if f.Error != "" {
			t.Errorf("budget exhaustion recorded as a failed URL: %+v", f)
		}
	}
	if info := col.crawlInfo("127.0.0.1"); info == nil || info.Requests != 1 {
		t.Errorf("crawl info = %+v (robots.txt + 1 page = budget of 2)", info)
	}
}

// TestResultsDoNotPersistBodiesOrSensitiveHeaders crawls a real HTTP server
// through the real client and checks what ends up in the stored result.
func TestResultsDoNotPersistBodiesOrSensitiveHeaders(t *testing.T) {
	const (
		bodyMarker   = "BODY-MARKER-7f3a9c"
		cookieValue  = "session-s3cr3t-cookie"
		authValue    = "Bearer leaked-token-123"
		customHeader = "internal-header-value-42"
	)
	var mu sync.Mutex
	var requestHeaders []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requestHeaders = append(requestHeaders, r.Header.Clone())
		mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: cookieValue})
		w.Header().Set("WWW-Authenticate", authValue)
		w.Header().Set("X-Internal", customHeader)
		w.Header().Set("Server", "test-server")
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><title>Visible title</title></head><body>` + bodyMarker +
			`<a href="/next">next</a><script>var token="` + bodyMarker + `";</script></body></html>`))
	}))
	defer srv.Close()

	tgt := discovery.Target{Domain: "127.0.0.1", StartURL: srv.URL + "/"}
	agg := results.NewAggregator(tgt, 0)
	host := discovery.HostView{Name: "127.0.0.1", HTTP: &discovery.HTTPInfo{Reachable: true, Scheme: "http", URL: srv.URL + "/"}}
	in := discovery.Input{Target: tgt, State: state{hosts: []discovery.HostView{host}}}

	const ua = "WebsiteMapperBot/0.1 (+https://mapper.example/bot)"
	client := fetch.New(fetch.Options{AllowPrivate: true, Timeout: 2 * time.Second, UserAgent: ua})
	if err := New(client, robotsOptions(), quiet).Discover(context.Background(), in, agg.Add); err != nil {
		t.Fatal(err)
	}

	res := agg.Result()
	out, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	stored := string(out)
	if !strings.Contains(stored, "Visible title") || !strings.Contains(stored, "test-server") {
		t.Fatalf("expected metadata missing from result: %s", stored)
	}
	for _, secret := range []string{bodyMarker, cookieValue, "Bearer", "leaked-token", customHeader, "Set-Cookie", "sid="} {
		if strings.Contains(stored, secret) {
			t.Errorf("stored result contains %q", secret)
		}
	}

	// Every request (robots.txt and pages) identified itself and sent no credentials.
	mu.Lock()
	defer mu.Unlock()
	if len(requestHeaders) < 3 {
		t.Fatalf("only %d requests made", len(requestHeaders))
	}
	for i, h := range requestHeaders {
		if h.Get("User-Agent") != ua {
			t.Errorf("request %d User-Agent = %q", i, h.Get("User-Agent"))
		}
		if h.Get("Cookie") != "" || h.Get("Authorization") != "" {
			t.Errorf("request %d sent credentials: %v", i, h)
		}
	}
}
