// Package archive is a discovery engine that lists a domain's URLs from a
// public web archive (the Wayback Machine's CDX index), covering the domain
// and all its subdomains in one query. It never contacts the target site.
//
// Archived URLs are historical: they show what was once publicly reachable
// and may no longer exist. They are reported as discovered, with when they
// were first archived and their archived content type, and are never
// requested by the scanner.
package archive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
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

// Listing is the archive's answer for one domain.
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

// Options configures the engine.
type Options struct {
	// MaxURLs bounds the URLs listed per domain.
	MaxURLs int
	// Cache, TTL and FailureTTL reuse listings across scans. Archives
	// change slowly, so TTL can be long; failures are kept briefly.
	Cache      *Cache
	TTL        time.Duration
	FailureTTL time.Duration
	// BaseURL is the archive's address; overridable for tests.
	BaseURL string
}

// Engine lists archived URLs.
type Engine struct {
	fetcher Fetcher
	opts    Options
}

// New creates an Engine.
func New(f Fetcher, opts Options) *Engine {
	if opts.BaseURL == "" {
		opts.BaseURL = "https://web.archive.org"
	}
	return &Engine{fetcher: f, opts: opts}
}

func (e *Engine) Name() string { return "archive" }

// errScanSpecific marks outcomes that belong to the scan (cancellation, an
// exhausted budget), which are never cached or shared.
var errScanSpecific = errors.New("archive query stopped")

// Discover reports every archived URL of the target domain and its
// subdomains. A truncated listing is reported as a partial error.
func (e *Engine) Discover(ctx context.Context, in discovery.Input, emit discovery.Emit) error {
	domain := in.Target.Domain
	listing, info, err := e.opts.Cache.Do(ctx, domain, func(ctx context.Context) (cache.Loaded[Listing], error) {
		l, err := e.query(ctx, in.Target)
		if err != nil {
			return cache.Loaded[Listing]{}, err
		}
		ttl := e.opts.TTL
		if l.Err != "" {
			ttl = min(e.opts.FailureTTL, e.opts.TTL)
		}
		return cache.Loaded[Listing]{Value: l, TTL: ttl, Group: domain, Cost: listingCost(l)}, nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	if listing.Err != "" {
		return errors.New(listing.Err)
	}
	var cachedAt time.Time
	if info.Hit || info.Shared {
		cachedAt = info.CreatedAt
	}
	for _, en := range listing.Entries {
		emit(discovery.Finding{
			URL: en.URL, Source: discovery.SourceArchive, Hint: discovery.HintArchive, CachedAt: cachedAt,
			Archive: &discovery.ArchiveInfo{FirstSeen: time.Unix(en.FirstSeen, 0).UTC(), ContentType: en.MIME},
		})
	}
	if listing.Truncated {
		return discovery.Partial(fmt.Errorf("listed the first %d archived URLs; the archive holds more", e.opts.MaxURLs))
	}
	return nil
}

// query asks the CDX index for URLs captured with HTTP 200, one row per
// distinct URL (its first capture).
func (e *Engine) query(ctx context.Context, t discovery.Target) (Listing, error) {
	q := url.Values{
		"url":       {t.Domain},
		"matchType": {"domain"},
		"fl":        {"original,timestamp,mimetype"},
		"collapse":  {"urlkey"},
		"filter":    {"statuscode:200"},
		"output":    {"json"},
		"limit":     {fmt.Sprint(e.opts.MaxURLs + 1)},
	}
	resp, err := e.fetcher.Get(ctx, e.opts.BaseURL+"/cdx/search/cdx?"+q.Encode())
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, resource.ErrBudgetExhausted) {
			return Listing{}, errScanSpecific
		}
		return Listing{Err: "web archive request failed: " + err.Error()}, nil
	}
	if resp.StatusCode != 200 {
		return Listing{Err: fmt.Sprintf("web archive returned HTTP %d", resp.StatusCode)}, nil
	}
	if resp.Truncated {
		return Listing{Err: "web archive response exceeded the size limit"}, nil
	}
	return Parse(resp.Body, t, e.opts.MaxURLs)
}

// Parse reads a CDX JSON response: a header row, then [original, timestamp,
// mimetype] rows. It keeps in-scope, well-formed URLs (normalized and
// deduplicated) and drops junk the archive picked up from broken links.
func Parse(body []byte, t discovery.Target, maxURLs int) (Listing, error) {
	var rows [][]string
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &rows); err != nil {
			return Listing{Err: "unexpected web archive response: " + err.Error()}, nil
		}
	}
	var l Listing
	seen := map[string]bool{}
	for i, row := range rows {
		if i == 0 && len(row) > 0 && row[0] == "original" {
			continue // header
		}
		if len(l.Entries) >= maxURLs {
			l.Truncated = true
			break
		}
		if len(row) < 3 || junk(row[0]) {
			continue
		}
		u, err := normalize.URL(row[0])
		if err != nil || !t.InScope(u.Hostname()) {
			continue
		}
		key := u.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		ts, err := time.Parse("20060102150405", row[1])
		if err != nil {
			continue
		}
		l.Entries = append(l.Entries, Entry{URL: key, FirstSeen: ts.Unix(), MIME: mime(row[2])})
	}
	return l, nil
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
