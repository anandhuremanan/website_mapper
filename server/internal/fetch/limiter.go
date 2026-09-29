package fetch

import (
	"context"
	"sync"
	"time"
)

// hostLimiter spaces requests to the same host at a fixed minimum interval.
// It is shared across scans, so politeness towards a host holds even when
// several scans target it at once.
type hostLimiter struct {
	interval time.Duration

	mu   sync.Mutex
	next map[string]time.Time
}

func newHostLimiter(rps float64) *hostLimiter {
	l := &hostLimiter{next: make(map[string]time.Time)}
	if rps > 0 {
		l.interval = time.Duration(float64(time.Second) / rps)
	}
	return l
}

func (l *hostLimiter) wait(ctx context.Context, host string) error {
	if l.interval <= 0 {
		return ctx.Err()
	}
	l.mu.Lock()
	now := time.Now()
	slot := l.next[host]
	if slot.Before(now) {
		slot = now
	}
	l.next[host] = slot.Add(l.interval)
	if len(l.next) > 1024 {
		l.prune(now)
	}
	l.mu.Unlock()

	delay := slot.Sub(now)
	if delay <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// prune drops hosts whose next slot has already passed. Caller holds mu.
func (l *hostLimiter) prune(now time.Time) {
	for h, t := range l.next {
		if t.Before(now) {
			delete(l.next, h)
		}
	}
}
