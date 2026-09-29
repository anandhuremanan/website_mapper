// Package results merges raw findings from all discovery engines into a
// normalized, deduplicated scan result that preserves provenance.
//
// The result is hierarchical: the target domain has hosts, and each host
// has the URLs (routes, API-like endpoints, assets) discovered on it.
package results

import (
	"websitemapper/internal/classify"
	"websitemapper/internal/discovery"
)

// Result is the complete, normalized output of a scan.
type Result struct {
	Hosts        []Host       `json:"hosts"`
	Technologies []Technology `json:"technologies"`
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
	Hostname string    `json:"hostname"`
	State    HostState `json:"state"`
	// Sources are how the host itself was discovered.
	Sources []discovery.Source   `json:"sources"`
	DNS     *discovery.DNSInfo   `json:"dns,omitempty"`
	HTTP    *discovery.HTTPInfo  `json:"http,omitempty"`
	Crawl   *discovery.CrawlInfo `json:"crawl,omitempty"`
	Counts  HostCounts           `json:"counts"`
	URLs    []URL                `json:"urls"`
	// Omitted counts URLs seen on this host but not recorded because the
	// per-host URL limit was reached.
	Omitted int `json:"urlsOmitted,omitempty"`
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
	// RobotsDisallowed is true when the URL was not requested because the
	// host's robots.txt disallows it.
	RobotsDisallowed bool `json:"robotsDisallowed,omitempty"`
}

// Technology is a technology detected from observable evidence.
type Technology struct {
	Name     string   `json:"name"`
	Evidence []string `json:"evidence"`
}

// Counts summarizes a result for progress display.
type Counts struct {
	Hosts          int `json:"hosts"`
	HostsResolved  int `json:"hostsResolved"`
	HostsReachable int `json:"hostsReachable"`
	HostsCrawled   int `json:"hostsCrawled"`
	URLs           int `json:"urls"`
	Pages          int `json:"pages"`
	APIs           int `json:"apis"`
	Assets         int `json:"assets"`
	JavaScript     int `json:"javascript"`
}
