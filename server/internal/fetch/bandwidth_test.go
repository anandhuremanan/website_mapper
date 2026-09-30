package fetch

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"websitemapper/internal/resource"
)

func pageServer(size int) *httptest.Server {
	page := "<title>x</title>" + strings.Repeat("a", size)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(page))
	}))
}

// TestBandwidthCapsAllScansTogether: three scans download in parallel
// through clients sharing one limiter; their combined rate stays at the cap.
func TestBandwidthCapsAllScansTogether(t *testing.T) {
	const rate = 512 << 10 // 512 KB/s
	srv := pageServer(100 << 10)
	defer srv.Close()
	bw := resource.NewBandwidth(rate)
	c := New(Options{AllowPrivate: true, Bandwidth: bw})

	var total atomic.Int64
	start := time.Now()
	var wg sync.WaitGroup
	for s := 0; s < 3; s++ {
		ctx := resource.WithAccount(context.Background(), resource.NewAccount(fmt.Sprint(s), 0))
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 2; i++ {
					resp, err := c.Get(ctx, srv.URL)
					if err != nil {
						t.Error(err)
						return
					}
					total.Add(int64(len(resp.Body)))
				}
			}()
		}
	}
	wg.Wait()
	elapsed := time.Since(start)
	// The first second's worth may go out as a burst; the rest is paced.
	achieved := float64(total.Load()-rate) / elapsed.Seconds()
	t.Logf("downloaded %.1f MB in %v: %.0f KB/s after the initial burst (cap %d KB/s)",
		float64(total.Load())/1e6, elapsed.Round(time.Millisecond), achieved/1024, rate>>10)
	if achieved > rate*1.15 {
		t.Errorf("combined rate %.0f B/s exceeds the cap %d B/s", achieved, rate)
	}
	if st := bw.Stats(); st.TotalBytes < total.Load() {
		t.Errorf("limiter accounted %d bytes, read %d", st.TotalBytes, total.Load())
	}
}

func TestDownloadBudgetStopsTheScan(t *testing.T) {
	srv := pageServer(30 << 10)
	defer srv.Close()
	c := New(Options{AllowPrivate: true})
	acct := resource.NewAccount("scan", 0).WithDownloadBudget(100 << 10)
	ctx := resource.WithAccount(context.Background(), acct)

	var err error
	fetched := 0
	for i := 0; i < 10 && err == nil; i++ {
		if _, err = c.Get(ctx, srv.URL); err == nil {
			fetched++
		}
	}
	if !errors.Is(err, resource.ErrDownloadBudget) || !errors.Is(err, resource.ErrBudgetExhausted) || !acct.DownloadExhausted() {
		t.Fatalf("err = %v after %d pages", err, fetched)
	}
	if fetched != 3 {
		t.Errorf("fetched %d pages of ~31 KB within a 100 KB budget, want 3", fetched)
	}
	// No further requests once the budget is gone.
	if _, err := c.Get(ctx, srv.URL); !errors.Is(err, resource.ErrDownloadBudget) {
		t.Errorf("request after budget: %v", err)
	}
}

func TestBandwidthWaitStopsOnCancel(t *testing.T) {
	srv := pageServer(200 << 10)
	defer srv.Close()
	c := New(Options{AllowPrivate: true, Bandwidth: resource.NewBandwidth(10 << 10)}) // 10 KB/s
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Get(ctx, srv.URL); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("took %v to stop", time.Since(start))
	}
}

func gzipServer(body []byte) *httptest.Server {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(body)
	zw.Close()
	compressed := buf.Bytes()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			w.Write(compressed)
			return
		}
		w.Write(body)
	}))
}

// TestBandwidthCountsWireBytes: a compressible page is charged at its
// compressed size, so a low cap does not throttle it by its decompressed size.
func TestBandwidthCountsWireBytes(t *testing.T) {
	page := []byte("<title>Docs</title>" + strings.Repeat("<p>Some repetitive documentation text.</p>", 25_000)) // ~1 MB
	srv := gzipServer(page)
	defer srv.Close()
	c := New(Options{AllowPrivate: true, MaxBodyBytes: 2 << 20, Bandwidth: resource.NewBandwidth(64 << 10)})
	acct := resource.NewAccount("scan", 0)
	start := time.Now()
	resp, err := c.Get(resource.WithAccount(context.Background(), acct), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(resp.Body, page) {
		t.Fatalf("decompressed body differs: %d bytes, want %d", len(resp.Body), len(page))
	}
	t.Logf("%d bytes of HTML charged as %d bytes on the wire, in %v", len(page), acct.Bytes(), time.Since(start).Round(time.Millisecond))
	if acct.Bytes() > 64<<10 {
		t.Errorf("charged %d bytes for a page that is a few KB compressed", acct.Bytes())
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("throttled as if uncompressed: %v", time.Since(start))
	}
}

func TestGzipBombIsBounded(t *testing.T) {
	srv := gzipServer(make([]byte, 50<<20)) // 50 MB of zeros, ~50 KB compressed
	defer srv.Close()
	c := New(Options{AllowPrivate: true, MaxBodyBytes: 1 << 20})
	resp, err := c.Get(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Body) != 1<<20 || !resp.Truncated {
		t.Errorf("body %d bytes, truncated %v", len(resp.Body), resp.Truncated)
	}
}
