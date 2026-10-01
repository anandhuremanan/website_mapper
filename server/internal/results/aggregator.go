package results

import (
	"net/url"
	"sort"
	"sync"

	"websitemapper/internal/classify"
	"websitemapper/internal/discovery"
	"websitemapper/internal/normalize"
)

// maxDiscoveredFrom bounds how many referrers are kept per URL.
const maxDiscoveredFrom = 5

// Limits bound the memory one scan's results can use. Zero means no limit.
type Limits struct {
	// MaxHosts bounds recorded hostnames. Certificate logs can list tens of
	// thousands of historical names for large domains.
	MaxHosts int
	// MaxURLsPerHost bounds recorded URLs per host. Pages that list
	// thousands of links would otherwise dominate the result.
	MaxURLsPerHost int
	// MaxURLs bounds recorded URLs across the whole scan.
	MaxURLs int
}

// Aggregator collects findings concurrently and builds a Result. It also
// serves as the discovery.State engines read from.
//
// Every URL and host is stored once (keyed by its normalized form), with
// only the metadata shown to users; response bodies are never retained.
type Aggregator struct {
	target discovery.Target
	limits Limits

	mu           sync.Mutex
	urls         map[string]*urlEntry
	hosts        map[string]*hostEntry
	hostsOmitted int
	// urlCounts is maintained incrementally so progress polling does not
	// reclassify every URL.
	urlCounts Counts
}

var _ discovery.State = (*Aggregator)(nil)

type urlEntry struct {
	u       *url.URL
	sources set[discovery.Source]
	hints   set[discovery.Hint]
	methods set[string]
	from    []string
	resp    *discovery.Response
	err     string
	archive *discovery.ArchiveInfo
	cls     classify.Result // cached; recomputed when hints, resp or archive change
}

type hostEntry struct {
	// liveSeen / cachedSeen record whether the host was discovered by this
	// scan's own work and/or through reused cache entries.
	liveSeen, cachedSeen bool
	urls                 int // recorded URLs
	archived             int // of those, first recorded from a web archive
	omitted              int // URLs seen after the per-host limit was reached
	sources              set[discovery.Source]
	dns                  *discovery.DNSInfo
	http                 *discovery.HTTPInfo
	crawl                *discovery.CrawlInfo
	sitemap              *discovery.SitemapInfo
}

// NewAggregator creates an Aggregator scoped to target.
func NewAggregator(target discovery.Target, limits Limits) *Aggregator {
	return &Aggregator{
		target: target,
		limits: limits,
		urls:   make(map[string]*urlEntry),
		hosts:  make(map[string]*hostEntry),
	}
}

// Add merges a finding. Out-of-scope and unparseable findings are ignored.
// Add is safe for concurrent use and can be passed as a discovery.Emit.
func (a *Aggregator) Add(f discovery.Finding) {
	if f.URL == "" {
		a.addHost(f)
		return
	}
	u, err := normalize.URL(f.URL)
	if err != nil || !a.target.InScope(u.Hostname()) {
		return
	}
	key := u.String()

	// A URL on a host is evidence the host exists, discovered the same way.
	// SourceHost means the URL came from the host, not the other way round.
	src := f.Source
	if src == discovery.SourceHost {
		src = ""
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	host := a.hostLocked(u.Hostname(), src, !f.CachedAt.IsZero())
	if host == nil {
		return // host not recorded: over the host limit
	}
	e, ok := a.urls[key]
	if !ok {
		// Over a limit, only URLs the scanner actually requested are
		// recorded (bounded by the request budget); the rest are counted.
		full := (a.limits.MaxURLsPerHost > 0 && host.urls >= a.limits.MaxURLsPerHost) ||
			(a.limits.MaxURLs > 0 && len(a.urls) >= a.limits.MaxURLs)
		// Archived URLs are historical and arrive first (the archive is
		// queried before the site is contacted). They may fill at most half
		// of a host's slots, so routes found on the live site (sitemaps,
		// links) always have room.
		fromArchive := f.Source == discovery.SourceArchive
		if fromArchive && a.limits.MaxURLsPerHost > 0 && host.archived >= max(a.limits.MaxURLsPerHost/2, 1) {
			full = true
		}
		if full && f.Response == nil && f.Error == "" {
			host.omitted++
			return
		}
		host.urls++
		if fromArchive {
			host.archived++
		}
		e = &urlEntry{u: u, sources: set[discovery.Source]{}, hints: set[discovery.Hint]{}, methods: set[string]{}}
		a.urls[key] = e
	} else {
		a.urlCounts.subtract(e)
	}
	defer a.urlCounts.add(e)
	reclassify := !ok
	if f.Source != "" {
		e.sources.add(f.Source)
	}
	if f.Hint != "" {
		if _, had := e.hints[f.Hint]; !had {
			e.hints.add(f.Hint)
			reclassify = true
		}
	}
	if f.Method != "" {
		e.methods.add(f.Method)
	}
	if f.Archive != nil && (e.archive == nil || f.Archive.FirstSeen.Before(e.archive.FirstSeen)) {
		e.archive = f.Archive
		reclassify = true
	}
	if f.From != "" && len(e.from) < maxDiscoveredFrom && !contains(e.from, f.From) {
		e.from = append(e.from, f.From)
	}
	if f.Response != nil {
		// Prefer a successful response over an earlier redirect or error,
		// and this scan's own response over a cached one.
		if e.resp == nil || (e.resp.Status >= 300 && f.Response.Status < 300) ||
			(e.resp.CachedAt != nil && f.Response.CachedAt == nil) {
			e.resp = f.Response
			reclassify = true
		}
		e.err = ""
	} else if f.Error != "" && e.resp == nil {
		e.err = f.Error
	}
	if reclassify {
		e.cls = classifyEntry(e)
	}
}

// add and subtract maintain the URL part of Counts for one entry.
func (c *Counts) add(e *urlEntry)      { c.applyURL(e, 1) }
func (c *Counts) subtract(e *urlEntry) { c.applyURL(e, -1) }

func (c *Counts) applyURL(e *urlEntry, d int) {
	c.URLs += d
	switch e.cls.Type {
	case classify.TypePage:
		c.Pages += d
	case classify.TypeAPI:
		c.APIs += d
	case classify.TypeAsset:
		c.Assets += d
		if e.cls.AssetKind == classify.AssetJavaScript {
			c.JavaScript += d
		}
	}
	switch {
	case e.resp != nil:
		c.URLsFetched += d
		if e.resp.CachedAt != nil {
			c.Cache.Pages += d
		}
	case e.err != "":
		c.URLsFailed += d
	}
}

func (a *Aggregator) addHost(f discovery.Finding) {
	h, err := normalize.Host(f.Host)
	if err != nil || !a.target.InScope(h) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.hostLocked(h, f.Source, !f.CachedAt.IsZero())
	if e == nil {
		return
	}
	if f.DNS != nil {
		e.dns = f.DNS
	}
	if f.HTTP != nil {
		e.http = f.HTTP
	}
	if f.Crawl != nil {
		e.crawl = f.Crawl
	}
	if f.Sitemap != nil {
		e.sitemap = f.Sitemap
	}
}

// hostLocked returns the entry for host, creating it if the host limit
// allows; otherwise it returns nil and counts the host as omitted. The
// target's own hosts are always recorded.
func (a *Aggregator) hostLocked(host string, src discovery.Source, cached bool) *hostEntry {
	h, ok := a.hosts[host]
	if !ok {
		if a.limits.MaxHosts > 0 && len(a.hosts) >= a.limits.MaxHosts && src != discovery.SourceTarget {
			a.hostsOmitted++
			return nil
		}
		h = &hostEntry{sources: set[discovery.Source]{}}
		a.hosts[host] = h
	}
	if src != "" && src != discovery.SourceHost {
		h.sources.add(src)
		if cached {
			h.cachedSeen = true
		} else {
			h.liveSeen = true
		}
	}
	return h
}

// Hosts implements discovery.State.
func (a *Aggregator) Hosts() []discovery.HostView {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]discovery.HostView, 0, len(a.hosts))
	for name, h := range a.hosts {
		out = append(out, discovery.HostView{Name: name, Sources: h.sources.sorted(), DNS: h.dns, HTTP: h.http, Crawl: h.crawl, Sitemap: h.sitemap})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// PageURLs implements discovery.State: page URLs not fetched yet, found on
// the live site (links, sitemaps). URLs known only from a web archive are
// left out: crawling them would mean requesting possibly dead URLs just to
// verify them.
func (a *Aggregator) PageURLs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for key, e := range a.urls {
		if e.archiveOnly() {
			continue
		}
		if e.resp == nil && e.err == "" && e.cls.Type == classify.TypePage {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// Counts returns the current summary counts. URL counters are maintained
// incrementally; host counters take one pass over the (bounded) hosts.
func (a *Aggregator) Counts() Counts {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.urlCounts
	c.Limits.HostsOmitted = a.hostsOmitted
	for _, h := range a.hosts {
		countHost(&c, hostState(h), h.dns, h.http, h.crawl, h.omitted)
		if h.fromCache() {
			c.Cache.Hosts++
		}
	}
	return c
}

// fromCache reports whether the host was discovered only through the cache.
func (h *hostEntry) fromCache() bool { return h.cachedSeen && !h.liveSeen }

// countHost adds one host to c. Live and final counts share it, so the two
// always agree.
func countHost(c *Counts, st HostState, dns *discovery.DNSInfo, http *discovery.HTTPInfo, crawl *discovery.CrawlInfo, omitted int) {
	c.Hosts++
	if dns != nil && dns.CachedAt != nil {
		c.Cache.DNS++
	}
	if http != nil && http.CachedAt != nil {
		c.Cache.Probes++
	}
	switch st {
	case HostReachable:
		c.HostsReachable++
		c.HostsResolved++
	case HostResolved:
		c.HostsResolved++
	}
	switch {
	case dns == nil:
		c.HostsResolvePending++
	case dns.Skipped != "":
		c.Limits.ResolveSkipped++
	case !dns.Resolved:
		c.HostsUnresolved++
	}
	switch {
	case http == nil:
		if dns != nil && dns.Resolved {
			c.HostsProbePending++
		}
	case http.Skipped != "":
		if isBudgetSkip(http.Skipped) {
			c.Limits.ProbeSkipped++
		}
	default:
		c.HostsProbed++
		if !http.Reachable {
			c.HostsUnreachable++
		}
	}
	switch {
	case crawl == nil:
		if http != nil && http.Reachable {
			c.HostsCrawlPending++
		}
	case crawl.Skipped != "":
		if isBudgetSkip(crawl.Skipped) {
			c.Limits.CrawlSkipped++
		}
	default:
		c.HostsCrawled++
		if crawl.LimitReached {
			c.Limits.CrawlLimited++
		}
	}
	c.Limits.URLsOmitted += omitted
}

func isBudgetSkip(reason string) bool {
	return reason == discovery.SkipHostLimit || reason == discovery.SkipRequestLimit
}

// Counts summarizes a finished result.
func (r Result) Counts() Counts {
	c := Counts{}
	c.Limits.HostsOmitted = r.HostsOmitted
	for _, h := range r.Hosts {
		countHost(&c, h.State, h.DNS, h.HTTP, h.Crawl, h.Omitted)
		if h.FromCache {
			c.Cache.Hosts++
		}
		for _, u := range h.URLs {
			c.URLs++
			switch u.Type {
			case classify.TypePage:
				c.Pages++
			case classify.TypeAPI:
				c.APIs++
			case classify.TypeAsset:
				c.Assets++
				if u.AssetKind == classify.AssetJavaScript {
					c.JavaScript++
				}
			}
			switch {
			case u.Error != "":
				c.URLsFailed++
			case u.Fetched:
				c.URLsFetched++
				if u.CachedAt != nil {
					c.Cache.Pages++
				}
			}
		}
	}
	return c
}

// Result builds a deterministic snapshot: hosts (apex first, then by how far
// they got, then by name), each with its URLs sorted.
func (a *Aggregator) Result() Result {
	a.mu.Lock()
	defer a.mu.Unlock()

	byHost := map[string][]URL{}
	for key, e := range a.urls {
		u := buildURL(key, e)
		byHost[u.Hostname] = append(byHost[u.Hostname], u)
	}

	res := Result{Hosts: make([]Host, 0, len(a.hosts)), HostsOmitted: a.hostsOmitted}
	for name, h := range a.hosts {
		urls := byHost[name]
		sort.Slice(urls, func(i, j int) bool { return urls[i].URL < urls[j].URL })
		host := Host{
			Hostname:  name,
			State:     hostState(h),
			Sources:   h.sources.sorted(),
			FromCache: h.fromCache(),
			DNS:       h.dns,
			HTTP:      h.http,
			Crawl:     h.crawl,
			Sitemap:   h.sitemap,
			URLs:      urls,
			Omitted:   h.omitted,
		}
		if host.URLs == nil {
			host.URLs = []URL{}
		}
		for _, u := range urls {
			host.Counts.URLs++
			switch u.Type {
			case classify.TypePage:
				host.Counts.Pages++
			case classify.TypeAPI:
				host.Counts.APIs++
			case classify.TypeAsset:
				host.Counts.Assets++
			}
		}
		res.Hosts = append(res.Hosts, host)
	}
	apex := a.target.Domain
	sort.Slice(res.Hosts, func(i, j int) bool { return hostLess(res.Hosts[i], res.Hosts[j], apex) })

	res.Technologies = detectTechnologies(a.urls, a.hosts)
	return res
}

func buildURL(key string, e *urlEntry) URL {
	out := URL{
		URL:            key,
		Hostname:       e.u.Hostname(),
		Path:           e.u.EscapedPath(),
		Methods:        e.methods.sorted(),
		Sources:        e.sources.sorted(),
		DiscoveredFrom: append([]string(nil), e.from...),
		Error:          e.err,
		Fetched:        e.resp != nil || e.err != "",
		State:          URLDiscovered,
		Archived:       e.archive,
	}
	switch {
	case e.resp != nil:
		out.State = URLVerified
		out.CachedAt = e.resp.CachedAt
	case e.err != "":
		out.State = URLFetched
	}
	if e.resp != nil {
		out.Status = e.resp.Status
		out.ContentType = e.resp.ContentType
		out.Title = e.resp.Title
		out.Redirect = e.resp.Redirect
		out.Server = e.resp.Server
	}
	out.Type, out.AssetKind, out.TypeEvidence = e.cls.Type, e.cls.AssetKind, e.cls.Evidence
	return out
}

// archiveOnly reports whether the URL is known only from a web archive.
func (e *urlEntry) archiveOnly() bool {
	_, archived := e.sources[discovery.SourceArchive]
	return archived && len(e.sources) == 1
}

func classifyEntry(e *urlEntry) classify.Result {
	in := classify.Input{Path: e.u.Path, Hints: e.hints.sorted()}
	if e.archive != nil {
		in.ArchivedContentType = e.archive.ContentType
	}
	if e.resp != nil {
		in.Status, in.ContentType = e.resp.Status, e.resp.ContentType
	}
	return classify.Classify(in)
}

func hostState(h *hostEntry) HostState {
	switch {
	case h.http != nil && h.http.Reachable:
		return HostReachable
	case h.dns != nil && h.dns.Resolved:
		return HostResolved
	}
	return HostDiscovered
}

// hostLess orders the apex domain first, then www, then reachable hosts,
// then resolved ones, then the rest, alphabetically within each group.
func hostLess(a, b Host, apex string) bool {
	rank := func(h Host) int {
		switch {
		case h.Hostname == apex:
			return 0
		case h.Hostname == "www."+apex:
			return 1
		case h.State == HostReachable:
			return 2
		case h.State == HostResolved:
			return 3
		}
		return 4
	}
	if ra, rb := rank(a), rank(b); ra != rb {
		return ra < rb
	}
	return a.Hostname < b.Hostname
}

type set[T ~string] map[T]struct{}

func (s set[T]) add(v T) { s[v] = struct{}{} }

func (s set[T]) sorted() []T {
	out := make([]T, 0, len(s))
	for v := range s {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
