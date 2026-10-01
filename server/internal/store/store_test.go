package store_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"websitemapper/internal/classify"
	"websitemapper/internal/discovery"
	"websitemapper/internal/results"
	"websitemapper/internal/scan"
	"websitemapper/internal/store"
	"websitemapper/internal/store/storetest"
)

var ctx = context.Background()

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// record runs a scan the way the service does: create it, record findings
// through an Aggregator, save the result and publish the finished status.
func record(t testing.TB, s *store.Store, id string, add func(a *results.Aggregator)) {
	t.Helper()
	if err := s.Create(ctx, scan.Scan{ID: id, Status: scan.StatusRunning, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	urls, err := s.OpenURLs(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	defer urls.Close()
	tgt, _ := discovery.ParseTarget("example.com")
	a := results.NewAggregator(tgt, results.Limits{}, urls)
	if add != nil {
		add(a)
	}
	res := a.Result()
	if err := a.Err(); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveResult(ctx, scan.Result{ScanID: id, Status: scan.StatusCompleted, Counts: a.Counts(), Result: res}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.Update(ctx, scan.Scan{ID: id, Status: scan.StatusCompleted, FinishedAt: &now}); err != nil {
		t.Fatal(err)
	}
}

// load reads a result with all its URLs.
func load(t testing.TB, s *store.Store, id string) scan.Result {
	t.Helper()
	r, err := s.OpenResult(ctx, id)
	if err != nil {
		t.Fatalf("OpenResult(%s): %v", id, err)
	}
	defer r.Close()
	res := r.Summary()
	hosts, err := r.Hosts(ctx, scan.HostQuery{})
	if err != nil {
		t.Fatal(err)
	}
	res.Hosts = hosts.Hosts
	for i := range res.Hosts {
		h := &res.Hosts[i]
		h.URLs = nil
		if _, err := r.URLs(ctx, scan.URLQuery{Host: h.Hostname}, func(u results.URL) error { h.URLs = append(h.URLs, u); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	return res
}

func exists(s *store.Store, id string) bool {
	_, err := s.Get(ctx, id)
	return err == nil
}

func TestStatusRecords(t *testing.T) {
	s := storetest.New(t, store.Options{})
	sc := scan.Scan{ID: "a", Status: scan.StatusQueued, Steps: []scan.Step{{ID: "html", Status: scan.StepPending}}}
	if err := s.Create(ctx, sc); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, sc); err == nil {
		t.Error("duplicate create should fail")
	}
	if err := s.Create(ctx, scan.Scan{ID: "../escape"}); err == nil {
		t.Error("an ID that is not a plain name should be rejected")
	}

	got, err := s.Get(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	// Mutating the returned copy must not affect the store.
	got.Steps[0].Status = scan.StepDone
	if again, _ := s.Get(ctx, "a"); again.Steps[0].Status != scan.StepPending {
		t.Error("Get returned shared state")
	}
	got.Status = scan.StatusRunning
	if err := s.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.Get(ctx, "a"); again.Status != scan.StatusRunning {
		t.Error("update not applied")
	}

	if _, err := s.OpenResult(ctx, "a"); !errors.Is(err, scan.ErrNotFound) {
		t.Errorf("OpenResult before the scan finished: %v", err)
	}
	if _, err := s.Get(ctx, "missing"); !errors.Is(err, scan.ErrNotFound) {
		t.Errorf("Get missing: %v", err)
	}
	if err := s.Update(ctx, scan.Scan{ID: "missing"}); !errors.Is(err, scan.ErrNotFound) {
		t.Errorf("Update missing: %v", err)
	}
}

// TestResultRoundTrip records every kind of URL detail and reads it back.
func TestResultRoundTrip(t *testing.T) {
	s := storetest.New(t, store.Options{})
	cached := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	archived := &discovery.ArchiveInfo{FirstSeen: time.Date(2015, 1, 2, 3, 4, 5, 0, time.UTC), ContentType: "text/html"}
	record(t, s, "scan1", func(a *results.Aggregator) {
		a.Add(discovery.Finding{Host: "example.com", Source: discovery.SourceTarget})
		a.Add(discovery.Finding{Host: "example.com", DNS: &discovery.DNSInfo{Resolved: true, Addresses: []string{"93.184.216.34"}}})
		a.Add(discovery.Finding{Host: "example.com", HTTP: &discovery.HTTPInfo{Reachable: true, Status: 200, Server: "nginx/1.25"}})
		a.Add(discovery.Finding{Host: "old.example.com", Source: discovery.SourceCT})

		// A page: linked twice, listed in a sitemap, then fetched.
		a.Add(discovery.Finding{URL: "https://example.com/about", Source: discovery.SourceHTML, Hint: discovery.HintLink, From: "https://example.com/"})
		a.Add(discovery.Finding{URL: "https://example.com/about", Source: discovery.SourceSitemap, Hint: discovery.HintSitemap, From: "https://example.com/sitemap.xml"})
		a.Add(discovery.Finding{URL: "https://example.com/about", Source: discovery.SourceHTML, Response: &discovery.Response{
			Status: 200, ContentType: "text/html", Title: "About <us> & \"co\"", Server: "nginx", CachedAt: &cached}})
		// A redirect, a failed request, a form target, an asset, an archived
		// URL, a query string and the same path over http.
		a.Add(discovery.Finding{URL: "https://example.com/old", Source: discovery.SourceHTML, Hint: discovery.HintLink,
			Response: &discovery.Response{Status: 301, Redirect: "https://example.com/new"}})
		a.Add(discovery.Finding{URL: "https://example.com/down", Source: discovery.SourceHTML, Hint: discovery.HintLink, Error: "timeout"})
		a.Add(discovery.Finding{URL: "https://example.com/login", Source: discovery.SourceHTML, Hint: discovery.HintForm, Method: "POST"})
		a.Add(discovery.Finding{URL: "https://example.com/app.js", Source: discovery.SourceHTML, Hint: discovery.HintScript})
		a.Add(discovery.Finding{URL: "https://example.com/2015/post", Source: discovery.SourceArchive, Hint: discovery.HintArchive, Archive: archived})
		a.Add(discovery.Finding{URL: "https://example.com/search?q=a&page=2", Source: discovery.SourceHTML, Hint: discovery.HintLink})
		a.Add(discovery.Finding{URL: "http://example.com/about", Source: discovery.SourceHTML, Hint: discovery.HintLink})

		// Unfetched pages from the live site are crawl candidates; archived
		// and already requested ones are not.
		want := []string{"http://example.com/about", "https://example.com/search?page=2&q=a"}
		if got := a.PageURLs(); !reflect.DeepEqual(got, want) {
			t.Errorf("PageURLs = %v, want %v", got, want)
		}
	})

	res := load(t, s, "scan1")
	if len(res.Hosts) != 2 || res.Hosts[0].Hostname != "example.com" || res.Hosts[1].Hostname != "old.example.com" {
		t.Fatalf("hosts = %+v", res.Hosts)
	}
	apex, old := res.Hosts[0], res.Hosts[1]
	if apex.State != results.HostReachable || apex.DNS == nil || apex.DNS.Addresses[0] != "93.184.216.34" || apex.HTTP.Server != "nginx/1.25" {
		t.Errorf("apex = %+v", apex)
	}
	if old.URLs != nil && len(old.URLs) != 0 {
		t.Errorf("old urls = %v", old.URLs)
	}
	if apex.Counts != (results.HostCounts{URLs: 8, Pages: 6, Assets: 1}) {
		t.Errorf("apex counts = %+v", apex.Counts)
	}
	if res.Counts.URLs != 8 || res.ScanID != "scan1" || len(res.Technologies) != 1 {
		t.Errorf("header = %+v, technologies = %+v", res.Counts, res.Technologies)
	}

	// URLs come back in path order; the two schemes of /about are adjacent.
	var order []string
	byURL := map[string]results.URL{}
	for _, u := range apex.URLs {
		order = append(order, u.URL)
		byURL[u.URL] = u
	}
	wantOrder := []string{
		"https://example.com/2015/post", "http://example.com/about", "https://example.com/about", "https://example.com/app.js",
		"https://example.com/down", "https://example.com/login", "https://example.com/old", "https://example.com/search?page=2&q=a",
	}
	if !reflect.DeepEqual(order, wantOrder) {
		t.Errorf("order = %v\nwant    %v", order, wantOrder)
	}

	about := byURL["https://example.com/about"]
	wantAbout := results.URL{
		URL: "https://example.com/about", Hostname: "example.com", Path: "/about",
		Status: 200, ContentType: "text/html", Title: "About <us> & \"co\"", Server: "nginx",
		Type: classify.TypePage, TypeEvidence: "Responded with text/html",
		Sources:        []discovery.Source{discovery.SourceHTML, discovery.SourceSitemap},
		DiscoveredFrom: []string{"https://example.com/", "https://example.com/sitemap.xml"},
		Fetched:        true, State: results.URLVerified, CachedAt: &cached,
	}
	if !reflect.DeepEqual(about, wantAbout) {
		t.Errorf("about =\n%+v\nwant\n%+v", about, wantAbout)
	}
	if u := byURL["https://example.com/old"]; u.Status != 301 || u.Redirect != "https://example.com/new" || u.State != results.URLVerified {
		t.Errorf("old = %+v", u)
	}
	if u := byURL["https://example.com/down"]; u.Error != "timeout" || u.State != results.URLFetched || !u.Fetched || u.Status != 0 {
		t.Errorf("down = %+v", u)
	}
	if u := byURL["https://example.com/login"]; !reflect.DeepEqual(u.Methods, []string{"POST"}) || u.Type != classify.TypeUnknown || u.Fetched {
		t.Errorf("login = %+v", u)
	}
	if u := byURL["https://example.com/app.js"]; u.Type != classify.TypeAsset || u.AssetKind != classify.AssetJavaScript || u.State != results.URLDiscovered {
		t.Errorf("app.js = %+v", u)
	}
	if u := byURL["https://example.com/2015/post"]; u.Archived == nil || !u.Archived.FirstSeen.Equal(archived.FirstSeen) ||
		u.Archived.ContentType != "text/html" || !reflect.DeepEqual(u.Sources, []discovery.Source{discovery.SourceArchive}) {
		t.Errorf("archived = %+v", u)
	}
	if u := byURL["https://example.com/search?page=2&q=a"]; u.Path != "/search" {
		t.Errorf("search path = %q", u.Path)
	}
}

// TestManyURLs records more URLs than the writer caches, so duplicates are
// found by querying the database, across several transactions.
func TestManyURLs(t *testing.T) {
	const n = 20000
	s := storetest.New(t, store.Options{})
	record(t, s, "big", func(a *results.Aggregator) {
		for round := 0; round < 2; round++ {
			for i := 0; i < n; i++ {
				f := discovery.Finding{URL: fmt.Sprintf("https://example.com/section-%d/page-%d", i%50, i), Source: discovery.SourceArchive, Hint: discovery.HintArchive}
				if round == 1 {
					// The second pass confirms every URL from a sitemap.
					f.Source, f.Hint = discovery.SourceSitemap, discovery.HintSitemap
				}
				a.Add(f)
			}
		}
		if c := a.Counts(); c.URLs != n || c.Pages != n {
			t.Errorf("counts = %+v", c)
		}
	})
	res := load(t, s, "big")
	h := res.Hosts[0]
	if len(h.URLs) != n || h.Counts.URLs != n {
		t.Fatalf("read %d URLs (count %d), want %d", len(h.URLs), h.Counts.URLs, n)
	}
	for i, u := range h.URLs {
		if len(u.Sources) != 2 || u.Type != classify.TypePage {
			t.Fatalf("url %d = %+v, want both sources and type page", i, u)
		}
		if i > 0 && h.URLs[i-1].URL >= u.URL {
			t.Fatalf("URLs out of order at %d: %q then %q", i, h.URLs[i-1].URL, u.URL)
		}
	}
}

// A finished result is one plain file: SQLite's side files are gone.
func TestFinishedScanIsOneFile(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(store.Options{Dir: dir, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	record(t, s, "one", func(a *results.Aggregator) {
		a.Add(discovery.Finding{URL: "https://example.com/", Source: discovery.SourceTarget})
	})
	files, _ := os.ReadDir(dir)
	if len(files) != 1 || files[0].Name() != "one.db" {
		var names []string
		for _, f := range files {
			names = append(names, f.Name())
		}
		t.Errorf("files = %v, want only one.db", names)
	}
	if st := s.Stats(); st.Scans != 1 || st.Results != 1 || st.Bytes <= 0 {
		t.Errorf("stats = %+v", st)
	}
}

func TestNeverEvictsActiveScans(t *testing.T) {
	s := storetest.New(t, store.Options{MaxScans: 2})
	// Far more queued/running scans than MaxScans: none may disappear, or
	// their progress would be lost and polls would return 404.
	for i := 0; i < 10; i++ {
		st := scan.StatusQueued
		if i < 3 {
			st = scan.StatusRunning
		}
		if err := s.Create(ctx, scan.Scan{ID: fmt.Sprint("active", i), Status: st}); err != nil {
			t.Fatal(err)
		}
	}
	record(t, s, "done1", nil)
	record(t, s, "done2", nil)
	record(t, s, "done3", nil)
	for i := 0; i < 10; i++ {
		if !exists(s, fmt.Sprint("active", i)) {
			t.Errorf("active scan %d was evicted", i)
		}
	}
	if exists(s, "done1") || !exists(s, "done2") || !exists(s, "done3") {
		t.Error("expected only the oldest finished scan to be evicted")
	}
	if _, err := s.OpenResult(ctx, "done1"); !errors.Is(err, scan.ErrNotFound) {
		t.Errorf("evicted result: %v", err)
	}
}

func TestEvictsByDiskSpace(t *testing.T) {
	dir := t.TempDir()
	// An empty result's file is a few pages; allow about two of them.
	s, err := store.Open(store.Options{Dir: dir, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	record(t, s, "probe", nil)
	one := s.Stats().Bytes
	s.Close()
	os.Remove(filepath.Join(dir, "probe.db"))

	s, err = store.Open(store.Options{Dir: dir, MaxBytes: 2*one + one/2, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	record(t, s, "a", nil)
	record(t, s, "b", nil)
	record(t, s, "c", nil) // three files exceed the budget: the oldest (a) goes
	if exists(s, "a") || !exists(s, "b") || !exists(s, "c") {
		t.Errorf("after c: a=%v b=%v c=%v", exists(s, "a"), exists(s, "b"), exists(s, "c"))
	}
	if _, err := os.Stat(filepath.Join(dir, "a.db")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a.db should be deleted: %v", err)
	}
	if st := s.Stats(); st.Bytes != 2*one || st.Results != 2 {
		t.Errorf("stats = %+v, want %d bytes in 2 results", st, 2*one)
	}

	// A single result larger than the budget is still kept (the newest
	// result is never evicted); everything older makes room for it.
	record(t, s, "huge", func(a *results.Aggregator) {
		for i := 0; i < 3000; i++ {
			a.Add(discovery.Finding{URL: fmt.Sprintf("https://example.com/a-long-path-segment/page-%d", i), Source: discovery.SourceHTML})
		}
	})
	if !exists(s, "huge") || exists(s, "b") || exists(s, "c") {
		t.Errorf("huge kept = %v, b = %v, c = %v", exists(s, "huge"), exists(s, "b"), exists(s, "c"))
	}
	if got := len(load(t, s, "huge").Hosts[0].URLs); got != 3000 {
		t.Errorf("huge has %d URLs", got)
	}
}

func TestEvictsByAge(t *testing.T) {
	s := storetest.New(t, store.Options{MaxAge: time.Hour})
	if err := s.Create(ctx, scan.Scan{ID: "old", Status: scan.StatusRunning}); err != nil {
		t.Fatal(err)
	}
	long := time.Now().Add(-2 * time.Hour)
	if err := s.Update(ctx, scan.Scan{ID: "old", Status: scan.StatusCancelled, FinishedAt: &long}); err != nil {
		t.Fatal(err)
	}
	// The scan that just finished is kept; the next one to finish evicts it.
	if !exists(s, "old") {
		t.Fatal("the scan that just finished must be kept")
	}
	record(t, s, "new", nil)
	if exists(s, "old") || !exists(s, "new") {
		t.Errorf("old = %v, new = %v", exists(s, "old"), exists(s, "new"))
	}
}

// TestRestart: finished scans and their results survive a restart; scans
// that were still queued or running are marked as failed.
func TestRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(store.Options{Dir: dir, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	record(t, s, "finished", func(a *results.Aggregator) {
		a.Add(discovery.Finding{URL: "https://example.com/kept", Source: discovery.SourceHTML, Hint: discovery.HintLink})
	})
	created := time.Now().UTC()
	if err := s.Create(ctx, scan.Scan{ID: "queued", Status: scan.StatusQueued, CreatedAt: created,
		Steps: []scan.Step{{ID: "crawl", Status: scan.StepPending}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, scan.Scan{ID: "running", Status: scan.StatusRunning, CreatedAt: created,
		Steps: []scan.Step{{ID: "crawl", Status: scan.StepRunning}}}); err != nil {
		t.Fatal(err)
	}
	urls, err := s.OpenURLs(ctx, "running")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := urls.Insert(&results.URLRecord{HostID: 1, Path: "/partial", Origin: "https"}); err != nil {
		t.Fatal(err)
	}
	// Stray files in the directory are not scans.
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(dir, "broken.db"), []byte("not a database"), 0o600)
	s.Close() // as on shutdown; the running scan never saved a result

	s, err = store.Open(store.Options{Dir: dir, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := load(t, s, "finished"); len(got.Hosts) != 1 || got.Hosts[0].URLs[0].URL != "https://example.com/kept" {
		t.Errorf("finished result after restart = %+v", got.Hosts)
	}
	for _, id := range []string{"queued", "running"} {
		sc, err := s.Get(ctx, id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if sc.Status != scan.StatusFailed || sc.Error == "" || sc.FinishedAt == nil || sc.Phase != scan.PhaseDone {
			t.Errorf("%s after restart = %+v", id, sc)
		}
		if st := sc.Steps[0].Status; st != scan.StepSkipped && st != scan.StepStopped {
			t.Errorf("%s step = %s", id, st)
		}
		if _, err := s.OpenResult(ctx, id); !errors.Is(err, scan.ErrNotFound) {
			t.Errorf("%s result: %v", id, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "broken.db")); !errors.Is(err, os.ErrNotExist) {
		t.Error("an unreadable database should be removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
		t.Error("files that are not scan databases must be left alone")
	}
	// A third start changes nothing more.
	s.Close()
	if s, err = store.Open(store.Options{Dir: dir, Log: quiet}); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if st := s.Stats(); st.Scans != 3 || st.Results != 1 {
		t.Errorf("stats after second restart = %+v", st)
	}
}

// TestLargeScanCost reports what a large scan costs on disk, in time and
// in Go heap. Run with -v; skipped with -short.
func TestLargeScanCost(t *testing.T) {
	if testing.Short() {
		t.Skip("measurement")
	}
	const hosts, perHost = 200, 1000
	s := storetest.New(t, store.Options{})
	heap := func() uint64 {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	before := heap()
	var during uint64
	start := time.Now()
	record(t, s, "large", func(a *results.Aggregator) {
		archived := &discovery.ArchiveInfo{FirstSeen: time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC), ContentType: "text/html"}
		for h := 0; h < hosts; h++ {
			name := fmt.Sprintf("host-%04d.example.com", h)
			for u := 0; u < perHost; u++ {
				a.Add(discovery.Finding{URL: fmt.Sprintf("https://%s/section-%d/some-article-slug-%d", name, u%20, u),
					Source: discovery.SourceArchive, Hint: discovery.HintArchive, Archive: archived})
			}
		}
		during = heap()
	})
	elapsed := time.Since(start)
	n := hosts * perHost
	st := s.Stats()
	t.Logf("%d URLs on %d hosts: %s (%.0f URLs/s), %.1f MB on disk (%d B/URL), Go heap +%.1f MB while recording",
		n, hosts, elapsed.Round(time.Millisecond), float64(n)/elapsed.Seconds(), float64(st.Bytes)/1e6, st.Bytes/int64(n),
		float64(int64(during)-int64(before))/1e6)

	start = time.Now()
	r, err := s.OpenResult(ctx, "large")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	read := 0
	if _, err := r.URLs(ctx, scan.URLQuery{}, func(results.URL) error { read++; return nil }); err != nil {
		t.Fatal(err)
	}
	if read != n {
		t.Errorf("read %d URLs, want %d", read, n)
	}
	t.Logf("read back in %s", time.Since(start).Round(time.Millisecond))
}
