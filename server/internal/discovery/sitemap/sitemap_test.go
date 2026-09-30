package sitemap

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"websitemapper/internal/cache"
	"websitemapper/internal/discovery"
	"websitemapper/internal/fetch"
	"websitemapper/internal/resource"
)

type fakeSite struct {
	mu        sync.Mutex
	files     map[string]*fetch.Response
	requested []string
}

func (s *fakeSite) Get(_ context.Context, u string) (*fetch.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requested = append(s.requested, u)
	if r, ok := s.files[u]; ok {
		return r, nil
	}
	return &fetch.Response{URL: u, StatusCode: 404, ContentType: "text/html", Header: http.Header{}}, nil
}

func text(body string) *fetch.Response {
	return &fetch.Response{StatusCode: 200, ContentType: "text/plain", Body: []byte(body), Header: http.Header{}}
}

func xmlDoc(body string) *fetch.Response {
	return &fetch.Response{StatusCode: 200, ContentType: "application/xml", Body: []byte(body), Header: http.Header{}}
}

func gz(body string) *fetch.Response {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write([]byte(body))
	w.Close()
	return &fetch.Response{StatusCode: 200, ContentType: "application/x-gzip", Body: buf.Bytes(), Header: http.Header{}}
}

func urlset(urls ...string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
	for _, u := range urls {
		fmt.Fprintf(&b, "<url><loc> %s </loc><lastmod>2026-01-01</lastmod></url>", u)
	}
	b.WriteString("</urlset>")
	return b.String()
}

type state struct{ hosts []discovery.HostView }

func (s state) Hosts() []discovery.HostView { return append([]discovery.HostView(nil), s.hosts...) }
func (s state) PageURLs() []string          { return nil }

func reachable(name string) discovery.HostView {
	return discovery.HostView{Name: name, HTTP: &discovery.HTTPInfo{Reachable: true, Scheme: "https", URL: "https://" + name + "/"}}
}

type collector struct {
	mu       sync.Mutex
	findings []discovery.Finding
}

func (c *collector) emit(f discovery.Finding) {
	c.mu.Lock()
	c.findings = append(c.findings, f)
	c.mu.Unlock()
}

func (c *collector) urls(src discovery.Source) []string {
	var out []string
	for _, f := range c.findings {
		if f.URL != "" && f.Source == src {
			out = append(out, f.URL)
		}
	}
	sort.Strings(out)
	return out
}

func (c *collector) info(host string) *discovery.SitemapInfo {
	for _, f := range c.findings {
		if f.Host == host && f.Sitemap != nil {
			return f.Sitemap
		}
	}
	return nil
}

func run(t *testing.T, e *Engine, hosts ...discovery.HostView) *collector {
	t.Helper()
	tgt, _ := discovery.ParseTarget("example.com")
	col := &collector{}
	if err := e.Discover(context.Background(), discovery.Input{Target: tgt, State: state{hosts}}, col.emit); err != nil {
		t.Fatal(err)
	}
	return col
}

func TestParseRobotsUsesOnlySitemapLines(t *testing.T) {
	got := ParseRobots([]byte("User-agent: *\nDisallow: /admin\nAllow: /public\n# Sitemap: /commented.xml\nSITEMAP: /sitemap_index.xml\nSitemap: https://cdn.example.com/s.xml  # trailing\n"),
		"https://example.com/robots.txt")
	want := []string{"https://example.com/sitemap_index.xml", "https://cdn.example.com/s.xml"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("sitemaps = %v", got)
	}
}

func TestParseSitemapVariants(t *testing.T) {
	sm, urls := ParseSitemap([]byte(`<sitemapindex><sitemap><loc>https://example.com/a.xml</loc></sitemap></sitemapindex>`), 10)
	if len(sm) != 1 || len(urls) != 0 {
		t.Errorf("index: %v %v", sm, urls)
	}
	_, urls = ParseSitemap(gz(urlset("https://example.com/1", "https://example.com/2")).Body, 10)
	if len(urls) != 2 {
		t.Errorf("gzip urlset: %v", urls)
	}
	_, urls = ParseSitemap([]byte("https://example.com/x\nnot a url\nhttps://example.com/y\n"), 10)
	if len(urls) != 2 {
		t.Errorf("text sitemap: %v", urls)
	}
	_, urls = ParseSitemap([]byte(urlset("https://example.com/1", "https://example.com/2", "https://example.com/3")), 2)
	if len(urls) != 2 {
		t.Errorf("limit: %v", urls)
	}
}

func TestEngineReadsRobotsAndSitemaps(t *testing.T) {
	site := &fakeSite{files: map[string]*fetch.Response{
		"https://example.com/robots.txt": text("User-agent: *\nDisallow: /secret-admin\nSitemap: https://example.com/sitemap_index.xml\nSitemap: https://other.org/sitemap.xml\n"),
		"https://example.com/sitemap_index.xml": xmlDoc(`<sitemapindex><sitemap><loc>https://example.com/posts.xml.gz</loc></sitemap>` +
			`<sitemap><loc>https://example.com/pages.xml</loc></sitemap></sitemapindex>`),
		"https://example.com/posts.xml.gz": gz(urlset("https://example.com/blog/1", "https://example.com/blog/2")),
		"https://example.com/pages.xml":    xmlDoc(urlset("https://example.com/about", "https://docs.example.com/guide", "https://elsewhere.net/x")),
	}}
	col := run(t, New(site, Options{MaxHosts: 10, MaxFilesPerHost: 5, MaxURLsPerHost: 100, Concurrency: 2}), reachable("example.com"))

	want := []string{"https://docs.example.com/guide", "https://example.com/about", "https://example.com/blog/1", "https://example.com/blog/2"}
	if got := col.urls(discovery.SourceSitemap); !contains(got, want) {
		t.Errorf("sitemap URLs = %v", got)
	}
	for _, f := range col.findings {
		if strings.Contains(f.URL, "secret-admin") {
			t.Error("a Disallow path was reported as a route")
		}
		if strings.Contains(f.URL, "elsewhere.net") {
			t.Error("out-of-scope URL reported")
		}
		if f.Hint == discovery.HintSitemap && f.Response != nil {
			t.Error("listed URLs must not be marked as fetched")
		}
	}
	for _, u := range site.requested {
		if strings.Contains(u, "other.org") {
			t.Error("fetched a sitemap on another domain")
		}
	}
	if info := col.info("example.com"); info == nil || info.RobotsStatus != 200 || info.Files != 3 || info.URLs != 4 || info.LimitReached {
		t.Errorf("info = %+v", info)
	}
}

func TestEngineFallsBackToSitemapXMLAndRespectsLimits(t *testing.T) {
	site := &fakeSite{files: map[string]*fetch.Response{
		"https://example.com/sitemap.xml": xmlDoc(urlset("https://example.com/1", "https://example.com/2", "https://example.com/3")),
	}}
	col := run(t, New(site, Options{MaxHosts: 1, MaxFilesPerHost: 5, MaxURLsPerHost: 2}), reachable("example.com"), reachable("a.example.com"))
	if info := col.info("example.com"); info == nil || info.RobotsStatus != 404 || info.Files != 1 || info.URLs != 2 || !info.LimitReached {
		t.Errorf("example.com = %+v", info)
	}
	if info := col.info("a.example.com"); info == nil || info.Skipped != discovery.SkipHostLimit {
		t.Errorf("a.example.com = %+v", info)
	}
	// A 404 robots.txt or sitemap is not listed as a route.
	for _, f := range col.findings {
		if f.Response != nil && f.Response.Status == 404 {
			t.Errorf("404 file reported: %s", f.URL)
		}
	}
}

func TestEngineCachesFiles(t *testing.T) {
	site := &fakeSite{files: map[string]*fetch.Response{
		"https://example.com/robots.txt":  text("Sitemap: https://example.com/sitemap.xml"),
		"https://example.com/sitemap.xml": xmlDoc(urlset("https://example.com/1")),
	}}
	c := cache.New[File](cache.Options{Name: "sitemap", MaxCost: 1 << 20})
	e := New(site, Options{MaxHosts: 5, MaxFilesPerHost: 5, MaxURLsPerHost: 10, Cache: c, TTL: 15 * time.Minute})
	run(t, e, reachable("example.com"))
	col := run(t, e, reachable("example.com"))
	if len(site.requested) != 2 {
		t.Errorf("second run made requests: %v", site.requested)
	}
	if got := col.urls(discovery.SourceSitemap); len(got) != 1 {
		t.Errorf("cached run lost URLs: %v", got)
	}
	for _, f := range col.findings {
		if f.URL != "" && f.CachedAt.IsZero() {
			t.Errorf("cached finding not marked: %+v", f)
		}
	}
}

func TestEngineStopsOnBudget(t *testing.T) {
	e := New(budgetFetcher{}, Options{MaxHosts: 5})
	col := run(t, e, reachable("example.com"))
	if len(col.findings) != 0 {
		t.Errorf("findings after budget: %+v", col.findings)
	}
}

type budgetFetcher struct{}

func (budgetFetcher) Get(context.Context, string) (*fetch.Response, error) {
	return nil, errors.Join(errors.New("x"), resourceBudget())
}

func contains(have, want []string) bool {
	set := map[string]bool{}
	for _, h := range have {
		set[h] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

func resourceBudget() error { return resource.ErrBudgetExhausted }
