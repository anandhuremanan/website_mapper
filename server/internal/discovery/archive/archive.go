// Package archive is a discovery engine that lists a domain's URLs from a
// public web archive (the Wayback Machine's CDX index), covering the domain
// and all its subdomains. It never contacts the target site.
//
// Archived URLs are historical: they show what was once publicly reachable
// and may no longer exist. They are reported as discovered, with when they
// were first archived and their archived content type, and are never
// requested by the scanner.
//
// A domain can have millions of archived URLs, so the listing is read a
// page at a time and reported as it arrives: memory stays at one page
// however long the listing is.
package archive

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"websitemapper/internal/cache"
	"websitemapper/internal/discovery"
	"websitemapper/internal/fetch"
	"websitemapper/internal/normalize"
	"websitemapper/internal/resource"
)

// Fetcher performs a single GET.
type Fetcher interface {
	Get(ctx context.Context, url string) (*fetch.Response, error)
}

// Entry is one archived URL.
type Entry struct {
	URL       string
	FirstSeen int64 // Unix seconds
	MIME      string
}

// Listing is a cached answer for one domain. Only listings small enough to
// keep in memory are cached; long ones are read from the archive each time.
type Listing struct {
	Entries []Entry
	// Truncated is true when the archive holds more URLs than were listed.
	Truncated bool
	// Err is set when the archive could not be queried.
	Err string
}

// Cache holds listings by domain, shared by all scans.
type Cache = cache.Cache[Listing]

// NewCache creates an archive cache.
func NewCache(maxBytes int64) *Cache {
	return cache.New[Listing](cache.Options{Name: "archive", MaxCost: maxBytes})
}

// Defaults.
const (
	// defaultPageSize is how many URLs are asked for per request. Larger
	// pages mean fewer requests against the archive's rate limit; this size
	// keeps a page's body at a few megabytes.
	defaultPageSize = 25000
	// defaultCooldown is how long the archive is left alone after it asks
	// the scanner to slow down. The Internet Archive blocks clients that
	// keep sending requests after an HTTP 429.
	defaultCooldown = 2 * time.Minute
	// maxCooldown bounds a Retry-After sent by the archive.
	maxCooldown = time.Hour
)

// maxCachedEntries is the longest listing kept in the cache.
var maxCachedEntries = 20000

// Options configures the engine.
type Options struct {
	// MaxURLs bounds the URLs listed per scan.
	MaxURLs int
	// PageSize is the number of URLs asked for per request (0: default).
	PageSize int
	// Cache, TTL and FailureTTL reuse short listings across scans. Archives
	// change slowly, so TTL can be long; failures are kept briefly.
	Cache      *Cache
	TTL        time.Duration
	FailureTTL time.Duration
	// Cooldown is how long no request is sent after the archive answers
	// with HTTP 429 (0: default).
	Cooldown time.Duration
	// BaseURL is the archive's address; overridable for tests.
	BaseURL string
	// Now is the clock; overridable for tests.
	Now func() time.Time
}

// Engine lists archived URLs. One Engine serves every scan, so its
// cooldown protects the archive from all of them together. The pace of
// requests is set by the Fetcher's per-host rate limit.
type Engine struct {
	fetcher Fetcher
	opts    Options
	// blockedUntil is when the archive may be asked again after an HTTP
	// 429, in Unix nanoseconds.
	blockedUntil atomic.Int64
}

// New creates an Engine.
func New(f Fetcher, opts Options) *Engine {
	if opts.BaseURL == "" {
		opts.BaseURL = "https://web.archive.org"
	}
	if opts.PageSize <= 0 {
		opts.PageSize = defaultPageSize
	}
	if opts.Cooldown <= 0 {
		opts.Cooldown = defaultCooldown
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Engine{fetcher: f, opts: opts}
}

func (e *Engine) Name() string { return "archive" }

// Discover reports every archived URL of the target domain and its
// subdomains, up to MaxURLs. A listing that could not be completed (the
// limit was reached, or the archive stopped answering part of the way) is
// reported as a partial error; what was listed is kept.
func (e *Engine) Discover(ctx context.Context, in discovery.Input, emit discovery.Emit) error {
	domain := in.Target.Domain
	if l, created, ok := e.opts.Cache.Get(domain); ok {
		if l.Err != "" {
			return fmt.Errorf("%s (answer cached at %s)", l.Err, created.UTC().Format(time.RFC3339))
		}
		for _, en := range l.Entries {
			emit(finding(en, created))
		}
		return e.truncated(l.Truncated)
	}

	l, listed, cacheable, err := e.list(ctx, in.Target, emit)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if cacheable {
		e.remember(ctx, domain, l)
	}
	if err != nil {
		if listed > 0 {
			return discovery.Partial(err) // what was listed is kept
		}
		return err
	}
	return e.truncated(l.Truncated)
}

func (e *Engine) truncated(yes bool) error {
	if !yes {
		return nil
	}
	return discovery.Partial(fmt.Errorf("listed the first %d archived URLs; the archive holds more", e.opts.MaxURLs))
}

func finding(en Entry, cachedAt time.Time) discovery.Finding {
	return discovery.Finding{
		URL: en.URL, Source: discovery.SourceArchive, Hint: discovery.HintArchive, CachedAt: cachedAt,
		Archive: &discovery.ArchiveInfo{FirstSeen: time.Unix(en.FirstSeen, 0).UTC(), ContentType: en.MIME},
	}
}

// remember caches a listing that is short enough to keep, or a failure.
func (e *Engine) remember(ctx context.Context, domain string, l Listing) {
	ttl := e.opts.TTL
	if l.Err != "" {
		ttl = min(e.opts.FailureTTL, e.opts.TTL)
	}
	_, _, _ = e.opts.Cache.Do(ctx, domain, func(context.Context) (cache.Loaded[Listing], error) {
		return cache.Loaded[Listing]{Value: l, TTL: ttl, Group: domain, Cost: listingCost(l)}, nil
	})
}

// list reads the listing page by page, emitting each URL as it arrives,
// and returns how many URLs the archive listed.
//
// If cacheable is true, l is what to cache: the whole listing when it is
// short, or a failure that happened before anything was listed. Otherwise
// l.Entries is empty. l.Truncated and err describe the whole listing either
// way.
func (e *Engine) list(ctx context.Context, t discovery.Target, emit discovery.Emit) (l Listing, listed int, cacheable bool, err error) {
	if wait := time.Duration(e.blockedUntil.Load() - e.opts.Now().UnixNano()); wait > 0 {
		return Listing{}, 0, false, fmt.Errorf("the web archive asked this server to slow down; it will be asked again in %s",
			wait.Round(time.Second))
	}

	// keep collects entries for the cache until the listing proves too long.
	keep, resume := true, ""
	for {
		size := min(e.opts.PageSize, e.opts.MaxURLs-listed)
		page, err := e.page(ctx, t, size, resume)
		if err != nil {
			switch {
			case ctx.Err() != nil:
				return Listing{}, listed, false, ctx.Err()
			case listed > 0:
				return Listing{}, listed, false, fmt.Errorf("the web archive stopped answering after %d URLs (%w); the listing is incomplete", listed, err)
			case errors.Is(err, resource.ErrBudgetExhausted), errors.Is(err, errRateLimited):
				return Listing{}, 0, false, err // about this scan or this moment, not the domain
			}
			return Listing{Err: err.Error()}, 0, true, err
		}
		for _, en := range page.entries {
			emit(finding(en, time.Time{}))
			if keep {
				if len(l.Entries) < maxCachedEntries {
					l.Entries = append(l.Entries, en)
				} else {
					keep, l.Entries = false, nil
				}
			}
		}
		listed += page.rows
		switch {
		case page.resume == "":
			return l, listed, keep, nil // the end of the listing
		case listed >= e.opts.MaxURLs:
			l.Truncated = true
			return l, listed, keep, nil
		case page.rows == 0:
			// A resume key without rows would loop forever.
			return Listing{}, listed, false, fmt.Errorf("the web archive returned an empty page after %d URLs; the listing is incomplete", listed)
		}
		resume = page.resume
	}
}

// errRateLimited marks an HTTP 429 from the archive.
var errRateLimited = errors.New("the web archive asked this server to slow down (HTTP 429)")

type page struct {
	entries []Entry
	// rows is how many URLs the archive returned, including ones dropped
	// as junk or out of scope.
	rows int
	// resume continues the listing; empty on the last page.
	resume string
}

// page asks the CDX index for the next URLs captured with HTTP 200, one row
// per distinct URL (its first capture).
func (e *Engine) page(ctx context.Context, t discovery.Target, size int, resume string) (page, error) {
	q := url.Values{
		"url":           {t.Domain},
		"matchType":     {"domain"},
		"fl":            {"original,timestamp,mimetype"},
		"collapse":      {"urlkey"},
		"filter":        {"statuscode:200"},
		"limit":         {strconv.Itoa(size)},
		"showResumeKey": {"true"},
	}
	if resume != "" {
		q.Set("resumeKey", resume)
	}
	resp, err := e.fetcher.Get(ctx, e.opts.BaseURL+"/cdx/search/cdx?"+q.Encode())
	if err != nil {
		if errors.Is(err, resource.ErrBudgetExhausted) {
			return page{}, err
		}
		return page{}, fmt.Errorf("web archive request failed: %w", err)
	}
	switch {
	case resp.StatusCode == 429:
		e.blockedUntil.Store(e.opts.Now().Add(e.cooldown(resp)).UnixNano())
		return page{}, errRateLimited
	case resp.StatusCode != 200:
		return page{}, fmt.Errorf("web archive returned HTTP %d", resp.StatusCode)
	case resp.Truncated:
		// The resume key is at the end of the body, so the listing cannot
		// continue from a cut-off page.
		return page{}, errors.New("web archive response exceeded the size limit")
	}
	return parsePage(resp.Body, t), nil
}

// cooldown is how long to leave the archive alone after an HTTP 429.
func (e *Engine) cooldown(resp *fetch.Response) time.Duration {
	d := e.opts.Cooldown
	if s, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil {
		d = max(d, time.Duration(s)*time.Second)
	}
	return min(d, maxCooldown)
}

// parsePage reads a CDX text response: one "original timestamp mimetype"
// line per URL, then, if more follow, a blank line and a resume key. It
// keeps in-scope, well-formed URLs (normalized) and drops junk the archive
// picked up from broken links.
func parsePage(body []byte, t discovery.Target) page {
	var p page
	ended := false // past the blank line: the next line is the resume key
	for len(body) > 0 {
		var line []byte
		line, body, _ = bytes.Cut(body, []byte("\n"))
		line = bytes.TrimSpace(line)
		switch {
		case len(line) == 0:
			ended = true
			continue
		case ended:
			p.resume = fromStart(string(line))
			return p
		}
		p.rows++
		fields := strings.Fields(string(line))
		if len(fields) < 3 || junk(fields[0]) {
			continue
		}
		u, err := normalize.URL(fields[0])
		if err != nil || !t.InScope(u.Hostname()) {
			continue
		}
		ts, err := time.Parse("20060102150405", fields[1])
		if err != nil {
			continue
		}
		p.entries = append(p.entries, Entry{URL: u.String(), FirstSeen: ts.Unix(), MIME: mime(fields[2])})
	}
	return p
}

// fromStart rewrites a resume key so that the listing continues at the
// first capture of the URL the key names.
//
// A resume key is "urlkey timestamp" (deflated, then base64): the listing
// continues after that capture. When captures are collapsed to one row per
// URL, the archive hands out the key of the first capture of the next URL,
// which it has read but not returned. Continuing after it would skip that
// capture, and with it any URL that was captured only once. Timestamp "0"
// sorts before every capture, so nothing is skipped. If the key is not in
// the expected form it is used as it is.
func fromStart(key string) string {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(key, "="))
	if err != nil {
		return key
	}
	zr, err := zlib.NewReader(bytes.NewReader(raw))
	if err != nil {
		return key
	}
	plain, err := io.ReadAll(io.LimitReader(zr, 16<<10))
	if err != nil {
		return key
	}
	urlkey, timestamp, ok := strings.Cut(string(plain), " ")
	if !ok || urlkey == "" || strings.Trim(timestamp, "0123456789") != "" {
		return key
	}
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	zw.Write([]byte(urlkey + " 0"))
	zw.Close()
	return base64.RawURLEncoding.EncodeToString(buf.Bytes())
}

// junk rejects archived URLs that are really fragments of broken markup or
// text (quotes, brackets, "!" or spaces), which archives collect from
// malformed links on other pages.
func junk(raw string) bool {
	if len(raw) > 2048 {
		return true
	}
	lower := strings.ToLower(raw)
	if strings.ContainsAny(raw, "\\"+`"'<>! `+"\t") {
		return true
	}
	for _, enc := range []string{"%22", "%27", "%3c", "%3e", "%5c", "%20%20"} {
		if strings.Contains(lower, enc) {
			return true
		}
	}
	return false
}

// mime keeps real media types and drops archive placeholders.
func mime(m string) string {
	m = strings.ToLower(strings.TrimSpace(m))
	if m == "" || m == "unk" || strings.HasPrefix(m, "warc/") || !strings.Contains(m, "/") {
		return ""
	}
	return m
}

func listingCost(l Listing) int64 {
	n := 64 + cache.StringCost(l.Err)
	for _, en := range l.Entries {
		n += 48 + int64(len(en.URL)+len(en.MIME))
	}
	return n
}
