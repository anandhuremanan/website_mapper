// Package discovery defines the contract shared by all discovery engines.
//
// An engine observes something public about a target (certificate logs,
// DNS, HTTP responses, HTML links, ...) and reports raw Findings. Engines do
// not deduplicate or classify; the results package merges findings from
// every engine while preserving where each one came from.
//
// A scan distinguishes three things:
//
//   - the target domain, which defines scope
//   - hosts: hostnames within scope, discovered by any engine
//   - URLs: routes and resources, each belonging to one host
package discovery

import (
	"context"
	"time"
)

// Source identifies how a URL or host was discovered. Sources are shown to
// users as provenance, so every discovery carries one.
type Source string

const (
	SourceTarget     Source = "target" // the domain or URL the user entered
	SourceHTML       Source = "html"
	SourceJavaScript Source = "javascript"
	SourceSitemap    Source = "sitemap"
	SourceRobots     Source = "robots"
	SourceCT         Source = "certificate-transparency"
	SourceRedirect   Source = "redirect"
	// SourceArchive: listed by a public web archive (the Wayback Machine).
	// The scanner did not request the URL; it may no longer exist.
	SourceArchive Source = "archive"
	// SourceHost marks a host's root URL, requested because the host itself
	// was discovered. It is URL provenance only, never host provenance.
	SourceHost Source = "host"
	// SourceDataset: listed by a public database of hostnames (collected
	// from DNS and other public data), rather than seen on a certificate.
	SourceDataset Source = "dataset"
)

// Sources lists every Source. Stored results record a URL's sources by
// position in this list, so only append to it: never reorder or remove.
var Sources = []Source{
	SourceTarget, SourceHTML, SourceJavaScript, SourceSitemap, SourceRobots,
	SourceCT, SourceRedirect, SourceArchive, SourceHost, SourceDataset,
}

// Hint describes how a URL was referenced, which helps classify it before
// (or without) fetching it.
type Hint string

const (
	HintLink       Hint = "link"       // <a href>, <area href>, canonical/alternate
	HintFrame      Hint = "frame"      // <iframe src>
	HintForm       Hint = "form"       // <form action>
	HintScript     Hint = "script"     // <script src>, modulepreload
	HintStylesheet Hint = "stylesheet" // <link rel=stylesheet>
	HintImage      Hint = "image"      // <img>, icons
	HintFont       Hint = "font"       // preloaded fonts
	HintMedia      Hint = "media"      // <video>, <audio>, <source>
	HintManifest   Hint = "manifest"   // web app manifest
	HintRedirect   Hint = "redirect"   // Location header target
	HintEntry      Hint = "entry"      // a URL a crawl starts from
	HintSitemap    Hint = "sitemap"    // a <loc> in a sitemap
	HintArchive    Hint = "archive"    // listed by a web archive
)

// Hints lists every Hint. Like Sources, it is append-only: stored results
// record hints by position.
var Hints = []Hint{
	HintLink, HintFrame, HintForm, HintScript, HintStylesheet, HintImage, HintFont,
	HintMedia, HintManifest, HintRedirect, HintEntry, HintSitemap, HintArchive,
}

// Finding is a single raw observation reported by an engine. It is either
// about a URL (URL set) or about a host (URL empty, Host set).
type Finding struct {
	// URL is an absolute http(s) URL.
	URL string
	// Host is a bare hostname, for host findings.
	Host string

	// Source is how the URL or host was discovered. Observations of an
	// already-known host (DNS, HTTP, crawl results) leave it empty.
	Source Source
	Hint   Hint
	// Method is the HTTP method if known from context (e.g. a form's method).
	Method string
	// From is the URL of the page or file the reference was found in.
	From string

	// Response is set when the engine fetched URL itself.
	Response *Response
	// Error describes a failed fetch of URL, if any.
	Error string

	// Host observations. Each replaces any earlier value for the host.
	DNS     *DNSInfo
	HTTP    *HTTPInfo
	Crawl   *CrawlInfo
	Sitemap *SitemapInfo

	// Archive is set for URLs listed by a web archive.
	Archive *ArchiveInfo

	// CachedAt is set when the discovery was reused from the shared cache:
	// it is when the original observation was made. Zero means it was made
	// by this scan.
	CachedAt time.Time
}

// ArchiveInfo is what a web archive recorded about a URL. It is historical:
// the scanner did not request the URL.
type ArchiveInfo struct {
	// FirstSeen is when the archive first captured the URL successfully.
	FirstSeen time.Time `json:"firstSeen"`
	// ContentType is the content type the archive captured.
	ContentType string `json:"contentType,omitempty"`
}

// Response is HTTP metadata observed while fetching a URL.
type Response struct {
	Status      int
	ContentType string
	Title       string
	Redirect    string
	Server      string
	PoweredBy   string
	// Generator is the content of <meta name="generator">, if present.
	Generator string
	// CachedAt is set when this response was reused from the page cache
	// instead of being requested again; it is when it was fetched.
	CachedAt *time.Time
}

// DNSInfo is the result of resolving a host. Values are immutable once emitted.
type DNSInfo struct {
	Resolved  bool     `json:"resolved"`
	Addresses []string `json:"addresses,omitempty"`
	CNAME     string   `json:"cname,omitempty"`
	// NonPublic is true when every address is private, loopback or otherwise
	// non-public. Such hosts are never contacted.
	NonPublic bool   `json:"nonPublic,omitempty"`
	Error     string `json:"error,omitempty"`
	// Skipped explains why resolution was not attempted.
	Skipped string `json:"skipped,omitempty"`
	// CachedAt is set when the answer came from the DNS cache; it is when
	// the lookup was made.
	CachedAt *time.Time `json:"cachedAt,omitempty"`
}

// HTTPInfo is the result of probing a host's root URL. Values are immutable
// once emitted.
type HTTPInfo struct {
	// Reachable is true when the host returned any HTTP response.
	Reachable bool `json:"reachable"`
	// URL is the root URL that answered, e.g. "https://api.example.com/".
	URL         string `json:"url,omitempty"`
	Scheme      string `json:"scheme,omitempty"`
	Status      int    `json:"status,omitempty"`
	Redirect    string `json:"redirect,omitempty"`
	FinalURL    string `json:"finalUrl,omitempty"`
	FinalStatus int    `json:"finalStatus,omitempty"`
	Title       string `json:"title,omitempty"`
	Server      string `json:"server,omitempty"`
	ContentType string `json:"contentType,omitempty"`
	Error       string `json:"error,omitempty"`
	// Skipped explains why the host was not probed.
	Skipped string `json:"skipped,omitempty"`
	// CachedAt is set when the result came from the probe cache; it is when
	// the host was probed.
	CachedAt *time.Time `json:"cachedAt,omitempty"`
}

// CrawlInfo summarizes the crawl of one host. Values are immutable once emitted.
type CrawlInfo struct {
	Requests int `json:"requests"`
	// LimitReached is true when the per-host request budget ran out.
	LimitReached bool `json:"limitReached,omitempty"`
	// FromCache counts pages reused from the page cache instead of being
	// requested (not included in Requests).
	FromCache int `json:"fromCache,omitempty"`
	// Skipped explains why the host was not crawled.
	Skipped string `json:"skipped,omitempty"`
}

// PartialError reports that an engine finished but part of its work failed,
// for example one of several independent providers. The scan records the
// failure in its errors without treating the engine as failed.
type PartialError struct{ Err error }

func (e *PartialError) Error() string { return e.Err.Error() }
func (e *PartialError) Unwrap() error { return e.Err }

// Partial wraps err as a PartialError; it returns nil for a nil err.
func Partial(err error) error {
	if err == nil {
		return nil
	}
	return &PartialError{Err: err}
}

// Skip reasons shared by host engines. Budget skips are counted and shown
// to users as resource limits, distinct from skips that are expected (such
// as hosts that only redirect elsewhere).
const (
	// SkipHostLimit: the scan's host budget for this stage was used up.
	SkipHostLimit = "host limit reached"
	// SkipRequestLimit: the scan's total request budget was used up.
	SkipRequestLimit = "scan request limit reached"
)

// SitemapInfo summarizes a host's robots.txt and sitemaps. Values are
// immutable once emitted.
type SitemapInfo struct {
	// RobotsStatus is the HTTP status of /robots.txt (0 if not requested).
	RobotsStatus int `json:"robotsStatus,omitempty"`
	// Files is how many sitemap files were read; URLs how many page URLs
	// they listed (within scope and limits).
	Files int `json:"files"`
	URLs  int `json:"urls"`
	// LimitReached is true when more sitemap files or URLs existed than
	// the per-host limits allow.
	LimitReached bool `json:"limitReached,omitempty"`
	// Skipped explains why the host's sitemaps were not read.
	Skipped string `json:"skipped,omitempty"`
}

// HostView is a read-only snapshot of what is known about a host.
type HostView struct {
	Name    string
	Sources []Source
	DNS     *DNSInfo
	HTTP    *HTTPInfo
	Crawl   *CrawlInfo
	Sitemap *SitemapInfo
}

// Reachable reports whether the host answered HTTP.
func (h HostView) Reachable() bool { return h.HTTP != nil && h.HTTP.Reachable }

// State is a read-only view of everything discovered so far in a scan. It
// lets engines build on earlier engines without depending on them directly.
type State interface {
	// Hosts returns all hosts discovered so far.
	Hosts() []HostView
	// PageURLs returns known page URLs that have not been fetched yet.
	PageURLs() []string
}

// Input is what an engine receives when it runs.
type Input struct {
	Target Target
	State  State
}

// Emit receives findings. It is safe to call from multiple goroutines.
type Emit func(Finding)

// Engine is a single, independent discovery mechanism.
//
// Engines that act on hosts (resolve, probe, crawl) only process hosts they
// have not processed before, based on State, so the scan can run them again
// for hosts discovered later.
type Engine interface {
	// Name is a stable identifier, e.g. "html".
	Name() string
	// Discover reports findings through emit. Returning an error marks this
	// engine as failed but does not fail the scan; findings already emitted
	// are kept. Discover must stop promptly when ctx is cancelled.
	Discover(ctx context.Context, in Input, emit Emit) error
}
