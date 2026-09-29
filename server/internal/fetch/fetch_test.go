package fetch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"websitemapper/internal/resource"
)

func TestGetDoesNotFollowRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, "/new", http.StatusMovedPermanently)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("<title>new</title>"))
	}))
	defer srv.Close()

	c := New(Options{AllowPrivate: true, Timeout: 2 * time.Second})
	resp, err := c.Get(context.Background(), srv.URL+"/old")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusMovedPermanently || resp.Location != srv.URL+"/new" {
		t.Errorf("got status %d location %q", resp.StatusCode, resp.Location)
	}
	if !resp.IsRedirect() {
		t.Error("IsRedirect() = false")
	}

	resp, err = c.Get(context.Background(), srv.URL+"/new")
	if err != nil {
		t.Fatal(err)
	}
	if resp.ContentType != "text/html" || string(resp.Body) != "<title>new</title>" {
		t.Errorf("got content type %q body %q", resp.ContentType, resp.Body)
	}
}

func TestGetTruncatesBodyAndSkipsBinary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/img" {
			w.Header().Set("Content-Type", "image/png")
		} else {
			w.Header().Set("Content-Type", "text/plain")
		}
		w.Write([]byte(strings.Repeat("x", 5000)))
	}))
	defer srv.Close()

	c := New(Options{AllowPrivate: true, MaxBodyBytes: 1024})
	resp, err := c.Get(context.Background(), srv.URL+"/text")
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Body) != 1024 || !resp.Truncated {
		t.Errorf("body len %d truncated %v", len(resp.Body), resp.Truncated)
	}
	resp, err = c.Get(context.Background(), srv.URL+"/img")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Body != nil {
		t.Error("binary body should not be read")
	}
}

func TestGetBlocksPrivateAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	c := New(Options{Timeout: 2 * time.Second})
	_, err := c.Get(context.Background(), srv.URL)
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("err = %v, want ErrBlockedAddress", err)
	}
}

func TestIsPublicAddr(t *testing.T) {
	public := []string{
		"8.8.8.8", "93.184.216.34", "1.1.1.1", "2606:4700::1111", "2a00:1450:4001::200e",
		"::ffff:93.184.216.34",
	}
	nonPublic := []string{
		"127.0.0.1", "127.8.9.10", // loopback
		"10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.0.1", // private
		"169.254.169.254", // link-local (cloud metadata)
		"100.64.0.1",      // CGNAT
		"0.0.0.0", "0.1.2.3", "255.255.255.255", "240.0.0.1",
		"224.0.0.1", // multicast
		"192.0.2.10", "198.51.100.7", "203.0.113.10", "198.18.0.1", "192.0.0.8",
		"::", "::1", "fe80::1", "fe80::1%eth0", "fd00::1", "fc00::1", "ff02::1", "fec0::1",
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254", // IPv4-mapped
		"64:ff9b::7f00:1", "64:ff9b::a00:1", // NAT64 to private IPv4
		"2002:7f00:1::1", "2001:0:4136:e378::1", "2001:db8::1", "100::1",
	}
	for _, s := range public {
		if !IsPublicAddr(netip.MustParseAddr(s)) {
			t.Errorf("IsPublicAddr(%s) = false, want true", s)
		}
	}
	for _, s := range nonPublic {
		if IsPublicAddr(netip.MustParseAddr(s)) {
			t.Errorf("IsPublicAddr(%s) = true, want false", s)
		}
	}
}

func TestDialerCheckRejectsNonPublicAfterResolution(t *testing.T) {
	// The Control hook receives the resolved IP, so a hostname that resolves
	// (or rebinds) to a private address is refused at connect time.
	for _, addr := range []string{"127.0.0.1:443", "[::1]:80", "10.0.0.5:8080", "[fd00::1]:443", "169.254.169.254:80"} {
		if err := blockNonPublic("tcp", addr, nil); !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("blockNonPublic(%s) = %v, want ErrBlockedAddress", addr, err)
		}
	}
	if err := blockNonPublic("tcp", "93.184.216.34:443", nil); err != nil {
		t.Errorf("public address rejected: %v", err)
	}
}

func TestGetBlocksHostnameResolvingToLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	// "localhost" resolves to a loopback address; the name itself is not checked.
	u := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	_, err := New(Options{Timeout: 2 * time.Second}).Get(context.Background(), u)
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("err = %v, want ErrBlockedAddress", err)
	}
}

func TestLimiterSpacesRequestsPerHost(t *testing.T) {
	l := newHostLimiter(20) // 50ms interval
	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := l.wait(context.Background(), "a"); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed < 90*time.Millisecond {
		t.Errorf("3 requests took %v, expected >= ~100ms", elapsed)
	}
	// A different host is not delayed.
	start = time.Now()
	if err := l.wait(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Millisecond {
		t.Errorf("other host delayed %v", elapsed)
	}
}

// TestGlobalPoolBoundsRequestsAcrossScans: several scans, each with many
// concurrent workers, share one client. The server measures how many
// requests are in flight at once; it must never exceed the pool size.
func TestGlobalPoolBoundsRequestsAcrossScans(t *testing.T) {
	const poolSize, scans, workers, perWorker = 4, 5, 10, 6
	var inFlight, peak, served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		served.Add(1)
		time.Sleep(2 * time.Millisecond)
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<title>x</title>"))
	}))
	defer srv.Close()

	pool := resource.NewPool("http", poolSize)
	c := New(Options{AllowPrivate: true, Timeout: 5 * time.Second, Pool: pool})
	var wg sync.WaitGroup
	for s := 0; s < scans; s++ {
		ctx := resource.WithAccount(context.Background(), resource.NewAccount(fmt.Sprint("scan", s), 0))
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < perWorker; i++ {
					if _, err := c.Get(ctx, fmt.Sprintf("%s/p%d", srv.URL, i)); err != nil {
						t.Error(err)
						return
					}
				}
			}()
		}
	}
	wg.Wait()

	if got := peak.Load(); got > poolSize {
		t.Fatalf("server saw %d concurrent requests, pool allows %d", got, poolSize)
	}
	if served.Load() != scans*workers*perWorker {
		t.Errorf("served %d requests", served.Load())
	}
	if st := pool.Stats(); st.InUse != 0 || st.Peak > poolSize {
		t.Errorf("pool stats = %+v", st)
	}
}

func TestGetChargesScanRequestBudget(t *testing.T) {
	var served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { served.Add(1) }))
	defer srv.Close()
	c := New(Options{AllowPrivate: true})
	acct := resource.NewAccount("scan", 3)
	ctx := resource.WithAccount(context.Background(), acct)
	for i := 0; i < 3; i++ {
		if _, err := c.Get(ctx, srv.URL); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Get(ctx, srv.URL); !errors.Is(err, resource.ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted", err)
	}
	if served.Load() != 3 || acct.Requests() != 3 {
		t.Errorf("served %d, charged %d", served.Load(), acct.Requests())
	}
}

func TestGetStopsWaitingForPoolOnCancel(t *testing.T) {
	pool := resource.NewPool("http", 1)
	hold, _, _ := pool.Acquire(context.Background())
	defer hold()
	c := New(Options{AllowPrivate: true, Pool: pool})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Get(ctx, "http://127.0.0.1:1/"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > time.Second || pool.Stats().Waiting != 0 {
		t.Errorf("did not stop promptly: %v, %+v", time.Since(start), pool.Stats())
	}
}

func TestHTMLOnlySkipsOtherBodies(t *testing.T) {
	big := strings.Repeat("x", 1<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/data.json":
			w.Header().Set("Content-Type", "application/json")
		case "/feed.xml":
			w.Header().Set("Content-Type", "application/rss+xml")
		default:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		}
		w.Write([]byte(big))
	}))
	defer srv.Close()
	c := New(Options{AllowPrivate: true, ReadBody: HTMLOnly})
	for path, wantBody := range map[string]bool{"/page": true, "/data.json": false, "/feed.xml": false} {
		resp, err := c.Get(context.Background(), srv.URL+path)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(resp.Body) > 0; got != wantBody || resp.StatusCode != 200 || resp.ContentType == "" {
			t.Errorf("%s: body read = %v (want %v), status %d, type %q", path, got, wantBody, resp.StatusCode, resp.ContentType)
		}
	}
	// The default still reads textual bodies (the certificate client needs JSON).
	resp, _ := New(Options{AllowPrivate: true}).Get(context.Background(), srv.URL+"/data.json")
	if len(resp.Body) == 0 {
		t.Error("default client should read JSON bodies")
	}
}
