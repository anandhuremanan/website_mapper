package scan_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"websitemapper/internal/discovery"
	"websitemapper/internal/discovery/dnsresolve"
	"websitemapper/internal/fetch"
	"websitemapper/internal/resource"
	"websitemapper/internal/scan"
	"websitemapper/internal/store"
)

// gateEngine blocks each scan until released (or cancelled) and records
// which scans started.
type gateEngine struct {
	release chan struct{}
	mu      sync.Mutex
	started map[string]bool // by target domain
}

func newGate() *gateEngine {
	return &gateEngine{release: make(chan struct{}), started: map[string]bool{}}
}

func (g *gateEngine) Name() string { return "gate" }
func (g *gateEngine) Discover(ctx context.Context, in discovery.Input, emit discovery.Emit) error {
	g.mu.Lock()
	g.started[in.Target.Domain] = true
	g.mu.Unlock()
	emit(discovery.Finding{URL: "https://" + in.Target.Domain + "/partial", Source: discovery.SourceHTML, Hint: discovery.HintLink})
	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *gateEngine) didStart(domain string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.started[domain]
}

func waitStatus(t *testing.T, svc *scan.Service, id string, want scan.Status) scan.Scan {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		sc, err := svc.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if sc.Status == want {
			return sc
		}
		if time.Now().After(deadline) {
			t.Fatalf("scan %s status = %s, want %s", id, sc.Status, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func mustCreate(t *testing.T, svc *scan.Service, target string) scan.Scan {
	t.Helper()
	sc, err := svc.Create(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

func TestSchedulerRunsUpToLimitAndQueuesTheRest(t *testing.T) {
	g := newGate()
	svc := startService(t, []scan.Stage{stage("crawl", g)}, scan.Options{MaxRunning: 2})

	a := mustCreate(t, svc, "a.com")
	b := mustCreate(t, svc, "b.com")
	c := mustCreate(t, svc, "c.com")
	d := mustCreate(t, svc, "d.com")
	if a.Status != scan.StatusRunning || b.Status != scan.StatusRunning {
		t.Fatalf("a, b = %s, %s; want running", a.Status, b.Status)
	}
	if c.Status != scan.StatusQueued || c.QueuePosition != 1 || d.QueuePosition != 2 {
		t.Fatalf("c = %s #%d, d #%d", c.Status, c.QueuePosition, d.QueuePosition)
	}
	if got, _ := svc.Get(context.Background(), d.ID); got.Phase != scan.PhaseQueued || got.QueuePosition != 2 {
		t.Errorf("d via Get = %s #%d", got.Phase, got.QueuePosition)
	}
	if st := svc.Stats(); st.Running != 2 || st.Queued != 2 {
		t.Errorf("stats = %+v", st)
	}

	// Finishing running scans starts queued ones in FIFO order.
	g.release <- struct{}{}
	waitStatus(t, svc, c.ID, scan.StatusRunning)
	if got, _ := svc.Get(context.Background(), d.ID); got.QueuePosition != 1 {
		t.Errorf("d moved to #%d, want #1", got.QueuePosition)
	}
	close(g.release)
	for _, id := range []string{a.ID, b.ID, c.ID, d.ID} {
		waitStatus(t, svc, id, scan.StatusCompleted)
	}
	if st := svc.Stats(); st.Running != 0 || st.Queued != 0 {
		t.Errorf("stats after = %+v", st)
	}
}

func TestCancelQueuedScan(t *testing.T) {
	g := newGate()
	svc := startService(t, []scan.Stage{stage("crawl", g)}, scan.Options{MaxRunning: 1})
	a := mustCreate(t, svc, "a.com")
	b := mustCreate(t, svc, "b.com")
	c := mustCreate(t, svc, "c.com")

	got, err := svc.Cancel(context.Background(), b.ID, "")
	if err != nil || got.Status != scan.StatusCancelled || got.StopReason != scan.StopCancel || got.FinishedAt == nil {
		t.Fatalf("cancel queued = %+v, %v", got, err)
	}
	if got, _ := svc.Get(context.Background(), c.ID); got.QueuePosition != 1 {
		t.Errorf("c moved to #%d, want #1", got.QueuePosition)
	}
	if _, err := svc.Cancel(context.Background(), b.ID, ""); !errors.Is(err, scan.ErrFinished) {
		t.Errorf("second cancel err = %v", err)
	}
	if _, err := svc.Cancel(context.Background(), "nope", ""); !errors.Is(err, scan.ErrNotFound) {
		t.Errorf("cancel unknown err = %v", err)
	}

	close(g.release)
	waitStatus(t, svc, a.ID, scan.StatusCompleted)
	waitStatus(t, svc, c.ID, scan.StatusCompleted)
	if g.didStart("b.com") {
		t.Error("cancelled queued scan ran")
	}
	// A scan cancelled before it ran has no result.
	if _, err := svc.Result(context.Background(), b.ID); !errors.Is(err, scan.ErrNotFound) {
		t.Errorf("result err = %v", err)
	}
}

func TestCancelRunningScanReleasesCapacity(t *testing.T) {
	g := newGate()
	svc := startService(t, []scan.Stage{stage("crawl", g), stage("later", &fakeEngine{name: "later"})}, scan.Options{MaxRunning: 1})
	a := mustCreate(t, svc, "a.com")
	b := mustCreate(t, svc, "b.com")
	waitFor(t, func() bool { return g.didStart("a.com") })

	if _, err := svc.Cancel(context.Background(), a.ID, ""); err != nil {
		t.Fatal(err)
	}
	got := waitStatus(t, svc, a.ID, scan.StatusCancelled)
	if got.StopReason != scan.StopCancel || got.Steps[1].Status != scan.StepStopped || got.Steps[2].Status != scan.StepSkipped {
		t.Errorf("cancelled scan = %+v", got)
	}
	// Partial results collected before cancellation are kept.
	res, err := svc.Result(context.Background(), a.ID)
	if err != nil || res.Status != scan.StatusCancelled || res.Counts.URLs != 1 {
		t.Errorf("result = %+v, %v", res.Counts, err)
	}
	// Its slot goes to the queued scan.
	waitStatus(t, svc, b.ID, scan.StatusRunning)
	close(g.release)
	waitStatus(t, svc, b.ID, scan.StatusCompleted)
}

func TestTimeoutReleasesCapacity(t *testing.T) {
	g := newGate()
	svc := startService(t, []scan.Stage{stage("crawl", g)}, scan.Options{MaxRunning: 1, ScanTimeout: 50 * time.Millisecond})
	a := mustCreate(t, svc, "a.com")
	b := mustCreate(t, svc, "b.com")
	for _, id := range []string{a.ID, b.ID} {
		sc := waitStatus(t, svc, id, scan.StatusCompleted)
		if sc.StopReason != scan.StopTimeout || !hasLimit(sc.Limits, scan.LimitScanTimeout) {
			t.Errorf("scan = %+v", sc)
		}
	}
	if st := svc.Stats(); st.Running != 0 {
		t.Errorf("running = %d", st.Running)
	}
}

func TestQueueIsBounded(t *testing.T) {
	g := newGate()
	defer close(g.release)
	svc := startService(t, []scan.Stage{stage("crawl", g)}, scan.Options{MaxRunning: 1, QueueSize: 2})
	mustCreate(t, svc, "a.com") // running
	mustCreate(t, svc, "b.com") // queued #1
	mustCreate(t, svc, "c.com") // queued #2
	if _, err := svc.Create(context.Background(), "d.com"); !errors.Is(err, scan.ErrQueueFull) {
		t.Errorf("err = %v, want ErrQueueFull", err)
	}
}

// poolEngine simulates scan work: workers repeatedly take a slot from the
// shared pool, "request" for a moment, and release it, until the scan ends
// (or ops operations are done, when ops > 0).
type poolEngine struct {
	pool    *resource.Pool
	workers int
	ops     int
	hold    time.Duration

	mu        sync.Mutex
	perScan   map[string]*atomic.Int64 // completed operations
	finished  map[string]time.Time
	afterStop atomic.Int64 // operations started after the scan's ctx ended
}

func newPoolEngine(pool *resource.Pool, workers, ops int, hold time.Duration) *poolEngine {
	return &poolEngine{pool: pool, workers: workers, ops: ops, hold: hold,
		perScan: map[string]*atomic.Int64{}, finished: map[string]time.Time{}}
}

func (e *poolEngine) Name() string { return "http" }
func (e *poolEngine) Discover(ctx context.Context, in discovery.Input, emit discovery.Emit) error {
	e.mu.Lock()
	done := &atomic.Int64{}
	e.perScan[in.Target.Domain] = done
	e.mu.Unlock()

	var remaining atomic.Int64
	remaining.Store(int64(e.ops))
	var wg sync.WaitGroup
	for w := 0; w < e.workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if e.ops > 0 && remaining.Add(-1) < 0 {
					return
				}
				release, _, err := e.pool.Acquire(ctx)
				if err != nil {
					return
				}
				if ctx.Err() != nil {
					e.afterStop.Add(1)
				}
				time.Sleep(e.hold)
				release()
				done.Add(1)
			}
		}()
	}
	wg.Wait()
	e.mu.Lock()
	e.finished[in.Target.Domain] = time.Now()
	e.mu.Unlock()
	return ctx.Err()
}

func (e *poolEngine) completed(domain string) int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c := e.perScan[domain]; c != nil {
		return c.Load()
	}
	return 0
}

// TestCancellationPropagatesAndLeavesNoGoroutines: a running scan with many
// workers busy on the shared pool is cancelled; its workers stop, its pool
// slots are released and no goroutines are left behind.
func TestCancellationPropagatesAndLeavesNoGoroutines(t *testing.T) {
	base := runtime.NumGoroutine()
	pool := resource.NewPool("http", 4)
	eng := newPoolEngine(pool, 32, 0, time.Millisecond)
	svc := startService(t, []scan.Stage{stage("crawl", eng)}, scan.Options{MaxRunning: 2, Pools: []*resource.Pool{pool}})

	a := mustCreate(t, svc, "a.com")
	waitFor(t, func() bool { return eng.completed("a.com") > 20 })
	if _, err := svc.Cancel(context.Background(), a.ID, ""); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, svc, a.ID, scan.StatusCancelled)

	before := eng.completed("a.com")
	time.Sleep(50 * time.Millisecond)
	if after := eng.completed("a.com"); after != before {
		t.Errorf("work continued after cancellation: %d -> %d ops", before, after)
	}
	if n := eng.afterStop.Load(); n != 0 {
		t.Errorf("%d operations started after cancellation", n)
	}
	if st := pool.Stats(); st.InUse != 0 || st.Waiting != 0 {
		t.Errorf("pool not released: %+v", st)
	}
	waitFor(t, func() bool { return runtime.NumGoroutine() <= base+3 })
}

// TestGlobalHTTPLimitAcrossConcurrentScans runs several scans at once, each
// with a high per-scan concurrency, through the real HTTP client against a
// local server that measures concurrent requests.
func TestGlobalHTTPLimitAcrossConcurrentScans(t *testing.T) {
	const globalHTTP = 3
	var inFlight, peak, served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		served.Add(1)
		time.Sleep(3 * time.Millisecond)
	}))
	defer srv.Close()

	pool := resource.NewPool("http", globalHTTP)
	client := fetch.New(fetch.Options{AllowPrivate: true, Timeout: 5 * time.Second, Pool: pool})
	eng := &httpWorkEngine{client: client, url: srv.URL, workers: 12, requests: 30}
	svc := startService(t, []scan.Stage{stage("crawl", eng)}, scan.Options{MaxRunning: 4, Pools: []*resource.Pool{pool}})

	var ids []string
	for _, d := range []string{"a.com", "b.com", "c.com", "d.com"} {
		ids = append(ids, mustCreate(t, svc, d).ID)
	}
	for _, id := range ids {
		sc := waitStatus(t, svc, id, scan.StatusCompleted)
		if sc.Resources.Requests != 30 {
			t.Errorf("scan requests = %d, want 30", sc.Resources.Requests)
		}
	}
	if p := peak.Load(); p > globalHTTP {
		t.Fatalf("server saw %d concurrent requests from 4 scans, global limit is %d", p, globalHTTP)
	}
	if served.Load() != 4*30 {
		t.Errorf("served %d", served.Load())
	}
	if st := pool.Stats(); st.Peak > globalHTTP || st.InUse != 0 {
		t.Errorf("pool = %+v", st)
	}
}

// httpWorkEngine issues a fixed number of requests with many workers.
type httpWorkEngine struct {
	client   *fetch.Client
	url      string
	workers  int
	requests int
}

func (e *httpWorkEngine) Name() string { return "html" }
func (e *httpWorkEngine) Discover(ctx context.Context, in discovery.Input, emit discovery.Emit) error {
	var next atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < e.workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := next.Add(1); i <= int64(e.requests); i = next.Add(1) {
				if _, err := e.client.Get(ctx, fmt.Sprintf("%s/%d", e.url, i)); err != nil {
					return
				}
			}
		}()
	}
	wg.Wait()
	return ctx.Err()
}

// countingResolver measures concurrent lookups.
type countingResolver struct{ inFlight, peak atomic.Int64 }

func (r *countingResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	n := r.inFlight.Add(1)
	defer r.inFlight.Add(-1)
	for old := r.peak.Load(); n > old && !r.peak.CompareAndSwap(old, n); old = r.peak.Load() {
	}
	time.Sleep(200 * time.Microsecond)
	return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
}
func (r *countingResolver) LookupCNAME(context.Context, string) (string, error) { return "", nil }

// hostsEngine reports many CT hostnames, like a large domain.
type hostsEngine struct{ n int }

func (e hostsEngine) Name() string { return "subdomains" }
func (e hostsEngine) Discover(ctx context.Context, in discovery.Input, emit discovery.Emit) error {
	for i := 0; i < e.n; i++ {
		emit(discovery.Finding{Host: fmt.Sprintf("h%d.%s", i, in.Target.Domain), Source: discovery.SourceCT})
	}
	return nil
}

func TestGlobalDNSLimitAcrossConcurrentScans(t *testing.T) {
	const globalDNS = 4
	r := &countingResolver{}
	pool := resource.NewPool("dns", globalDNS)
	dns := dnsresolve.New(r, dnsresolve.Options{MaxHosts: 5000, Concurrency: 64, Pool: pool})
	svc := startService(t, []scan.Stage{stage("subdomains", hostsEngine{n: 800}), stage("resolve", dns)},
		scan.Options{MaxRunning: 3, Pools: []*resource.Pool{pool}})
	var ids []string
	for _, d := range []string{"a.com", "b.com", "c.com"} {
		ids = append(ids, mustCreate(t, svc, d).ID)
	}
	for _, id := range ids {
		sc := waitStatus(t, svc, id, scan.StatusCompleted)
		if sc.Counts.HostsResolved != 801 || sc.Counts.HostsResolvePending != 0 {
			t.Errorf("counts = %+v", sc.Counts)
		}
	}
	if p := r.peak.Load(); p > globalDNS {
		t.Fatalf("peak concurrent lookups = %d, global limit %d", p, globalDNS)
	}
}

// TestLargeScanDoesNotStarveSmallScans: one scan keeps 200 workers busy on
// a pool of 4 slots; small scans started afterwards must still finish
// quickly. Plain FIFO would make each small operation wait behind ~200
// queued operations of the large scan.
func TestLargeScanDoesNotStarveSmallScans(t *testing.T) {
	pool := resource.NewPool("http", 4)
	big := newPoolEngine(pool, 200, 0, 2*time.Millisecond)
	small := newPoolEngine(pool, 1, 20, 2*time.Millisecond)
	route := &routeEngine{byDomain: map[string]discovery.Engine{"big.com": big}, fallback: small}
	svc := startService(t, []scan.Stage{stage("crawl", route)}, scan.Options{MaxRunning: 3, Pools: []*resource.Pool{pool}})

	bigScan := mustCreate(t, svc, "big.com")
	waitFor(t, func() bool { return pool.Stats().Waiting > 150 })

	start := time.Now()
	s1 := mustCreate(t, svc, "small1.com")
	s2 := mustCreate(t, svc, "small2.com")
	waitStatus(t, svc, s1.ID, scan.StatusCompleted)
	waitStatus(t, svc, s2.ID, scan.StatusCompleted)
	elapsed := time.Since(start)

	// 20 sequential 2 ms operations each: about 40 ms of work. Allow ample
	// slack for slow machines; FIFO would need roughly 20 x 200 x 2 ms / 4
	// = 2 s.
	if elapsed > time.Second {
		t.Errorf("small scans took %v while a large scan was running", elapsed)
	}
	if got, _ := svc.Get(context.Background(), bigScan.ID); got.Status != scan.StatusRunning {
		t.Errorf("big scan = %s; it should still be running", got.Status)
	}
	if _, err := svc.Cancel(context.Background(), bigScan.ID, ""); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, svc, bigScan.ID, scan.StatusCancelled)
}

// routeEngine dispatches to a different engine per target domain.
type routeEngine struct {
	byDomain map[string]discovery.Engine
	fallback discovery.Engine
}

func (r *routeEngine) Name() string { return "html" }
func (r *routeEngine) Discover(ctx context.Context, in discovery.Input, emit discovery.Emit) error {
	if e, ok := r.byDomain[in.Target.Domain]; ok {
		return e.Discover(ctx, in, emit)
	}
	return r.fallback.Discover(ctx, in, emit)
}

// TestGlobalWaitIsReported: a scan that had to wait for shared capacity is
// told so, without it being treated as lost coverage.
func TestGlobalWaitIsReported(t *testing.T) {
	pool := resource.NewPool("http", 1)
	eng := newPoolEngine(pool, 1, 1, 0)
	svc := startService(t, []scan.Stage{stage("crawl", eng)}, scan.Options{MaxRunning: 1, Pools: []*resource.Pool{pool}})
	hold, _, _ := pool.Acquire(context.Background())
	a := mustCreate(t, svc, "a.com")
	time.Sleep(1100 * time.Millisecond)
	hold()
	sc := waitStatus(t, svc, a.ID, scan.StatusCompleted)
	if u := sc.Resources.Pools["http"]; !hasLimit(sc.Limits, scan.LimitGlobalResource) || u.Delayed != 1 || u.AvgWaitMs < 1000 {
		t.Errorf("limits = %+v resources = %+v", sc.Limits, sc.Resources)
	}
	if sc.StopReason != "" {
		t.Errorf("stop reason = %q", sc.StopReason)
	}
}

func TestRequestBudgetIsEnforcedAndReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	client := fetch.New(fetch.Options{AllowPrivate: true})
	eng := &httpWorkEngine{client: client, url: srv.URL, workers: 4, requests: 100}
	svc := startService(t, []scan.Stage{stage("crawl", eng)}, scan.Options{MaxRequests: 25})
	sc := waitStatus(t, svc, mustCreate(t, svc, "a.com").ID, scan.StatusCompleted)
	if sc.Resources.Requests != 25 || sc.Resources.MaxRequests != 25 || !hasLimit(sc.Limits, scan.LimitRequestBudget) {
		t.Errorf("resources = %+v limits = %+v", sc.Resources, sc.Limits)
	}
}

func TestShutdownCancelsQueuedAndRunningScans(t *testing.T) {
	g := newGate()
	repo := store.NewMemory(store.Limits{MaxScans: 100})
	svc := scan.NewService(repo, []scan.Stage{stage("crawl", g)},
		scan.Options{MaxRunning: 1, QueueSize: 10, ProgressInterval: 10 * time.Millisecond}, quiet)
	svc.Start(context.Background())
	a := mustCreate(t, svc, "a.com")
	b := mustCreate(t, svc, "b.com")
	waitFor(t, func() bool { return g.didStart("a.com") })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := svc.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	ra, _ := svc.Get(context.Background(), a.ID)
	rb, _ := svc.Get(context.Background(), b.ID)
	if ra.Status != scan.StatusCancelled || ra.StopReason != scan.StopShutdown {
		t.Errorf("running scan = %s / %q", ra.Status, ra.StopReason)
	}
	if rb.Status != scan.StatusCancelled || rb.StopReason != scan.StopShutdown || g.didStart("b.com") {
		t.Errorf("queued scan = %s / %q", rb.Status, rb.StopReason)
	}
	if _, err := svc.Create(context.Background(), "c.com"); !errors.Is(err, scan.ErrShuttingDown) {
		t.Errorf("create after shutdown err = %v", err)
	}
	svc.Wait() // must not block
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}
