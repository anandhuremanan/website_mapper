package htmlcrawl

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"websitemapper/internal/discovery"
	"websitemapper/internal/fetch"
)

// politeSite serves generated sites for many hosts and enforces a minimum
// interval between requests to the same host, like the real per-host rate
// limiter. Each host's root links pagesPerHost[host]-1 further pages.
type politeSite struct {
	interval time.Duration
	latency  time.Duration
	pages    map[string]int

	mu   sync.Mutex
	next map[string]time.Time
}

func (s *politeSite) Get(ctx context.Context, raw string) (*fetch.Response, error) {
	u, _ := url.Parse(raw)
	s.mu.Lock()
	now := time.Now()
	slot := s.next[u.Host]
	if slot.Before(now) {
		slot = now
	}
	s.next[u.Host] = slot.Add(s.interval)
	s.mu.Unlock()
	select {
	case <-time.After(time.Until(slot) + s.latency):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	var b strings.Builder
	if u.Path == "/" {
		for i := 1; i < s.pages[u.Host]; i++ {
			fmt.Fprintf(&b, `<a href="/p%d">p</a>`, i)
		}
	}
	return htmlPage(b.String()), nil
}

func sites(t *testing.T, s *politeSite) discovery.Input {
	var hosts []discovery.HostView
	for h := range s.pages {
		hosts = append(hosts, reachable(h))
	}
	return input(t, "example.com", hosts...)
}

// TestLargeHostDoesNotBlockOtherHosts: with two host slots, one host with
// many pages holds one slot for a long time; all small hosts still finish
// through the other slot before the large one does.
func TestLargeHostDoesNotBlockOtherHosts(t *testing.T) {
	site := &politeSite{interval: 5 * time.Millisecond, pages: map[string]int{"example.com": 100}, next: map[string]time.Time{}}
	for i := 0; i < 10; i++ {
		site.pages[fmt.Sprintf("s%d.example.com", i)] = 3
	}
	var mu sync.Mutex
	var order []string
	emit := func(f discovery.Finding) {
		if f.Crawl != nil {
			mu.Lock()
			order = append(order, f.Host)
			mu.Unlock()
		}
	}
	c := New(site, Options{MaxHosts: 20, MaxDepth: 1, MaxPagesPerHost: 200, Concurrency: 4, HostConcurrency: 2}, quiet)
	if err := c.Discover(context.Background(), sites(t, site), emit); err != nil {
		t.Fatal(err)
	}
	if len(order) != 11 || order[len(order)-1] != "example.com" {
		t.Errorf("crawl completion order = %v; the large host should finish last", order)
	}
}

// TestCrawlThroughputScalesWithHostConcurrency measures a scan of many
// rate-limited hosts. Per-host politeness is unchanged; crawling more hosts
// at once is what shortens the scan. Run with -v for timings.
func TestCrawlThroughputScalesWithHostConcurrency(t *testing.T) {
	if testing.Short() {
		t.Skip("timing measurement")
	}
	run := func(hostConcurrency int) time.Duration {
		// 40 hosts x 20 pages at 100 requests/second/host (the real
		// default is 5/s; everything is scaled 20x so the test is quick).
		site := &politeSite{interval: 10 * time.Millisecond, latency: 2 * time.Millisecond, pages: map[string]int{}, next: map[string]time.Time{}}
		for i := 0; i < 40; i++ {
			site.pages[fmt.Sprintf("h%d.example.com", i)] = 20
		}
		c := New(site, Options{MaxHosts: 100, MaxDepth: 1, MaxPagesPerHost: 100, Concurrency: 4, HostConcurrency: hostConcurrency}, quiet)
		start := time.Now()
		if err := c.Discover(context.Background(), sites(t, site), func(discovery.Finding) {}); err != nil {
			t.Fatal(err)
		}
		return time.Since(start)
	}
	d8, d32 := run(8), run(32)
	t.Logf("40 hosts x 20 pages: host concurrency 8 = %v, 32 = %v (%.1fx)", d8.Round(time.Millisecond), d32.Round(time.Millisecond), float64(d8)/float64(d32))
	if d32 >= d8 {
		t.Errorf("more parallel hosts should be faster: %v vs %v", d32, d8)
	}
}
