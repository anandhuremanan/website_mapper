package resource

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestPoolGlobalLimitAcrossAccounts runs many "scans", each with far more
// workers than the pool allows, and checks the total never exceeds capacity.
func TestPoolGlobalLimitAcrossAccounts(t *testing.T) {
	const capacity, scans, workersPerScan, opsPerWorker = 5, 6, 8, 20
	p := NewPool("http", capacity)
	var inFlight, peak atomic.Int64

	var wg sync.WaitGroup
	for s := 0; s < scans; s++ {
		ctx := WithAccount(context.Background(), NewAccount("scan", 0))
		for w := 0; w < workersPerScan; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < opsPerWorker; i++ {
					release, _, err := p.Acquire(ctx)
					if err != nil {
						t.Error(err)
						return
					}
					n := inFlight.Add(1)
					for {
						old := peak.Load()
						if n <= old || peak.CompareAndSwap(old, n) {
							break
						}
					}
					time.Sleep(50 * time.Microsecond)
					inFlight.Add(-1)
					release()
				}
			}()
		}
	}
	wg.Wait()

	if got := peak.Load(); got > capacity {
		t.Fatalf("peak concurrency %d exceeds capacity %d", got, capacity)
	}
	st := p.Stats()
	if st.Peak > capacity || st.InUse != 0 || st.Waiting != 0 {
		t.Errorf("stats = %+v", st)
	}
	if st.Acquired != scans*workersPerScan*opsPerWorker {
		t.Errorf("acquired = %d", st.Acquired)
	}
}

// TestPoolFairHandoff: a large scan holds every slot and has more work
// queued; a small scan that starts waiting gets the next released slots.
func TestPoolFairHandoff(t *testing.T) {
	p := NewPool("http", 4)
	big := WithAccount(context.Background(), NewAccount("big", 0))
	small := WithAccount(context.Background(), NewAccount("small", 0))

	var bigReleases []func()
	for i := 0; i < 4; i++ {
		r, _, _ := p.Acquire(big)
		bigReleases = append(bigReleases, r)
	}
	// The big scan queues lots more work before the small scan arrives.
	bigGot := make(chan func(), 10)
	for i := 0; i < 10; i++ {
		go func() {
			r, _, err := p.Acquire(big)
			if err == nil {
				bigGot <- r
			}
		}()
	}
	waitFor(t, func() bool { return p.Stats().Waiting == 10 })

	smallGot := make(chan func(), 2)
	for i := 0; i < 2; i++ {
		go func() {
			r, _, _ := p.Acquire(small)
			smallGot <- r
		}()
	}
	waitFor(t, func() bool { return p.Stats().Waiting == 12 })

	// The next two slots freed go to the small scan despite arriving last.
	bigReleases[0]()
	bigReleases[1]()
	for i := 0; i < 2; i++ {
		select {
		case <-smallGot:
		case <-bigGot:
			t.Fatal("slot went to the big scan while the small scan was waiting")
		case <-time.After(2 * time.Second):
			t.Fatal("small scan never got a slot")
		}
	}
	if h := p.Held(FromContext(small)); h != 2 {
		t.Errorf("small holds %d, want 2", h)
	}
	// With the small scan satisfied, freed slots go back to the big scan.
	bigReleases[2]()
	select {
	case <-bigGot:
	case <-time.After(2 * time.Second):
		t.Fatal("big scan did not get the freed slot")
	}
}

func TestPoolCancelWhileWaiting(t *testing.T) {
	p := NewPool("dns", 1)
	release, _, _ := p.Acquire(context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, _, err := p.Acquire(ctx)
		errc <- err
	}()
	waitFor(t, func() bool { return p.Stats().Waiting == 1 })
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if st := p.Stats(); st.Waiting != 0 || st.InUse != 1 {
		t.Errorf("after cancel: %+v", st)
	}
	release()
	if st := p.Stats(); st.InUse != 0 {
		t.Errorf("after release: %+v", st)
	}
	// Already-cancelled contexts never take a slot.
	if _, _, err := p.Acquire(ctx); err == nil {
		t.Error("acquire with cancelled ctx succeeded")
	}
}

func TestPoolRecordsWaitPerAccount(t *testing.T) {
	p := NewPool("http", 1)
	release, _, _ := p.Acquire(context.Background())
	acct := NewAccount("a", 0)
	done := make(chan time.Duration)
	go func() {
		r, waited, _ := p.Acquire(WithAccount(context.Background(), acct))
		r()
		done <- waited
	}()
	waitFor(t, func() bool { return p.Stats().Waiting == 1 })
	time.Sleep(20 * time.Millisecond)
	release()
	waited := <-done
	u := acct.Usage()["http"]
	if waited < 20*time.Millisecond || u.Waited != waited || u.Acquired != 1 || u.Delayed != 1 {
		t.Errorf("waited %v, usage %+v", waited, u)
	}
	// An immediate acquisition is counted but not as delayed.
	r, _, _ := p.Acquire(WithAccount(context.Background(), acct))
	r()
	if u := acct.Usage()["http"]; u.Acquired != 2 || u.Delayed != 1 {
		t.Errorf("usage after fast acquire = %+v", u)
	}
}

func TestAccountBudget(t *testing.T) {
	a := NewAccount("a", 2)
	if a.TakeRequest() != nil || a.TakeRequest() != nil {
		t.Fatal("budget refused too early")
	}
	if err := a.TakeRequest(); !errors.Is(err, ErrBudgetExhausted) || !a.Exhausted() || a.Requests() != 2 {
		t.Errorf("err = %v exhausted = %v requests = %d", err, a.Exhausted(), a.Requests())
	}
	var unlimited *Account
	if unlimited.TakeRequest() != nil || unlimited.Exhausted() {
		t.Error("nil account must be unlimited")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}
