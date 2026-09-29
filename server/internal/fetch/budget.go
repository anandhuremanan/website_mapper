package fetch

import (
	"context"
	"errors"
	"sync/atomic"
)

// ErrRequestLimit is returned when a scan has used its request budget.
var ErrRequestLimit = errors.New("scan request limit reached")

// Budget bounds the requests one scan may make: a total count and an
// overall rate across all hosts. It is carried in the scan's context, so
// the shared Client enforces it for every engine without extra plumbing.
type Budget struct {
	max     int64
	used    atomic.Int64
	limiter *hostLimiter
}

// NewBudget allows at most maxRequests requests (0: unlimited) at no more
// than rps requests per second across all hosts (0: unlimited).
func NewBudget(maxRequests int, rps float64) *Budget {
	return &Budget{max: int64(maxRequests), limiter: newHostLimiter(rps)}
}

// Used returns the number of requests made so far.
func (b *Budget) Used() int { return int(b.used.Load()) }

func (b *Budget) take(ctx context.Context) error {
	if n := b.used.Add(1); b.max > 0 && n > b.max {
		b.used.Add(-1)
		return ErrRequestLimit
	}
	return b.limiter.wait(ctx, "") // one shared slot sequence for the scan
}

type budgetKey struct{}

// WithBudget returns a context whose requests draw from b.
func WithBudget(ctx context.Context, b *Budget) context.Context {
	return context.WithValue(ctx, budgetKey{}, b)
}

func budgetFrom(ctx context.Context) *Budget {
	b, _ := ctx.Value(budgetKey{}).(*Budget)
	return b
}
