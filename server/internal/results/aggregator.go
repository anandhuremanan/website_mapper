package results

import (
	"sort"
	"sync"

	"websitemapper/internal/classify"
	"websitemapper/internal/discovery"
	"websitemapper/internal/normalize"
)

// maxDiscoveredFrom bounds how many referrers are kept per URL.
const maxDiscoveredFrom = 5

// Limits are safety ceilings on what one scan records. Zero means no limit.
type Limits struct {
	// MaxHosts bounds recorded hostnames, which are kept in memory.
	// Certificate logs can list tens of thousands of historical names.
	MaxHosts int
	// MaxURLs bounds recorded URLs across the whole scan, so that one scan
	// cannot fill the disk.
	MaxURLs int
}

// Aggregator collects findings concurrently and builds a Result. It also
// serves as the discovery.State engines read from.
//
// Hosts are kept in memory: they are few, small, and read constantly by the
// engines. URLs go to a URLStore, so a scan's memory does not grow with the
// number of URLs it finds. Every URL and host is stored once (keyed by its
// normalized form), with only the metadata shown to users; response bodies
// are never retained.
type Aggregator struct {
	target discovery.Target
	limits Limits

	mu    sync.Mutex
	store URLStore
	// err is the first store failure; after it nothing more is recorded.
	err          error
	hosts        map[string]*hostEntry
	hostNames    []string // by host ID - 1
	hostsOmitted int
	// urlCounts is maintained incrementally so progress polling never needs
	// a pass over the URLs.
	urlCounts Counts
	tech      techSet
}

var _ discovery.State = (*Aggregator)(nil)

type hostEntry struct {
	id int64
	// liveSeen / cachedSeen record whether the host was discovered by this
	// scan's own work and/or through reused cache entries.
	liveSeen, cachedSeen bool
	counts               HostCounts // recorded URLs
	omitted              int        // URLs seen after the recorded-URL limit was reached
	sources              set[discovery.Source]
	dns                  *discovery.DNSInfo
	http                 *discovery.HTTPInfo
	crawl                *discovery.CrawlInfo
	sitemap              *discovery.SitemapInfo
}

// NewAggregator creates an Aggregator scoped to target that records URLs in
// store.
func NewAggregator(target discovery.Target, limits Limits, store URLStore) *Aggregator {
	return &Aggregator{
		target: target,
		limits: limits,
		store:  store,
		hosts:  make(map[string]*hostEntry),
		tech:   techSet{},
	}
}

// Err returns the first failure of the URL store, if any. After a failure
// the Aggregator records nothing more.
func (a *Aggregator) Err() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.err
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

	// A URL on a host is evidence the host exists, discovered the same way.
	// SourceHost means the URL came from the host, not the other way round.
	src := f.Source
	if src == discovery.SourceHost {
		src = ""
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return
	}
	host := a.hostLocked(u.Hostname(), src, !f.CachedAt.IsZero())
	if host == nil {
		return // host not recorded: over the host limit
	}
	origin, path := splitURL(u)

	// At the recorded-URL limit, only URLs the scanner actually requested
	// are still recorded (bounded by the request budget); the rest are
	// counted.
	full := a.limits.MaxURLs > 0 && a.urlCounts.URLs >= a.limits.MaxURLs
	if !full || f.Response != nil || f.Error != "" {
		fresh := &URLRecord{HostID: host.id, Path: path, Origin: origin}
		fresh.merge(f)
		fresh.classify()
		inserted, err := a.store.Insert(fresh)
		if err != nil {
			a.err = err
			return
		}
		if inserted {
			a.count(host, fresh, 1)
			a.tech.addPath(u.Path)
			a.addResponseEvidence(f)
			return
		}
	}

	rec, ok, err := a.store.Get(host.id, path, origin)
	if err != nil {
		a.err = err
		return
	}
	if !ok {
		host.omitted++
		return
	}
	before := *rec
	changed, reclassify, took := rec.merge(f)
	if !changed {
		return
	}
	if reclassify {
		rec.classify()
	}
	if err := a.store.Update(rec); err != nil {
		a.err = err
		return
	}
	a.count(host, &before, -1)
	a.count(host, rec, 1)
	if took {
		a.addResponseEvidence(f)
	}
}

func (a *Aggregator) addResponseEvidence(f discovery.Finding) {
	if r := f.Response; r != nil {
		a.tech.addResponse(r.Server, r.PoweredBy, r.Generator)
	}
}

// count adds (d = 1) or removes (d = -1) a record from the scan's and its
// host's counters.
func (a *Aggregator) count(h *hostEntry, r *URLRecord, d int) {
	c := &a.urlCounts
	c.URLs += d
	h.counts.URLs += d
	switch r.Type {
	case classify.TypePage:
		c.Pages += d
		h.counts.Pages += d
	case classify.TypeAPI:
		c.APIs += d
		h.counts.APIs += d
	case classify.TypeAsset:
		c.Assets += d
		h.counts.Assets += d
		if r.AssetKind == classify.AssetJavaScript {
			c.JavaScript += d
		}
	}
	switch {
	case r.Responded:
		c.URLsFetched += d
		if r.CachedAt != nil {
			c.Cache.Pages += d
		}
	case r.Err != "":
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
		a.hostNames = append(a.hostNames, host)
		h = &hostEntry{id: int64(len(a.hostNames)), sources: set[discovery.Source]{}}
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
	if a.err != nil {
		return nil
	}
	var out []string
	a.err = a.store.PendingPages(func(hostID int64, path, origin string) {
		out = append(out, JoinURL(a.hostNames[hostID-1], origin, path))
	})
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

// countHost adds one host to c.
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

// Result flushes the URL store and builds a deterministic snapshot of
// everything except the URLs themselves, which stay in the store: hosts
// (apex first, then by how far they got, then by name) with their URL
// counts, and the technologies detected.
func (a *Aggregator) Result() Result {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err == nil {
		a.err = a.store.Flush()
	}

	tech := techSet{}
	for name, evs := range a.tech {
		tech[name] = append([]string(nil), evs...)
	}
	res := Result{Hosts: make([]Host, 0, len(a.hosts)), HostsOmitted: a.hostsOmitted}
	for name, h := range a.hosts {
		res.Hosts = append(res.Hosts, Host{
			ID:        h.id,
			Hostname:  name,
			State:     hostState(h),
			Sources:   h.sources.sorted(),
			FromCache: h.fromCache(),
			DNS:       h.dns,
			HTTP:      h.http,
			Crawl:     h.crawl,
			Sitemap:   h.sitemap,
			Counts:    h.counts,
			Omitted:   h.omitted,
			URLs:      []URL{},
		})
		if h.http != nil && h.http.Server != "" {
			tech.addResponse(h.http.Server, "", "")
		}
	}
	apex := a.target.Domain
	sort.Slice(res.Hosts, func(i, j int) bool { return hostLess(res.Hosts[i], res.Hosts[j], apex) })
	res.Technologies = tech.list()
	return res
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
