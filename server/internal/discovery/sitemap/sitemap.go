// Package sitemap is a discovery engine that reads each reachable host's
// robots.txt and XML sitemaps, which list a site's pages in a few requests
// instead of a crawl.
//
// Only "Sitemap:" lines of robots.txt are used. Disallow rules describe what
// crawlers should avoid; they are never treated as evidence that a route
// exists. Listed URLs are reported as discovered and are not requested.
package sitemap

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/url"
	"strings"
	"time"

	"websitemapper/internal/cache"
	"websitemapper/internal/discovery"
	"websitemapper/internal/fetch"
	"websitemapper/internal/normalize"
	"websitemapper/internal/resource"
)

const (
	// maxRobotsSitemaps bounds the Sitemap: lines taken from one robots.txt.
	maxRobotsSitemaps = 50
	// maxUnzipped bounds a decompressed .xml.gz sitemap.
	maxUnzipped = 16 << 20
)

// Fetcher performs a single GET.
type Fetcher interface {
	Get(ctx context.Context, url string) (*fetch.Response, error)
}

// File is what the engine keeps about a fetched robots.txt or sitemap: its
// status and the URLs it lists, never its body.
type File struct {
	Status      int
	ContentType string
	// Sitemaps are sitemap URLs listed (robots.txt, sitemap indexes).
	Sitemaps []string
	// URLs are page URLs listed (sitemaps), up to the engine's limit.
	URLs []string
	// Err is set when the request failed (never stored).
	Err string
}

// Cache holds parsed robots.txt and sitemap files by URL.
type Cache = cache.Cache[File]

// NewCache creates a sitemap cache.
func NewCache(maxBytes int64) *Cache {
	return cache.New[File](cache.Options{Name: "sitemap", MaxCost: maxBytes})
}

// Options bounds the engine.
type Options struct {
	// MaxHosts bounds hosts whose sitemaps are read per scan.
	MaxHosts int
	// MaxFilesPerHost bounds sitemap files read per host.
	MaxFilesPerHost int
	// MaxURLsPerHost bounds page URLs taken from one host's sitemaps.
	MaxURLsPerHost int
	// Concurrency is how many hosts are processed at once.
	Concurrency int
	// Cache and TTL reuse parsed files across scans.
	Cache *Cache
	TTL   time.Duration
}

// Engine reads robots.txt and sitemaps.
type Engine struct {
	fetcher Fetcher
	opts    Options
}

// New creates an Engine.
func New(f Fetcher, opts Options) *Engine {
	opts.MaxFilesPerHost = max(opts.MaxFilesPerHost, 1)
	opts.MaxURLsPerHost = max(opts.MaxURLsPerHost, 1)
	opts.Concurrency = max(opts.Concurrency, 1)
	return &Engine{fetcher: f, opts: opts}
}

func (e *Engine) Name() string { return "sitemap" }

// Discover reads sitemaps of every reachable host not processed yet, up to
// MaxHosts per scan.
func (e *Engine) Discover(ctx context.Context, in discovery.Input, emit discovery.Emit) error {
	hosts := in.State.Hosts()
	discovery.PrioritizeHosts(hosts, in.Target)
	done := 0
	var todo []discovery.HostView
	for _, h := range hosts {
		switch {
		case h.Sitemap != nil:
			if h.Sitemap.Skipped == "" {
				done++
			}
		case h.Reachable():
			todo = append(todo, h)
		}
	}
	budget := max(e.opts.MaxHosts-done, 0)
	if len(todo) > budget {
		for _, h := range todo[budget:] {
			emit(discovery.Finding{Host: h.Name, Sitemap: &discovery.SitemapInfo{Skipped: discovery.SkipHostLimit}})
		}
		todo = todo[:budget]
	}
	acct := resource.FromContext(ctx)
	discovery.ForEachLimited(todo, e.opts.Concurrency, func(h discovery.HostView) {
		if ctx.Err() != nil {
			return
		}
		if acct.Exhausted() {
			emit(discovery.Finding{Host: h.Name, Sitemap: &discovery.SitemapInfo{Skipped: discovery.SkipRequestLimit}})
			return
		}
		if info := e.host(ctx, in.Target, h, emit); info != nil {
			emit(discovery.Finding{Host: h.Name, Sitemap: info})
		}
	})
	return ctx.Err()
}

type queued struct {
	url    string
	from   string
	source discovery.Source
}

// host processes one host. It returns nil if the scan stopped.
func (e *Engine) host(ctx context.Context, t discovery.Target, h discovery.HostView, emit discovery.Emit) *discovery.SitemapInfo {
	scheme := h.HTTP.Scheme
	if scheme == "" {
		scheme = "https"
	}
	base := scheme + "://" + h.Name
	robotsURL := base + "/robots.txt"
	info := &discovery.SitemapInfo{}

	robots, stopped := e.get(ctx, robotsURL, t.Domain, false)
	if stopped {
		return nil
	}
	info.RobotsStatus = robots.Status
	var queue []queued
	if ok(robots) {
		emit(fileFinding(robotsURL, discovery.SourceRobots, "", robots))
		for _, s := range robots.Sitemaps {
			queue = append(queue, queued{url: s, from: robotsURL, source: discovery.SourceRobots})
		}
	}
	if len(queue) == 0 {
		// The conventional location, tried only when robots.txt lists none.
		queue = append(queue, queued{url: base + "/sitemap.xml", source: discovery.SourceSitemap})
	}

	seen := map[string]bool{}
	for len(queue) > 0 && ctx.Err() == nil {
		q := queue[0]
		queue = queue[1:]
		if seen[q.url] || !t.URLInScope(q.url) {
			continue // never fetch sitemaps hosted on other domains
		}
		seen[q.url] = true
		if info.Files >= e.opts.MaxFilesPerHost {
			info.LimitReached = true
			break
		}
		f, stopped := e.get(ctx, q.url, t.Domain, true)
		if stopped {
			return nil
		}
		if !ok(f) {
			continue
		}
		info.Files++
		emit(fileFinding(q.url, q.source, q.from, f))
		for _, s := range f.Sitemaps {
			queue = append(queue, queued{url: s, from: q.url, source: discovery.SourceSitemap})
		}
		for _, u := range f.URLs {
			if info.URLs >= e.opts.MaxURLsPerHost {
				info.LimitReached = true
				break
			}
			if !t.URLInScope(u) {
				continue
			}
			info.URLs++
			emit(discovery.Finding{URL: u, Source: discovery.SourceSitemap, Hint: discovery.HintSitemap, From: q.url, CachedAt: f.cachedAt()})
		}
	}
	return info
}

func ok(f cachedFile) bool { return f.Err == "" && f.Status >= 200 && f.Status < 300 }

// fileFinding reports a robots.txt or sitemap file the engine fetched.
func fileFinding(u string, src discovery.Source, from string, f cachedFile) discovery.Finding {
	resp := &discovery.Response{Status: f.Status, ContentType: f.ContentType}
	if at := f.cachedAt(); !at.IsZero() {
		resp.CachedAt = &at
	}
	return discovery.Finding{URL: u, Source: src, From: from, Response: resp, CachedAt: f.cachedAt()}
}

// cachedFile is a File and when it was originally fetched, if reused.
type cachedFile struct {
	File
	at time.Time
}

func (f cachedFile) cachedAt() time.Time { return f.at }

// errStopped marks outcomes that belong to the scan, never cached.
var errStopped = errors.New("sitemap request stopped")

// get returns a parsed file from the cache or fetches it. stopped is true
// when the scan was cancelled or its budget ran out.
func (e *Engine) get(ctx context.Context, rawURL, domain string, isSitemap bool) (cachedFile, bool) {
	f, ci, err := e.opts.Cache.Do(ctx, rawURL, func(ctx context.Context) (cache.Loaded[File], error) {
		resp, err := e.fetcher.Get(ctx, rawURL)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, resource.ErrBudgetExhausted) {
				return cache.Loaded[File]{}, errStopped
			}
			return cache.Loaded[File]{Value: File{Err: err.Error()}}, nil // shared, not stored
		}
		f := File{Status: resp.StatusCode, ContentType: resp.ContentType}
		if f.Status >= 200 && f.Status < 300 {
			if isSitemap {
				// One past the limit, so the engine can tell that more exist.
				f.Sitemaps, f.URLs = ParseSitemap(resp.Body, e.opts.MaxURLsPerHost+1)
			} else {
				f.Sitemaps = ParseRobots(resp.Body, rawURL)
			}
		}
		return cache.Loaded[File]{Value: f, TTL: e.opts.TTL, Group: domain, Cost: fileCost(rawURL, f)}, nil
	})
	if err != nil {
		return cachedFile{}, true
	}
	cf := cachedFile{File: f}
	if ci.Hit || ci.Shared {
		cf.at = ci.CreatedAt
	}
	return cf, false
}

func fileCost(key string, f File) int64 {
	n := 96 + cache.StringCost(key, f.ContentType, f.Err) + cache.StringCost(f.Sitemaps...)
	for _, u := range f.URLs {
		n += 16 + int64(len(u))
	}
	return n
}

// ParseRobots returns the sitemap URLs listed in a robots.txt, resolved
// against its URL. Allow/Disallow and other rules are ignored.
func ParseRobots(body []byte, robotsURL string) []string {
	base, err := url.Parse(robotsURL)
	if err != nil {
		return nil
	}
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() && len(out) < maxRobotsSitemaps {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		key, value, found := strings.Cut(line, ":")
		if !found || !strings.EqualFold(strings.TrimSpace(key), "sitemap") {
			continue
		}
		if u, err := normalize.Resolve(base, strings.TrimSpace(value)); err == nil {
			out = append(out, u.String())
		}
	}
	return out
}

// ParseSitemap returns the child sitemaps (<sitemapindex>) and page URLs
// (<urlset>) of an XML sitemap, or the URLs of a plain-text sitemap (one
// per line). Gzipped sitemaps are decompressed. At most maxURLs page URLs
// are returned.
func ParseSitemap(body []byte, maxURLs int) (sitemaps, urls []string) {
	if len(body) > 2 && body[0] == 0x1f && body[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, nil
		}
		unzipped, err := io.ReadAll(io.LimitReader(zr, maxUnzipped))
		if err != nil {
			return nil, nil
		}
		body = unzipped
	}
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 && trimmed[0] != '<' {
		return nil, textSitemap(trimmed, maxURLs)
	}

	dec := xml.NewDecoder(bytes.NewReader(body))
	dec.Strict = false
	var inSitemap, inURL, inLoc bool
	var loc strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			return sitemaps, urls
		}
		switch el := tok.(type) {
		case xml.StartElement:
			switch el.Name.Local {
			case "sitemap":
				inSitemap = true
			case "url":
				inURL = true
			case "loc":
				inLoc = true
				loc.Reset()
			}
		case xml.CharData:
			if inLoc {
				loc.Write(el)
			}
		case xml.EndElement:
			switch el.Name.Local {
			case "sitemap":
				inSitemap = false
			case "url":
				inURL = false
			case "loc":
				inLoc = false
				u, err := normalize.String(strings.TrimSpace(loc.String()))
				if err != nil {
					continue
				}
				switch {
				case inSitemap && len(sitemaps) < maxRobotsSitemaps:
					sitemaps = append(sitemaps, u)
				case inURL:
					if len(urls) >= maxURLs {
						return sitemaps, urls
					}
					urls = append(urls, u)
				}
			}
		}
	}
}

func textSitemap(body []byte, maxURLs int) []string {
	var urls []string
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() && len(urls) < maxURLs {
		if u, err := normalize.String(strings.TrimSpace(sc.Text())); err == nil {
			urls = append(urls, u)
		}
	}
	return urls
}
