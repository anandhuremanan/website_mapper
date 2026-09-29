package htmlcrawl

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"websitemapper/internal/cache"
	"websitemapper/internal/fetch"
)

func cachedCrawler(site Fetcher) (*Crawler, *Cache) {
	pc := cache.New[Page](cache.Options{Name: "page", MaxCost: 4 << 20})
	return New(site, Options{MaxHosts: 5, MaxDepth: 2, MaxPagesPerHost: 50, Concurrency: 2, Cache: pc, CacheTTL: 15 * time.Minute}, quiet), pc
}

// summary reduces findings to comparable strings (URL, source, hint and
// response status/title), ignoring cache timestamps.
func summary(col *collector) []string {
	var out []string
	for _, f := range col.findings {
		s := f.URL + " " + string(f.Source) + " " + string(f.Hint) + " " + f.Host
		if f.Response != nil {
			s += " status=" + fmt.Sprint(f.Response.Status) + " title=" + f.Response.Title
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func TestPageCacheReusesMetadataWithoutRequests(t *testing.T) {
	site := &fakeSite{pages: map[string]*fetch.Response{
		"https://example.com/": htmlPage(`<title>Home</title>
			<a href="/about">About</a>
			<a href="https://blog.example.com/post">Blog</a>
			<script src="/app.js"></script><img src="/logo.png">
			<link rel="stylesheet" href="/site.css">`),
		"https://example.com/about": htmlPage(`<title>About</title><a href="/team">Team</a>`),
		"https://example.com/team":  htmlPage(`<title>Team</title>`),
	}}
	c, pc := cachedCrawler(site)

	first := &collector{}
	if err := c.Discover(context.Background(), input(t, "example.com", reachable("example.com")), first.emit); err != nil {
		t.Fatal(err)
	}
	requestsAfterFirst := len(site.requested)
	if requestsAfterFirst != 3 {
		t.Fatalf("first crawl requested %v", site.requested)
	}

	second := &collector{}
	if err := c.Discover(context.Background(), input(t, "example.com", reachable("example.com")), second.emit); err != nil {
		t.Fatal(err)
	}
	if len(site.requested) != requestsAfterFirst {
		t.Errorf("cached crawl made requests: %v", site.requested[requestsAfterFirst:])
	}
	// Same discoveries: links, other hosts, assets and page metadata.
	if a, b := summary(first), summary(second); strings.Join(a, "\n") != strings.Join(b, "\n") {
		t.Errorf("cached crawl differs:\nfirst:\n%s\nsecond:\n%s", strings.Join(a, "\n"), strings.Join(b, "\n"))
	}
	for _, want := range []string{"https://blog.example.com/post", "https://example.com/app.js", "https://example.com/logo.png", "https://example.com/site.css"} {
		fs := second.byURL(want)
		if len(fs) == 0 || fs[0].CachedAt.IsZero() {
			t.Errorf("%s: %+v", want, fs)
		}
	}
	if r := second.fetched("https://example.com/about"); r == nil || r.Title != "About" || r.CachedAt == nil {
		t.Errorf("about = %+v", r)
	}
	if info := second.crawlInfo("example.com"); info == nil || info.Requests != 0 || info.FromCache != 3 {
		t.Errorf("crawl info = %+v", info)
	}
	// Assets were never requested, by either crawl.
	for _, u := range site.requested {
		if strings.HasSuffix(u, ".js") || strings.HasSuffix(u, ".png") || strings.HasSuffix(u, ".css") {
			t.Errorf("asset requested: %s", u)
		}
	}
	if st := pc.Stats(); st.Hits != 3 || st.Entries != 3 {
		t.Errorf("cache stats = %+v", st)
	}
}

func TestPageCacheDoesNotRetainBodies(t *testing.T) {
	big := `<title>Big</title><a href="/a">a</a>` + strings.Repeat("<p>filler text</p>", 60_000) // ~1 MB
	site := &fakeSite{pages: map[string]*fetch.Response{"https://example.com/": htmlPage(big)}}
	c, pc := cachedCrawler(site)
	c.Discover(context.Background(), input(t, "example.com", reachable("example.com")), (&collector{}).emit)
	st := pc.Stats()
	if st.Entries != 1 || st.Cost > 2048 {
		t.Errorf("a %d-byte page is cached at %d bytes; the body must not be kept", len(big), st.Cost)
	}
	p, _, ok := pc.Get("https://example.com/")
	if !ok || p.Title != "Big" || len(p.Refs) != 1 {
		t.Errorf("cached page = %+v", p)
	}
}

func TestPageCacheRetriesFailures(t *testing.T) {
	site := &fakeSite{pages: map[string]*fetch.Response{}} // every request fails
	c, pc := cachedCrawler(site)
	for i := 0; i < 2; i++ {
		col := &collector{}
		c.Discover(context.Background(), input(t, "example.com", reachable("example.com")), col.emit)
		if fs := col.byURL("https://example.com/"); len(fs) == 0 || fs[0].Error == "" {
			t.Errorf("crawl %d: failure not reported: %+v", i, fs)
		}
	}
	if len(site.requested) != 2 || pc.Stats().Entries != 0 {
		t.Errorf("failed fetches must not be cached: requests %v, entries %d", site.requested, pc.Stats().Entries)
	}
}
