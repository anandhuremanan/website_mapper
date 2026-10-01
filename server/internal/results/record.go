package results

import (
	"net/url"
	"sort"
	"strings"
	"time"

	"websitemapper/internal/classify"
	"websitemapper/internal/discovery"
)

// URLRecord is everything recorded about one URL. It is the unit a URLStore
// persists: plain data, small, and never a response body.
//
// A URL is identified by its host, Origin and Path, so a host's records
// sort by path: the order lists and the path tree are read in.
type URLRecord struct {
	// HostID is the Aggregator's number for the URL's host.
	HostID int64
	// Path is the URL after its authority: the escaped path plus "?query".
	Path string
	// Origin is the scheme and any non-default port: "https", "http:8080".
	Origin string

	Sources SourceSet
	Hints   HintSet
	// Methods are HTTP methods seen in context (a form's method), sorted.
	Methods []string
	// From lists up to maxDiscoveredFrom pages or files that referenced the URL.
	From []string

	// Responded is true when an HTTP response was received; the fields
	// below it describe that response.
	Responded   bool
	Status      int
	ContentType string
	Title       string
	Redirect    string
	Server      string
	// CachedAt is set when the response was reused from the page cache.
	CachedAt *time.Time
	// Err describes a request that got no response.
	Err string

	Archive *discovery.ArchiveInfo

	// Type and AssetKind are the classification, stored so URLs can be
	// counted and filtered without reclassifying them.
	Type      classify.Type
	AssetKind classify.AssetKind
}

// URLStore keeps one scan's URL records. The Aggregator is its only user
// and serializes every call, so implementations need not be safe for
// concurrent use.
type URLStore interface {
	// Insert stores r unless its URL is already stored, and reports
	// whether it was inserted.
	Insert(r *URLRecord) (bool, error)
	// Get returns the stored record of a URL. The caller may modify it and
	// must then pass it to Update.
	Get(hostID int64, path, origin string) (*URLRecord, bool, error)
	// Update stores a record returned by Get after it changed.
	Update(r *URLRecord) error
	// PendingPages calls fn for every page URL that was not requested yet
	// and is not known only from a web archive.
	PendingPages(fn func(hostID int64, path, origin string)) error
	// Flush makes everything stored so far durable.
	Flush() error
}

// Pending reports whether the URL is a page the crawler may still request:
// see URLStore.PendingPages.
func (r *URLRecord) Pending() bool {
	return !r.Responded && r.Err == "" && r.Type == classify.TypePage && r.Sources != ArchiveOnly
}

// State distinguishes discovering a URL from requesting it.
func (r *URLRecord) State() URLState {
	switch {
	case r.Responded:
		return URLVerified
	case r.Err != "":
		return URLFetched
	}
	return URLDiscovered
}

// splitURL returns a normalized URL's Origin and Path.
func splitURL(u *url.URL) (origin, path string) {
	origin = u.Scheme
	if p := u.Port(); p != "" {
		origin += ":" + p
	}
	path = u.EscapedPath()
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	return origin, path
}

// JoinURL rebuilds the URL string splitURL took apart.
func JoinURL(hostname, origin, path string) string {
	scheme, port, hasPort := strings.Cut(origin, ":")
	if hasPort {
		return scheme + "://" + hostname + ":" + port + path
	}
	return scheme + "://" + hostname + path
}

// pathOnly returns the escaped path without the query.
func (r *URLRecord) pathOnly() string {
	p, _, _ := strings.Cut(r.Path, "?")
	return p
}

// classification decides the record's type from everything known about it.
func (r *URLRecord) classification() classify.Result {
	p := r.pathOnly()
	if decoded, err := url.PathUnescape(p); err == nil {
		p = decoded
	}
	in := classify.Input{Path: p, Hints: r.Hints.List()}
	if r.Archive != nil {
		in.ArchivedContentType = r.Archive.ContentType
	}
	if r.Responded {
		in.Status, in.ContentType = r.Status, r.ContentType
	}
	return classify.Classify(in)
}

func (r *URLRecord) classify() {
	c := r.classification()
	r.Type, r.AssetKind = c.Type, c.AssetKind
}

// View builds the URL shown to users. hostname is the record's host.
func (r *URLRecord) View(hostname string) URL {
	out := URL{
		URL:            JoinURL(hostname, r.Origin, r.Path),
		Hostname:       hostname,
		Path:           r.pathOnly(),
		Methods:        r.Methods,
		Sources:        r.Sources.List(),
		DiscoveredFrom: r.From,
		Error:          r.Err,
		Fetched:        r.Responded || r.Err != "",
		State:          r.State(),
		Archived:       r.Archive,
	}
	if r.Responded {
		out.Status = r.Status
		out.ContentType = r.ContentType
		out.Title = r.Title
		out.Redirect = r.Redirect
		out.Server = r.Server
		out.CachedAt = r.CachedAt
	}
	c := r.classification()
	out.Type, out.AssetKind, out.TypeEvidence = c.Type, c.AssetKind, c.Evidence
	return out
}

// merge applies a finding to the record. changed reports whether anything
// stored changed; reclassify whether the classification may have; took
// whether the finding's response became the record's response.
func (r *URLRecord) merge(f discovery.Finding) (changed, reclassify, took bool) {
	if s := r.Sources.with(f.Source); s != r.Sources {
		r.Sources, changed = s, true
	}
	if h := r.Hints.with(f.Hint); h != r.Hints {
		r.Hints, changed, reclassify = h, true, true
	}
	if f.Method != "" && !contains(r.Methods, f.Method) {
		r.Methods = append(r.Methods, f.Method)
		sort.Strings(r.Methods)
		changed = true
	}
	if f.Archive != nil && (r.Archive == nil || f.Archive.FirstSeen.Before(r.Archive.FirstSeen)) {
		a := *f.Archive
		r.Archive, changed, reclassify = &a, true, true
	}
	if f.From != "" && len(r.From) < maxDiscoveredFrom && !contains(r.From, f.From) {
		r.From = append(r.From, f.From)
		changed = true
	}
	switch resp := f.Response; {
	case resp != nil:
		// Prefer a successful response over an earlier redirect or error,
		// and this scan's own response over a cached one.
		if !r.Responded || (r.Status >= 300 && resp.Status < 300) || (r.CachedAt != nil && resp.CachedAt == nil) {
			r.Responded = true
			r.Status, r.ContentType, r.Title = resp.Status, resp.ContentType, resp.Title
			r.Redirect, r.Server, r.CachedAt = resp.Redirect, resp.Server, resp.CachedAt
			changed, reclassify, took = true, true, true
		}
		if r.Err != "" {
			r.Err, changed = "", true
		}
	case f.Error != "" && !r.Responded && r.Err != f.Error:
		r.Err, changed = f.Error, true
	}
	return changed, reclassify, took
}

// SourceSet is a set of discovery.Sources, one bit per position in
// discovery.Sources.
type SourceSet uint32

// ArchiveOnly is the set of a URL known only from a web archive.
var ArchiveOnly = SourceSet(0).with(discovery.SourceArchive)

func (s SourceSet) with(src discovery.Source) SourceSet {
	return s | SourceSet(bit(discovery.Sources, src))
}

// List returns the sources, sorted.
func (s SourceSet) List() []discovery.Source { return list(discovery.Sources, uint32(s)) }

// HintSet is a set of discovery.Hints, one bit per position in discovery.Hints.
type HintSet uint32

func (h HintSet) with(hint discovery.Hint) HintSet {
	return h | HintSet(bit(discovery.Hints, hint))
}

// List returns the hints, sorted.
func (h HintSet) List() []discovery.Hint { return list(discovery.Hints, uint32(h)) }

// bit returns the bit of v in all, or 0 if v is empty or not listed.
func bit[T ~string](all []T, v T) uint32 {
	if v == "" {
		return 0
	}
	for i, x := range all {
		if x == v {
			return 1 << i
		}
	}
	return 0
}

func list[T ~string](all []T, bits uint32) []T {
	out := []T{}
	for i, x := range all {
		if bits&(1<<i) != 0 {
			out = append(out, x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
