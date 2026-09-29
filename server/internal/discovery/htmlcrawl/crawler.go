// Package htmlcrawl is a discovery engine that crawls reachable in-scope
// hosts and reports the links and resources their HTML pages reference.
package htmlcrawl

import (
	"context"
	"log/slog"
	"net/url"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"websitemapper/internal/classify"
	"websitemapper/internal/discovery"
	"websitemapper/internal/fetch"
	"websitemapper/internal/normalize"
	"websitemapper/internal/resource"
)

// maxRedirects bounds a single redirect chain.
const maxRedirects = 5

// Fetcher performs a single GET without following redirects.
type Fetcher interface {
	Get(ctx context.Context, url string) (*fetch.Response, error)
}

// Options bounds the crawl. Budgets are per host so one large host cannot
// starve the others.
type Options struct {
	// MaxHosts is the maximum number of hosts crawled per scan.
	MaxHosts int
	// MaxPagesPerHost is the maximum number of requests made to one host.
	MaxPagesPerHost int
	// MaxDepth is how many link hops to follow from a host's start page.
	MaxDepth int
	// Concurrency is the number of parallel requests within one host.
	Concurrency int
	// HostConcurrency is the number of hosts crawled in parallel.
	HostConcurrency int
	// Cache, when set, reuses fetched-page metadata across scans.
	Cache *Cache
	// CacheTTL is how long a fetched page's metadata is reused.
	CacheTTL time.Duration
}

// Crawler is the HTML discovery engine.
type Crawler struct {
	fetcher Fetcher
	opts    Options
	log     *slog.Logger
}

// New creates a Crawler.
func New(f Fetcher, opts Options, log *slog.Logger) *Crawler {
	opts.Concurrency = max(opts.Concurrency, 1)
	opts.HostConcurrency = max(opts.HostConcurrency, 1)
	opts.MaxPagesPerHost = max(opts.MaxPagesPerHost, 1)
	return &Crawler{fetcher: f, opts: opts, log: log}
}

func (c *Crawler) Name() string { return "html" }

type item struct {
	url    string
	source discovery.Source
	hint   discovery.Hint
	from   string
}

// Discover crawls every reachable host that has not been crawled yet, up to
// MaxHosts per scan, and reports a CrawlInfo for each host it considered.
func (c *Crawler) Discover(ctx context.Context, in discovery.Input, emit discovery.Emit) error {
	hosts := in.State.Hosts()
	discovery.PrioritizeHosts(hosts, in.Target)

	done := 0
	var todo []discovery.HostView
	for _, h := range hosts {
		switch {
		case h.Crawl != nil:
			if h.Crawl.Skipped == "" {
				done++
			}
		case !h.Reachable():
		case redirectsAway(in.Target, h):
			// Nothing to crawl on this host: record what the probe saw
			// without spending a request or a crawl slot.
			recordRedirectOnly(in.Target, h, emit)
		default:
			todo = append(todo, h)
		}
	}
	// Hosts that serve content come before those answering 3xx/4xx/5xx.
	sort.SliceStable(todo, func(i, j int) bool { return serves(todo[i]) && !serves(todo[j]) })
	budget := max(c.opts.MaxHosts-done, 0)
	if len(todo) > budget {
		for _, h := range todo[budget:] {
			emit(discovery.Finding{Host: h.Name, Crawl: &discovery.CrawlInfo{Skipped: discovery.SkipHostLimit}})
		}
		todo = todo[:budget]
	}

	seeds := map[string][]string{}
	for _, u := range in.State.PageURLs() {
		h := discovery.HostOf(u)
		seeds[h] = append(seeds[h], u)
	}

	acct := resource.FromContext(ctx)
	discovery.ForEachLimited(todo, c.opts.HostConcurrency, func(h discovery.HostView) {
		if ctx.Err() != nil {
			return
		}
		if acct.Exhausted() {
			emit(discovery.Finding{Host: h.Name, Crawl: &discovery.CrawlInfo{Skipped: discovery.SkipRequestLimit}})
			return
		}
		info := c.crawlHost(ctx, in.Target, h, seeds[h.Name], emit)
		if ctx.Err() == nil {
			emit(discovery.Finding{Host: h.Name, Crawl: info})
		}
	})
	return ctx.Err()
}

// redirectsAway reports whether a host's root redirects to a different host.
// The entered host is always crawled, since the entered path may differ.
func redirectsAway(t discovery.Target, h discovery.HostView) bool {
	return h.Name != discovery.HostOf(t.StartURL) && h.HTTP.Redirect != "" &&
		discovery.HostOf(h.HTTP.Redirect) != h.Name
}

func recordRedirectOnly(t discovery.Target, h discovery.HostView, emit discovery.Emit) {
	start := startURL(t, h)
	emit(discovery.Finding{URL: start.url, Source: start.source, Hint: discovery.HintEntry, Response: &discovery.Response{
		Status: h.HTTP.Status, Redirect: h.HTTP.Redirect, Server: h.HTTP.Server, ContentType: h.HTTP.ContentType,
	}})
	emit(discovery.Finding{URL: h.HTTP.Redirect, Source: discovery.SourceRedirect, Hint: discovery.HintRedirect, From: start.url})
	emit(discovery.Finding{Host: h.Name, Crawl: &discovery.CrawlInfo{Skipped: "root redirects to another host"}})
}

// serves reports whether the host's root ended in a 2xx response.
func serves(h discovery.HostView) bool {
	st := h.HTTP.FinalStatus
	if st == 0 {
		st = h.HTTP.Status
	}
	return st >= 200 && st < 300
}

// startURL is where a host's crawl begins: the URL the user entered for the
// target host, otherwise the root URL that answered the probe.
func startURL(t discovery.Target, h discovery.HostView) item {
	root := h.HTTP.URL
	if h.Name == discovery.HostOf(t.StartURL) {
		// Keep the entered path, with the scheme that actually answered.
		if u, err := url.Parse(t.StartURL); err == nil && h.HTTP.Scheme != "" {
			u.Scheme = h.HTTP.Scheme
			return item{url: u.String(), source: discovery.SourceTarget, hint: discovery.HintEntry}
		}
	}
	return item{url: root, source: discovery.SourceHost, hint: discovery.HintEntry}
}

func (c *Crawler) crawlHost(ctx context.Context, t discovery.Target, h discovery.HostView, seeds []string, emit discovery.Emit) *discovery.CrawlInfo {
	run := &crawl{
		Crawler: c,
		target:  t,
		host:    h.Name,
		emit:    emit,
		seen:    make(map[string]bool),
		budget:  int64(c.opts.MaxPagesPerHost),
	}
	var frontier []item
	start := startURL(t, h)
	if run.markSeen(start.url) {
		frontier = append(frontier, start)
	}
	for _, s := range seeds {
		// Seeds were already reported, with provenance, by the engine that found them.
		if run.markSeen(s) {
			frontier = append(frontier, item{url: s, hint: discovery.HintLink})
		}
	}
	for depth := 0; len(frontier) > 0 && ctx.Err() == nil && !run.stopped.Load(); depth++ {
		frontier = run.level(ctx, frontier, depth < c.opts.MaxDepth)
		// Backpressure: links beyond the host's remaining request budget
		// can never be fetched, so they are not queued (they were already
		// reported as discovered URLs).
		if remaining := int(atomic.LoadInt64(&run.budget)); len(frontier) > remaining {
			frontier = frontier[:max(remaining, 0)]
			run.exhausted.Store(true)
		}
	}
	c.log.Debug("host crawled", "host", h.Name, "requests", run.fetched.Load())
	return &discovery.CrawlInfo{Requests: int(run.fetched.Load()), FromCache: int(run.cached.Load()), LimitReached: run.exhausted.Load()}
}

// crawl is the state of one host's crawl.
type crawl struct {
	*Crawler
	target discovery.Target
	host   string
	emit   discovery.Emit

	mu   sync.Mutex
	seen map[string]bool

	budget    int64
	fetched   atomic.Int64 // requests made
	cached    atomic.Int64 // pages reused from the cache
	exhausted atomic.Bool
	// stopped is set when the scan's request budget runs out.
	stopped atomic.Bool
}

// markSeen records a URL and reports whether it was new.
func (r *crawl) markSeen(raw string) bool {
	key, err := normalize.String(raw)
	if err != nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen[key] {
		return false
	}
	r.seen[key] = true
	return true
}

func (r *crawl) takeBudget() bool {
	if atomic.AddInt64(&r.budget, -1) >= 0 {
		return true
	}
	r.exhausted.Store(true)
	return false
}

// level fetches all items of one depth with bounded concurrency and returns
// the next frontier (only when follow is true).
func (r *crawl) level(ctx context.Context, items []item, follow bool) []item {
	var (
		mu   sync.Mutex
		next []item
		wg   sync.WaitGroup
		sem  = make(chan struct{}, r.opts.Concurrency)
	)
	for _, it := range items {
		if r.stopped.Load() {
			break
		}
		select {
		case <-ctx.Done():
			wg.Wait()
			return nil
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(it item) {
			defer func() { <-sem; wg.Done() }()
			links := r.visit(ctx, it)
			if !follow {
				return
			}
			mu.Lock()
			next = append(next, links...)
			mu.Unlock()
		}(it)
	}
	wg.Wait()
	return next
}

// visit fetches one page (following in-scope redirects) and returns the
// crawlable links it contains. Pages may come from the shared page cache;
// they count against the host's page budget either way, so a cached crawl
// covers the same pages, but cached pages make no request.
func (r *crawl) visit(ctx context.Context, it item) []item {
	cur, source, hint, from := it.url, it.source, it.hint, it.from
	for hop := 0; hop <= maxRedirects; hop++ {
		if !r.takeBudget() {
			return nil
		}
		p := r.getPage(ctx, cur)
		switch {
		case p.budgetStop:
			r.exhausted.Store(true)
			r.stopped.Store(true)
			return nil
		case ctx.Err() != nil:
			return nil
		case p.cachedAt.IsZero():
			r.fetched.Add(1)
		default:
			r.cached.Add(1)
		}
		if p.Err != "" {
			r.log.Debug("fetch failed", "url", cur, "error", p.Err)
			r.emit(discovery.Finding{URL: cur, Source: source, Hint: hint, From: from, Error: p.Err, CachedAt: p.cachedAt})
			return nil
		}

		// A page from the cache was not contacted by this scan, so neither
		// the page nor its host counts as newly observed.
		f := discovery.Finding{URL: cur, Source: source, Hint: hint, From: from, Response: p.response(), CachedAt: p.cachedAt}
		if p.Status >= 300 && p.Status < 400 && p.Location != "" {
			f.Response.Redirect = p.Location
			r.emit(f)
			r.emit(discovery.Finding{URL: p.Location, Source: discovery.SourceRedirect, Hint: discovery.HintRedirect, From: cur, CachedAt: p.cachedAt})
			// Redirects to other hosts are recorded (registering the host)
			// but followed only within this host's own crawl.
			if discovery.HostOf(p.Location) != r.host {
				return nil
			}
			// A redirect to the same canonical URL (e.g. adding a trailing
			// slash) is followed even though that URL is already "seen".
			same := sameKey(cur, p.Location)
			if !same && !r.markSeen(p.Location) {
				return nil
			}
			cur, source, hint, from = p.Location, discovery.SourceRedirect, discovery.HintRedirect, cur
			continue
		}
		links := r.references(cur, p.references(), p.cachedAt)
		r.emit(f)
		return links
	}
	return nil
}

// response is the metadata reported for a page.
func (p pageResult) response() *discovery.Response {
	resp := &discovery.Response{
		Status: p.Status, ContentType: p.ContentType, Server: p.Server, PoweredBy: p.PoweredBy,
		Title: p.Title, Generator: p.Generator,
	}
	if !p.cachedAt.IsZero() {
		at := p.cachedAt
		resp.CachedAt = &at
	}
	return resp
}

// references emits every in-scope reference and returns those on this host
// worth crawling. References to other in-scope hosts register those hosts.
// cachedAt is set when the page came from the cache.
func (r *crawl) references(pageURL string, refs []reference, cachedAt time.Time) []item {
	var links []item
	for _, ref := range refs {
		u := ref.URL.String()
		if !r.target.InScope(ref.URL.Hostname()) {
			continue
		}
		r.emit(discovery.Finding{URL: u, Source: discovery.SourceHTML, Hint: ref.Hint, Method: ref.Method, From: pageURL, CachedAt: cachedAt})
		if ref.URL.Hostname() == r.host && crawlable(ref) && r.markSeen(u) {
			links = append(links, item{url: u, source: discovery.SourceHTML, hint: ref.Hint, from: pageURL})
		}
	}
	return links
}

// crawlable reports whether a reference should be fetched as a page.
func crawlable(ref reference) bool {
	if ref.Hint != discovery.HintLink && ref.Hint != discovery.HintFrame {
		return false
	}
	if _, isAsset := classify.AssetKindForPath(ref.URL.Path); isAsset {
		return false
	}
	return true
}

func sameKey(a, b string) bool {
	ka, errA := normalize.String(a)
	kb, errB := normalize.String(b)
	return errA == nil && errB == nil && ka == kb
}

func isHTML(resp *fetch.Response) bool {
	return resp.StatusCode >= 200 && resp.StatusCode < 300 &&
		(resp.ContentType == "text/html" || resp.ContentType == "application/xhtml+xml")
}
