package subdomains

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"websitemapper/internal/discovery"
	"websitemapper/internal/fetch"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func target(t *testing.T, in string) discovery.Target {
	t.Helper()
	tgt, err := discovery.ParseTarget(in)
	if err != nil {
		t.Fatal(err)
	}
	return tgt
}

const sampleCRTSh = `[
  {"name_value": "example.com"},
  {"name_value": "*.example.com"},
  {"name_value": "api.example.com\nwww.example.com"},
  {"name_value": "evil-example.com"}
]`

func TestParseAndFilterCRTSh(t *testing.T) {
	names, err := ParseCRTSh([]byte(sampleCRTSh))
	if err != nil {
		t.Fatal(err)
	}
	got := Filter(names, target(t, "example.com"))
	want := []string{"api.example.com", "example.com", "www.example.com"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFilterScopeAndNormalization(t *testing.T) {
	got := Filter([]string{
		"EXAMPLE.COM", "example.com.", "*.example.com", "*.Staging.Example.com",
		"example.org", "evil-example.com", "example.com.evil.io", "admin@example.com",
		"deep.api.example.com", "api.example.com",
	}, target(t, "example.com"))
	want := []string{"api.example.com", "deep.api.example.com", "example.com", "staging.example.com"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseCRTShRejectsGarbage(t *testing.T) {
	if _, err := ParseCRTSh([]byte("<html>busy</html>")); err == nil {
		t.Error("expected error for non-JSON body")
	}
}

func TestCRTShQueriesAndRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "%.example.com" || r.URL.Query().Get("output") != "json" {
			t.Errorf("unexpected query %q", r.URL.RawQuery)
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(sampleCRTSh))
	}))
	defer srv.Close()

	c := NewCRTSh(fetch.New(fetch.Options{AllowPrivate: true, Timeout: 2 * time.Second}))
	c.BaseURL, c.RetryDelay = srv.URL, time.Millisecond
	names, err := c.Discover(context.Background(), "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || len(names) != 5 {
		t.Errorf("calls %d names %v", calls.Load(), names)
	}
}

func TestCRTShGivesUpAfterRetry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := NewCRTSh(fetch.New(fetch.Options{AllowPrivate: true}))
	c.BaseURL, c.RetryDelay = srv.URL, time.Millisecond
	if _, err := c.Discover(context.Background(), "example.com"); err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("err = %v", err)
	}
}

type fakeSource struct {
	name  string
	names []string
	err   error
}

func (f fakeSource) Name() string                 { return f.name }
func (f fakeSource) Provenance() discovery.Source { return discovery.SourceCT }
func (f fakeSource) Discover(context.Context, string) ([]string, error) {
	return f.names, f.err
}

func TestEngineMergesSourcesAndKeepsPartialResults(t *testing.T) {
	e := New([]Source{
		fakeSource{name: "one", names: []string{"*.example.com", "api.example.com", "other.net"}},
		fakeSource{name: "two", names: []string{"API.example.com.", "status.example.com"}},
		fakeSource{name: "broken", err: errors.New("timeout")},
	}, quiet)

	var mu sync.Mutex
	var got []string
	err := e.Discover(context.Background(), discovery.Input{Target: target(t, "example.com")}, func(f discovery.Finding) {
		mu.Lock()
		defer mu.Unlock()
		if f.URL != "" || f.Source != discovery.SourceCT {
			t.Errorf("unexpected finding %+v", f)
		}
		got = append(got, f.Host)
	})
	// One provider failing is not an engine failure when others succeed.
	if err != nil {
		t.Errorf("err = %v", err)
	}
	sort.Strings(got)
	// api.example.com comes from both providers; the aggregator merges it.
	if !reflect.DeepEqual(got, []string{"api.example.com", "api.example.com", "example.com", "status.example.com"}) {
		t.Errorf("hosts = %v", got)
	}
}

func TestEngineFailsWhenEverySourceFails(t *testing.T) {
	e := New([]Source{
		fakeSource{name: "a", err: errors.New("HTTP 502")},
		fakeSource{name: "b", err: errors.New("timeout")},
	}, quiet)
	err := e.Discover(context.Background(), discovery.Input{Target: target(t, "example.com")}, func(discovery.Finding) {})
	if err == nil || !strings.Contains(err.Error(), "a: HTTP 502") || !strings.Contains(err.Error(), "b: timeout") {
		t.Errorf("err = %v", err)
	}
}

func TestCertSpotterPaginates(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.URL.Path != "/v1/issuances" || q.Get("domain") != "example.com" || q.Get("include_subdomains") != "true" {
			t.Errorf("unexpected request %s", r.URL)
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch q.Get("after") {
		case "":
			w.Write([]byte(`[{"id":"1","dns_names":["example.com","*.example.com"]},{"id":"2","dns_names":["api.example.com"]}]`))
		case "2":
			w.Write([]byte(`[{"id":"3","dns_names":["status.example.com"]}]`))
		default:
			w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()

	c := NewCertSpotter(fetch.New(fetch.Options{AllowPrivate: true}))
	c.BaseURL = srv.URL
	names, err := c.Discover(context.Background(), "example.com")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"example.com", "*.example.com", "api.example.com", "status.example.com"}
	if !reflect.DeepEqual(names, want) || calls.Load() != 3 {
		t.Errorf("names = %v (calls %d)", names, calls.Load())
	}
}

func TestCertSpotterRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := NewCertSpotter(fetch.New(fetch.Options{AllowPrivate: true}))
	c.BaseURL = srv.URL
	if _, err := c.Discover(context.Background(), "example.com"); err == nil || !strings.Contains(err.Error(), "429") {
		t.Errorf("err = %v", err)
	}
}
