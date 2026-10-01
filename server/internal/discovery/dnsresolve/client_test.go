package dnsresolve

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// dnsServer runs a DNS server on a local port (UDP and TCP) and returns
// its address. handle builds the answer for each question.
func dnsServer(t *testing.T, handle func(q dns.Question, viaTCP bool, m *dns.Msg)) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", pc.LocalAddr().String())
	if err != nil {
		t.Skipf("cannot listen on the same TCP port: %v", err)
	}
	handler := func(tcp bool) dns.Handler {
		return dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
			m := new(dns.Msg)
			m.SetReply(r)
			handle(r.Question[0], tcp, m)
			w.WriteMsg(m)
		})
	}
	udp := &dns.Server{PacketConn: pc, Handler: handler(false)}
	tcp := &dns.Server{Listener: ln, Handler: handler(true)}
	go udp.ActivateAndServe()
	go tcp.ActivateAndServe()
	t.Cleanup(func() { udp.Shutdown(); tcp.Shutdown() })
	return pc.LocalAddr().String()
}

func rr(t *testing.T, s string) dns.RR {
	t.Helper()
	r, err := dns.NewRR(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// zone answers a few names the way a recursive resolver would.
func zone(t *testing.T, questions *atomic.Int32) func(dns.Question, bool, *dns.Msg) {
	return func(q dns.Question, _ bool, m *dns.Msg) {
		questions.Add(1)
		switch {
		case q.Name == "example.com." && q.Qtype == dns.TypeA:
			m.Answer = []dns.RR{rr(t, "example.com. 60 IN A 93.184.216.34"), rr(t, "example.com. 60 IN A 93.184.216.35")}
		case q.Name == "app.example.com." && q.Qtype == dns.TypeA:
			// An alias chain, as resolvers return it: in order, then the address.
			m.Answer = []dns.RR{
				rr(t, "app.example.com. 60 IN CNAME edge.provider.net."),
				rr(t, "edge.provider.net. 60 IN CNAME node7.Provider.net."),
				rr(t, "node7.provider.net. 60 IN A 203.0.113.9"),
			}
		case q.Name == "v6.example.com." && q.Qtype == dns.TypeAAAA:
			m.Answer = []dns.RR{rr(t, "v6.example.com. 60 IN AAAA 2606:2800:220:1::1")}
		case q.Name == "v6.example.com.", q.Name == "mail.example.com.":
			// The name exists but has no record of this type.
		default:
			m.Rcode = dns.RcodeNameError
		}
	}
}

func addrs(ss ...string) []netip.Addr {
	var out []netip.Addr
	for _, s := range ss {
		out = append(out, netip.MustParseAddr(s))
	}
	return out
}

func TestClientLookup(t *testing.T) {
	var questions atomic.Int32
	c := &Client{Servers: []string{dnsServer(t, zone(t, &questions))}}
	ctx := context.Background()

	// One question for a host with an IPv4 address.
	got, cname, err := c.Lookup(ctx, "example.com")
	if err != nil || cname != "" || !reflect.DeepEqual(got, addrs("93.184.216.34", "93.184.216.35")) {
		t.Errorf("example.com = %v, %q, %v", got, cname, err)
	}
	if n := questions.Swap(0); n != 1 {
		t.Errorf("example.com took %d questions, want 1", n)
	}

	// The alias comes with the same answer: where the chain ends.
	got, cname, err = c.Lookup(ctx, "app.example.com")
	if err != nil || cname != "node7.Provider.net" || !reflect.DeepEqual(got, addrs("203.0.113.9")) {
		t.Errorf("app.example.com = %v, %q, %v", got, cname, err)
	}
	if n := questions.Swap(0); n != 1 {
		t.Errorf("app.example.com took %d questions, want 1", n)
	}

	// IPv6 only: a second question finds it.
	got, _, err = c.Lookup(ctx, "v6.example.com")
	if err != nil || !reflect.DeepEqual(got, addrs("2606:2800:220:1::1")) {
		t.Errorf("v6.example.com = %v, %v", got, err)
	}
	questions.Store(0)

	// A name with no address records at all: no addresses, no error.
	if got, _, err := c.Lookup(ctx, "mail.example.com"); err != nil || len(got) != 0 {
		t.Errorf("mail.example.com = %v, %v", got, err)
	}

	// An unknown name is "not found", after a single question.
	questions.Store(0)
	_, _, err = c.Lookup(ctx, "gone.example.com")
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) || !dnsErr.IsNotFound {
		t.Errorf("gone.example.com err = %v", err)
	}
	if n := questions.Load(); n != 1 {
		t.Errorf("an unknown name took %d questions, want 1", n)
	}
}

func TestClientTriesTheNextServer(t *testing.T) {
	broken := dnsServer(t, func(_ dns.Question, _ bool, m *dns.Msg) { m.Rcode = dns.RcodeServerFailure })
	var questions atomic.Int32
	good := dnsServer(t, zone(t, &questions))
	c := &Client{Servers: []string{broken, good}}
	for i := 0; i < 4; i++ { // whichever server a lookup starts with
		if got, _, err := c.Lookup(context.Background(), "example.com"); err != nil || len(got) != 2 {
			t.Fatalf("lookup %d = %v, %v", i, got, err)
		}
	}

	// When every server fails, the lookup fails, but not as "not found":
	// the name was not resolved either way.
	c = &Client{Servers: []string{broken}}
	_, _, err := c.Lookup(context.Background(), "example.com")
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) || dnsErr.IsNotFound {
		t.Errorf("err = %v", err)
	}
}

func TestClientRetriesTruncatedAnswersOverTCP(t *testing.T) {
	server := dnsServer(t, func(q dns.Question, viaTCP bool, m *dns.Msg) {
		if !viaTCP {
			m.Truncated = true
			return
		}
		m.Answer = []dns.RR{rr(t, q.Name+" 60 IN A 93.184.216.34")}
	})
	c := &Client{Servers: []string{server}}
	if got, _, err := c.Lookup(context.Background(), "big.example.com"); err != nil || !reflect.DeepEqual(got, addrs("93.184.216.34")) {
		t.Errorf("lookup = %v, %v", got, err)
	}
}

// systemStub stands in for the operating system's resolver.
type systemStub struct{ calls atomic.Int32 }

func (s *systemStub) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	s.calls.Add(1)
	return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
}
func (s *systemStub) LookupCNAME(context.Context, string) (string, error) { return "", nil }

// TestClientFallsBackWhenServersAreUnreachable: on a network that blocks
// outside DNS, lookups go to the system resolver after a few failures, and
// the servers are tried again later.
func TestClientFallsBackWhenServersAreUnreachable(t *testing.T) {
	// A port nothing listens on.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := pc.LocalAddr().String()
	pc.Close()

	system := &systemStub{}
	var now atomic.Int64
	c := &Client{Servers: []string{dead}, Timeout: 100 * time.Millisecond, Fallback: system,
		Now: func() time.Time { return time.Unix(0, now.Load()) }}
	ctx := context.Background()

	for i := 1; i < unreachableLimit; i++ {
		if _, _, err := c.Lookup(ctx, "example.com"); err == nil {
			t.Fatalf("lookup %d succeeded without a reachable server", i)
		}
	}
	if system.calls.Load() != 0 {
		t.Fatal("the fallback was used too early")
	}
	// The next failure switches over, and answers this lookup too.
	if got, _, err := c.Lookup(ctx, "example.com"); err != nil || len(got) != 1 || system.calls.Load() != 1 {
		t.Fatalf("switching lookup = %v, %v (fallback calls %d)", got, err, system.calls.Load())
	}
	if _, _, err := c.Lookup(ctx, "other.example.com"); err != nil || system.calls.Load() != 2 {
		t.Errorf("during the fallback period: err %v, fallback calls %d", err, system.calls.Load())
	}

	// After the period the servers are tried again.
	now.Add(int64(fallbackPeriod + time.Second))
	if _, _, err := c.Lookup(ctx, "example.com"); err == nil || system.calls.Load() != 2 {
		t.Errorf("after the fallback period: err %v, fallback calls %d", err, system.calls.Load())
	}
}

// Through the engine, a Client's answers become the same host details as
// the system resolver's.
func TestEngineWithClient(t *testing.T) {
	var questions atomic.Int32
	c := &Client{Servers: []string{dnsServer(t, zone(t, &questions))}}
	e := NewWith(c, Options{MaxHosts: 10, Concurrency: 4})
	app, cacheable := e.lookup(context.Background(), "app.example.com")
	if !cacheable || !app.Resolved || app.CNAME != "node7.provider.net" || !reflect.DeepEqual(app.Addresses, []string{"203.0.113.9"}) {
		t.Errorf("app = %+v", app)
	}
	gone, cacheable := e.lookup(context.Background(), "gone.example.com")
	if !cacheable || gone.Resolved || gone.Error != "no such host" {
		t.Errorf("gone = %+v (cacheable %v)", gone, cacheable)
	}
	mail, _ := e.lookup(context.Background(), "mail.example.com")
	if mail.Resolved || mail.Error != "no A or AAAA records" {
		t.Errorf("mail = %+v", mail)
	}
}
