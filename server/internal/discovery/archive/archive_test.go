package archive

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"websitemapper/internal/cache"
	"websitemapper/internal/discovery"
	"websitemapper/internal/fetch"
)

const cdxBody = `[["original","timestamp","mimetype"],
["http://example.com:80/","20100101000000","text/html"],
["https://example.com/about","20150601120000","text/html"],
["https://EXAMPLE.com/about/","20160601120000","text/html"],
["https://api.example.com/v1/items","20200101000000","application/json"],
["https://example.com/logo.png","20190101000000","image/png"],
["http://old.example.com/page","20090101000000","warc/revisit"],
["https://example.com/!27","20130101000000","text/html"],
["https://example.com/%22/","20130101000000","text/html"],
["https://example.com/a b","20130101000000","text/html"],
["https://evil-example.com/","20130101000000","text/html"],
["not a url","20130101000000","text/html"]]`

func target(t *testing.T) discovery.Target {
	t.Helper()
	tgt, err := discovery.ParseTarget("example.com")
	if err != nil {
		t.Fatal(err)
	}
	return tgt
}

func TestParseKeepsCleanInScopeURLs(t *testing.T) {
	l, err := Parse([]byte(cdxBody), target(t), 100)
	if err != nil || l.Err != "" || l.Truncated {
		t.Fatalf("parse: %v %+v", err, l)
	}
	var got []string
	for _, e := range l.Entries {
		got = append(got, e.URL+" "+e.MIME)
	}
	want := []string{
		"http://example.com/ text/html",
		"https://example.com/about text/html", // /about/ is the same URL
		"https://api.example.com/v1/items application/json",
		"https://example.com/logo.png image/png",
		"http://old.example.com/page ", // archive placeholder type dropped
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("entries:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if first := time.Unix(l.Entries[1].FirstSeen, 0).UTC(); first != time.Date(2015, 6, 1, 12, 0, 0, 0, time.UTC) {
		t.Errorf("first seen = %v", first)
	}
}

func TestParseTruncates(t *testing.T) {
	l, _ := Parse([]byte(cdxBody), target(t), 2)
	if len(l.Entries) != 2 || !l.Truncated {
		t.Errorf("entries %d truncated %v", len(l.Entries), l.Truncated)
	}
	empty, _ := Parse([]byte("[]"), target(t), 10)
	if len(empty.Entries) != 0 || empty.Truncated || empty.Err != "" {
		t.Errorf("empty = %+v", empty)
	}
}

type clock struct{ now atomic.Int64 }

func (c *clock) Now() time.Time          { return time.Unix(0, c.now.Load()) }
func (c *clock) Advance(d time.Duration) { c.now.Add(int64(d)) }

func archiveServer(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		q := r.URL.Query()
		if r.URL.Path != "/cdx/search/cdx" || q.Get("url") != "example.com" || q.Get("matchType") != "domain" || q.Get("filter") != "statuscode:200" {
			t.Errorf("unexpected query %s", r.URL)
		}
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
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

func TestEngineEmitsArchivedURLsAndCaches(t *testing.T) {
	srv, calls := archiveServer(t, 200, cdxBody)
	clk := &clock{}
	c := cache.New[Listing](cache.Options{Name: "archive", MaxCost: 1 << 20, Now: clk.Now})
	e := New(fetch.New(fetch.Options{AllowPrivate: true}), Options{MaxURLs: 100, Cache: c, TTL: 24 * time.Hour, FailureTTL: 5 * time.Minute, BaseURL: srv.URL})
	in := discovery.Input{Target: target(t)}

	first := &collector{}
	if err := e.Discover(context.Background(), in, first.emit); err != nil {
		t.Fatal(err)
	}
	if len(first.findings) != 5 {
		t.Fatalf("findings = %d", len(first.findings))
	}
	f := first.findings[1]
	if f.Source != discovery.SourceArchive || f.Hint != discovery.HintArchive || f.Archive == nil ||
		f.Archive.ContentType != "text/html" || f.Response != nil || !f.CachedAt.IsZero() {
		t.Errorf("finding = %+v", f)
	}

	clk.Advance(23 * time.Hour)
	second := &collector{}
	e.Discover(context.Background(), in, second.emit)
	if calls.Load() != 1 || len(second.findings) != 5 || second.findings[0].CachedAt.IsZero() {
		t.Errorf("cached run: archive calls %d, findings %d", calls.Load(), len(second.findings))
	}
}

func TestEngineReportsTruncationAndFailures(t *testing.T) {
	srv, _ := archiveServer(t, 200, cdxBody)
	e := New(fetch.New(fetch.Options{AllowPrivate: true}), Options{MaxURLs: 2, BaseURL: srv.URL})
	col := &collector{}
	err := e.Discover(context.Background(), discovery.Input{Target: target(t)}, col.emit)
	var partial *discovery.PartialError
	if !errors.As(err, &partial) || len(col.findings) != 2 {
		t.Errorf("err = %v, findings %d", err, len(col.findings))
	}

	down, calls := archiveServer(t, 503, "")
	clk := &clock{}
	c := cache.New[Listing](cache.Options{Name: "archive", MaxCost: 1 << 20, Now: clk.Now})
	e = New(fetch.New(fetch.Options{AllowPrivate: true}), Options{MaxURLs: 10, Cache: c, TTL: time.Hour, FailureTTL: 5 * time.Minute, BaseURL: down.URL})
	for i := 0; i < 2; i++ {
		if err := e.Discover(context.Background(), discovery.Input{Target: target(t)}, col.emit); err == nil || !strings.Contains(err.Error(), "503") {
			t.Errorf("err = %v", err)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("a failure should be reused briefly: calls = %d", calls.Load())
	}
	clk.Advance(6 * time.Minute)
	e.Discover(context.Background(), discovery.Input{Target: target(t)}, col.emit)
	if calls.Load() != 2 {
		t.Errorf("failure outlived its TTL: calls = %d", calls.Load())
	}
}
