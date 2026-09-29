// Package dnsresolve is a discovery engine that resolves discovered hosts,
// separating names that merely appeared somewhere (e.g. on an old
// certificate) from names that currently resolve.
package dnsresolve

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sort"
	"strings"
	"time"

	"websitemapper/internal/cache"
	"websitemapper/internal/discovery"
	"websitemapper/internal/fetch"
	"websitemapper/internal/resource"
)

// Cache holds DNS answers by hostname, shared by all scans.
//
// Go's resolver does not report record TTLs, so answers are kept for a
// bounded, configured time instead. Caching only affects what a scan
// reports: every HTTP connection still resolves the name again and checks
// the address it connects to, so the cache cannot be used to bypass the
// non-public address guard.
type Cache = cache.Cache[discovery.DNSInfo]

// NewCache creates a DNS cache.
func NewCache(maxBytes int64) *Cache {
	return cache.New[discovery.DNSInfo](cache.Options{Name: "dns", MaxCost: maxBytes})
}

// errStopped means a lookup was interrupted because the scan stopped.
var errStopped = errors.New("lookup interrupted")

// Resolver looks up addresses. *net.Resolver satisfies it.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
	LookupCNAME(ctx context.Context, host string) (string, error)
}

// Options bounds resolution.
type Options struct {
	// MaxHosts is the maximum number of hosts resolved per scan.
	MaxHosts int
	// Concurrency is the number of this scan's lookups in flight.
	Concurrency int
	// Timeout bounds a single host's lookups.
	Timeout time.Duration
	// Pool bounds lookups in flight across all scans (nil: unbounded).
	Pool *resource.Pool
	// Cache, when set, reuses answers across scans. A fresh hit takes no
	// Pool slot and makes no lookup.
	Cache *Cache
	// TTL is how long an answer is reused.
	TTL time.Duration
	// NegativeTTL is how long "no such host" / "no records" is reused.
	// Other failures (timeouts, server errors) are never cached.
	NegativeTTL time.Duration
}

// Engine resolves hosts that have not been resolved yet.
type Engine struct {
	resolver Resolver
	opts     Options
}

// New creates an Engine.
func New(r Resolver, opts Options) *Engine {
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Second
	}
	return &Engine{resolver: r, opts: opts}
}

func (e *Engine) Name() string { return "dns" }

// Discover resolves every host without DNS information, up to MaxHosts per
// scan. Hosts over the limit are reported as skipped.
func (e *Engine) Discover(ctx context.Context, in discovery.Input, emit discovery.Emit) error {
	hosts := in.State.Hosts()
	discovery.PrioritizeHosts(hosts, in.Target)

	done := 0
	var todo []string
	for _, h := range hosts {
		if h.DNS != nil {
			done++
		} else {
			todo = append(todo, h.Name)
		}
	}
	budget := max(e.opts.MaxHosts-done, 0)
	if len(todo) > budget {
		for _, h := range todo[budget:] {
			emit(discovery.Finding{Host: h, DNS: &discovery.DNSInfo{Skipped: discovery.SkipHostLimit}})
		}
		todo = todo[:budget]
	}

	discovery.ForEachLimited(todo, e.opts.Concurrency, func(h string) {
		if ctx.Err() != nil {
			return
		}
		if info := e.resolve(ctx, h, in.Target.Domain); info != nil {
			emit(discovery.Finding{Host: h, DNS: info})
		}
	})
	return ctx.Err()
}

// resolve returns nil if ctx ended first, leaving the host unresolved.
func (e *Engine) resolve(ctx context.Context, host, domain string) *discovery.DNSInfo {
	info, ci, err := e.opts.Cache.Do(ctx, host, func(ctx context.Context) (cache.Loaded[discovery.DNSInfo], error) {
		v, cacheable := e.lookup(ctx, host)
		if v == nil {
			return cache.Loaded[discovery.DNSInfo]{}, errStopped
		}
		l := cache.Loaded[discovery.DNSInfo]{Value: *v, Group: domain,
			Cost: 96 + cache.StringCost(host, v.CNAME, v.Error) + cache.StringCost(v.Addresses...)}
		switch {
		case v.Resolved:
			l.TTL = e.opts.TTL
		case cacheable:
			l.TTL = min(e.opts.NegativeTTL, e.opts.TTL)
		}
		return l, nil
	})
	if err != nil {
		return nil
	}
	if ci.Hit || ci.Shared {
		at := ci.CreatedAt
		info.CachedAt = &at
	}
	return &info
}

// lookup resolves host, taking a shared pool slot. It returns nil if ctx
// ended first; cacheable is false for transient failures.
func (e *Engine) lookup(ctx context.Context, host string) (info *discovery.DNSInfo, cacheable bool) {
	// Waiting for a shared slot does not count against the lookup timeout.
	release, _, err := e.opts.Pool.Acquire(ctx)
	if err != nil {
		return nil, false
	}
	defer release()

	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, e.opts.Timeout)
	defer cancel()

	addrs, err := e.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		if parent.Err() != nil {
			return nil, false // the scan stopped; this is not a DNS answer
		}
		var dnsErr *net.DNSError
		notFound := errors.As(err, &dnsErr) && dnsErr.IsNotFound
		return &discovery.DNSInfo{Resolved: false, Error: describe(err)}, notFound
	}
	if len(addrs) == 0 {
		return &discovery.DNSInfo{Resolved: false, Error: "no A or AAAA records"}, true
	}

	info = &discovery.DNSInfo{Resolved: true, NonPublic: true}
	seen := map[string]bool{}
	for _, a := range addrs {
		ip, ok := netip.AddrFromSlice(a.IP)
		if !ok {
			continue
		}
		ip = ip.Unmap()
		if s := ip.String(); !seen[s] {
			seen[s] = true
			info.Addresses = append(info.Addresses, s)
		}
		if fetch.IsPublicAddr(ip) {
			info.NonPublic = false
		}
	}
	sort.Strings(info.Addresses)

	// The CNAME is informative only; failures are ignored.
	if cname, err := e.resolver.LookupCNAME(ctx, host); err == nil {
		cname = strings.TrimSuffix(strings.ToLower(cname), ".")
		if cname != "" && cname != host {
			info.CNAME = cname
		}
	}
	return info, true
}

func describe(err error) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		switch {
		case dnsErr.IsNotFound:
			return "no such host"
		case dnsErr.IsTimeout:
			return "lookup timed out"
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "lookup timed out"
	}
	return err.Error()
}
