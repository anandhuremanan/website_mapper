package resource

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ErrDownloadBudget is returned when a scan has downloaded its byte budget.
// It wraps ErrBudgetExhausted, so engines stop exactly as they do when the
// request budget runs out.
var ErrDownloadBudget = fmt.Errorf("%w: scan download limit reached", ErrBudgetExhausted)

// Bandwidth limits the bytes all scans download together, as a token
// bucket refilled at a fixed rate. Because the rate is a hard ceiling, it
// also bounds monthly transfer: rate x seconds in a month, even if the
// server scans continuously. A nil *Bandwidth is unlimited.
type Bandwidth struct {
	rate  float64 // bytes per second
	burst float64

	mu     sync.Mutex
	tokens float64
	last   time.Time
	total  int64
}

// NewBandwidth allows bytesPerSecond on average, with bursts of up to one
// second's worth. It returns nil (unlimited) for a non-positive rate.
func NewBandwidth(bytesPerSecond int64) *Bandwidth {
	if bytesPerSecond <= 0 {
		return nil
	}
	r := float64(bytesPerSecond)
	return &Bandwidth{rate: r, burst: r, tokens: r, last: time.Now()}
}

// Wait accounts for n bytes that were just read and sleeps as long as
// needed to keep the average rate, or until ctx is done. Readers pace
// themselves by waiting before they read more, so the sender is slowed by
// TCP flow control rather than the data being dropped.
func (b *Bandwidth) Wait(ctx context.Context, n int) error {
	if b == nil || n <= 0 {
		return ctx.Err()
	}
	b.mu.Lock()
	now := time.Now()
	b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	b.last = now
	b.tokens -= float64(n)
	b.total += int64(n)
	var delay time.Duration
	if b.tokens < 0 {
		delay = time.Duration(-b.tokens / b.rate * float64(time.Second))
	}
	b.mu.Unlock()
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

// BandwidthStats is a snapshot of a Bandwidth limiter.
type BandwidthStats struct {
	BytesPerSecond int64 `json:"bytesPerSecond"`
	// TotalBytes is everything accounted since the server started.
	TotalBytes int64 `json:"totalBytes"`
}

// Stats returns a snapshot.
func (b *Bandwidth) Stats() BandwidthStats {
	if b == nil {
		return BandwidthStats{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return BandwidthStats{BytesPerSecond: int64(b.rate), TotalBytes: b.total}
}
