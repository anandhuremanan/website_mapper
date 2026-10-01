// Package subdomains is a discovery engine that finds hostnames under the
// target domain through passive sources such as Certificate Transparency.
// It never guesses or brute-forces names.
package subdomains

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"websitemapper/internal/cache"
	"websitemapper/internal/discovery"
	"websitemapper/internal/normalize"
)

// ErrRateLimited marks a provider answer refusing more requests for now.
// Such answers are cached briefly so scans stop spending the allowance.
var ErrRateLimited = errors.New("rate limited")

// Answer is one provider's cached answer for one domain: the in-scope
// hostnames it listed, or why it failed.
type Answer struct {
	Hosts []string
	Err   string
}

// Cache holds provider answers, keyed by provider and domain.
type Cache = cache.Cache[Answer]

// NewCache creates a certificate-discovery cache.
func NewCache(maxBytes int64) *Cache {
	return cache.New[Answer](cache.Options{Name: "certificate", MaxCost: maxBytes})
}

// CacheOptions configures the engine's use of a Cache.
type CacheOptions struct {
	Cache *Cache
	// TTL is how long a provider's hostnames are reused.
	TTL time.Duration
	// RateLimitTTL is how long a rate-limited answer is reused, so the
	// provider is not asked again until its allowance may have recovered.
	RateLimitTTL time.Duration
	// FailureTTL is how long other failures (errors, timeouts) are reused.
	// Keep it short: they are often transient, but retrying a provider that
	// is down on every scan costs each scan up to the provider timeout.
	FailureTTL time.Duration
}

// Source is a passive provider of hostnames for a domain.
type Source interface {
	// Name identifies the provider in errors and logs, e.g. "crt.sh".
	Name() string
	// Provenance is recorded on every host the provider finds.
	Provenance() discovery.Source
	// Discover returns raw names; they are normalized and scope-filtered by
	// the engine, so providers may return wildcards or unrelated names.
	Discover(ctx context.Context, domain string) ([]string, error)
}

// Engine runs all sources concurrently and reports in-scope hostnames.
type Engine struct {
	sources []Source
	timeout time.Duration
	cache   CacheOptions
	log     *slog.Logger
}

// New creates an Engine.
// Each source gets at most timeout in total, including its retries, so a
// slow or hanging provider cannot hold up the scan (0: no per-source limit).
func New(sources []Source, timeout time.Duration, log *slog.Logger) *Engine {
	return &Engine{sources: sources, timeout: timeout, log: log}
}

// WithCache makes the engine reuse provider answers across scans. A fresh
// answer means the provider is not contacted at all.
func (e *Engine) WithCache(opts CacheOptions) *Engine {
	e.cache = opts
	return e
}

func (e *Engine) Name() string { return "subdomains" }

// Discover queries every source concurrently and reports the union of
// their hosts. Providers are unreliable and isolated from each other: hosts
// from sources that succeed are always kept. If some sources fail, the
// failures are returned as a discovery.PartialError (recorded, not a
// failure); only when every source fails does the engine fail.
//
// If ctx ends while providers are still being asked, Discover returns with
// what the others answered. The outstanding queries are not abandoned:
// they run on to their time limit so that their answers are cached, and a
// later scan gets them at once instead of waiting again.
func (e *Engine) Discover(ctx context.Context, in discovery.Input, emit discovery.Emit) error {
	type reply struct {
		src    Source
		answer Answer
		info   cache.Info
		err    error
	}
	replies := make(chan reply, len(e.sources))
	// The scan's values (its resource account) without its cancellation.
	detached := context.WithoutCancel(ctx)
	for _, src := range e.sources {
		go func() {
			answer, info, err := e.cache.Cache.Do(detached, src.Name()+"|"+in.Target.Domain, func(ctx context.Context) (cache.Loaded[Answer], error) {
				return e.ask(ctx, src, in.Target)
			})
			replies <- reply{src, answer, info, err}
		}()
	}

	var errs []error
	waiting := map[string]bool{}
	for _, src := range e.sources {
		waiting[src.Name()] = true
	}
collect:
	for len(waiting) > 0 {
		select {
		case <-ctx.Done():
			break collect
		case r := <-replies:
			delete(waiting, r.src.Name())
			if r.err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", r.src.Name(), r.err))
				continue
			}
			var cachedAt time.Time
			if r.info.Hit || r.info.Shared {
				cachedAt = r.info.CreatedAt
			}
			if r.answer.Err != "" {
				msg := r.answer.Err
				if r.info.Hit {
					msg += fmt.Sprintf(" (answer cached at %s)", r.info.CreatedAt.UTC().Format(time.RFC3339))
				}
				e.log.Warn("passive source failed", "source", r.src.Name(), "error", msg, "cached", r.info.Hit)
				errs = append(errs, fmt.Errorf("%s: %s", r.src.Name(), msg))
				continue
			}
			e.log.Info("passive source finished", "source", r.src.Name(), "hosts", len(r.answer.Hosts), "cached", !cachedAt.IsZero())
			for _, h := range r.answer.Hosts {
				emit(discovery.Finding{Host: h, Source: r.src.Provenance(), CachedAt: cachedAt})
			}
		}
	}
	// In source order, so the message is the same every time.
	for _, src := range e.sources {
		if waiting[src.Name()] {
			errs = append(errs, fmt.Errorf("%s: had not answered when the scan moved on; its answer will be used by later scans", src.Name()))
		}
	}
	switch {
	case len(errs) == 0:
		return nil
	case len(errs) == len(e.sources):
		return errors.Join(errs...)
	}
	return discovery.Partial(errors.Join(errs...))
}

// ask queries one provider. A provider failure is an answer: it is shared
// with concurrent scans and cached briefly (RateLimitTTL when rate limited,
// FailureTTL otherwise). Only the scan's own cancellation is an error.
func (e *Engine) ask(ctx context.Context, src Source, t discovery.Target) (cache.Loaded[Answer], error) {
	sctx := ctx
	if e.timeout > 0 {
		var cancel context.CancelFunc
		sctx, cancel = context.WithTimeout(ctx, e.timeout)
		defer cancel()
	}
	names, err := src.Discover(sctx, t.Domain)
	if ctx.Err() != nil {
		return cache.Loaded[Answer]{}, ctx.Err()
	}
	l := cache.Loaded[Answer]{Group: t.Domain}
	switch {
	case err == nil:
		l.Value.Hosts = Filter(names, t)
		l.TTL = e.cache.TTL
	case sctx.Err() != nil:
		l.Value.Err = fmt.Sprintf("no answer within %s", e.timeout)
		l.TTL = min(e.cache.FailureTTL, e.cache.TTL)
	case errors.Is(err, ErrRateLimited):
		l.Value.Err = err.Error()
		l.TTL = min(e.cache.RateLimitTTL, e.cache.TTL)
	default:
		l.Value.Err = err.Error()
		l.TTL = min(e.cache.FailureTTL, e.cache.TTL)
	}
	l.Cost = 64 + cache.StringCost(l.Value.Err) + cache.StringCost(l.Value.Hosts...)
	return l, nil
}

// Filter normalizes raw names (lowercase, no trailing dot, no "*." prefix),
// drops invalid names and names outside the target's scope, and returns
// the remaining hostnames sorted and deduplicated.
func Filter(names []string, t discovery.Target) []string {
	seen := make(map[string]bool)
	var out []string
	for _, n := range names {
		h, err := normalize.Host(n)
		if err != nil || !t.InScope(h) || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}
