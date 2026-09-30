// Package fetch is the single outbound HTTP client used by discovery engines.
//
// It enforces request timeouts, response size limits, per-host rate limits,
// the server-wide concurrency limit (a resource.Pool shared by all scans),
// each scan's request budget (the resource.Account in the request context),
// and refuses to connect to private or loopback addresses (unless explicitly
// allowed for development). Redirects are never followed automatically so
// that callers can record them and decide whether they stay in scope.
package fetch

import (
	"compress/gzip"
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

	"websitemapper/internal/resource"
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
	// Pool bounds requests in flight across every scan using this client.
	// Nil means unbounded (tests only).
	Pool *resource.Pool
	// Bandwidth paces the bytes read by every client sharing it (nil:
	// unlimited). Bodies count as they are read; each response also counts
	// headerBytes for request and response headers.
	Bandwidth *resource.Bandwidth
	// ReadBody decides, by media type, whether a response body is read.
	// Bodies that are not read are never downloaded beyond what the
	// connection has already buffered. Nil reads textual types (HTML, JSON,
	// XML, JavaScript, text).
	ReadBody func(mediaType string) bool
}

// headerBytes approximates the request and response headers of one
// exchange, which are transferred even when the body is not read.
const headerBytes = 1024

// readChunk is how much of a body is read between bandwidth checks.
const readChunk = 32 << 10

// HTMLOnly reads only HTML bodies: enough for crawling and page titles,
// without downloading JSON, XML, feeds or scripts that are only classified.
func HTMLOnly(mediaType string) bool {
	return mediaType == "" || mediaType == "text/html" || mediaType == "application/xhtml+xml"
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
	if opts.ReadBody == nil {
		opts.ReadBody = isTextual
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
		MaxIdleConns:          idleConns(opts.Pool),
		IdleConnTimeout:       60 * time.Second,
		// Get asks for gzip and decompresses itself, so that bandwidth is
		// measured in bytes on the wire (what the network carries and the
		// provider bills), not in decompressed bytes.
		DisableCompression: true,
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

// idleConns bounds idle keep-alive connections kept across all hosts.
func idleConns(p *resource.Pool) int {
	if p == nil {
		return 64
	}
	return 2 * p.Stats().Capacity
}

// Get performs a single GET request without following redirects.
//
// Order of admission: the scan's request budget, then the per-host rate
// limit, then a slot in the shared pool. The pool slot is held only for the
// request itself (including reading the body), never while sleeping for the
// rate limit, so politeness delays do not consume server-wide capacity.
func (c *Client) Get(ctx context.Context, rawURL string) (*Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	if err := resource.FromContext(ctx).TakeRequest(); err != nil {
		return nil, err
	}
	if err := c.limiter.wait(ctx, u.Host); err != nil {
		return nil, err
	}
	release, waited, err := c.opts.Pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if waited > 0 {
		// The rate-limit slot reserved above may have passed while waiting
		// for capacity; reserve a fresh one so per-host spacing still holds.
		if err := c.limiter.wait(ctx, u.Host); err != nil {
			return nil, err
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.opts.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/json;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Encoding", "gzip")

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
	acct := resource.FromContext(ctx)
	if err := c.account(ctx, acct, headerBytes); err != nil {
		return nil, err
	}
	if c.opts.ReadBody(out.ContentType) {
		body, truncated, err := c.readBody(ctx, acct, resp)
		if err != nil {
			return nil, err
		}
		out.Body, out.Truncated = body, truncated
	}
	return out, nil
}

// account charges n transferred bytes to the scan's download budget and the
// shared bandwidth limit.
func (c *Client) account(ctx context.Context, acct *resource.Account, n int) error {
	if err := acct.TakeBytes(n); err != nil {
		return err
	}
	return c.opts.Bandwidth.Wait(ctx, n)
}

// readBody reads up to MaxBodyBytes of (decompressed) body. Bytes are
// charged as they arrive from the network, before decompression, and each
// chunk is paced by the bandwidth limit before more is read, so the limit
// slows the transfer itself. MaxBodyBytes applies to the decompressed size,
// which also bounds a response that decompresses to something huge.
func (c *Client) readBody(ctx context.Context, acct *resource.Account, resp *http.Response) ([]byte, bool, error) {
	var r io.Reader = &meteredReader{ctx: ctx, c: c, acct: acct, r: resp.Body}
	if strings.EqualFold(strings.TrimSpace(resp.Header.Get("Content-Encoding")), "gzip") {
		zr, err := gzip.NewReader(r)
		if err != nil {
			return nil, false, fmt.Errorf("reading gzip body: %w", err)
		}
		defer zr.Close()
		r = zr
	}
	limit := c.opts.MaxBodyBytes
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		var budget *budgetError
		if errors.As(err, &budget) {
			return nil, false, budget.err
		}
		return nil, false, fmt.Errorf("reading body: %w", err)
	}
	if int64(len(body)) > limit {
		return body[:limit], true, nil
	}
	return body, false, nil
}

// meteredReader charges every byte read from the network to the scan's
// download budget and the shared bandwidth limit.
type meteredReader struct {
	ctx  context.Context
	c    *Client
	acct *resource.Account
	r    io.Reader
}

// budgetError carries a budget or cancellation error through readers that
// would otherwise wrap it.
type budgetError struct{ err error }

func (e *budgetError) Error() string { return e.err.Error() }

func (m *meteredReader) Read(p []byte) (int, error) {
	n, err := m.r.Read(p[:min(len(p), readChunk)])
	if n > 0 {
		if aerr := m.c.account(m.ctx, m.acct, n); aerr != nil {
			return n, &budgetError{aerr}
		}
	}
	return n, err
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

// WithBodies returns a client that reads bodies of the given kinds, up to
// maxBytes, and otherwise shares everything with c: its connections,
// per-host rate limit, pool and bandwidth limit. Used for robots.txt and
// sitemaps, which are text and XML rather than HTML.
func (c *Client) WithBodies(read func(mediaType string) bool, maxBytes int64) *Client {
	cp := *c
	cp.opts.ReadBody = read
	if maxBytes > 0 {
		cp.opts.MaxBodyBytes = maxBytes
	}
	return &cp
}

// TextAndXML reads plain text, XML and gzip bodies (robots.txt, sitemaps
// and compressed .xml.gz sitemaps).
func TextAndXML(mediaType string) bool {
	return mediaType == "" || strings.HasPrefix(mediaType, "text/") || strings.Contains(mediaType, "xml") ||
		strings.Contains(mediaType, "gzip")
}
