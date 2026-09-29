// Package fetch is the single outbound HTTP client used by discovery engines.
//
// It enforces request timeouts, response size limits, per-host rate limits,
// and refuses to connect to private or loopback addresses (unless explicitly
// allowed for development). Redirects are never followed automatically so
// that callers can record them and decide whether they stay in scope.
package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// ErrBlockedAddress is returned when a host resolves to a non-public address.
var ErrBlockedAddress = errors.New("refusing to connect to non-public address")

// Options configures a Client.
type Options struct {
	Timeout           time.Duration
	MaxBodyBytes      int64
	UserAgent         string
	RequestsPerSecond float64
	AllowPrivate      bool
}

// Client performs polite, bounded GET requests.
type Client struct {
	http    *http.Client
	opts    Options
	limiter *hostLimiter
}

// Response is the relevant subset of an HTTP response.
type Response struct {
	// URL is the requested URL.
	URL        string
	StatusCode int
	Header     http.Header
	// ContentType is the media type without parameters, e.g. "text/html".
	ContentType string
	// Location is the resolved redirect target for 3xx responses.
	Location string
	// Body holds up to MaxBodyBytes of textual responses. Binary bodies are not read.
	Body      []byte
	Truncated bool
}

// IsRedirect reports whether the response is a redirect with a target.
func (r *Response) IsRedirect() bool {
	return r.StatusCode >= 300 && r.StatusCode < 400 && r.Location != ""
}

// New creates a Client.
func New(opts Options) *Client {
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = 2 << 20
	}
	dialer := &net.Dialer{Timeout: opts.Timeout, KeepAlive: 30 * time.Second}
	if !opts.AllowPrivate {
		dialer.Control = blockNonPublic
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   opts.Timeout,
		ResponseHeaderTimeout: opts.Timeout,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       60 * time.Second,
	}
	return &Client{
		http: &http.Client{
			Transport: transport,
			Timeout:   opts.Timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		opts:    opts,
		limiter: newHostLimiter(opts.RequestsPerSecond),
	}
}

// Get performs a single GET request without following redirects.
func (c *Client) Get(ctx context.Context, rawURL string) (*Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	if b := budgetFrom(ctx); b != nil {
		if err := b.take(ctx); err != nil {
			return nil, err
		}
	}
	if err := c.limiter.wait(ctx, u.Host); err != nil {
		return nil, err
	}

	// Requests carry no cookies, credentials or body: there is no cookie
	// jar, URL user info is dropped, and only these headers are set.
	u.User = nil
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.opts.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/json;q=0.9,*/*;q=0.8")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	out := &Response{
		URL:         u.String(),
		StatusCode:  resp.StatusCode,
		Header:      resp.Header,
		ContentType: mediaType(resp.Header.Get("Content-Type")),
	}
	if loc := resp.Header.Get("Location"); loc != "" && resp.StatusCode >= 300 && resp.StatusCode < 400 {
		if ref, err := u.Parse(loc); err == nil {
			out.Location = ref.String()
		}
	}
	if isTextual(out.ContentType) {
		body, err := io.ReadAll(io.LimitReader(resp.Body, c.opts.MaxBodyBytes+1))
		if err != nil {
			return nil, fmt.Errorf("reading body: %w", err)
		}
		if int64(len(body)) > c.opts.MaxBodyBytes {
			body = body[:c.opts.MaxBodyBytes]
			out.Truncated = true
		}
		out.Body = body
	}
	return out, nil
}

func mediaType(header string) string {
	if header == "" {
		return ""
	}
	mt, _, err := mime.ParseMediaType(header)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(strings.SplitN(header, ";", 2)[0]))
	}
	return mt
}

func isTextual(mt string) bool {
	switch {
	case mt == "", strings.HasPrefix(mt, "text/"):
		return true
	case strings.Contains(mt, "html"), strings.Contains(mt, "xml"),
		strings.Contains(mt, "json"), strings.Contains(mt, "javascript"):
		return true
	}
	return false
}

// nonPublicPrefixes are ranges that are not globally routable or are
// reserved for special use, beyond what netip's predicates cover.
var nonPublicPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range []string{
		"0.0.0.0/8",          // "this" network
		"100.64.0.0/10",      // shared address space (CGNAT)
		"192.0.0.0/24",       // IETF protocol assignments
		"192.0.2.0/24",       // documentation (TEST-NET-1)
		"198.18.0.0/15",      // benchmarking
		"198.51.100.0/24",    // documentation (TEST-NET-2)
		"203.0.113.0/24",     // documentation (TEST-NET-3)
		"240.0.0.0/4",        // reserved
		"255.255.255.255/32", // broadcast
		"64:ff9b::/96",       // NAT64: can embed any IPv4 address, including private ones
		"64:ff9b:1::/48",     // local-use NAT64
		"100::/64",           // discard-only
		"2001::/32",          // Teredo: embeds IPv4
		"2001:db8::/32",      // documentation
		"2002::/16",          // 6to4: embeds IPv4
		"fc00::/7",           // unique local
		"fec0::/10",          // deprecated site-local
	} {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

// blockNonPublic runs after DNS resolution, so it sees the real IP being dialed.
func blockNonPublic(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return ErrBlockedAddress
	}
	if !IsPublicAddr(ip) {
		return fmt.Errorf("%w: %s", ErrBlockedAddress, ip)
	}
	return nil
}

// IsPublicAddr reports whether ip is a globally routable unicast address
// that is safe to connect to. Loopback, private, link-local, multicast,
// unspecified and special-purpose ranges are all rejected.
func IsPublicAddr(ip netip.Addr) bool {
	ip = ip.Unmap().WithZone("")
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	for _, p := range nonPublicPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}
