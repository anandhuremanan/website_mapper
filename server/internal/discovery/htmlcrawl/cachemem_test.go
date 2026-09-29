package htmlcrawl

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"

	"websitemapper/internal/cache"
	"websitemapper/internal/discovery"
)

func heapNow() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// TestCacheCostEstimateMatchesHeap fills caches past their budgets with
// realistic entries and compares the estimated cost with real heap growth.
// Run with -v.
func TestCacheCostEstimateMatchesHeap(t *testing.T) {
	if testing.Short() {
		t.Skip("memory measurement")
	}
	const budget = 16 << 20
	before := heapNow()
	pc := cache.New[Page](cache.Options{Name: "page", MaxCost: budget})
	for i := 0; i < 20000; i++ {
		key := fmt.Sprintf("https://host-%d.example.com/section/article-%d", i%50, i)
		p := Page{Status: 200, ContentType: "text/html", Server: "nginx", Title: "A typical article title for a page"}
		p.Refs = make([]PageRef, 60) // ~60 links and resources per page, allocated exactly as the crawler does
		for r := range p.Refs {
			p.Refs[r] = (PageRef{URL: fmt.Sprintf("https://host-%d.example.com/section/other-article-%d", i%50, r), Hint: discovery.HintLink})
		}
		pc.Do(context.Background(), key, func(context.Context) (cache.Loaded[Page], error) {
			return cache.Loaded[Page]{Value: p, TTL: time.Hour, Group: fmt.Sprint(i % 50), Cost: pageCost(key, p)}, nil
		})
	}
	pageHeap := heapNow() - before
	st := pc.Stats()
	t.Logf("page cache: %d entries, estimated %.1f MB (budget %d MB), heap %.1f MB, heap/estimate %.2f",
		st.Entries, float64(st.Cost)/1e6, budget>>20, float64(pageHeap)/1e6, float64(pageHeap)/float64(st.Cost))
	if st.Cost > budget {
		t.Errorf("estimated cost %d exceeds budget", st.Cost)
	}
	if float64(pageHeap) > 2*float64(budget) {
		t.Errorf("real heap %d is more than twice the budget", pageHeap)
	}
	runtime.KeepAlive(pc)

	before = heapNow()
	dc := cache.New[discovery.DNSInfo](cache.Options{Name: "dns", MaxCost: 4 << 20})
	for i := 0; i < 100000; i++ {
		host := fmt.Sprintf("host-%d.example.com", i)
		info := discovery.DNSInfo{Resolved: true, Addresses: []string{"93.184.216.34", "2606:2800:220:1:248:1893:25c8:1946"}}
		dc.Do(context.Background(), host, func(context.Context) (cache.Loaded[discovery.DNSInfo], error) {
			return cache.Loaded[discovery.DNSInfo]{Value: info, TTL: time.Hour, Group: fmt.Sprint(i % 20),
				Cost: 96 + cache.StringCost(host) + cache.StringCost(info.Addresses...)}, nil
		})
	}
	dnsHeap := heapNow() - before
	ds := dc.Stats()
	t.Logf("dns cache:  %d entries, estimated %.1f MB (budget 4 MB), heap %.1f MB, heap/estimate %.2f",
		ds.Entries, float64(ds.Cost)/1e6, float64(dnsHeap)/1e6, float64(dnsHeap)/float64(ds.Cost))
	runtime.KeepAlive(dc)
}
