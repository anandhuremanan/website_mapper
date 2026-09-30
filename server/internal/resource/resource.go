// Package resource bounds the work all scans do together.
//
// A Pool is a counting semaphore shared by every scan (for example "at most
// 32 HTTP requests in flight on this server"). When a slot is released and
// several scans are waiting, it goes to the waiting scan that currently
// holds the fewest slots, so a huge scan cannot starve small ones: as soon
// as a small scan asks for capacity, freed slots flow to it until the scans
// hold roughly equal shares.
//
// An Account represents one scan. It carries the scan's request budget and
// records how long the scan waited for shared capacity. It travels in the
// scan's context, so the HTTP client and DNS resolver can charge the right
// scan without every engine passing it along.
package resource

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// ErrBudgetExhausted is returned when a scan has used its request budget.
var ErrBudgetExhausted = errors.New("scan request limit reached")

// Account is one scan's view of shared resources. It is safe for
// concurrent use. A nil *Account is valid and unlimited.
type Account struct {
	id          string
	maxRequests int64
	maxBytes    int64

	requests  atomic.Int64
	bytes     atomic.Int64
	exhausted atomic.Bool // request budget
	bytesOut  atomic.Bool // download budget

	mu    sync.Mutex
	usage map[string]*Usage // per pool name
}

// Usage is one account's use of one pool.
type Usage struct {
	// Acquired counts slots obtained; Delayed counts those that had to wait.
	Acquired int
	Delayed  int
	// Waited is the total wait. Parallel workers wait at the same time, so
	// it can exceed the scan's wall-clock duration.
	Waited time.Duration
}

// NewAccount creates an account allowing maxRequests requests (0: unlimited).
func NewAccount(id string, maxRequests int) *Account {
	return &Account{id: id, maxRequests: int64(maxRequests), usage: map[string]*Usage{}}
}

// WithDownloadBudget limits the bytes the scan may download (0: unlimited).
func (a *Account) WithDownloadBudget(maxBytes int64) *Account {
	a.maxBytes = maxBytes
	return a
}

// TakeBytes charges n downloaded bytes. It returns ErrDownloadBudget once
// the budget is used up; the bytes are still counted.
func (a *Account) TakeBytes(n int) error {
	if a == nil {
		return nil
	}
	if total := a.bytes.Add(int64(n)); a.maxBytes > 0 && total > a.maxBytes {
		a.bytesOut.Store(true)
		return ErrDownloadBudget
	}
	return nil
}

// Bytes returns the bytes downloaded so far.
func (a *Account) Bytes() int64 {
	if a == nil {
		return 0
	}
	return a.bytes.Load()
}

// MaxBytes returns the download budget (0: unlimited).
func (a *Account) MaxBytes() int64 {
	if a == nil {
		return 0
	}
	return a.maxBytes
}

// DownloadExhausted reports whether the download budget ran out.
func (a *Account) DownloadExhausted() bool {
	return a != nil && a.bytesOut.Load()
}

// RequestsExhausted reports whether the request budget ran out.
func (a *Account) RequestsExhausted() bool {
	return a != nil && a.exhausted.Load()
}

// ID identifies the scan.
func (a *Account) ID() string {
	if a == nil {
		return ""
	}
	return a.id
}

// TakeRequest charges one outbound request to the budget.
func (a *Account) TakeRequest() error {
	if a == nil {
		return nil
	}
	if a.bytesOut.Load() {
		return ErrDownloadBudget
	}
	if n := a.requests.Add(1); a.maxRequests > 0 && n > a.maxRequests {
		a.requests.Add(-1)
		a.exhausted.Store(true)
		return ErrBudgetExhausted
	}
	return nil
}

// Requests returns the requests charged so far.
func (a *Account) Requests() int {
	if a == nil {
		return 0
	}
	return int(a.requests.Load())
}

// MaxRequests returns the request budget (0: unlimited).
func (a *Account) MaxRequests() int {
	if a == nil {
		return 0
	}
	return int(a.maxRequests)
}

// Exhausted reports whether the scan ran out of its request or download
// budget, after which it makes no more requests.
func (a *Account) Exhausted() bool {
	return a != nil && (a.exhausted.Load() || a.bytesOut.Load())
}

// Usage returns the account's use of each pool, by pool name.
func (a *Account) Usage() map[string]Usage {
	out := map[string]Usage{}
	if a == nil {
		return out
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, v := range a.usage {
		out[k] = *v
	}
	return out
}

func (a *Account) recordAcquire(pool string, waited time.Duration) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	u := a.usage[pool]
	if u == nil {
		u = &Usage{}
		a.usage[pool] = u
	}
	u.Acquired++
	if waited > 0 {
		u.Delayed++
		u.Waited += waited
	}
}

type accountKey struct{}

// WithAccount returns a context whose resource use is charged to a.
func WithAccount(ctx context.Context, a *Account) context.Context {
	return context.WithValue(ctx, accountKey{}, a)
}

// FromContext returns the account in ctx, or nil.
func FromContext(ctx context.Context) *Account {
	a, _ := ctx.Value(accountKey{}).(*Account)
	return a
}

// Pool is a fair counting semaphore shared by all scans. A nil *Pool never
// blocks.
type Pool struct {
	name     string
	capacity int

	mu       sync.Mutex
	inUse    int
	held     map[*Account]int // nil key: work not tied to a scan
	waiters  []*waiter        // arrival order
	peak     int
	acquired int64
	waitSum  time.Duration
}

type waiter struct {
	acct  *Account
	ready chan struct{} // closed when the slot is handed over
}

// NewPool creates a pool with the given number of slots.
func NewPool(name string, capacity int) *Pool {
	return &Pool{name: name, capacity: max(capacity, 1), held: map[*Account]int{}}
}

// Name returns the pool's name.
func (p *Pool) Name() string { return p.name }

// Acquire waits for a slot, charging the account in ctx. It returns a
// release function that must be called exactly once, and how long it waited.
// If ctx is done first it returns ctx's error and holds no slot.
func (p *Pool) Acquire(ctx context.Context) (release func(), waited time.Duration, err error) {
	if p == nil {
		return func() {}, 0, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	acct := FromContext(ctx)

	p.mu.Lock()
	// Only take a free slot directly when nobody is queued, so arrivals
	// cannot overtake waiting scans.
	if p.inUse < p.capacity && len(p.waiters) == 0 {
		p.grantLocked(acct)
		p.inUse++
		p.peak = max(p.peak, p.inUse)
		p.mu.Unlock()
		acct.recordAcquire(p.name, 0)
		return p.releaser(acct), 0, nil
	}
	w := &waiter{acct: acct, ready: make(chan struct{})}
	p.waiters = append(p.waiters, w)
	p.mu.Unlock()

	start := time.Now()
	select {
	case <-w.ready:
		waited = time.Since(start)
		p.recordWait(waited)
		// select picks randomly when both cases are ready; never start work
		// for a scan that has already ended.
		if err := ctx.Err(); err != nil {
			p.releaser(acct)()
			return nil, waited, err
		}
		acct.recordAcquire(p.name, waited)
		return p.releaser(acct), waited, nil
	case <-ctx.Done():
		p.mu.Lock()
		if p.removeWaiterLocked(w) {
			p.mu.Unlock()
			return nil, 0, ctx.Err()
		}
		p.mu.Unlock()
		// The slot was handed over just as ctx ended: give it back.
		<-w.ready
		p.releaser(acct)()
		return nil, 0, ctx.Err()
	}
}

func (p *Pool) releaser(acct *Account) func() {
	var once sync.Once
	return func() { once.Do(func() { p.release(acct) }) }
}

func (p *Pool) grantLocked(acct *Account) {
	p.held[acct]++
	p.acquired++
}

func (p *Pool) release(acct *Account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.held[acct]--; p.held[acct] <= 0 {
		delete(p.held, acct)
	}
	if len(p.waiters) == 0 {
		p.inUse--
		return
	}
	// Hand the slot straight to the waiter whose scan holds the fewest
	// slots (earliest arrival breaks ties); inUse is unchanged.
	best := 0
	for i, w := range p.waiters {
		if p.held[w.acct] < p.held[p.waiters[best].acct] {
			best = i
		}
	}
	w := p.waiters[best]
	p.waiters = append(p.waiters[:best], p.waiters[best+1:]...)
	p.grantLocked(w.acct)
	close(w.ready)
}

func (p *Pool) removeWaiterLocked(w *waiter) bool {
	for i, x := range p.waiters {
		if x == w {
			p.waiters = append(p.waiters[:i], p.waiters[i+1:]...)
			return true
		}
	}
	return false
}

func (p *Pool) recordWait(d time.Duration) {
	p.mu.Lock()
	p.waitSum += d
	p.mu.Unlock()
}

// Held returns how many slots acct holds right now.
func (p *Pool) Held(acct *Account) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.held[acct]
}

// Stats is a point-in-time snapshot of a pool.
type Stats struct {
	Name     string `json:"name"`
	Capacity int    `json:"capacity"`
	InUse    int    `json:"inUse"`
	Waiting  int    `json:"waiting"`
	// Peak is the highest InUse ever observed; it never exceeds Capacity.
	Peak     int   `json:"peak"`
	Acquired int64 `json:"acquired"`
	// WaitedMs is the total time all acquirers spent waiting.
	WaitedMs int64 `json:"waitedMs"`
}

// Stats returns a snapshot of the pool.
func (p *Pool) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Stats{
		Name: p.name, Capacity: p.capacity, InUse: p.inUse, Waiting: len(p.waiters),
		Peak: p.peak, Acquired: p.acquired, WaitedMs: p.waitSum.Milliseconds(),
	}
}
