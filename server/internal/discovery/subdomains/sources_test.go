package subdomains

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"websitemapper/internal/discovery"
	"websitemapper/internal/fetch"
)

// provider serves one canned response and records the request it got.
func provider(t *testing.T, status int, body string) (*httptest.Server, *string) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.RequestURI()
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func client() Fetcher { return fetch.New(fetch.Options{AllowPrivate: true}) }

func TestAnubis(t *testing.T) {
	srv, req := provider(t, 200, `["api.example.com","*.example.com","EXAMPLE.com","other.org"]`)
	a := NewAnubis(client())
	a.BaseURL = srv.URL
	names, err := a.Discover(context.Background(), "example.com")
	if err != nil || *req != "/anubis/subdomains/example.com" {
		t.Fatalf("err = %v, request = %s", err, *req)
	}
	// The engine's filter normalizes names and drops what is out of scope.
	if got := Filter(names, target(t, "example.com")); !reflect.DeepEqual(got, []string{"api.example.com", "example.com"}) {
		t.Errorf("names = %v", got)
	}
	if a.Provenance() != discovery.SourceDataset {
		t.Errorf("provenance = %s", a.Provenance())
	}

	// A domain it does not know is an empty list, not a failure.
	srv, _ = provider(t, 200, `[]`)
	a.BaseURL = srv.URL
	if names, err := a.Discover(context.Background(), "example.com"); err != nil || len(names) != 0 {
		t.Errorf("unknown domain = %v, %v", names, err)
	}
}

func TestTHC(t *testing.T) {
	srv, req := provider(t, 200, "subdomain\nexample.com\r\napi.example.com\n\nmail.example.com,extra\n")
	s := NewTHC(client())
	s.BaseURL = srv.URL
	names, err := s.Discover(context.Background(), "example.com")
	if err != nil || !strings.HasPrefix(*req, "/api/v1/subdomains/download?") || !strings.Contains(*req, "domain=example.com") {
		t.Fatalf("err = %v, request = %s", err, *req)
	}
	if want := []string{"example.com", "api.example.com", "mail.example.com"}; !reflect.DeepEqual(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
	if s.Provenance() != discovery.SourceDataset {
		t.Errorf("provenance = %s", s.Provenance())
	}

	// A list without the header line loses nothing.
	srv, _ = provider(t, 200, "a.example.com\nb.example.com")
	s.BaseURL = srv.URL
	if names, _ := s.Discover(context.Background(), "example.com"); len(names) != 2 {
		t.Errorf("names without a header = %v", names)
	}
}

func TestShodanCT(t *testing.T) {
	srv, req := provider(t, 200, `[
		{"hash":"31c0","subject_cn":"example.com","san_dns_names":["example.com","www.example.com"]},
		{"hash":"5085","subject_cn":"*.example.com","san_dns_names":["*.example.com","api.example.com"]}]`)
	s := NewShodanCT(client())
	s.BaseURL = srv.URL
	names, err := s.Discover(context.Background(), "example.com")
	if err != nil || *req != "/api/v1/domain/example.com" {
		t.Fatalf("err = %v, request = %s", err, *req)
	}
	if got := Filter(names, target(t, "example.com")); !reflect.DeepEqual(got, []string{"api.example.com", "example.com", "www.example.com"}) {
		t.Errorf("names = %v", got)
	}
	// Certificates are certificates, whoever serves the search.
	if s.Provenance() != discovery.SourceCT {
		t.Errorf("provenance = %s", s.Provenance())
	}
}

func TestSourceFailuresNameTheProvider(t *testing.T) {
	sources := func(base string) []Source {
		a, th, sh := NewAnubis(client()), NewTHC(client()), NewShodanCT(client())
		a.BaseURL, th.BaseURL, sh.BaseURL = base, base, base
		return []Source{a, th, sh}
	}

	limited, _ := provider(t, 429, "slow down")
	for _, s := range sources(limited.URL) {
		// A rate limit is recognizable, so the engine can wait longer
		// before asking that provider again.
		if _, err := s.Discover(context.Background(), "example.com"); !errors.Is(err, ErrRateLimited) {
			t.Errorf("%s on HTTP 429: %v", s.Name(), err)
		}
	}
	down, _ := provider(t, 503, "")
	for _, s := range sources(down.URL) {
		if _, err := s.Discover(context.Background(), "example.com"); err == nil || !strings.Contains(err.Error(), "503") || errors.Is(err, ErrRateLimited) {
			t.Errorf("%s on HTTP 503: %v", s.Name(), err)
		}
	}
	garbage, _ := provider(t, 200, "<html>maintenance</html>")
	for _, s := range sources(garbage.URL)[:1] {
		if _, err := s.Discover(context.Background(), "example.com"); err == nil || !strings.Contains(err.Error(), "unexpected") {
			t.Errorf("%s on a non-JSON answer: %v", s.Name(), err)
		}
	}
}

// slowSource answers only when released.
type slowSource struct {
	release chan struct{}
	started chan struct{}
}

func (s *slowSource) Name() string                 { return "slow" }
func (s *slowSource) Provenance() discovery.Source { return discovery.SourceCT }
func (s *slowSource) Discover(ctx context.Context, _ string) ([]string, error) {
	close(s.started)
	select {
	case <-s.release:
		return []string{"late.example.com"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type quickSource struct{}

func (quickSource) Name() string                 { return "quick" }
func (quickSource) Provenance() discovery.Source { return discovery.SourceDataset }
func (quickSource) Discover(context.Context, string) ([]string, error) {
	return []string{"api.example.com"}, nil
}

// TestSlowProviderDoesNotHoldTheScan: when the scan moves on, the engine
// returns what it has, and the provider still being asked finishes for the
// cache, so the next scan has its answer at once.
func TestSlowProviderDoesNotHoldTheScan(t *testing.T) {
	slow := &slowSource{release: make(chan struct{}), started: make(chan struct{})}
	e := New([]Source{quickSource{}, slow}, time.Minute, quiet).WithCache(CacheOptions{
		Cache: NewCache(1 << 20), TTL: time.Hour, RateLimitTTL: time.Minute, FailureTTL: time.Minute,
	})
	in := discovery.Input{Target: target(t, "example.com")}
	hosts := func(run func(emit discovery.Emit) error) ([]string, error) {
		var mu sync.Mutex
		var got []string
		err := run(func(f discovery.Finding) { mu.Lock(); got = append(got, f.Host); mu.Unlock() })
		sort.Strings(got)
		return got, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-slow.started; time.Sleep(20 * time.Millisecond); cancel() }()
	got, err := hosts(func(emit discovery.Emit) error { return e.Discover(ctx, in, emit) })
	var partial *discovery.PartialError
	if !errors.As(err, &partial) || !strings.Contains(err.Error(), "slow: had not answered") {
		t.Errorf("err = %v", err)
	}
	if !reflect.DeepEqual(got, []string{"api.example.com"}) {
		t.Errorf("hosts = %v", got)
	}

	// The provider answers after the scan has moved on...
	close(slow.release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		// ...and the next scan gets both answers without asking anyone.
		got, err := hosts(func(emit discovery.Emit) error { return e.Discover(context.Background(), in, emit) })
		if err == nil && reflect.DeepEqual(got, []string{"api.example.com", "late.example.com"}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the late answer was not cached: hosts %v, err %v", got, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
