package fetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
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

func TestGetSendsUserAgentAndNoCredentials(t *testing.T) {
	var mu sync.Mutex
	var seen []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		mu.Unlock()
		if r.Method != http.MethodGet {
			t.Errorf("method = %s", r.Method)
		}
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "s3cret"})
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<title>x</title>"))
	}))
	defer srv.Close()

	const ua = "WebsiteMapperBot/0.1 (+https://mapper.example/bot)"
	c := New(Options{AllowPrivate: true, UserAgent: ua})
	// Credentials embedded in the URL are never sent.
	u := strings.Replace(srv.URL, "http://", "http://user:pass@", 1)
	for i := 0; i < 2; i++ {
		if _, err := c.Get(context.Background(), u+"/page"); err != nil {
			t.Fatal(err)
		}
	}
	for i, h := range seen {
		if h.Get("User-Agent") != ua {
			t.Errorf("request %d User-Agent = %q", i, h.Get("User-Agent"))
		}
		// The cookie set by the first response is not sent back.
		for _, k := range []string{"Cookie", "Authorization", "Proxy-Authorization"} {
			if v := h.Get(k); v != "" {
				t.Errorf("request %d sent %s: %q", i, k, v)
			}
		}
	}
}

func TestBudgetLimitsRequestsPerScan(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	c := New(Options{AllowPrivate: true})

	b := NewBudget(2, 0)
	ctx := WithBudget(context.Background(), b)
	for i := 0; i < 2; i++ {
		if _, err := c.Get(ctx, srv.URL); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Get(ctx, srv.URL); !errors.Is(err, ErrRequestLimit) {
		t.Errorf("third request err = %v, want ErrRequestLimit", err)
	}
	if b.Used() != 2 {
		t.Errorf("used = %d", b.Used())
	}
	// Other scans (contexts without this budget) are unaffected.
	if _, err := c.Get(context.Background(), srv.URL); err != nil {
		t.Errorf("unrelated request: %v", err)
	}
}

func TestBudgetRateLimitsAcrossHosts(t *testing.T) {
	b := NewBudget(0, 20) // 50ms apart, regardless of host
	ctx := WithBudget(context.Background(), b)
	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := b.take(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed < 90*time.Millisecond {
		t.Errorf("3 requests took %v, want >= ~100ms", elapsed)
	}
}
