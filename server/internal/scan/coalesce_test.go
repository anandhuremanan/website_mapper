package scan_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"websitemapper/internal/discovery"
	"websitemapper/internal/scan"
)

// countingGate is a gateEngine that also counts how many scans ran it.
type countingGate struct {
	*gateEngine
	runs atomic.Int32
}

func (g *countingGate) Discover(ctx context.Context, in discovery.Input, emit discovery.Emit) error {
	g.runs.Add(1)
	return g.gateEngine.Discover(ctx, in, emit)
}

func TestIdenticalScansShareOneScan(t *testing.T) {
	g := &countingGate{gateEngine: newGate()}
	svc := startService(t, []scan.Stage{stage("crawl", g)}, scan.Options{MaxRunning: 2})

	first := mustCreate(t, svc, "example.com")
	var joined []scan.Scan
	for _, in := range []string{"example.com", "https://example.com", "https://EXAMPLE.com/"} {
		joined = append(joined, mustCreate(t, svc, in))
	}
	tokens := map[string]bool{first.SubscriptionID: true}
	for _, j := range joined {
		if j.ID != first.ID || !j.Coalesced || j.SubscriptionID == "" || tokens[j.SubscriptionID] {
			t.Fatalf("joined = %+v (first %s)", j, first.ID)
		}
		tokens[j.SubscriptionID] = true
	}
	if first.Coalesced || first.Subscribers != 1 {
		t.Errorf("first = coalesced %v, subscribers %d", first.Coalesced, first.Subscribers)
	}
	if got, _ := svc.Get(context.Background(), first.ID); got.Subscribers != 4 {
		t.Errorf("subscribers = %d, want 4", got.Subscribers)
	}
	if st := svc.Stats(); st.CoalescedRequests != 3 || st.Running != 1 {
		t.Errorf("stats = %+v", st)
	}

	close(g.release)
	sc := waitStatus(t, svc, first.ID, scan.StatusCompleted)
	if g.runs.Load() != 1 {
		t.Errorf("engine ran %d times, want once", g.runs.Load())
	}
	// Every subscriber reads the same scan and result.
	if _, err := svc.Result(context.Background(), sc.ID); err != nil {
		t.Error(err)
	}
	// A finished scan is not joined: a new request starts a new scan.
	again := mustCreate(t, svc, "example.com")
	if again.ID == first.ID || again.Coalesced {
		t.Errorf("request after completion joined the finished scan: %+v", again)
	}
}

func TestDifferentStartURLsDoNotCoalesce(t *testing.T) {
	g := newGate()
	defer close(g.release)
	svc := startService(t, []scan.Stage{stage("crawl", g)}, scan.Options{MaxRunning: 5})
	ids := map[string]bool{}
	for _, in := range []string{"example.com", "www.example.com", "http://example.com", "https://example.com/docs"} {
		sc := mustCreate(t, svc, in)
		if sc.Coalesced || ids[sc.ID] {
			t.Errorf("%s coalesced: %+v", in, sc)
		}
		ids[sc.ID] = true
	}
}

func TestSubscriberCancelDetachesOnlyThatSubscriber(t *testing.T) {
	g := newGate()
	svc := startService(t, []scan.Stage{stage("crawl", g)}, scan.Options{MaxRunning: 1})
	a := mustCreate(t, svc, "example.com")
	b := mustCreate(t, svc, "example.com")
	waitFor(t, func() bool { return g.didStart("example.com") })

	// Without a subscription, a shared scan cannot be cancelled.
	if _, err := svc.Cancel(context.Background(), a.ID, ""); !errors.Is(err, scan.ErrShared) {
		t.Fatalf("cancel without subscription: %v", err)
	}
	// B leaves: the scan continues for A.
	got, err := svc.Cancel(context.Background(), a.ID, b.SubscriptionID)
	if err != nil || !got.Detached || got.Subscribers != 1 || got.Status != scan.StatusRunning {
		t.Fatalf("B leaving = %+v, %v", got, err)
	}
	if _, err := svc.Cancel(context.Background(), a.ID, b.SubscriptionID); !errors.Is(err, scan.ErrNotSubscribed) {
		t.Errorf("B leaving twice: %v", err)
	}
	if got, _ := svc.Get(context.Background(), a.ID); got.Status != scan.StatusRunning {
		t.Fatalf("scan stopped when one of two subscribers left: %s", got.Status)
	}
	// A, the last subscriber, leaves: the scan is cancelled.
	if got, err := svc.Cancel(context.Background(), a.ID, a.SubscriptionID); err != nil || got.Detached {
		t.Fatalf("A leaving = %+v, %v", got, err)
	}
	sc := waitStatus(t, svc, a.ID, scan.StatusCancelled)
	if sc.StopReason != scan.StopCancel {
		t.Errorf("stop reason = %q", sc.StopReason)
	}
}

func TestLastSubscriberLeavingQueuedScanRemovesIt(t *testing.T) {
	g := newGate()
	defer close(g.release)
	svc := startService(t, []scan.Stage{stage("crawl", g)}, scan.Options{MaxRunning: 1, QueueSize: 1})
	mustCreate(t, svc, "busy.com") // occupies the only slot
	q1 := mustCreate(t, svc, "example.com")
	// Joining is allowed even though the queue is full: it adds no job.
	q2 := mustCreate(t, svc, "example.com")
	if q1.Status != scan.StatusQueued || !q2.Coalesced || q2.QueuePosition != 1 {
		t.Fatalf("q1 = %+v, q2 = %+v", q1, q2)
	}
	svc.Cancel(context.Background(), q1.ID, q1.SubscriptionID)
	if got, _ := svc.Get(context.Background(), q1.ID); got.Status != scan.StatusQueued || got.Subscribers != 1 {
		t.Fatalf("after one subscriber left: %+v", got)
	}
	got, err := svc.Cancel(context.Background(), q1.ID, q2.SubscriptionID)
	if err != nil || got.Status != scan.StatusCancelled || svc.Stats().Queued != 0 {
		t.Fatalf("after last subscriber left: %+v, %v, queued %d", got, err, svc.Stats().Queued)
	}
	if g.didStart("example.com") {
		t.Error("abandoned queued scan ran")
	}
}

func TestAllSubscribersObserveFailure(t *testing.T) {
	block := newGate()
	failing := &failAfterGate{gate: block}
	svc := startService(t, []scan.Stage{stage("crawl", failing)}, scan.Options{})
	a := mustCreate(t, svc, "example.com")
	b := mustCreate(t, svc, "example.com")
	close(block.release)
	for _, id := range []string{a.ID, b.ID} {
		if sc := waitStatus(t, svc, id, scan.StatusFailed); sc.Error == "" {
			t.Errorf("scan %s = %+v", id, sc)
		}
	}
}

type failAfterGate struct{ gate *gateEngine }

func (f *failAfterGate) Name() string { return "html" }
func (f *failAfterGate) Discover(ctx context.Context, in discovery.Input, emit discovery.Emit) error {
	<-f.gate.release
	return errors.New("site unreachable")
}
