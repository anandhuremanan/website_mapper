// Package cache is a bounded, in-process cache for reusable discovery data
// (certificate lookups, DNS answers, host probes, fetched-page metadata),
// shared by all scans.
//
// Every cache is bounded by an estimated memory budget: entries carry a cost
// (approximate bytes) and the least recently used entries are evicted when
// the budget is exceeded. Entries also expire after a TTL, a single entry
// larger than MaxEntryCost is never stored, and each group (the scanned
// domain) may use at most GroupQuota, so one enormous domain cannot push
// every other domain out.
//
// Do coalesces concurrent misses for the same key: one caller loads, the
// others wait for its answer instead of repeating the network work.
package cache

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// entryOverhead is the cache's own memory per entry (map slot, entry
// struct with timestamps, and two list elements), measured on amd64. It is
// added to every entry's cost so small entries are not undercounted.
const entryOverhead = 320

// Options configures a Cache.
type Options struct {
	// Name identifies the cache in stats, e.g. "dns".
	Name string
	// MaxCost is the budget in estimated bytes.
	MaxCost int64
	// MaxEntryCost bounds a single entry; larger values are not stored.
	// Zero means MaxCost/4.
	MaxEntryCost int64
	// GroupQuota bounds the total cost of one group. Zero means MaxCost/4.
	GroupQuota int64
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Loaded is what a load function returns.
type Loaded[V any] struct {
	Value V
	// Cost is the entry's estimated size in bytes.
	Cost int64
	// TTL is how long the value stays fresh. Zero shares the value with
	// callers waiting for this load but does not store it.
	TTL time.Duration
	// Group is the quota bucket, normally the scanned domain.
	Group string
}

// Info describes where a value returned by Do came from.
type Info struct {
	// Hit is true when a stored, fresh value was returned.
	Hit bool
	// Shared is true when the value came from another caller's load.
	Shared bool
	// CreatedAt is when the value was loaded.
	CreatedAt time.Time
}

// Stats are a cache's counters.
type Stats struct {
	Name      string `json:"name"`
	Hits      int64  `json:"hits"`
	Misses    int64  `json:"misses"`
	Coalesced int64  `json:"coalesced"`
	Evictions int64  `json:"evictions"`
	Expired   int64  `json:"expired"`
	Rejected  int64  `json:"rejected"`
	Entries   int    `json:"entries"`
	Cost      int64  `json:"costBytes"`
	MaxCost   int64  `json:"maxCostBytes"`
}

// Cache is a bounded TTL cache with single-flight loading. It is safe for
// concurrent use. A nil *Cache never stores anything and always loads.
type Cache[V any] struct {
	opts Options

	mu       sync.Mutex
	items    map[string]*entry[V]
	lru      *list.List // of *entry[V], front = most recently used
	groups   map[string]*group
	cost     int64
	inflight map[string]*call[V]
	stats    Stats
}

type entry[V any] struct {
	key, group string
	value      V
	cost       int64
	created    time.Time
	expires    time.Time
	elem       *list.Element // in lru
	gelem      *list.Element // in its group's list
}

type group struct {
	cost    int64
	entries *list.List // front = most recently used
}

type call[V any] struct {
	done    chan struct{}
	value   V
	created time.Time
	err     error
}

// New creates a Cache.
func New[V any](opts Options) *Cache[V] {
	if opts.MaxEntryCost <= 0 {
		opts.MaxEntryCost = opts.MaxCost / 4
	}
	if opts.GroupQuota <= 0 {
		opts.GroupQuota = opts.MaxCost / 4
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Cache[V]{
		opts:     opts,
		items:    make(map[string]*entry[V]),
		lru:      list.New(),
		groups:   make(map[string]*group),
		inflight: make(map[string]*call[V]),
		stats:    Stats{Name: opts.Name, MaxCost: opts.MaxCost},
	}
}

// Get returns a fresh stored value.
func (c *Cache[V]) Get(key string) (V, time.Time, bool) {
	var zero V
	if c == nil {
		return zero, time.Time{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.freshLocked(key); e != nil {
		c.stats.Hits++
		return e.value, e.created, true
	}
	return zero, time.Time{}, false
}

// Do returns the fresh value for key, or loads it. Concurrent callers that
// miss the same key share one load. An error from load is never stored:
// callers that were waiting for a load that failed with an error try again
// themselves, since such errors (cancellation, an exhausted request budget)
// belong to the caller that loaded, not to the key.
func (c *Cache[V]) Do(ctx context.Context, key string, load func(context.Context) (Loaded[V], error)) (V, Info, error) {
	if c == nil {
		l, err := load(ctx)
		return l.Value, Info{CreatedAt: time.Now()}, err
	}
	for {
		c.mu.Lock()
		if e := c.freshLocked(key); e != nil {
			c.stats.Hits++
			c.mu.Unlock()
			return e.value, Info{Hit: true, CreatedAt: e.created}, nil
		}
		if cl, ok := c.inflight[key]; ok {
			c.stats.Coalesced++
			c.mu.Unlock()
			select {
			case <-cl.done:
			case <-ctx.Done():
				var zero V
				return zero, Info{}, ctx.Err()
			}
			if cl.err == nil {
				return cl.value, Info{Shared: true, CreatedAt: cl.created}, nil
			}
			if err := ctx.Err(); err != nil {
				var zero V
				return zero, Info{}, err
			}
			continue // the loader's own failure: load for ourselves
		}
		c.stats.Misses++
		cl := &call[V]{done: make(chan struct{})}
		c.inflight[key] = cl
		c.mu.Unlock()

		l, err := load(ctx)
		now := c.opts.Now()
		c.mu.Lock()
		delete(c.inflight, key)
		if err == nil && l.TTL > 0 {
			c.storeLocked(key, l, now)
		}
		c.mu.Unlock()
		cl.value, cl.created, cl.err = l.Value, now, err
		close(cl.done)
		return l.Value, Info{CreatedAt: now}, err
	}
}

// freshLocked returns the entry for key if it has not expired, removing it
// if it has. Caller holds mu.
func (c *Cache[V]) freshLocked(key string) *entry[V] {
	e, ok := c.items[key]
	if !ok {
		return nil
	}
	if !c.opts.Now().Before(e.expires) {
		c.removeLocked(e)
		c.stats.Expired++
		return nil
	}
	c.lru.MoveToFront(e.elem)
	c.groups[e.group].entries.MoveToFront(e.gelem)
	return e
}

func (c *Cache[V]) storeLocked(key string, l Loaded[V], now time.Time) {
	if old, ok := c.items[key]; ok {
		c.removeLocked(old)
	}
	cost := max(l.Cost, 0) + entryOverhead
	if cost > c.opts.MaxEntryCost || cost > c.opts.GroupQuota {
		c.stats.Rejected++
		return
	}
	g := c.groups[l.Group]
	if g == nil {
		g = &group{entries: list.New()}
		c.groups[l.Group] = g
	}
	e := &entry[V]{key: key, group: l.Group, value: l.Value, cost: cost, created: now, expires: now.Add(l.TTL)}
	e.elem = c.lru.PushFront(e)
	e.gelem = g.entries.PushFront(e)
	c.items[key] = e
	c.cost += cost
	g.cost += cost

	// Within its quota, a group evicts its own oldest entries first.
	for g.cost > c.opts.GroupQuota {
		c.removeLocked(g.entries.Back().Value.(*entry[V]))
		c.stats.Evictions++
	}
	for c.cost > c.opts.MaxCost {
		c.removeLocked(c.lru.Back().Value.(*entry[V]))
		c.stats.Evictions++
	}
}

func (c *Cache[V]) removeLocked(e *entry[V]) {
	c.lru.Remove(e.elem)
	g := c.groups[e.group]
	g.entries.Remove(e.gelem)
	g.cost -= e.cost
	if g.entries.Len() == 0 {
		delete(c.groups, e.group)
	}
	delete(c.items, e.key)
	c.cost -= e.cost
}

// Stats returns a snapshot of the counters.
func (c *Cache[V]) Stats() Stats {
	if c == nil {
		return Stats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stats
	s.Entries = len(c.items)
	s.Cost = c.cost
	return s
}

// StringCost estimates the memory used by strings, including headers.
func StringCost(ss ...string) int64 {
	var n int64
	for _, s := range ss {
		n += int64(len(s)) + 16
	}
	return n
}
