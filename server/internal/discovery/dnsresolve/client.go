package dnsresolve

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
)

// Lookuper resolves a host in one step: its addresses and, if the name is
// an alias, the name it finally points to. An unknown name is reported as a
// *net.DNSError with IsNotFound set; a name without address records as no
// addresses and no error.
type Lookuper interface {
	Lookup(ctx context.Context, host string) (addrs []netip.Addr, cname string, err error)
}

// Client resolves names by asking DNS servers directly.
//
// The operating system's resolver is built for a program that looks up a
// few names: through it, one host costs separate questions for IPv4, IPv6
// and the alias. A scan resolves thousands of names, so Client asks one
// question per host (A; the answer carries the alias chain) and a second
// (AAAA) only for names without an IPv4 address.
//
// Servers are tried in turn, starting with a different one for each
// lookup. If none can be reached several lookups in a row (a network that
// blocks outside DNS), Client hands lookups to Fallback for a while.
type Client struct {
	// Servers are "host:port" addresses of recursive resolvers.
	Servers []string
	// Timeout bounds one question to one server.
	Timeout time.Duration
	// Fallback resolves names while the servers cannot be reached; nil
	// means lookups fail instead.
	Fallback Resolver
	Log      *slog.Logger
	// Now is the clock; overridable for tests.
	Now func() time.Time

	next atomic.Uint32
	// unreachable counts lookups in a row that reached no server;
	// fallbackUntil is when the servers are tried again (Unix nanoseconds).
	unreachable   atomic.Int32
	fallbackUntil atomic.Int64
}

const (
	// unreachableLimit is how many lookups in a row may reach no server
	// before the fallback takes over.
	unreachableLimit = 5
	// fallbackPeriod is how long the fallback is used before the servers
	// are tried again.
	fallbackPeriod = 10 * time.Minute
)

var _ Lookuper = (*Client)(nil)

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) Lookup(ctx context.Context, host string) ([]netip.Addr, string, error) {
	if c.Fallback != nil && c.now().UnixNano() < c.fallbackUntil.Load() {
		return lookupWith(ctx, c.Fallback, host)
	}
	addrs, cname, err := c.lookup(ctx, host)
	var unreachable *unreachableError
	switch {
	case err == nil || !errors.As(err, &unreachable):
		c.unreachable.Store(0)
	case ctx.Err() != nil:
		// The scan stopped; this says nothing about the servers.
	case c.Fallback != nil && c.unreachable.Add(1) >= unreachableLimit:
		c.unreachable.Store(0)
		c.fallbackUntil.Store(c.now().Add(fallbackPeriod).UnixNano())
		if c.Log != nil {
			c.Log.Warn("DNS servers cannot be reached; using the system resolver for a while",
				"event", "dns_fallback", "servers", strings.Join(c.Servers, ","), "for", fallbackPeriod, "error", err)
		}
		return lookupWith(ctx, c.Fallback, host)
	}
	return addrs, cname, err
}

// unreachableError means no server answered: a network problem, not a fact
// about the name.
type unreachableError struct{ err error }

func (e *unreachableError) Error() string { return e.err.Error() }
func (e *unreachableError) Unwrap() error { return e.err }

func (c *Client) lookup(ctx context.Context, host string) ([]netip.Addr, string, error) {
	addrs, cname, err := c.query(ctx, host, dns.TypeA)
	if err != nil || len(addrs) > 0 {
		return addrs, cname, err
	}
	// No IPv4 address: the host may be IPv6 only.
	addrs6, cname6, err := c.query(ctx, host, dns.TypeAAAA)
	if err != nil {
		return nil, cname, err
	}
	if cname == "" {
		cname = cname6
	}
	return addrs6, cname, nil
}

// query asks one question, trying each server until one gives an answer
// that settles it.
func (c *Client) query(ctx context.Context, host string, qtype uint16) ([]netip.Addr, string, error) {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(host), qtype)
	m.RecursionDesired = true

	start := int(c.next.Add(1))
	var lastErr error
	reached := false
	for i := range c.Servers {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		server := c.Servers[(start+i)%len(c.Servers)]
		resp, err := c.exchange(ctx, m, server)
		if err != nil {
			lastErr = err
			continue
		}
		reached = true
		switch resp.Rcode {
		case dns.RcodeSuccess:
			addrs, cname := parseAnswer(resp, qtype)
			return addrs, cname, nil
		case dns.RcodeNameError:
			return nil, "", &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		default:
			// SERVFAIL, REFUSED...: this server could not answer; another may.
			lastErr = &net.DNSError{Err: "server failure (" + dns.RcodeToString[resp.Rcode] + ")", Name: host, IsTemporary: true}
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no DNS servers configured")
	}
	if !reached {
		return nil, "", &unreachableError{lastErr}
	}
	return nil, "", lastErr
}

// exchange sends a question over UDP and, if the answer did not fit, again
// over TCP.
func (c *Client) exchange(ctx context.Context, m *dns.Msg, server string) (*dns.Msg, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	udp := &dns.Client{Net: "udp", Timeout: timeout}
	resp, _, err := udp.ExchangeContext(ctx, m, server)
	if err != nil {
		return nil, err
	}
	if resp.Truncated {
		tcp := &dns.Client{Net: "tcp", Timeout: timeout}
		if full, _, err := tcp.ExchangeContext(ctx, m, server); err == nil {
			return full, nil
		}
	}
	return resp, nil
}

// parseAnswer returns the addresses of the asked type and the name the
// alias chain ends at, if there is a chain.
func parseAnswer(resp *dns.Msg, qtype uint16) (addrs []netip.Addr, cname string) {
	for _, rr := range resp.Answer {
		switch rec := rr.(type) {
		case *dns.CNAME:
			cname = rec.Target // answers list the chain in order
		case *dns.A:
			if a, ok := netip.AddrFromSlice(rec.A.To4()); ok && qtype == dns.TypeA {
				addrs = append(addrs, a)
			}
		case *dns.AAAA:
			if a, ok := netip.AddrFromSlice(rec.AAAA); ok && qtype == dns.TypeAAAA {
				addrs = append(addrs, a)
			}
		}
	}
	return addrs, strings.TrimSuffix(cname, ".")
}

// lookupWith resolves a host with a Resolver, which needs separate
// questions for the addresses and the alias.
func lookupWith(ctx context.Context, r Resolver, host string) ([]netip.Addr, string, error) {
	ips, err := r.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, "", err
	}
	var addrs []netip.Addr
	for _, ip := range ips {
		if a, ok := netip.AddrFromSlice(ip.IP); ok {
			addrs = append(addrs, a)
		}
	}
	// The alias is informative only; failures are ignored.
	cname, _ := r.LookupCNAME(ctx, host)
	return addrs, cname, nil
}
