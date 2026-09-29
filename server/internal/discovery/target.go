package discovery

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"

	"websitemapper/internal/normalize"
)

// ErrInvalidTarget is returned for inputs that are not a public domain.
var ErrInvalidTarget = errors.New("invalid target")

// Target is a validated scan target.
type Target struct {
	// Input is what the user entered.
	Input string
	// Domain is the canonical domain that defines scan scope: the entered
	// hostname lowercased, with a leading "www." removed. The domain and
	// all of its subdomains are in scope.
	Domain string
	// StartURL is the normalized URL crawling begins from.
	StartURL string
}

var hostnamePattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$`)

// ParseTarget validates user input such as "example.com" or
// "https://www.example.com/docs" and derives the scan scope.
// IP addresses, single-label hosts and non-default ports are rejected.
func ParseTarget(input string) (Target, error) {
	raw := strings.TrimSpace(input)
	if raw == "" {
		return Target{}, fmt.Errorf("%w: enter a domain such as example.com", ErrInvalidTarget)
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := normalize.URL(raw)
	if err != nil {
		return Target{}, fmt.Errorf("%w: %q is not a valid http(s) URL", ErrInvalidTarget, input)
	}
	host := u.Hostname()
	if _, err := netip.ParseAddr(host); err == nil {
		return Target{}, fmt.Errorf("%w: IP addresses are not supported, enter a domain name", ErrInvalidTarget)
	}
	if !hostnamePattern.MatchString(host) || len(host) > 253 {
		return Target{}, fmt.Errorf("%w: %q is not a public domain name", ErrInvalidTarget, host)
	}
	if u.Port() != "" {
		return Target{}, fmt.Errorf("%w: custom ports are not supported", ErrInvalidTarget)
	}
	if strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return Target{}, fmt.Errorf("%w: %q is not a public domain name", ErrInvalidTarget, host)
	}
	u.RawQuery = ""
	return Target{
		Input:    strings.TrimSpace(input),
		Domain:   strings.TrimPrefix(host, "www."),
		StartURL: u.String(),
	}, nil
}

// InScope reports whether host is the target domain or one of its subdomains.
func (t Target) InScope(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	return host == t.Domain || strings.HasSuffix(host, "."+t.Domain)
}

// URLInScope reports whether rawURL's host is in scope.
func (t Target) URLInScope(rawURL string) bool {
	h := normalize.Hostname(rawURL)
	return h != "" && t.InScope(h)
}

// HostOf returns the canonical hostname of a URL, or "" if it is invalid.
func HostOf(rawURL string) string {
	return normalize.Hostname(rawURL)
}

// Hosts returns the hosts every scan starts with: the entered host and the
// apex domain (the same host when the user entered the apex).
func (t Target) Hosts() []string {
	start := HostOf(t.StartURL)
	if start == t.Domain {
		return []string{start}
	}
	return []string{start, t.Domain}
}
