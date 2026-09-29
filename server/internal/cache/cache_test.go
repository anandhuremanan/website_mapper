package cache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// clock is a manually advanced test clock.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newClock() *clock { return &clock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)} }

func value(v string, ttl time.Duration) func(context.Context) (Loaded[string], error) {
	return func(context.Context) (Loaded[string], error) {
		return Loaded[string]{Value: v, Cost: 10, TTL: ttl, Group: "g"}, nil
	}
}

func TestHitMissAndExpiry(t *testing.T) {
	clk := newClock()
	c := New[string](Options{Name: "t", MaxCost: 10_000, Now: clk.Now})
	var loads int
	load := func(context.Context) (Loaded[string], error) {
		loads++
		return Loaded[string]{Value: fmt.Sprint("v", loads), Cost: 10, TTL: time.Minute}, nil
	}

	v, info, _ := c.Do(context.Background(), "k", load)
	if v != "v1" || info.Hit {
		t.Fatalf("first = %q %+v", v, info)
	}
	clk.Advance(59 * time.Second)
	v, info, _ = c.Do(context.Background(), "k", load)
	if v != "v1" || !info.Hit || !info.CreatedAt.Equal(newClock().now) {
		t.Fatalf("fresh = %q %+v", v, info)
	}
	clk.Advance(time.Second) // now exactly at expiry
	v, info, _ = c.Do(context.Background(), "k", load)
	if v != "v2" || info.Hit || loads != 2 {
		t.Fatalf("expired = %q %+v loads %d", v, info, loads)
	}
	st := c.Stats()
	if st.Hits != 1 || st.Misses != 2 || st.Expired != 1 || st.Entries != 1 {
		t.Errorf("stats = %+v", st)
	}
}

func TestZeroTTLIsSharedButNotStored(t *testing.T) {
	c := New[string](Options{MaxCost: 10_000})
	c.Do(context.Background(), "k", value("x", 0))
	if _, _, ok := c.Get("k"); ok || c.Stats().Entries != 0 {
		t.Error("zero-TTL value was stored")
	}
}

func TestConcurrentMissesShareOneLoad(t *testing.T) {
	c := New[string](Options{MaxCost: 10_000})
	var loads atomic.Int32
	release := make(chan struct{})
	load := func(context.Context) (Loaded[string], error) {
		loads.Add(1)
		<-release
		return Loaded[string]{Value: "v", Cost: 10, TTL: time.Minute}, nil
	}
	var wg sync.WaitGroup
	results := make(chan Info, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, info, err := c.Do(context.Background(), "k", load)
			if err != nil || v != "v" {
				t.Errorf("got %q %v", v, err)
			}
			results <- info
		}()
	}
	for c.Stats().Coalesced < 19 {
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()
	close(results)
	shared := 0
	for info := range results {
		if info.Shared {
			shared++
		}
	}
	if loads.Load() != 1 || shared != 19 {
		t.Errorf("loads = %d, shared = %d", loads.Load(), shared)
	}
}

func TestWaitersRetryWhenLoaderFails(t *testing.T) {
	c := New[string](Options{MaxCost: 10_000})
	leaderStarted := make(chan struct{})
	failLeader := make(chan struct{})
	go c.Do(context.Background(), "k", func(context.Context) (Loaded[string], error) {
		close(leaderStarted)
		<-failLeader
		return Loaded[string]{}, errors.New("leader's request budget is exhausted")
	})
	<-leaderStarted
	done := make(chan string)
	go func() {
		v, _, err := c.Do(context.Background(), "k", value("mine", time.Minute))
		if err != nil {
			t.Error(err)
		}
		done <- v
	}()
	for c.Stats().Coalesced < 1 {
		time.Sleep(time.Millisecond)
	}
	close(failLeader)
	if v := <-done; v != "mine" {
		t.Errorf("waiter got %q, want its own load", v)
	}
}

func TestWaiterStopsOnItsOwnCancel(t *testing.T) {
	c := New[string](Options{MaxCost: 10_000})
	block := make(chan struct{})
	defer close(block)
	go c.Do(context.Background(), "k", func(context.Context) (Loaded[string], error) {
		<-block
		return Loaded[string]{}, nil
	})
	for len(c.inflightKeys()) == 0 {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := c.Do(ctx, "k", value("x", time.Minute)); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

func (c *Cache[V]) inflightKeys() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for k := range c.inflight {
		out = append(out, k)
	}
	return out
}

func TestBudgetEvictsLeastRecentlyUsed(t *testing.T) {
	// Each entry costs 30 + entryOverhead (350); four exceed the budget.
	c := New[string](Options{MaxCost: 1200, MaxEntryCost: 1200, GroupQuota: 1200})
	put := func(k string) {
		c.Do(context.Background(), k, func(context.Context) (Loaded[string], error) {
			return Loaded[string]{Value: k, Cost: 30, TTL: time.Hour, Group: "g"}, nil
		})
	}
	put("a")
	put("b")
	put("c")
	c.Get("a") // a is now more recent than b
	put("d")   // 1400 > 1200: evict the least recently used (b)
	if _, _, ok := c.Get("b"); ok {
		t.Error("b should be evicted")
	}
	for _, k := range []string{"a", "c", "d"} {
		if _, _, ok := c.Get(k); !ok {
			t.Errorf("%s evicted", k)
		}
	}
	if st := c.Stats(); st.Cost > 1200 || st.Evictions != 1 {
		t.Errorf("stats = %+v", st)
	}
}

func TestOversizedEntryAndGroupQuota(t *testing.T) {
	// Entries of cost 100 take 420 with the per-entry overhead: a quota of
	// 1300 holds three of them.
	c := New[string](Options{MaxCost: 10_000, MaxEntryCost: 700, GroupQuota: 1300})
	c.Do(context.Background(), "huge", func(context.Context) (Loaded[string], error) {
		return Loaded[string]{Value: "x", Cost: 500, TTL: time.Hour, Group: "big.com"}, nil
	})
	if _, _, ok := c.Get("huge"); ok || c.Stats().Rejected != 1 {
		t.Error("oversized entry was stored")
	}
	// One domain storing far more than its quota only evicts its own entries.
	c.Do(context.Background(), "other", func(context.Context) (Loaded[string], error) {
		return Loaded[string]{Value: "x", Cost: 100, TTL: time.Hour, Group: "small.com"}, nil
	})
	for i := 0; i < 50; i++ {
		k := fmt.Sprint("big", i)
		c.Do(context.Background(), k, func(context.Context) (Loaded[string], error) {
			return Loaded[string]{Value: "x", Cost: 100, TTL: time.Hour, Group: "big.com"}, nil
		})
	}
	if _, _, ok := c.Get("other"); !ok {
		t.Error("another domain's entry was evicted by one large domain")
	}
	if st := c.Stats(); st.Cost != 4*(100+entryOverhead) || st.Entries != 4 {
		t.Errorf("stats = %+v, want 3 big.com entries (quota 1300) + 1 other", st)
	}
}

// TestBoundedUnderLargeWorkload stores far more than the budget from many
// domains concurrently; the cost must never exceed the budget.
func TestBoundedUnderLargeWorkload(t *testing.T) {
	c := New[string](Options{MaxCost: 50_000})
	var wg sync.WaitGroup
	for d := 0; d < 20; d++ {
		wg.Add(1)
		go func(d int) {
			defer wg.Done()
			for i := 0; i < 5000; i++ {
				k := fmt.Sprintf("d%d/%d", d, i)
				c.Do(context.Background(), k, func(context.Context) (Loaded[string], error) {
					return Loaded[string]{Value: k, Cost: 97, TTL: time.Hour, Group: fmt.Sprint("d", d)}, nil
				})
				if st := c.Stats(); st.Cost > 50_000 {
					t.Errorf("cost %d exceeds budget", st.Cost)
					return
				}
			}
		}(d)
	}
	wg.Wait()
	st := c.Stats()
	if st.Cost > 50_000 || st.Evictions < 99_000 {
		t.Errorf("stats = %+v", st)
	}
}

func TestNilCacheAlwaysLoads(t *testing.T) {
	var c *Cache[string]
	n := 0
	for i := 0; i < 2; i++ {
		c.Do(context.Background(), "k", func(context.Context) (Loaded[string], error) {
			n++
			return Loaded[string]{Value: "v", TTL: time.Hour}, nil
		})
	}
	if n != 2 {
		t.Errorf("loads = %d", n)
	}
}
