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

// Aggregator collects findings concurrently and builds a Result. It also
// serves as the discovery.State engines read from.
type Aggregator struct {
	target discovery.Target
	// maxURLsPerHost bounds how many URLs are recorded per host. Pages that
	// list thousands of links would otherwise make results unusably large.
	maxURLsPerHost int

	mu    sync.Mutex
	urls  map[string]*urlEntry
	hosts map[string]*hostEntry
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
	// robotsDisallowed: not fetched because robots.txt disallows it.
	robotsDisallowed bool
}

type hostEntry struct {
	urls    int // recorded URLs
	omitted int // URLs seen after the per-host limit was reached
	sources set[discovery.Source]
	dns     *discovery.DNSInfo
	http    *discovery.HTTPInfo
	crawl   *discovery.CrawlInfo
}

// NewAggregator creates an Aggregator scoped to target that records at most
// maxURLsPerHost URLs per host (0 means no limit).
func NewAggregator(target discovery.Target, maxURLsPerHost int) *Aggregator {
	return &Aggregator{
		target:         target,
		maxURLsPerHost: maxURLsPerHost,
		urls:           make(map[string]*urlEntry),
		hosts:          make(map[string]*hostEntry),
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
	// Stored URLs never contain credentials that appeared in links.
	u = normalize.RedactURL(u)
	key := u.String()

	// A URL on a host is evidence the host exists, discovered the same way.
	// SourceHost means the URL came from the host, not the other way round.
	src := f.Source
	if src == discovery.SourceHost {
		src = ""
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	host := a.hostLocked(u.Hostname(), src)
	e, ok := a.urls[key]
	if !ok {
		// Over the limit, only URLs the scanner actually requested are
		// recorded (bounded by the request budget); the rest are counted.
		if a.maxURLsPerHost > 0 && host.urls >= a.maxURLsPerHost && f.Response == nil && f.Error == "" {
			host.omitted++
			return
		}
		host.urls++
		e = &urlEntry{u: u, sources: set[discovery.Source]{}, hints: set[discovery.Hint]{}, methods: set[string]{}}
		a.urls[key] = e
	}
	if f.Source != "" {
		e.sources.add(f.Source)
	}
	if f.Hint != "" {
		e.hints.add(f.Hint)
	}
	if f.Method != "" {
		e.methods.add(f.Method)
	}
	if from := normalize.RedactString(f.From); from != "" && len(e.from) < maxDiscoveredFrom && !contains(e.from, from) {
		e.from = append(e.from, from)
	}
	if f.Response != nil {
		// Prefer a successful response over an earlier redirect or error.
		if e.resp == nil || (e.resp.Status >= 300 && f.Response.Status < 300) {
			e.resp = f.Response
		}
		e.err = ""
	} else if f.Error != "" && e.resp == nil {
		e.err = f.Error
	}
	if f.RobotsDisallowed {
		e.robotsDisallowed = true
	}
}

func (a *Aggregator) addHost(f discovery.Finding) {
	h, err := normalize.Host(f.Host)
	if err != nil || !a.target.InScope(h) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.hostLocked(h, f.Source)
	if f.DNS != nil {
		e.dns = f.DNS
	}
	if f.HTTP != nil {
		h := *f.HTTP
		h.URL, h.Redirect, h.FinalURL = normalize.RedactString(h.URL), normalize.RedactString(h.Redirect), normalize.RedactString(h.FinalURL)
		e.http = &h
	}
	if f.Crawl != nil {
		e.crawl = f.Crawl
	}
}

func (a *Aggregator) hostLocked(host string, src discovery.Source) *hostEntry {
	h, ok := a.hosts[host]
	if !ok {
		h = &hostEntry{sources: set[discovery.Source]{}}
		a.hosts[host] = h
	}
	if src != "" && src != discovery.SourceHost {
		h.sources.add(src)
	}
	return h
}

// Hosts implements discovery.State.
func (a *Aggregator) Hosts() []discovery.HostView {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]discovery.HostView, 0, len(a.hosts))
	for name, h := range a.hosts {
		out = append(out, discovery.HostView{Name: name, Sources: h.sources.sorted(), DNS: h.dns, HTTP: h.http, Crawl: h.crawl})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// PageURLs implements discovery.State: page URLs not fetched yet.
func (a *Aggregator) PageURLs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for key, e := range a.urls {
		if e.resp == nil && e.err == "" && !e.robotsDisallowed && classifyEntry(e).Type == classify.TypePage {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// Counts returns the current summary counts.
func (a *Aggregator) Counts() Counts {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := Counts{Hosts: len(a.hosts), URLs: len(a.urls)}
	for _, h := range a.hosts {
		switch hostState(h) {
		case HostReachable:
			c.HostsReachable++
			c.HostsResolved++
		case HostResolved:
			c.HostsResolved++
		}
		if h.crawl != nil && h.crawl.Skipped == "" {
			c.HostsCrawled++
		}
	}
	for _, e := range a.urls {
		countURL(&c, classifyEntry(e))
	}
	return c
}

func countURL(c *Counts, r classify.Result) {
	switch r.Type {
	case classify.TypePage:
		c.Pages++
	case classify.TypeAPI:
		c.APIs++
	case classify.TypeAsset:
		c.Assets++
		if r.AssetKind == classify.AssetJavaScript {
			c.JavaScript++
		}
	}
}

// Counts summarizes a finished result.
func (r Result) Counts() Counts {
	c := Counts{Hosts: len(r.Hosts)}
	for _, h := range r.Hosts {
		switch h.State {
		case HostReachable:
			c.HostsReachable++
			c.HostsResolved++
		case HostResolved:
			c.HostsResolved++
		}
		if h.Crawl != nil && h.Crawl.Skipped == "" {
			c.HostsCrawled++
		}
		c.URLs += h.Counts.URLs
		for _, u := range h.URLs {
			countURL(&c, classify.Result{Type: u.Type, AssetKind: u.AssetKind})
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

	res := Result{Hosts: make([]Host, 0, len(a.hosts))}
	for name, h := range a.hosts {
		urls := byHost[name]
		sort.Slice(urls, func(i, j int) bool { return urls[i].URL < urls[j].URL })
		host := Host{
			Hostname: name,
			State:    hostState(h),
			Sources:  h.sources.sorted(),
			DNS:      h.dns,
			HTTP:     h.http,
			Crawl:    h.crawl,
			URLs:     urls,
			Omitted:  h.omitted,
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
		URL:              key,
		Hostname:         e.u.Hostname(),
		Path:             e.u.EscapedPath(),
		Methods:          e.methods.sorted(),
		Sources:          e.sources.sorted(),
		DiscoveredFrom:   append([]string(nil), e.from...),
		Error:            e.err,
		Fetched:          e.resp != nil || e.err != "",
		RobotsDisallowed: e.robotsDisallowed && e.resp == nil,
	}
	if e.resp != nil {
		out.Status = e.resp.Status
		out.ContentType = e.resp.ContentType
		out.Title = e.resp.Title
		out.Redirect = normalize.RedactString(e.resp.Redirect)
		out.Server = e.resp.Server
	}
	c := classifyEntry(e)
	out.Type, out.AssetKind, out.TypeEvidence = c.Type, c.AssetKind, c.Evidence
	return out
}

func classifyEntry(e *urlEntry) classify.Result {
	in := classify.Input{Path: e.u.Path, Hints: e.hints.sorted()}
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
