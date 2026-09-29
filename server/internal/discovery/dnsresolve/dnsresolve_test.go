package dnsresolve

import (
	"context"
	"net"
	"reflect"
	"sync"
	"testing"

	"websitemapper/internal/discovery"
)

type fakeResolver struct {
	mu     sync.Mutex
	addrs  map[string][]string
	cnames map[string]string
	calls  []string
}

func (r *fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	r.mu.Lock()
	r.calls = append(r.calls, host)
	r.mu.Unlock()
	ips, ok := r.addrs[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	var out []net.IPAddr
	for _, s := range ips {
		out = append(out, net.IPAddr{IP: net.ParseIP(s)})
	}
	return out, nil
}

func (r *fakeResolver) LookupCNAME(_ context.Context, host string) (string, error) {
	if c, ok := r.cnames[host]; ok {
		return c, nil
	}
	return host + ".", nil
}

type state struct{ hosts []discovery.HostView }

func (s state) Hosts() []discovery.HostView { return append([]discovery.HostView(nil), s.hosts...) }
func (s state) PageURLs() []string          { return nil }

func run(t *testing.T, r Resolver, opts Options, hosts ...discovery.HostView) map[string]*discovery.DNSInfo {
	t.Helper()
	tgt, _ := discovery.ParseTarget("example.com")
	var mu sync.Mutex
	got := map[string]*discovery.DNSInfo{}
	err := New(r, opts).Discover(context.Background(), discovery.Input{Target: tgt, State: state{hosts}}, func(f discovery.Finding) {
		mu.Lock()
		defer mu.Unlock()
		got[f.Host] = f.DNS
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestResolve(t *testing.T) {
	r := &fakeResolver{
		addrs: map[string][]string{
			"example.com":       {"93.184.216.34", "2606:2800:220:1::1", "93.184.216.34"},
			"app.example.com":   {"93.184.216.35"},
			"intra.example.com": {"10.0.0.8", "127.0.0.1"},
			"mixed.example.com": {"10.0.0.8", "93.184.216.36"},
		},
		cnames: map[string]string{"app.example.com": "App.Hosting-Provider.net."},
	}
	got := run(t, r, Options{MaxHosts: 10, Concurrency: 2},
		discovery.HostView{Name: "example.com"},
		discovery.HostView{Name: "app.example.com"},
		discovery.HostView{Name: "old.example.com"},
		discovery.HostView{Name: "intra.example.com"},
		discovery.HostView{Name: "mixed.example.com"},
	)

	if d := got["example.com"]; !d.Resolved || d.NonPublic || !reflect.DeepEqual(d.Addresses, []string{"2606:2800:220:1::1", "93.184.216.34"}) || d.CNAME != "" {
		t.Errorf("example.com = %+v", d)
	}
	if d := got["app.example.com"]; d.CNAME != "app.hosting-provider.net" {
		t.Errorf("app cname = %+v", d)
	}
	if d := got["old.example.com"]; d.Resolved || d.Error != "no such host" {
		t.Errorf("old = %+v", d)
	}
	if d := got["intra.example.com"]; !d.Resolved || !d.NonPublic {
		t.Errorf("intra should be flagged non-public: %+v", d)
	}
	if d := got["mixed.example.com"]; d.NonPublic {
		t.Errorf("mixed has a public address: %+v", d)
	}
}

func TestResolveSkipsKnownAndRespectsLimit(t *testing.T) {
	r := &fakeResolver{addrs: map[string][]string{}}
	got := run(t, r, Options{MaxHosts: 2, Concurrency: 1},
		discovery.HostView{Name: "known.example.com", DNS: &discovery.DNSInfo{Resolved: true}},
		discovery.HostView{Name: "b.example.com", Sources: []discovery.Source{discovery.SourceCT}},
		discovery.HostView{Name: "a.example.com", Sources: []discovery.Source{discovery.SourceCT}},
	)
	if len(r.calls) != 1 || r.calls[0] != "a.example.com" {
		t.Errorf("lookups = %v, want only a.example.com (known host skipped, limit 2)", r.calls)
	}
	if _, ok := got["known.example.com"]; ok {
		t.Error("already-resolved host was re-reported")
	}
	if d := got["b.example.com"]; d == nil || d.Skipped == "" {
		t.Errorf("b over limit = %+v", d)
	}
}
