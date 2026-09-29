package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"websitemapper/internal/resource"
)

// TestRateLimitUnderPoolContention: many hosts, each crawled by several
// workers, share a small global pool. Each host must still be requested at
// (close to) its configured rate: waiting for a pool slot must not cost a
// host rate-limit slots. Run with -v for the measured rate.
func TestRateLimitUnderPoolContention(t *testing.T) {
	if testing.Short() {
		t.Skip("timing measurement")
	}
	const (
		hosts, workersPerHost, requestsPerHost = 32, 4, 20
		rps                                    = 50.0 // per host
		poolSize                               = 8
	)
	var servers []*httptest.Server
	for i := 0; i < hosts; i++ {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(10 * time.Millisecond) // response latency
		}))
		defer srv.Close()
		servers = append(servers, srv)
	}
	c := New(Options{AllowPrivate: true, RequestsPerSecond: rps, Pool: resource.NewPool("http", poolSize)})

	start := time.Now()
	var wg sync.WaitGroup
	for _, srv := range servers {
		var mu sync.Mutex
		left := requestsPerHost
		for w := 0; w < workersPerHost; w++ {
			wg.Add(1)
			go func(url string) {
				defer wg.Done()
				for {
					mu.Lock()
					if left == 0 {
						mu.Unlock()
						return
					}
					left--
					mu.Unlock()
					if _, err := c.Get(context.Background(), url); err != nil {
						t.Error(err)
						return
					}
				}
			}(srv.URL)
		}
	}
	wg.Wait()
	elapsed := time.Since(start)

	// Lower bounds: the per-host rate (20 requests at 50/s = 0.4 s) and the
	// pool (640 requests x 10 ms / 8 slots = 0.8 s).
	ideal := max(time.Duration(float64(requestsPerHost)/rps*float64(time.Second)),
		time.Duration(hosts*requestsPerHost)*10*time.Millisecond/poolSize)
	t.Logf("%d hosts x %d requests, pool %d: %v (lower bound %v, %.1fx)",
		hosts, requestsPerHost, poolSize, elapsed.Round(time.Millisecond), ideal, float64(elapsed)/float64(ideal))
	if elapsed > 3*ideal {
		t.Errorf("took %v, more than 3x the lower bound %v: pool waits are costing too many rate-limit slots", elapsed, ideal)
	}
}
