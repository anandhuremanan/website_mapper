package subdomains

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"websitemapper/internal/cache"
	"websitemapper/internal/discovery"
)

// countingSource counts provider requests and can block or fail.
type countingSource struct {
	name  string
	calls atomic.Int32
	names []string
	err   error
	gate  chan struct{}
}

func (s *countingSource) Name() string                 { return s.name }
func (s *countingSource) Provenance() discovery.Source { return discovery.SourceCT }
func (s *countingSource) Discover(ctx context.Context, _ string) ([]string, error) {
	s.calls.Add(1)
	if s.gate != nil {
		<-s.gate
	}
	return s.names, s.err
}

type testClock struct{ now atomic.Int64 }

func (c *testClock) Now() time.Time          { return time.Unix(0, c.now.Load()) }
func (c *testClock) Advance(d time.Duration) { c.now.Add(int64(d)) }

func cachedEngine(clk *testClock, sources ...Source) *Engine {
	c := cache.New[Answer](cache.Options{Name: "certificate", MaxCost: 1 << 20, Now: clk.Now})
	return New(sources, time.Minute, quiet).WithCache(CacheOptions{Cache: c, TTL: 6 * time.Hour, RateLimitTTL: 15 * time.Minute, FailureTTL: 5 * time.Minute})
}

type collected struct {
	mu       sync.Mutex
	findings []discovery.Finding
}

func (c *collected) emit(f discovery.Finding) {
	c.mu.Lock()
	c.findings = append(c.findings, f)
	c.mu.Unlock()
}

func scanOnce(t *testing.T, e *Engine) (*collected, error) {
	t.Helper()
	col := &collected{}
	err := e.Discover(context.Background(), discovery.Input{Target: target(t, "example.com")}, col.emit)
	return col, err
}

func TestCertificateCacheHitAvoidsProvider(t *testing.T) {
	clk := &testClock{}
	src := &countingSource{name: "crt.sh", names: []string{"api.example.com", "*.example.com", "evil.org"}}
	e := cachedEngine(clk, src)

	first, err := scanOnce(t, e)
	if err != nil || len(first.findings) != 2 || !first.findings[0].CachedAt.IsZero() {
		t.Fatalf("first scan: %v %+v", err, first.findings)
	}
	clk.Advance(5 * time.Hour)
	second, err := scanOnce(t, e)
	if err != nil || src.calls.Load() != 1 {
		t.Fatalf("second scan: err %v, provider calls %d (want 1)", err, src.calls.Load())
	}
	// Same hosts, marked as coming from the cache with the original time.
	if len(second.findings) != 2 || second.findings[0].CachedAt.IsZero() || second.findings[0].Source != discovery.SourceCT {
		t.Errorf("cached findings = %+v", second.findings)
	}

	clk.Advance(2 * time.Hour) // past the 6 h TTL
	third, _ := scanOnce(t, e)
	if src.calls.Load() != 2 || !third.findings[0].CachedAt.IsZero() {
		t.Errorf("expired entry: provider calls %d, cachedAt %v", src.calls.Load(), third.findings[0].CachedAt)
	}
}

func TestCertificateCacheKeepsProviderFailuresIsolated(t *testing.T) {
	clk := &testClock{}
	good := &countingSource{name: "crt.sh", names: []string{"api.example.com"}}
	limited := &countingSource{name: "certspotter", err: fmt.Errorf("%w: Cert Spotter returned HTTP 429", ErrRateLimited)}
	e := cachedEngine(clk, good, limited)

	for i := 0; i < 2; i++ {
		col, err := scanOnce(t, e)
		var partial *discovery.PartialError
		if !errors.As(err, &partial) || !strings.Contains(err.Error(), "certspotter") || !strings.Contains(err.Error(), "429") {
			t.Fatalf("scan %d: err = %v, want a partial certspotter error", i, err)
		}
		if len(col.findings) != 1 {
			t.Fatalf("scan %d: hosts from the working provider were lost: %+v", i, col.findings)
		}
		if i == 1 && !strings.Contains(err.Error(), "answer cached at") {
			t.Errorf("cached failure should say so: %v", err)
		}
	}
	// The rate-limited answer is reused for 15 minutes, not re-requested.
	if limited.calls.Load() != 1 || good.calls.Load() != 1 {
		t.Errorf("calls: certspotter %d, crt.sh %d", limited.calls.Load(), good.calls.Load())
	}
	clk.Advance(16 * time.Minute)
	scanOnce(t, e)
	if limited.calls.Load() != 2 || good.calls.Load() != 1 {
		t.Errorf("after 16 min: certspotter %d (want retried), crt.sh %d (want cached)", limited.calls.Load(), good.calls.Load())
	}
}

func TestCertificateCacheKeepsFailuresOnlyBriefly(t *testing.T) {
	clk := &testClock{}
	flaky := &countingSource{name: "crt.sh", err: errors.New("crt.sh returned HTTP 502")}
	e := cachedEngine(clk, flaky)
	scanOnce(t, e)
	_, err := scanOnce(t, e)
	if flaky.calls.Load() != 1 || err == nil || !strings.Contains(err.Error(), "answer cached at") {
		t.Errorf("a failure is reused briefly and says so: calls = %d, err = %v", flaky.calls.Load(), err)
	}
	clk.Advance(6 * time.Minute) // past the 5 min failure TTL
	scanOnce(t, e)
	if flaky.calls.Load() != 2 {
		t.Errorf("a failure must not be kept longer than FailureTTL: calls = %d", flaky.calls.Load())
	}
}

func TestConcurrentScansShareOneProviderRequest(t *testing.T) {
	clk := &testClock{}
	src := &countingSource{name: "crt.sh", names: []string{"api.example.com"}, gate: make(chan struct{})}
	e := cachedEngine(clk, src)
	var wg sync.WaitGroup
	results := make([]*collected, 5)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], _ = scanOnce(t, e)
		}(i)
	}
	for e.cache.Cache.Stats().Coalesced < 4 {
		time.Sleep(time.Millisecond)
	}
	close(src.gate)
	wg.Wait()
	if src.calls.Load() != 1 {
		t.Fatalf("provider calls = %d, want 1", src.calls.Load())
	}
	for i, r := range results {
		if len(r.findings) != 1 {
			t.Errorf("scan %d got %d hosts", i, len(r.findings))
		}
	}
}
