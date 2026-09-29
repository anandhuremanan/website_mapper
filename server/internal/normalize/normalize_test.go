package normalize

import (
	"errors"
	"net/url"
	"testing"
)

func TestString(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"https://example.com", "https://example.com/"},
		{"https://example.com/", "https://example.com/"},
		{"https://EXAMPLE.COM/about", "https://example.com/about"},
		{"https://example.com/about/", "https://example.com/about"},
		{"HTTPS://example.com:443/about", "https://example.com/about"},
		{"http://example.com:80/", "http://example.com/"},
		{"http://example.com:8080/x", "http://example.com:8080/x"},
		{"https://example.com./a", "https://example.com/a"},
		{"https://example.com/a/../b/./c", "https://example.com/b/c"},
		{"https://example.com//a//b", "https://example.com/a/b"},
		{"https://example.com/a#section", "https://example.com/a"},
		{"https://user:pw@example.com/a", "https://example.com/a"},
		{"https://example.com/s?b=2&a=1", "https://example.com/s?a=1&b=2"},
		{"https://example.com/s?utm_source=x&id=3&fbclid=y", "https://example.com/s?id=3"},
		{"https://example.com/s?", "https://example.com/s"},
		{"https://example.com/caf%C3%A9", "https://example.com/caf%C3%A9"},
		{"https://example.com/a%2Fb", "https://example.com/a%2Fb"},
	}
	for _, tt := range tests {
		got, err := String(tt.in)
		if err != nil {
			t.Errorf("String(%q) error: %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("String(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestStringRejectsNonWeb(t *testing.T) {
	for _, in := range []string{"mailto:a@example.com", "ftp://example.com/", "javascript:void(0)", "/relative", ""} {
		if _, err := String(in); err == nil {
			t.Errorf("String(%q) expected error", in)
		}
	}
}

func TestResolve(t *testing.T) {
	base, _ := url.Parse("https://example.com/docs/intro/")
	tests := []struct {
		ref, want string
	}{
		{"../about", "https://example.com/docs/about"},
		{"guide", "https://example.com/docs/intro/guide"},
		{"/pricing/", "https://example.com/pricing"},
		{"//cdn.example.com/app.js", "https://cdn.example.com/app.js"},
		{"https://Other.example.com/x#y", "https://other.example.com/x"},
		{"?page=2", "https://example.com/docs/intro?page=2"},
	}
	for _, tt := range tests {
		got, err := Resolve(base, tt.ref)
		if err != nil {
			t.Errorf("Resolve(%q) error: %v", tt.ref, err)
			continue
		}
		if got.String() != tt.want {
			t.Errorf("Resolve(%q) = %q, want %q", tt.ref, got.String(), tt.want)
		}
	}
	for _, ref := range []string{"#top", "mailto:x@example.com", "tel:+123", "javascript:alert(1)", "data:text/plain,hi", "  "} {
		if _, err := Resolve(base, ref); !errors.Is(err, ErrUnsupported) {
			t.Errorf("Resolve(%q) err = %v, want ErrUnsupported", ref, err)
		}
	}
}

func TestHost(t *testing.T) {
	for in, want := range map[string]string{
		"EXAMPLE.COM":           "example.com",
		"example.com.":          "example.com",
		"*.example.com":         "example.com",
		"*.*.Api.Example.com":   "api.example.com",
		" www.example.com ":     "www.example.com",
		"xn--bcher-kva.example": "xn--bcher-kva.example",
	} {
		got, err := Host(in)
		if err != nil || got != want {
			t.Errorf("Host(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"", "*", "*.", "localhost", "a@example.com", "ex ample.com", "-a.example.com",
		"a-.example.com", "a..example.com", "api.*.example.com", "127.0.0.1", "_dmarc.example.com",
		"example.com/path",
	} {
		if got, err := Host(in); err == nil {
			t.Errorf("Host(%q) = %q, want error", in, got)
		}
	}
}
