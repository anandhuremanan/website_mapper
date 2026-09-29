package httpprobe

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"websitemapper/internal/cache"
	"websitemapper/internal/discovery"
	"websitemapper/internal/fetch"
	"websitemapper/internal/resource"
)

type clock struct{ now atomic.Int64 }

func (c *clock) Now() time.Time          { return time.Unix(0, c.now.Load()) }
func (c *clock) Advance(d time.Duration) { c.now.Add(int64(d)) }

// pooledFetcher takes a slot from the shared HTTP pool for every request,
// as the real client does.
type pooledFetcher struct {
	pool *resource.Pool
	next Fetcher
}

func (p pooledFetcher) Get(ctx context.Context, u string) (*fetch.Response, error) {
	release, _, err := p.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return p.next.Get(ctx, u)
}

func TestProbeCache(t *testing.T) {
	clk := &clock{}
	hosts := &fakeHosts{responses: map[string]*fetch.Response{
		"https://app.example.com/": {StatusCode: 200, ContentType: "text/html", Body: []byte("<title>App</title>")},
	}}
	pool := resource.NewPool("http", 4)
	opts := Options{
		MaxHosts: 10, Concurrency: 2, TTL: 2 * time.Minute,
		Cache: cache.New[discovery.HTTPInfo](cache.Options{Name: "probe", MaxCost: 1 << 20, Now: clk.Now}),
	}
	f := pooledFetcher{pool: pool, next: hosts}

	first := run(t, f, opts, resolved("app.example.com"), resolved("down.example.com"))
	// app: one https request; down: https and http both refused.
	if len(hosts.requested) != 3 || pool.Stats().Acquired != 3 || first["app.example.com"].CachedAt != nil {
		t.Fatalf("first: requests %v, pool %d", hosts.requested, pool.Stats().Acquired)
	}

	clk.Advance(time.Minute)
	second := run(t, f, opts, resolved("app.example.com"), resolved("down.example.com"))
	if len(hosts.requested) != 3 || pool.Stats().Acquired != 3 {
		t.Errorf("cache hits made requests %v or took pool slots (%d)", hosts.requested, pool.Stats().Acquired)
	}
	app, down := second["app.example.com"], second["down.example.com"]
	if !app.Reachable || app.Title != "App" || app.CachedAt == nil {
		t.Errorf("cached app = %+v", app)
	}
	// Unreachable results are reused for the (short) TTL too.
	if down.Reachable || down.Error != "connection refused" || down.CachedAt == nil {
		t.Errorf("cached down = %+v", down)
	}

	clk.Advance(2 * time.Minute) // past the TTL
	run(t, f, opts, resolved("app.example.com"))
	if len(hosts.requested) != 4 {
		t.Errorf("expired entry was not probed again: %v", hosts.requested)
	}
}

func TestProbeCacheIgnoresScanSpecificOutcomes(t *testing.T) {
	hosts := &fakeHosts{responses: map[string]*fetch.Response{"https://app.example.com/": {StatusCode: 200}}}
	c := cache.New[discovery.HTTPInfo](cache.Options{Name: "probe", MaxCost: 1 << 20})
	// A scan whose request budget is spent gets a "skipped" result...
	budget := budgetFetcher{}
	got := run(t, budget, Options{MaxHosts: 10, Concurrency: 1, TTL: time.Hour, Cache: c}, resolved("app.example.com"))
	if got["app.example.com"].Skipped != discovery.SkipRequestLimit {
		t.Fatalf("got %+v", got["app.example.com"])
	}
	// ...which must not be served to the next scan.
	got = run(t, hosts, Options{MaxHosts: 10, Concurrency: 1, TTL: time.Hour, Cache: c}, resolved("app.example.com"))
	if !got["app.example.com"].Reachable || len(hosts.requested) != 1 {
		t.Errorf("next scan got %+v", got["app.example.com"])
	}
}

type budgetFetcher struct{}

func (budgetFetcher) Get(context.Context, string) (*fetch.Response, error) {
	return nil, resource.ErrBudgetExhausted
}
