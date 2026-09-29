package discovery

import (
	"errors"
	"testing"
)

func TestParseTarget(t *testing.T) {
	tests := []struct {
		in, domain, start string
	}{
		{"example.com", "example.com", "https://example.com/"},
		{"  Example.COM  ", "example.com", "https://example.com/"},
		{"https://www.example.com", "example.com", "https://www.example.com/"},
		{"http://example.com/docs/", "example.com", "http://example.com/docs"},
		{"https://blog.example.co.uk/?q=1#x", "blog.example.co.uk", "https://blog.example.co.uk/"},
		{"https://example.com:443", "example.com", "https://example.com/"},
	}
	for _, tt := range tests {
		got, err := ParseTarget(tt.in)
		if err != nil {
			t.Errorf("ParseTarget(%q) error: %v", tt.in, err)
			continue
		}
		if got.Domain != tt.domain || got.StartURL != tt.start {
			t.Errorf("ParseTarget(%q) = %+v, want domain %q start %q", tt.in, got, tt.domain, tt.start)
		}
	}
}

func TestParseTargetRejects(t *testing.T) {
	for _, in := range []string{
		"", "localhost", "http://127.0.0.1", "10.0.0.1", "[::1]", "ftp://example.com",
		"example", "exa mple.com", "https://example.com:8443", "printer.local", "app.localhost",
		"-bad.example.com",
	} {
		if _, err := ParseTarget(in); !errors.Is(err, ErrInvalidTarget) {
			t.Errorf("ParseTarget(%q) err = %v, want ErrInvalidTarget", in, err)
		}
	}
}

func TestInScope(t *testing.T) {
	tgt, _ := ParseTarget("www.example.com")
	for host, want := range map[string]bool{
		"example.com":         true,
		"www.example.com":     true,
		"API.Example.com":     true,
		"a.b.example.com":     true,
		"example.com.":        true,
		"notexample.com":      false,
		"example.com.evil.io": false,
		"cdn.other.com":       false,
		"example.org":         false,
		"evil-example.com":    false,
		"example.com.au":      false,
	} {
		if got := tgt.InScope(host); got != want {
			t.Errorf("InScope(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestTargetHosts(t *testing.T) {
	a, _ := ParseTarget("example.com")
	b, _ := ParseTarget("https://www.example.com/docs")
	if got := a.Hosts(); len(got) != 1 || got[0] != "example.com" {
		t.Errorf("apex hosts = %v", got)
	}
	if got := b.Hosts(); len(got) != 2 || got[0] != "www.example.com" || got[1] != "example.com" {
		t.Errorf("www hosts = %v", got)
	}
}

func TestPrioritizeHosts(t *testing.T) {
	tgt, _ := ParseTarget("www.example.com")
	hosts := []HostView{
		{Name: "zeta.example.com", Sources: []Source{SourceCT}},
		{Name: "alpha.example.com", Sources: []Source{SourceCT}},
		{Name: "docs.example.com", Sources: []Source{SourceCT, SourceHTML}},
		{Name: "example.com"},
		{Name: "www.example.com"},
	}
	PrioritizeHosts(hosts, tgt)
	var got []string
	for _, h := range hosts {
		got = append(got, h.Name)
	}
	want := []string{"www.example.com", "example.com", "docs.example.com", "alpha.example.com", "zeta.example.com"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}
