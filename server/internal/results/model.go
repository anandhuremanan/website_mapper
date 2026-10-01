// Package results merges raw findings from all discovery engines into a
// normalized, deduplicated scan result that preserves provenance.
//
// The result is hierarchical: the target domain has hosts, and each host
// has the URLs (routes, API-like endpoints, assets) discovered on it.
package results

import (
	"time"

	"websitemapper/internal/classify"
	"websitemapper/internal/discovery"
)

// Result is the complete, normalized output of a scan.
type Result struct {
	Hosts        []Host       `json:"hosts"`
	Technologies []Technology `json:"technologies"`
	// HostsOmitted counts hostnames discovered after the scan's host limit
	// was reached; they were not recorded.
	HostsOmitted int `json:"hostsOmitted,omitempty"`
}

// HostState summarizes how far a host got through the pipeline.
type HostState string

const (
	// HostDiscovered: the name was seen but does not (currently) resolve,
	// or has not been checked.
	HostDiscovered HostState = "discovered"
	// HostResolved: the name resolves but did not answer HTTP(S).
	HostResolved HostState = "resolved"
	// HostReachable: the host answered HTTP(S).
	HostReachable HostState = "reachable"
)

// Host is a hostname within the target's scope and everything found on it.
type Host struct {
	// ID is the number the scan's URL records use for this host.
	ID       int64     `json:"-"`
	Hostname string    `json:"hostname"`
	State    HostState `json:"state"`
	// Sources are how the host itself was discovered.
	Sources []discovery.Source `json:"sources"`
	// FromCache is true when every discovery of this host was reused from
	// the shared cache (e.g. an earlier scan's certificate lookup) rather
	// than made by this scan.
	FromCache bool                   `json:"fromCache,omitempty"`
	DNS       *discovery.DNSInfo     `json:"dns,omitempty"`
	HTTP      *discovery.HTTPInfo    `json:"http,omitempty"`
	Crawl     *discovery.CrawlInfo   `json:"crawl,omitempty"`
	Sitemap   *discovery.SitemapInfo `json:"sitemap,omitempty"`
	Counts    HostCounts             `json:"counts"`
	// Omitted counts URLs seen on this host but not recorded because the
	// scan's recorded-URL limit was reached.
	Omitted int `json:"urlsOmitted,omitempty"`
	// URLs are stored apart from the host (a host can have very many) and
	// filled in only when a result is read together with its URLs.
	URLs []URL `json:"urls"`
}

// HostCounts summarizes a host's URLs.
type HostCounts struct {
	URLs   int `json:"urls"`
	Pages  int `json:"pages"`
	APIs   int `json:"apis"`
	Assets int `json:"assets"`
}

// URL is a single deduplicated URL.
type URL struct {
	URL      string `json:"url"`
	Hostname string `json:"hostname"`
	Path     string `json:"path"`
	// Methods lists HTTP methods seen in context (e.g. a POST form).
	Methods     []string           `json:"methods,omitempty"`
	Status      int                `json:"status,omitempty"`
	ContentType string             `json:"contentType,omitempty"`
	Title       string             `json:"title,omitempty"`
	Redirect    string             `json:"redirect,omitempty"`
	Server      string             `json:"server,omitempty"`
	Type        classify.Type      `json:"type"`
	AssetKind   classify.AssetKind `json:"assetKind,omitempty"`
	// TypeEvidence explains why Type was chosen.
	TypeEvidence string             `json:"typeEvidence"`
	Sources      []discovery.Source `json:"sources"`
	// DiscoveredFrom lists up to a few pages/files that referenced this URL.
	DiscoveredFrom []string `json:"discoveredFrom,omitempty"`
	// Fetched is true when the scanner requested this URL itself.
	Fetched bool   `json:"fetched"`
	Error   string `json:"error,omitempty"`
	// State is discovered (referenced, never requested; assets stay here),
	// fetched (requested, no HTTP response) or verified (an HTTP response
	// was received).
	State URLState `json:"state"`
	// CachedAt is set when the response was reused from the page cache; it
	// is when the page was actually fetched.
	CachedAt *time.Time `json:"cachedAt,omitempty"`
	// Archived is what a web archive recorded about the URL (historical;
	// the URL may no longer exist).
	Archived *discovery.ArchiveInfo `json:"archived,omitempty"`
}

// URLState distinguishes discovering a URL from requesting it.
type URLState string

const (
	URLDiscovered URLState = "discovered"
	URLFetched    URLState = "fetched"
	URLVerified   URLState = "verified"
)

// Technology is a technology detected from observable evidence.
type Technology struct {
	Name     string   `json:"name"`
	Evidence []string `json:"evidence"`
}

// Counts summarizes a scan's progress and result. It is a fixed set of
// aggregate counters, so it stays small however large the scan grows.
type Counts struct {
	Hosts          int `json:"hosts"`
	HostsResolved  int `json:"hostsResolved"`
	HostsReachable int `json:"hostsReachable"`
	HostsCrawled   int `json:"hostsCrawled"`

	// Pipeline progress. "Pending" hosts are waiting for that stage.
	HostsUnresolved     int `json:"hostsUnresolved"`
	HostsResolvePending int `json:"hostsResolvePending"`
	HostsProbed         int `json:"hostsProbed"`
	HostsUnreachable    int `json:"hostsUnreachable"`
	HostsProbePending   int `json:"hostsProbePending"`
	HostsCrawlPending   int `json:"hostsCrawlPending"`

	URLs       int `json:"urls"`
	Pages      int `json:"pages"`
	APIs       int `json:"apis"`
	Assets     int `json:"assets"`
	JavaScript int `json:"javascript"`
	// URLsFetched counts verified URLs (an HTTP response was received);
	// URLsFailed counts requests that got no response.
	URLsFetched int `json:"urlsFetched"`
	URLsFailed  int `json:"urlsFailed"`

	Limits LimitCounts `json:"limits"`
	// Cache counts results reused from the shared cache instead of being
	// looked up, probed or fetched by this scan.
	Cache CacheCounts `json:"cache"`
}

// CacheCounts counts reused observations. Everything else was newly
// observed by this scan.
type CacheCounts struct {
	// Hosts discovered only through cached observations.
	Hosts int `json:"hosts"`
	// DNS answers, probe results and verified pages reused from the cache.
	DNS    int `json:"dns"`
	Probes int `json:"probes"`
	Pages  int `json:"pages"`
}

// LimitCounts counts work cut short by a per-scan budget. All zero means
// no budget affected the scan.
type LimitCounts struct {
	// HostsOmitted: hostnames not recorded (discovered-host limit).
	HostsOmitted int `json:"hostsOmitted"`
	// ResolveSkipped: hosts not resolved (resolve host limit).
	ResolveSkipped int `json:"resolveSkipped"`
	// ProbeSkipped: hosts not probed (probe host limit or request limit).
	ProbeSkipped int `json:"probeSkipped"`
	// CrawlSkipped: reachable hosts not crawled (crawl host limit or
	// request limit).
	CrawlSkipped int `json:"crawlSkipped"`
	// CrawlLimited: hosts whose crawl stopped at the per-host request limit.
	CrawlLimited int `json:"crawlLimited"`
	// URLsOmitted: URLs seen but not recorded (recorded-URL limits).
	URLsOmitted int `json:"urlsOmitted"`
}
