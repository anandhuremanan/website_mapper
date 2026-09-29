// Package normalize canonicalizes URLs so the same resource discovered
// through different sources collapses into a single finding.
package normalize

import (
	"errors"
	"net"
	"net/url"
	"path"
	"sort"
	"strings"
)

// ErrUnsupported is returned for URLs that are not http(s) web resources.
var ErrUnsupported = errors.New("unsupported URL")

// trackingParams are query parameters that never identify a distinct resource.
var trackingParams = map[string]bool{
	"fbclid": true, "gclid": true, "msclkid": true, "mc_cid": true, "mc_eid": true,
}

// URL parses and canonicalizes an absolute http(s) URL:
//
//   - scheme and host are lowercased, default ports and trailing dots removed
//   - user info and fragments are dropped
//   - dot segments and duplicate slashes are resolved
//   - a trailing slash is removed (except for the root path)
//   - query parameters are sorted and tracking parameters removed
func URL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, err
	}
	return canonicalize(u)
}

// String is URL followed by String, for use as a deduplication key.
func String(raw string) (string, error) {
	u, err := URL(raw)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

// Resolve resolves a (possibly relative) reference found on a page against
// the page's base URL and canonicalizes the result. Non-web references such
// as mailto:, tel:, javascript: and data: return ErrUnsupported.
func Resolve(base *url.URL, ref string) (*url.URL, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.HasPrefix(ref, "#") {
		return nil, ErrUnsupported
	}
	r, err := url.Parse(ref)
	if err != nil {
		return nil, err
	}
	return canonicalize(base.ResolveReference(r))
}

// Hostname returns the canonical hostname of a URL string, or "" if invalid.
func Hostname(raw string) string {
	u, err := URL(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func canonicalize(in *url.URL) (*url.URL, error) {
	u := *in
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, ErrUnsupported
	}
	if u.Opaque != "" {
		return nil, ErrUnsupported
	}

	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return nil, ErrUnsupported
	}
	port := u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}

	u.User = nil
	u.Fragment = ""
	u.RawFragment = ""

	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	p = path.Clean(p)
	if p == "." {
		p = "/"
	}
	if unescaped, err := url.PathUnescape(p); err == nil {
		u.Path = unescaped
		u.RawPath = p
		// Let url.URL decide whether RawPath is needed.
		if u.EscapedPath() != p {
			u.RawPath = ""
		}
	}

	u.RawQuery = canonicalQuery(u.RawQuery)
	u.ForceQuery = false
	return &u, nil
}

func canonicalQuery(raw string) string {
	if raw == "" {
		return ""
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return raw
	}
	for k := range values {
		lk := strings.ToLower(k)
		if trackingParams[lk] || strings.HasPrefix(lk, "utm_") {
			delete(values, k)
		}
	}
	for _, v := range values {
		sort.Strings(v)
	}
	return values.Encode() // Encode sorts by key.
}

// Host canonicalizes a bare hostname such as one found in a certificate:
// lowercased, trailing dot and wildcard prefixes ("*.") removed. It rejects
// anything that is not a syntactically valid DNS hostname, including IP
// literals and email addresses.
func Host(raw string) (string, error) {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
	for strings.HasPrefix(h, "*.") {
		h = h[2:]
	}
	if h == "" || len(h) > 253 {
		return "", ErrUnsupported
	}
	if net.ParseIP(h) != nil {
		return "", ErrUnsupported
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return "", ErrUnsupported
	}
	for _, l := range labels {
		if !validLabel(l) {
			return "", ErrUnsupported
		}
	}
	return h, nil
}

func validLabel(l string) bool {
	if len(l) == 0 || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
		return false
	}
	for i := 0; i < len(l); i++ {
		c := l[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}
