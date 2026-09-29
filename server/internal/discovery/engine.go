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

import "context"

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
	// SourceHost marks a host's root URL, requested because the host itself
	// was discovered. It is URL provenance only, never host provenance.
	SourceHost Source = "host"
)

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
)

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
	// RobotsDisallowed means URL was not fetched because the host's
	// robots.txt disallows it.
	RobotsDisallowed bool

	// Host observations. Each replaces any earlier value for the host.
	DNS   *DNSInfo
	HTTP  *HTTPInfo
	Crawl *CrawlInfo
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
}

// CrawlInfo summarizes the crawl of one host. Values are immutable once emitted.
type CrawlInfo struct {
	// Requests counts page requests; the robots.txt request is not included.
	Requests int `json:"requests"`
	// LimitReached is true when the per-host request budget ran out.
	LimitReached bool `json:"limitReached,omitempty"`
	// Robots describes robots.txt handling: "respected" (rules applied),
	// "not found", "unavailable" (could not be fetched; crawled normally)
	// or "ignored" (checking disabled by configuration).
	Robots string `json:"robots,omitempty"`
	// RobotsDisallowed counts URLs not fetched because robots.txt disallows them.
	RobotsDisallowed int `json:"robotsDisallowed,omitempty"`
	// Skipped explains why the host was not crawled.
	Skipped string `json:"skipped,omitempty"`
}

// HostView is a read-only snapshot of what is known about a host.
type HostView struct {
	Name    string
	Sources []Source
	DNS     *DNSInfo
	HTTP    *HTTPInfo
	Crawl   *CrawlInfo
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
