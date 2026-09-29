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
	"sync"

	"websitemapper/internal/discovery"
	"websitemapper/internal/normalize"
)

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
	log     *slog.Logger
}

// New creates an Engine.
func New(sources []Source, log *slog.Logger) *Engine {
	return &Engine{sources: sources, log: log}
}

func (e *Engine) Name() string { return "subdomains" }

// Discover queries every source concurrently and reports the union of
// their hosts. Providers are unreliable, so the engine fails only when every
// source fails; individual failures are logged.
func (e *Engine) Discover(ctx context.Context, in discovery.Input, emit discovery.Emit) error {
	var (
		mu   sync.Mutex
		errs []error
	)
	var wg sync.WaitGroup
	for _, src := range e.sources {
		wg.Add(1)
		go func(src Source) {
			defer wg.Done()
			names, err := src.Discover(ctx, in.Target.Domain)
			if err != nil {
				e.log.Warn("passive source failed", "source", src.Name(), "error", err)
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s: %w", src.Name(), err))
				mu.Unlock()
				return
			}
			hosts := Filter(names, in.Target)
			e.log.Info("passive source finished", "source", src.Name(), "names", len(names), "hosts", len(hosts))
			for _, h := range hosts {
				emit(discovery.Finding{Host: h, Source: src.Provenance()})
			}
		}(src)
	}
	wg.Wait()
	if len(errs) == len(e.sources) {
		return errors.Join(errs...)
	}
	return nil
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
