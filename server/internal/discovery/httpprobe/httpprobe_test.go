package httpprobe

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"websitemapper/internal/discovery"
	"websitemapper/internal/fetch"
)

// fakeHosts routes requests by URL, standing in for several hosts.
type fakeHosts struct {
	mu        sync.Mutex
	responses map[string]*fetch.Response
	requested []string
}

func (f *fakeHosts) Get(_ context.Context, u string) (*fetch.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requested = append(f.requested, u)
	if r, ok := f.responses[u]; ok {
		r.URL = u
		if r.Header == nil {
			r.Header = http.Header{}
		}
		return r, nil
	}
	return nil, fmt.Errorf("Get %q: dial tcp: connection refused", u)
}

type state struct{ hosts []discovery.HostView }

func (s state) Hosts() []discovery.HostView { return append([]discovery.HostView(nil), s.hosts...) }
func (s state) PageURLs() []string          { return nil }

func resolved(name string) discovery.HostView {
	return discovery.HostView{Name: name, DNS: &discovery.DNSInfo{Resolved: true, Addresses: []string{"93.184.216.34"}}}
}

func run(t *testing.T, f Fetcher, opts Options, hosts ...discovery.HostView) map[string]*discovery.HTTPInfo {
	t.Helper()
	tgt, _ := discovery.ParseTarget("example.com")
	var mu sync.Mutex
	got := map[string]*discovery.HTTPInfo{}
	err := New(f, opts).Discover(context.Background(), discovery.Input{Target: tgt, State: state{hosts}}, func(fd discovery.Finding) {
		mu.Lock()
		defer mu.Unlock()
		got[fd.Host] = fd.HTTP
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func html(title string) *fetch.Response {
	return &fetch.Response{StatusCode: 200, ContentType: "text/html", Header: http.Header{"Server": {"nginx"}},
		Body: []byte("<html><title>" + title + "</title></html>")}
}

func TestProbe(t *testing.T) {
	f := &fakeHosts{responses: map[string]*fetch.Response{
		"https://example.com/":       {StatusCode: 301, Location: "https://www.example.com/"},
		"https://www.example.com/":   html("Home"),
		"http://legacy.example.com/": html("Legacy"),
		"https://api.example.com/":   {StatusCode: 404, ContentType: "application/json"},
		"https://app.example.com/":   {StatusCode: 302, Location: "https://login.other.net/"},
	}}
	got := run(t, f, Options{MaxHosts: 10, Concurrency: 2},
		resolved("example.com"), resolved("www.example.com"), resolved("legacy.example.com"),
		resolved("api.example.com"), resolved("app.example.com"), resolved("down.example.com"),
		discovery.HostView{Name: "old.example.com", DNS: &discovery.DNSInfo{Resolved: false}},
		discovery.HostView{Name: "unresolved-yet.example.com"},
	)

	if h := got["example.com"]; !h.Reachable || h.Status != 301 || h.Redirect != "https://www.example.com/" ||
		h.FinalURL != "https://www.example.com/" || h.FinalStatus != 200 || h.Title != "Home" {
		t.Errorf("apex = %+v", h)
	}
	if h := got["legacy.example.com"]; !h.Reachable || h.Scheme != "http" || h.URL != "http://legacy.example.com/" || h.Title != "Legacy" {
		t.Errorf("http fallback = %+v", h)
	}
	if h := got["api.example.com"]; !h.Reachable || h.Status != 404 {
		t.Errorf("a 404 still means reachable: %+v", h)
	}
	if h := got["app.example.com"]; h.FinalURL != "https://app.example.com/" || h.Redirect != "https://login.other.net/" {
		t.Errorf("out-of-scope redirect must not be followed: %+v", h)
	}
	if h := got["down.example.com"]; h.Reachable || h.Error != "connection refused" {
		t.Errorf("down = %+v", h)
	}
	for _, h := range []string{"old.example.com", "unresolved-yet.example.com"} {
		if _, ok := got[h]; ok {
			t.Errorf("%s should not be probed", h)
		}
	}
	for _, u := range f.requested {
		if strings.Contains(u, "other.net") {
			t.Errorf("requested out-of-scope %s", u)
		}
	}
}

func TestProbeSkipsNonPublicAndRespectsLimit(t *testing.T) {
	f := &fakeHosts{responses: map[string]*fetch.Response{}}
	intra := discovery.HostView{Name: "intra.example.com", DNS: &discovery.DNSInfo{Resolved: true, NonPublic: true}}
	known := resolved("known.example.com")
	known.HTTP = &discovery.HTTPInfo{Reachable: true}
	got := run(t, f, Options{MaxHosts: 2, Concurrency: 1}, intra, known, resolved("a.example.com"), resolved("b.example.com"))

	if h := got["intra.example.com"]; h == nil || h.Skipped == "" || h.Reachable {
		t.Errorf("intra = %+v", h)
	}
	if h := got["b.example.com"]; h == nil || h.Skipped != "host limit reached" {
		t.Errorf("b = %+v", h)
	}
	for _, u := range f.requested {
		if strings.Contains(u, "intra") || strings.Contains(u, "known") || strings.Contains(u, "b.example") {
			t.Errorf("unexpected request %s", u)
		}
	}
}

// TestProbeRealServer exercises the engine against a real HTTP server
// through the real fetch client.
func TestProbeRealServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<title>Local</title>"))
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://") // 127.0.0.1:port

	client := fetch.New(fetch.Options{AllowPrivate: true, Timeout: 2 * time.Second})
	e := New(client, Options{MaxHosts: 1, Concurrency: 1, AllowPrivate: true})
	info := e.probe(context.Background(), discovery.Target{Domain: "127.0.0.1"}, host)
	if info == nil || !info.Reachable || info.Scheme != "http" || info.Title != "Local" {
		t.Errorf("info = %+v", info)
	}

	// With the default policy the same server is refused.
	e = New(fetch.New(fetch.Options{Timeout: 2 * time.Second}), Options{MaxHosts: 1, Concurrency: 1})
	info = e.probe(context.Background(), discovery.Target{Domain: "127.0.0.1"}, host)
	if info.Reachable || info.Error != "resolves to a non-public address" {
		t.Errorf("blocked info = %+v", info)
	}
}
