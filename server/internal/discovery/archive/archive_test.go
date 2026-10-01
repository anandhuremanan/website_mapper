package archive

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"websitemapper/internal/cache"
	"websitemapper/internal/discovery"
	"websitemapper/internal/fetch"
)

func target(t *testing.T) discovery.Target {
	t.Helper()
	tgt, err := discovery.ParseTarget("example.com")
	if err != nil {
		t.Fatal(err)
	}
	return tgt
}

// capture is one line of the archive's index.
type capture struct {
	urlkey, original, timestamp, mime string
}

func key(s string) string {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	zw.Write([]byte(s))
	zw.Close()
	return base64.RawURLEncoding.EncodeToString(buf.Bytes())
}

func unkey(t *testing.T, k string) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(k)
	if err != nil {
		t.Fatalf("resume key %q: %v", k, err)
	}
	zr, err := zlib.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("resume key %q: %v", k, err)
	}
	b, _ := io.ReadAll(zr)
	return string(b)
}

// fakeArchive answers CDX queries the way the real index does (observed
// 2026-10): captures in (urlkey, timestamp) order, collapsed to the first
// capture of each urlkey; `limit` rows per response; and, when more
// follow, a blank line and a resume key. With collapsing, that key names
// the first capture of the next URL, which was read but not returned, and
// a listing resumed from a key continues strictly after it.
type fakeArchive struct {
	t        *testing.T
	captures []capture
	calls    atomic.Int32
	// status, if set, is returned instead of a listing from request
	// number failFrom on (1-based).
	status     int
	failFrom   int32
	retryAfter string

	mu      sync.Mutex
	resumes []string // decoded resume keys received
}

func newFakeArchive(t *testing.T, captures ...capture) (*fakeArchive, string) {
	sort.SliceStable(captures, func(i, j int) bool {
		if captures[i].urlkey != captures[j].urlkey {
			return captures[i].urlkey < captures[j].urlkey
		}
		return captures[i].timestamp < captures[j].timestamp
	})
	a := &fakeArchive{t: t, captures: captures}
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	return a, srv.URL
}

func (a *fakeArchive) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := a.calls.Add(1)
	q := r.URL.Query()
	if r.URL.Path != "/cdx/search/cdx" || q.Get("matchType") != "domain" || q.Get("filter") != "statuscode:200" ||
		q.Get("collapse") != "urlkey" || q.Get("fl") != "original,timestamp,mimetype" || q.Get("showResumeKey") != "true" {
		a.t.Errorf("unexpected query %s", r.URL)
	}
	if a.status != 0 && n >= max(a.failFrom, 1) {
		if a.retryAfter != "" {
			w.Header().Set("Retry-After", a.retryAfter)
		}
		w.WriteHeader(a.status)
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	after := ""
	if k := q.Get("resumeKey"); k != "" {
		after = unkey(a.t, k)
		a.mu.Lock()
		a.resumes = append(a.resumes, after)
		a.mu.Unlock()
	}

	var out strings.Builder
	rows, last := 0, ""
	for _, c := range a.captures {
		if after != "" && c.urlkey+" "+c.timestamp <= after {
			continue
		}
		if c.urlkey == last {
			continue // collapsed
		}
		if rows == limit {
			// One capture too many was read: it becomes the resume key.
			fmt.Fprintf(&out, "\n%s\n", key(c.urlkey+" "+c.timestamp))
			break
		}
		fmt.Fprintf(&out, "%s %s %s\n", c.original, c.timestamp, c.mime)
		rows, last = rows+1, c.urlkey
	}
	w.Header().Set("Content-Type", "text/plain")
	io.WriteString(w, out.String())
}

// site is an index of n URLs under example.com: /p00 … with one capture
// each, except every third URL, which has two.
func site(n int) []capture {
	var out []capture
	for i := 0; i < n; i++ {
		path := fmt.Sprintf("/p%02d", i)
		out = append(out, capture{"com,example)" + path, "https://example.com" + path, "20200101000000", "text/html"})
		if i%3 == 0 {
			out = append(out, capture{"com,example)" + path, "https://example.com" + path, "20210101000000", "text/html"})
		}
	}
	return out
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

func (c *collector) urls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, f := range c.findings {
		out = append(out, f.URL)
	}
	return out
}

type clock struct{ now atomic.Int64 }

func (c *clock) Now() time.Time          { return time.Unix(0, c.now.Load()) }
func (c *clock) Advance(d time.Duration) { c.now.Add(int64(d)) }

func engine(baseURL string, opts Options) *Engine {
	opts.BaseURL = baseURL
	return New(fetch.New(fetch.Options{AllowPrivate: true}), opts)
}

func discover(t *testing.T, e *Engine) (*collector, error) {
	t.Helper()
	col := &collector{}
	err := e.Discover(context.Background(), discovery.Input{Target: target(t)}, col.emit)
	return col, err
}

func TestParsePage(t *testing.T) {
	body := strings.Join([]string{
		"http://example.com:80/ 20100101000000 text/html",
		"https://example.com/about 20150601120000 text/html",
		"https://EXAMPLE.com/about/ 20160601120000 text/html",
		"https://api.example.com/v1/items 20200101000000 application/json",
		"http://old.example.com/page 20090101000000 warc/revisit",
		"https://example.com/!27 20130101000000 text/html",
		"https://example.com/%22/ 20130101000000 text/html",
		"https://evil-example.com/ 20130101000000 text/html",
		"not-a-url 20130101000000 text/html",
		"https://example.com/bad-time yesterday text/html",
		"",
		key("com,example)/next 20200101000000"),
		"",
	}, "\r\n")
	p := parsePage([]byte(body), target(t))

	var got []string
	for _, e := range p.entries {
		got = append(got, e.URL+" "+e.MIME)
	}
	want := []string{
		"http://example.com/ text/html",
		"https://example.com/about text/html",
		"https://example.com/about text/html", // /about/ is the same URL; results merge it
		"https://api.example.com/v1/items application/json",
		"http://old.example.com/page ", // archive placeholder type dropped
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("entries:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if p.rows != 10 {
		t.Errorf("rows = %d, want 10 (junk and out-of-scope rows count)", p.rows)
	}
	if first := time.Unix(p.entries[1].FirstSeen, 0).UTC(); first != time.Date(2015, 6, 1, 12, 0, 0, 0, time.UTC) {
		t.Errorf("first seen = %v", first)
	}
	// The resume key is moved to the start of the URL it names.
	if got := unkey(t, p.resume); got != "com,example)/next 0" {
		t.Errorf("resume key = %q", got)
	}

	if last := parsePage([]byte("https://example.com/only 20200101000000 text/html\n"), target(t)); len(last.entries) != 1 || last.resume != "" {
		t.Errorf("last page = %+v", last)
	}
	if empty := parsePage(nil, target(t)); empty.rows != 0 || empty.resume != "" {
		t.Errorf("empty page = %+v", empty)
	}
}

func TestResumeKeyOfUnknownFormIsKeptAsIs(t *testing.T) {
	for _, k := range []string{"not base64 !", "aGVsbG8", key("no-timestamp"), key("com,example)/x notdigits")} {
		if got := fromStart(k); got != k {
			t.Errorf("fromStart(%q) = %q, want it unchanged", k, got)
		}
	}
}

// TestListsEveryURLAcrossPages: URLs captured once sit exactly where the
// archive's own resume key would skip them, on every page boundary.
func TestListsEveryURLAcrossPages(t *testing.T) {
	arch, url := newFakeArchive(t, site(23)...)
	col, err := discover(t, engine(url, Options{MaxURLs: 1000, PageSize: 4}))
	if err != nil {
		t.Fatal(err)
	}
	got := col.urls()
	if len(got) != 23 {
		t.Fatalf("listed %d URLs, want all 23: %v", len(got), got)
	}
	for i, u := range got {
		if want := fmt.Sprintf("https://example.com/p%02d", i); u != want {
			t.Fatalf("URL %d = %s, want %s", i, u, want)
		}
	}
	if calls := arch.calls.Load(); calls != 6 {
		t.Errorf("requests = %d, want 6 pages of 4", calls)
	}
	// Each page was resumed from the start of the next URL.
	for _, r := range arch.resumes {
		if !strings.HasSuffix(r, " 0") {
			t.Errorf("resumed from %q, want the start of that URL", r)
		}
	}
	f := col.findings[0]
	if f.Source != discovery.SourceArchive || f.Hint != discovery.HintArchive || f.Archive == nil ||
		f.Archive.ContentType != "text/html" || f.Response != nil || !f.CachedAt.IsZero() ||
		!f.Archive.FirstSeen.Equal(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("finding = %+v", f)
	}
}

func TestStopsAtTheLimit(t *testing.T) {
	arch, url := newFakeArchive(t, site(23)...)
	col, err := discover(t, engine(url, Options{MaxURLs: 10, PageSize: 4}))
	var partial *discovery.PartialError
	if !errors.As(err, &partial) || !strings.Contains(err.Error(), "first 10 archived URLs") {
		t.Errorf("err = %v", err)
	}
	if len(col.findings) != 10 || arch.calls.Load() != 3 {
		t.Errorf("findings = %d in %d requests, want 10 in 3 (4 + 4 + 2)", len(col.findings), arch.calls.Load())
	}

	// A listing that ends exactly at the limit is complete.
	_, url = newFakeArchive(t, site(10)...)
	if col, err := discover(t, engine(url, Options{MaxURLs: 10, PageSize: 4})); err != nil || len(col.findings) != 10 {
		t.Errorf("exact fit: %d findings, err %v", len(col.findings), err)
	}
}

func TestShortListingsAreCached(t *testing.T) {
	arch, url := newFakeArchive(t, site(5)...)
	clk := &clock{}
	c := cache.New[Listing](cache.Options{Name: "archive", MaxCost: 1 << 20, Now: clk.Now})
	e := engine(url, Options{MaxURLs: 100, PageSize: 2, Cache: c, TTL: 24 * time.Hour, FailureTTL: 5 * time.Minute})

	if col, err := discover(t, e); err != nil || len(col.findings) != 5 {
		t.Fatalf("first run: %d findings, err %v", len(col.findings), err)
	}
	calls := arch.calls.Load()

	clk.Advance(23 * time.Hour)
	col, err := discover(t, e)
	if err != nil || arch.calls.Load() != calls || len(col.findings) != 5 || col.findings[0].CachedAt.IsZero() {
		t.Errorf("cached run: %d more requests, %d findings, err %v", arch.calls.Load()-calls, len(col.findings), err)
	}

	clk.Advance(2 * time.Hour) // past the TTL
	if col, _ := discover(t, e); arch.calls.Load() == calls || !col.findings[0].CachedAt.IsZero() {
		t.Error("an expired listing was not read again")
	}
}

func TestLongListingsAreNotCached(t *testing.T) {
	old := maxCachedEntries
	maxCachedEntries = 6
	defer func() { maxCachedEntries = old }()

	arch, url := newFakeArchive(t, site(9)...)
	c := cache.New[Listing](cache.Options{Name: "archive", MaxCost: 1 << 20})
	e := engine(url, Options{MaxURLs: 100, PageSize: 4, Cache: c, TTL: time.Hour, FailureTTL: time.Minute})
	for run := 1; run <= 2; run++ {
		col, err := discover(t, e)
		if err != nil || len(col.findings) != 9 || !col.findings[0].CachedAt.IsZero() {
			t.Fatalf("run %d: %d findings, err %v", run, len(col.findings), err)
		}
		if got := arch.calls.Load(); got != int32(3*run) {
			t.Errorf("run %d: %d requests so far, want %d", run, got, 3*run)
		}
	}
}

func TestFailureBeforeAnythingIsListed(t *testing.T) {
	arch, url := newFakeArchive(t, site(5)...)
	arch.status = 503
	clk := &clock{}
	c := cache.New[Listing](cache.Options{Name: "archive", MaxCost: 1 << 20, Now: clk.Now})
	e := engine(url, Options{MaxURLs: 10, Cache: c, TTL: time.Hour, FailureTTL: 5 * time.Minute})

	for i := 0; i < 2; i++ {
		col, err := discover(t, e)
		var partial *discovery.PartialError
		if err == nil || errors.As(err, &partial) || !strings.Contains(err.Error(), "503") || len(col.findings) != 0 {
			t.Errorf("run %d: err = %v, findings %d", i, err, len(col.findings))
		}
	}
	// The failure is remembered briefly, so the archive is not asked again.
	if calls := arch.calls.Load(); calls != 1 {
		t.Errorf("requests = %d, want 1", calls)
	}
	clk.Advance(6 * time.Minute)
	arch.status = 0
	if col, err := discover(t, e); err != nil || len(col.findings) != 5 {
		t.Errorf("after the failure expired: %d findings, err %v", len(col.findings), err)
	}
}

func TestFailurePartWayKeepsWhatWasListed(t *testing.T) {
	arch, url := newFakeArchive(t, site(12)...)
	arch.status, arch.failFrom = 500, 3
	c := cache.New[Listing](cache.Options{Name: "archive", MaxCost: 1 << 20})
	e := engine(url, Options{MaxURLs: 100, PageSize: 4, Cache: c, TTL: time.Hour, FailureTTL: time.Minute})

	col, err := discover(t, e)
	var partial *discovery.PartialError
	if !errors.As(err, &partial) || !strings.Contains(err.Error(), "after 8 URLs") || !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v", err)
	}
	if len(col.findings) != 8 {
		t.Errorf("findings = %d, want the 8 listed before the failure", len(col.findings))
	}
	// An incomplete listing is not cached: the next scan asks again.
	arch.status = 0
	if col, err := discover(t, e); err != nil || len(col.findings) != 12 {
		t.Errorf("next run: %d findings, err %v", len(col.findings), err)
	}
}

// TestBacksOffWhenRateLimited: after an HTTP 429 the archive is left alone
// by every scan, for at least the cooldown and for as long as it asks.
func TestBacksOffWhenRateLimited(t *testing.T) {
	arch, url := newFakeArchive(t, site(5)...)
	arch.status, arch.retryAfter = 429, "600"
	clk := &clock{}
	e := engine(url, Options{MaxURLs: 10, Cooldown: 2 * time.Minute, Now: clk.Now})

	if _, err := discover(t, e); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("err = %v", err)
	}
	arch.status = 0
	// Another scan, of another domain, during the cooldown: no request.
	other, _ := discovery.ParseTarget("example.org")
	err := e.Discover(context.Background(), discovery.Input{Target: other}, (&collector{}).emit)
	if err == nil || !strings.Contains(err.Error(), "slow down") || arch.calls.Load() != 1 {
		t.Errorf("during the cooldown: err = %v, requests = %d", err, arch.calls.Load())
	}
	clk.Advance(5 * time.Minute) // past the cooldown, but not the Retry-After
	if _, err := discover(t, e); err == nil || arch.calls.Load() != 1 {
		t.Errorf("before Retry-After passed: err = %v, requests = %d", err, arch.calls.Load())
	}
	clk.Advance(6 * time.Minute)
	if col, err := discover(t, e); err != nil || len(col.findings) != 5 {
		t.Errorf("after the cooldown: %d findings, err %v", len(col.findings), err)
	}
}

func TestRateLimitPartWay(t *testing.T) {
	arch, url := newFakeArchive(t, site(12)...)
	arch.status, arch.failFrom = 429, 2
	col, err := discover(t, engine(url, Options{MaxURLs: 100, PageSize: 4}))
	var partial *discovery.PartialError
	if !errors.As(err, &partial) || len(col.findings) != 4 {
		t.Errorf("err = %v, findings = %d", err, len(col.findings))
	}
	// It stopped at the first 429 instead of trying again.
	if calls := arch.calls.Load(); calls != 2 {
		t.Errorf("requests = %d, want 2", calls)
	}
}

func TestCancelledListingStops(t *testing.T) {
	_, url := newFakeArchive(t, site(12)...)
	ctx, cancel := context.WithCancel(context.Background())
	e := engine(url, Options{MaxURLs: 100, PageSize: 4})
	n := 0
	err := e.Discover(ctx, discovery.Input{Target: target(t)}, func(discovery.Finding) {
		if n++; n == 4 {
			cancel() // after the first page
		}
	})
	if !errors.Is(err, context.Canceled) || n != 4 {
		t.Errorf("err = %v after %d findings", err, n)
	}
}
