package htmlcrawl

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"websitemapper/internal/cache"
	"websitemapper/internal/discovery"
	"websitemapper/internal/resource"
)

// Page is what the crawler keeps about a fetched URL: response metadata and
// the references found in it. It never holds the response body.
type Page struct {
	Status      int
	ContentType string
	Server      string
	PoweredBy   string
	// Location is the redirect target of a 3xx response.
	Location  string
	Title     string
	Generator string
	// Refs are every link and resource referenced by an HTML page, in
	// document order, whether or not they are in scope: scope depends on the
	// scan, the page does not.
	Refs []PageRef
	// Err is set when the request failed (such pages are never stored).
	Err string
}

// PageRef is one reference found in a page.
type PageRef struct {
	URL    string
	Hint   discovery.Hint
	Method string
}

// Cache holds fetched-page metadata by URL, shared by all scans.
type Cache = cache.Cache[Page]

// NewCache creates a page cache.
func NewCache(maxBytes int64) *Cache {
	return cache.New[Page](cache.Options{Name: "page", MaxCost: maxBytes})
}

// errScanSpecific marks outcomes that belong to one scan (cancellation, an
// exhausted request budget) rather than to the page.
var errScanSpecific = errors.New("fetch outcome specific to this scan")

// pageResult is a page and whether it came from the cache.
type pageResult struct {
	Page
	cachedAt   time.Time // zero when fetched by this scan
	budgetStop bool      // the scan's request budget ran out
}

// getPage returns a page from the cache or fetches it. Only HTML bodies are
// read (by the fetcher) and parsed; bodies are dropped after extraction.
func (r *crawl) getPage(ctx context.Context, rawURL string) pageResult {
	var scanErr error
	p, ci, err := r.opts.Cache.Do(ctx, rawURL, func(ctx context.Context) (cache.Loaded[Page], error) {
		resp, err := r.fetcher.Get(ctx, rawURL)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, resource.ErrBudgetExhausted) {
				scanErr = err
				return cache.Loaded[Page]{}, errScanSpecific
			}
			// A failed request is shared with scans waiting for the same
			// URL, but never stored: failures are often transient.
			return cache.Loaded[Page]{Value: Page{Err: err.Error()}}, nil
		}
		p := Page{
			Status: resp.StatusCode, ContentType: resp.ContentType, Location: resp.Location,
			Server:    strings.TrimSpace(resp.Header.Get("Server")),
			PoweredBy: strings.TrimSpace(resp.Header.Get("X-Powered-By")),
		}
		if isHTML(resp) {
			pageURL, _ := url.Parse(rawURL)
			ex := extract(pageURL, resp.Body)
			p.Title, p.Generator = ex.Title, ex.Generator
			p.Refs = make([]PageRef, len(ex.Refs))
			for i, ref := range ex.Refs {
				p.Refs[i] = PageRef{URL: ref.URL.String(), Hint: ref.Hint, Method: ref.Method}
			}
		}
		return cache.Loaded[Page]{Value: p, TTL: r.opts.CacheTTL, Group: r.target.Domain, Cost: pageCost(rawURL, p)}, nil
	})
	if err != nil {
		return pageResult{Page: Page{Err: errString(scanErr)}, budgetStop: errors.Is(scanErr, resource.ErrBudgetExhausted)}
	}
	res := pageResult{Page: p}
	if ci.Hit || ci.Shared {
		res.cachedAt = ci.CreatedAt
	}
	return res
}

func errString(err error) string {
	if err == nil {
		return "stopped"
	}
	return err.Error()
}

// pageCost estimates a Page's memory, including its cache key.
func pageCost(key string, p Page) int64 {
	n := 192 + cache.StringCost(key, p.ContentType, p.Server, p.PoweredBy, p.Location, p.Title, p.Generator, p.Err)
	for _, ref := range p.Refs {
		// PageRef is three string headers (48 bytes) plus the URL bytes,
		// rounded up by the allocator.
		n += 64 + int64(len(ref.URL)+len(ref.Method))
	}
	return n
}

// references converts cached refs back to the extractor's form.
func (p Page) references() []reference {
	out := make([]reference, 0, len(p.Refs))
	for _, ref := range p.Refs {
		if u, err := url.Parse(ref.URL); err == nil {
			out = append(out, reference{URL: u, Hint: ref.Hint, Method: ref.Method})
		}
	}
	return out
}
