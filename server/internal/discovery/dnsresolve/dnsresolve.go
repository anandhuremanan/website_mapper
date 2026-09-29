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

	"websitemapper/internal/discovery"
	"websitemapper/internal/fetch"
)

// Resolver looks up addresses. *net.Resolver satisfies it.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
	LookupCNAME(ctx context.Context, host string) (string, error)
}

// Options bounds resolution.
type Options struct {
	// MaxHosts is the maximum number of hosts resolved per scan.
	MaxHosts int
	// Concurrency is the number of lookups in flight.
	Concurrency int
	// Timeout bounds a single host's lookups.
	Timeout time.Duration
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
			emit(discovery.Finding{Host: h, DNS: &discovery.DNSInfo{Skipped: "host limit reached"}})
		}
		todo = todo[:budget]
	}

	discovery.ForEachLimited(todo, e.opts.Concurrency, func(h string) {
		if ctx.Err() != nil {
			return
		}
		emit(discovery.Finding{Host: h, DNS: e.resolve(ctx, h)})
	})
	return ctx.Err()
}

func (e *Engine) resolve(ctx context.Context, host string) *discovery.DNSInfo {
	ctx, cancel := context.WithTimeout(ctx, e.opts.Timeout)
	defer cancel()

	addrs, err := e.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return &discovery.DNSInfo{Resolved: false, Error: describe(err)}
	}
	if len(addrs) == 0 {
		return &discovery.DNSInfo{Resolved: false, Error: "no A or AAAA records"}
	}

	info := &discovery.DNSInfo{Resolved: true, NonPublic: true}
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
	return info
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
