package store_test

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"websitemapper/internal/classify"
	"websitemapper/internal/discovery"
	"websitemapper/internal/results"
	"websitemapper/internal/scan"
	"websitemapper/internal/store"
	"websitemapper/internal/store/storetest"
)

func openResult(t testing.TB, s *store.Store, id string) scan.ResultReader {
	t.Helper()
	r, err := s.OpenResult(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func link(url string) discovery.Finding {
	return discovery.Finding{URL: url, Source: discovery.SourceHTML, Hint: discovery.HintLink}
}

func TestHostPages(t *testing.T) {
	s := storetest.New(t, store.Options{})
	record(t, s, "scan", func(a *results.Aggregator) {
		a.Add(discovery.Finding{Host: "example.com", Source: discovery.SourceTarget})
		for i := 0; i < 24; i++ {
			a.Add(discovery.Finding{Host: fmt.Sprintf("h%02d.example.com", i), Source: discovery.SourceCT})
		}
		a.Add(link("https://h05.example.com/x"))
	})
	r := openResult(t, s, "scan")

	if sum := r.Summary(); sum.ScanID != "scan" || len(sum.Hosts) != 0 {
		t.Errorf("summary = %+v", sum)
	}
	var names []string
	after, pages := "", 0
	for {
		page, err := r.Hosts(ctx, scan.HostQuery{After: after, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, h := range page.Hosts {
			names = append(names, h.Hostname)
		}
		if after = page.Next; after == "" {
			break
		}
	}
	// Display order: the apex first, then the rest by name.
	if pages != 3 || len(names) != 25 || names[0] != "example.com" || names[1] != "h00.example.com" || names[24] != "h23.example.com" {
		t.Errorf("pages = %d, hosts = %v", pages, names)
	}

	page, err := r.Hosts(ctx, scan.HostQuery{Search: "H05", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Hosts) != 1 || page.Hosts[0].Hostname != "h05.example.com" || page.Hosts[0].Counts.URLs != 1 || page.Next != "" {
		t.Errorf("search = %+v", page)
	}
	// "_" and "%" are text, not wildcards.
	if page, _ := r.Hosts(ctx, scan.HostQuery{Search: "h_5"}); len(page.Hosts) != 0 {
		t.Errorf("wildcard search matched %d hosts", len(page.Hosts))
	}
	if _, err := r.Hosts(ctx, scan.HostQuery{After: "not a cursor"}); !errors.Is(err, scan.ErrBadQuery) {
		t.Errorf("bad cursor: %v", err)
	}
}

func TestURLPages(t *testing.T) {
	s := storetest.New(t, store.Options{})
	record(t, s, "scan", func(a *results.Aggregator) {
		for h := 0; h < 3; h++ {
			host := fmt.Sprintf("h%d.example.com", h)
			for i := 0; i < 40; i++ {
				a.Add(link(fmt.Sprintf("https://%s/page-%02d", host, i)))
			}
			a.Add(discovery.Finding{URL: "https://" + host + "/app.js", Source: discovery.SourceHTML, Hint: discovery.HintScript})
			a.Add(link("https://" + host + "/api/v1/users"))
			a.Add(discovery.Finding{URL: "https://" + host + "/login", Source: discovery.SourceHTML, Hint: discovery.HintForm})
		}
		a.Add(discovery.Finding{URL: "https://h1.example.com/page-07", Source: discovery.SourceHTML,
			Response: &discovery.Response{Status: 200, ContentType: "text/html", Title: "Pricing 100%"}})
	})
	r := openResult(t, s, "scan")

	collect := func(q scan.URLQuery) (urls []results.URL, pages int) {
		t.Helper()
		for {
			next, err := r.URLs(ctx, q, func(u results.URL) error { urls = append(urls, u); return nil })
			if err != nil {
				t.Fatal(err)
			}
			pages++
			if q.After = next; next == "" {
				return urls, pages
			}
		}
	}

	// Every URL, in pages, without gaps or repeats, in (host, path) order.
	all, pages := collect(scan.URLQuery{Limit: 50})
	if len(all) != 129 || pages != 3 {
		t.Fatalf("read %d URLs in %d pages, want 129 in 3", len(all), pages)
	}
	seen := map[string]bool{}
	for i, u := range all {
		if seen[u.URL] {
			t.Fatalf("URL listed twice: %s", u.URL)
		}
		seen[u.URL] = true
		if i > 0 && all[i-1].Hostname == u.Hostname && all[i-1].URL >= u.URL {
			t.Fatalf("out of order: %s then %s", all[i-1].URL, u.URL)
		}
	}
	// A page that ends exactly at the last URL has no next page.
	if _, pages := collect(scan.URLQuery{Limit: 129}); pages != 1 {
		t.Errorf("exact-size page reported %d pages", pages)
	}

	if urls, _ := collect(scan.URLQuery{Host: "h1.example.com", Limit: 30}); len(urls) != 43 || urls[0].Hostname != "h1.example.com" {
		t.Errorf("host filter: %d URLs", len(urls))
	}
	if urls, _ := collect(scan.URLQuery{Host: "missing.example.com"}); len(urls) != 0 {
		t.Errorf("unknown host: %d URLs", len(urls))
	}
	if urls, _ := collect(scan.URLQuery{Types: []classify.Type{classify.TypeAPI}}); len(urls) != 3 {
		t.Errorf("api filter: %d URLs", len(urls))
	}
	if urls, _ := collect(scan.URLQuery{Types: []classify.Type{classify.TypePage, classify.TypeUnknown}, Limit: 100}); len(urls) != 123 {
		t.Errorf("page+unknown filter: %d URLs, want 123", len(urls))
	}
	if urls, _ := collect(scan.URLQuery{Types: []classify.Type{classify.TypeAsset}, Host: "h2.example.com"}); len(urls) != 1 || urls[0].Path != "/app.js" {
		t.Errorf("asset filter on one host: %+v", urls)
	}

	// Search matches the path, the title, or (across hosts) the hostname.
	if urls, _ := collect(scan.URLQuery{Search: "PAGE-3"}); len(urls) != 30 {
		t.Errorf("path search: %d URLs, want 30", len(urls))
	}
	if urls, _ := collect(scan.URLQuery{Search: "100%"}); len(urls) != 1 || urls[0].Title != "Pricing 100%" {
		t.Errorf("title search: %+v", urls)
	}
	if urls, _ := collect(scan.URLQuery{Search: "h2.example"}); len(urls) != 43 {
		t.Errorf("hostname search: %d URLs, want 43", len(urls))
	}
	if urls, _ := collect(scan.URLQuery{Host: "h2.example.com", Search: "h2.example"}); len(urls) != 0 {
		t.Errorf("within a host, the hostname is not searched: %d URLs", len(urls))
	}

	if _, err := r.URLs(ctx, scan.URLQuery{After: "%%%"}, func(results.URL) error { return nil }); !errors.Is(err, scan.ErrBadQuery) {
		t.Errorf("bad cursor: %v", err)
	}
	stop := errors.New("stop")
	if _, err := r.URLs(ctx, scan.URLQuery{}, func(results.URL) error { return stop }); !errors.Is(err, stop) {
		t.Errorf("callback error: %v", err)
	}
}

// treeNames lists a level's children as "name(total)" with "+" for nodes
// that can be expanded.
func treeNames(tr scan.Tree) []string {
	var out []string
	for _, c := range tr.Children {
		s := fmt.Sprintf("%s(%d)", c.Name, c.Total)
		if c.HasChildren {
			s += "+"
		}
		out = append(out, s)
	}
	return out
}

func paths(urls []results.URL) []string {
	out := []string{}
	for _, u := range urls {
		out = append(out, u.URL)
	}
	return out
}

func TestTree(t *testing.T) {
	s := storetest.New(t, store.Options{})
	record(t, s, "scan", func(a *results.Aggregator) {
		for _, u := range []string{
			"/", "/?lang=en",
			"/a", "/a-b", "/a/x", "/a/y/z", "/a0", "/a?q=1", // names that sort between a node's own ranges
			"/b/c",           // a node with no URL of its own
			"/c?x=1",         // a node known only with a query
			"/e/f", "/e?z=1", // deeper paths first, then the node with a query
		} {
			a.Add(link("https://example.com" + u))
		}
		a.Add(link("http://example.com/a")) // the same path over http
		a.Add(discovery.Finding{URL: "https://example.com/d.js", Source: discovery.SourceHTML, Hint: discovery.HintScript})
		a.Add(link("https://other.example.com/only"))
	})
	r := openResult(t, s, "scan")
	tree := func(q scan.TreeQuery) scan.Tree {
		t.Helper()
		q.Host = "example.com"
		tr, err := r.Tree(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		return tr
	}

	root := tree(scan.TreeQuery{})
	if root.Path != "/" || root.Total != 13 || root.Next != "" {
		t.Errorf("root = %+v", root)
	}
	if got, want := paths(root.URLs), []string{"https://example.com/", "https://example.com/?lang=en"}; !reflect.DeepEqual(got, want) {
		t.Errorf("root URLs = %v, want %v", got, want)
	}
	want := []string{"a(5)+", "a-b(1)", "a0(1)", "b(1)+", "c(1)", "e(2)+"}
	if got := treeNames(root); !reflect.DeepEqual(got, want) {
		t.Errorf("root children = %v, want %v", got, want)
	}
	a := root.Children[0]
	if got, want := paths(a.URLs), []string{"http://example.com/a", "https://example.com/a", "https://example.com/a?q=1"}; a.Path != "/a" || !reflect.DeepEqual(got, want) {
		t.Errorf("a = %+v", a)
	}
	if b := root.Children[3]; len(b.URLs) != 0 || b.Path != "/b" {
		t.Errorf("b = %+v", b)
	}
	if e := root.Children[5]; !reflect.DeepEqual(paths(e.URLs), []string{"https://example.com/e?z=1"}) {
		t.Errorf("e = %+v", e)
	}

	// Assets are part of the tree only on request.
	withAssets := tree(scan.TreeQuery{Assets: true})
	if got := treeNames(withAssets); withAssets.Total != 14 || len(got) != 7 || got[5] != "d.js(1)" {
		t.Errorf("with assets: total %d, children %v", withAssets.Total, got)
	}

	// One level down, by either spelling of the path.
	for _, p := range []string{"/a", "/a/"} {
		sub := tree(scan.TreeQuery{Path: p})
		if got := treeNames(sub); sub.Path != "/a" || sub.Total != 5 || len(sub.URLs) != 3 || !reflect.DeepEqual(got, []string{"x(1)", "y(1)+"}) {
			t.Errorf("tree(%q) = %+v", p, sub)
		}
	}
	if leaf := tree(scan.TreeQuery{Path: "/a/y"}); !reflect.DeepEqual(treeNames(leaf), []string{"z(1)"}) || len(leaf.URLs) != 0 {
		t.Errorf("/a/y = %+v", leaf)
	}
	if none := tree(scan.TreeQuery{Path: "/nothing/here"}); none.Total != 0 || len(none.Children) != 0 {
		t.Errorf("unknown path = %+v", none)
	}

	// Children in pages: the same list, with the parent's own URLs and
	// total on the first page only.
	var got []string
	after, pages := "", 0
	for {
		page := tree(scan.TreeQuery{After: after, Limit: 2})
		pages++
		if (pages == 1) != (page.Total == 13 && len(page.URLs) == 2) {
			t.Errorf("page %d: total %d, %d URLs", pages, page.Total, len(page.URLs))
		}
		got = append(got, treeNames(page)...)
		if after = page.Next; after == "" {
			break
		}
	}
	if pages != 3 || !reflect.DeepEqual(got, want) {
		t.Errorf("paged children = %v in %d pages, want %v in 3", got, pages, want)
	}

	if _, err := r.Tree(ctx, scan.TreeQuery{Host: "missing.example.com"}); !errors.Is(err, scan.ErrNotFound) {
		t.Errorf("unknown host: %v", err)
	}
	if _, err := r.Tree(ctx, scan.TreeQuery{Host: "example.com", Path: "no-slash"}); !errors.Is(err, scan.ErrBadQuery) {
		t.Errorf("bad path: %v", err)
	}
	if _, err := r.Tree(ctx, scan.TreeQuery{Host: "example.com", After: "x"}); !errors.Is(err, scan.ErrBadQuery) {
		t.Errorf("bad cursor: %v", err)
	}
}

// TestPageCost reports how long pages take on a large result. Run with -v;
// skipped with -short.
func TestPageCost(t *testing.T) {
	if testing.Short() {
		t.Skip("measurement")
	}
	const hosts, perHost = 100, 5000
	s := storetest.New(t, store.Options{})
	record(t, s, "large", func(a *results.Aggregator) {
		for h := 0; h < hosts; h++ {
			name := fmt.Sprintf("host-%03d.example.com", h)
			for u := 0; u < perHost; u++ {
				a.Add(discovery.Finding{URL: fmt.Sprintf("https://%s/section-%d/article-%d", name, u%50, u),
					Source: discovery.SourceArchive, Hint: discovery.HintArchive})
			}
		}
		// One API-like URL at the very end: the worst case for a type filter.
		a.Add(link(fmt.Sprintf("https://host-%03d.example.com/zz/api/v1/last", hosts-1)))
	})
	r := openResult(t, s, "large")
	timed := func(name string, fn func() int) {
		start := time.Now()
		n := fn()
		t.Logf("%-42s %6d rows  %s", name, n, time.Since(start).Round(100*time.Microsecond))
	}
	count := func(q scan.URLQuery) func() int {
		return func() int {
			n := 0
			if _, err := r.URLs(ctx, q, func(results.URL) error { n++; return nil }); err != nil {
				t.Fatal(err)
			}
			return n
		}
	}
	t.Logf("%d URLs on %d hosts", hosts*perHost+1, hosts)
	timed("first page of all URLs", count(scan.URLQuery{Limit: 100}))
	timed("first page of one host", count(scan.URLQuery{Host: "host-050.example.com", Limit: 100}))
	timed("rare type (scans everything)", count(scan.URLQuery{Types: []classify.Type{classify.TypeAPI}, Limit: 100}))
	timed("search across hosts, first page", count(scan.URLQuery{Search: "article-49", Limit: 100}))
	timed("search with no match (scans everything)", count(scan.URLQuery{Search: "no-such-text", Limit: 100}))
	timed("tree: root of one host (50 children)", func() int {
		tr, err := r.Tree(ctx, scan.TreeQuery{Host: "host-050.example.com", Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		return len(tr.Children)
	})
	timed("tree: one section (100 children)", func() int {
		tr, err := r.Tree(ctx, scan.TreeQuery{Host: "host-050.example.com", Path: "/section-7", Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		return len(tr.Children)
	})
}
