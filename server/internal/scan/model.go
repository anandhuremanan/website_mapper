// Package scan owns the scan lifecycle: creating scans, running discovery
// engines in the background, tracking progress and storing results.
package scan

import (
	"context"
	"errors"
	"time"

	"websitemapper/internal/classify"
	"websitemapper/internal/results"
)

// Status is the lifecycle state of a scan.
type Status string

const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// Finished reports whether the scan has reached a terminal state.
func (s Status) Finished() bool {
	return s == StatusCompleted || s == StatusFailed || s == StatusCancelled
}

// Mode chooses how deep a scan goes. Every mode lists subdomains; they
// differ in how routes are found and how much the scanner contacts the site.
type Mode string

const (
	// ModePassive never contacts the site: certificate logs, a web archive
	// and DNS only. Routes are historical (archived) and unverified.
	ModePassive Mode = "passive"
	// ModeLight adds, per live host, one probe and its robots.txt and
	// sitemaps: a few requests per host, current routes without crawling.
	ModeLight Mode = "light"
	// ModeFull also crawls pages, following links (the most requests).
	ModeFull Mode = "full"
)

// ErrInvalidMode is returned for an unknown scan mode.
var ErrInvalidMode = errors.New(`mode must be "passive", "light" or "full"`)

// ParseMode validates a mode; "" means def.
func ParseMode(s string, def Mode) (Mode, error) {
	switch m := Mode(s); m {
	case "":
		return def, nil
	case ModePassive, ModeLight, ModeFull:
		return m, nil
	}
	return "", ErrInvalidMode
}

// Phase is what a scan is doing right now, for progress display.
type Phase string

const (
	PhaseQueued     Phase = "queued"
	PhaseSubdomains Phase = "discovering_subdomains"
	PhaseArchive    Phase = "searching_archives"
	PhaseSitemaps   Phase = "reading_sitemaps"
	PhaseResolving  Phase = "resolving_hosts"
	PhaseProbing    Phase = "probing_hosts"
	PhaseCrawling   Phase = "crawling_hosts"
	PhaseFinalizing Phase = "finalizing"
	PhaseDone       Phase = "done"
)

// Stop reasons explain why a scan ended before finishing all its work.
const (
	StopTimeout  = "scan_timeout"
	StopCancel   = "cancelled"
	StopShutdown = "server_shutdown"
)

// Limit notice codes.
const (
	LimitScanTimeout    = "scan_timeout"
	LimitHostBudget     = "host_budget_reached"
	LimitCrawl          = "crawl_limit_reached"
	LimitURLBudget      = "url_budget_reached"
	LimitRequestBudget  = "request_budget_reached"
	LimitDownload       = "download_budget_reached"
	LimitGlobalResource = "global_resource_wait"
	// LimitDiscoveryRounds: hosts found in the final crawl round were
	// recorded but not resolved, probed or crawled.
	LimitDiscoveryRounds = "discovery_round_limit"
)

// LimitNotice tells the user that a resource limit shaped the result.
type LimitNotice struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// PhaseProgress is the progress of the current phase, in hosts.
type PhaseProgress struct {
	Total     int `json:"total"`
	Completed int `json:"completed"`
	Pending   int `json:"pending"`
}

// Resources reports a scan's use of shared server capacity.
type Resources struct {
	Requests    int `json:"requests"`
	MaxRequests int `json:"maxRequests,omitempty"`
	// DownloadedBytes counts response bytes read (bodies are read only for
	// HTML) plus an allowance for headers.
	DownloadedBytes  int64 `json:"downloadedBytes"`
	MaxDownloadBytes int64 `json:"maxDownloadBytes,omitempty"`
	// Pools reports, per shared pool, how often this scan had to wait.
	Pools map[string]PoolUsage `json:"pools,omitempty"`
}

// PoolUsage is a scan's use of one shared pool.
type PoolUsage struct {
	Operations int `json:"operations"`
	// Delayed counts operations that waited for a free slot.
	Delayed int `json:"delayed"`
	// AvgWaitMs is the average wait of delayed operations.
	AvgWaitMs int64 `json:"avgWaitMs"`
}

// StepStatus is the state of a single progress step.
type StepStatus string

const (
	StepPending StepStatus = "pending"
	StepRunning StepStatus = "running"
	StepDone    StepStatus = "done"
	StepFailed  StepStatus = "failed"
	StepSkipped StepStatus = "skipped"
	// StepStopped: interrupted by cancellation or the scan time limit.
	StepStopped StepStatus = "stopped"
)

// Scan is the status record of a scan. The (potentially large) result is
// stored separately so status polling stays cheap: every field here has a
// small, bounded size regardless of how much the scan discovers.
type Scan struct {
	ID         string     `json:"id"`
	Target     string     `json:"target"`
	Domain     string     `json:"domain"`
	StartURL   string     `json:"startUrl"`
	Mode       Mode       `json:"mode"`
	Status     Status     `json:"status"`
	CreatedAt  time.Time  `json:"createdAt"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	DurationMs int64      `json:"durationMs,omitempty"`

	Phase Phase `json:"phase"`
	// QueuePosition is the 1-based position while queued.
	QueuePosition int `json:"queuePosition,omitempty"`
	// Subscribers is how many requesters currently share this active scan.
	Subscribers int `json:"subscribers,omitempty"`
	// SubscriptionID identifies the requester's subscription. It is set
	// only in create responses; pass it when cancelling a shared scan.
	SubscriptionID string `json:"subscriptionId,omitempty"`
	// Coalesced is set in a create response that joined an equivalent scan
	// already queued or running, instead of starting a new one.
	Coalesced bool `json:"coalesced,omitempty"`
	// Detached is set in a cancel response when only the requester's
	// subscription was released; the scan continues for the others.
	Detached bool `json:"detached,omitempty"`
	// Progress is the current phase's progress, when it is measured in hosts.
	Progress *PhaseProgress `json:"progress,omitempty"`
	// StopReason is set when the scan ended before finishing its work.
	StopReason string `json:"stopReason,omitempty"`
	// Limits lists resource limits that shaped the result.
	Limits    []LimitNotice `json:"limits"`
	Resources Resources     `json:"resources"`

	Steps  []Step         `json:"steps"`
	Counts results.Counts `json:"counts"`
	// Errors lists engine failures. A scan can complete with engine errors.
	Errors []EngineError `json:"errors"`
	// Error is set when the scan as a whole failed.
	Error string `json:"error,omitempty"`
}

// Step is one visible unit of scan progress, usually one engine.
type Step struct {
	ID     string     `json:"id"`
	Label  string     `json:"label"`
	Status StepStatus `json:"status"`
	// Findings is the number of raw findings the step reported.
	Findings int `json:"findings"`
}

// EngineError records a discovery engine failure.
type EngineError struct {
	// Stage is the stage the engine ran in; empty for scan-level notices.
	Stage   string `json:"stage,omitempty"`
	Engine  string `json:"engine"`
	Message string `json:"message"`
	// Partial is true when the engine still produced results, e.g. one of
	// several certificate providers failed.
	Partial bool `json:"partial,omitempty"`
}

// Result is a finished scan's normalized result plus scan context. Its
// hosts carry their URLs only when the result was read with them (see
// ResultReader); stored and summarized results leave them empty.
type Result struct {
	ScanID     string         `json:"scanId"`
	Status     Status         `json:"status"`
	StopReason string         `json:"stopReason,omitempty"`
	Domain     Domain         `json:"domain"`
	Errors     []EngineError  `json:"errors"`
	Limits     []LimitNotice  `json:"limits"`
	Counts     results.Counts `json:"counts"`
	results.Result
}

// Domain describes the scanned target.
type Domain struct {
	Mode       Mode      `json:"mode"`
	Target     string    `json:"target"`
	Canonical  string    `json:"canonical"`
	StartURL   string    `json:"startUrl"`
	ScannedAt  time.Time `json:"scannedAt"`
	DurationMs int64     `json:"durationMs"`
}

// ErrNotFound is returned when a scan or result does not exist.
var ErrNotFound = errors.New("not found")

// ErrStorageFull is returned when the disk holding results is too full to
// start another scan.
var ErrStorageFull = errors.New("the server's result storage is full; try again later")

// Repository persists scans and their results. Implementations must return
// copies that callers may modify.
//
// A result has two parts. The URLs, which can number in the millions, are
// written to a URLStore while the scan runs and read back one host at a
// time. Everything else (hosts, counts, notices) is small and saved once,
// with SaveResult, when the scan ends.
type Repository interface {
	Create(ctx context.Context, s Scan) error
	Get(ctx context.Context, id string) (Scan, error)
	Update(ctx context.Context, s Scan) error
	// OpenURLs returns the store a running scan records its URLs in. The
	// caller closes it when the scan ends, after SaveResult if there is a
	// result to keep.
	OpenURLs(ctx context.Context, id string) (URLStore, error)
	// SaveResult stores a finished scan's result. Its hosts' URLs are the
	// ones already written to the scan's URLStore.
	SaveResult(ctx context.Context, r Result) error
	// OpenResult opens a finished scan's result for reading. It returns
	// ErrNotFound if the scan has no result.
	OpenResult(ctx context.Context, id string) (ResultReader, error)
}

// URLStore is where one running scan records its URLs.
type URLStore interface {
	results.URLStore
	Close() error
}

// ResultReader reads one stored result a page at a time, so that reading
// costs the same however large the result is. It must be closed.
type ResultReader interface {
	// Summary returns the result without its hosts.
	Summary() Result
	// Hosts returns hosts (without URLs) in display order.
	Hosts(ctx context.Context, q HostQuery) (HostPage, error)
	// URLs calls fn for each matching URL, ordered by host and then path,
	// until q.Limit URLs were passed or fn returns an error. next is where
	// to continue, or "" if there are no more.
	URLs(ctx context.Context, q URLQuery, fn func(results.URL) error) (next string, err error)
	// Tree returns one level of a host's path tree. It returns ErrNotFound
	// for a host the result does not have.
	Tree(ctx context.Context, q TreeQuery) (Tree, error)
	Close() error
}

// ErrBadQuery is returned for a cursor or path that cannot be used.
var ErrBadQuery = errors.New("invalid query")

// HostQuery selects a page of hosts. A zero Limit means no limit.
type HostQuery struct {
	// Search keeps hosts whose name contains it.
	Search string
	// After is the Next of the previous page.
	After string
	Limit int
}

// HostPage is one page of hosts.
type HostPage struct {
	Hosts []results.Host `json:"hosts"`
	// Next continues the listing; empty on the last page.
	Next string `json:"next,omitempty"`
}

// URLQuery selects URLs. A zero Limit means no limit.
type URLQuery struct {
	// Host restricts the listing to one host; empty means every host.
	Host string
	// Types keeps URLs of these types; empty means every type.
	Types []classify.Type
	// Search keeps URLs whose path or title contains it (or, when listing
	// every host, whose hostname does).
	Search string
	// After is the next cursor of the previous page.
	After string
	Limit int
}

// TreeQuery selects one level of a host's path tree: the children of Path.
type TreeQuery struct {
	Host string
	// Path is the parent, such as "/blog/2024"; "" or "/" is the root.
	Path string
	// Assets includes asset URLs; by default the tree shows routes only.
	Assets bool
	// After is the Next of the previous page of children.
	After string
	Limit int
}

// Tree is one level of a host's path tree.
type Tree struct {
	// Path is the parent node, "/" for the root.
	Path string `json:"path"`
	// Total counts the URLs at or below the parent.
	Total int `json:"total"`
	// URLs end exactly at the parent (several if they differ by query or
	// scheme). Sent with the first page only.
	URLs     []results.URL `json:"urls"`
	Children []TreeNode    `json:"children"`
	// Next continues the children; empty on the last page.
	Next string `json:"next,omitempty"`
}

// TreeNode is a path segment below a Tree's parent.
type TreeNode struct {
	// Name is the segment, such as "2024"; Path the full path to it.
	Name string `json:"name"`
	Path string `json:"path"`
	// Total counts the URLs at or below this node.
	Total int `json:"total"`
	// HasChildren is true when there are deeper paths to expand.
	HasChildren bool `json:"hasChildren"`
	// URLs end exactly at this node.
	URLs []results.URL `json:"urls"`
}

// Clone returns a deep copy of s.
func (s Scan) Clone() Scan {
	c := s
	c.Steps = append(make([]Step, 0, len(s.Steps)), s.Steps...)
	c.Errors = append(make([]EngineError, 0, len(s.Errors)), s.Errors...)
	c.Limits = append(make([]LimitNotice, 0, len(s.Limits)), s.Limits...)
	if s.Progress != nil {
		p := *s.Progress
		c.Progress = &p
	}
	if s.Resources.Pools != nil {
		c.Resources.Pools = make(map[string]PoolUsage, len(s.Resources.Pools))
		for k, v := range s.Resources.Pools {
			c.Resources.Pools[k] = v
		}
	}
	if s.StartedAt != nil {
		t := *s.StartedAt
		c.StartedAt = &t
	}
	if s.FinishedAt != nil {
		t := *s.FinishedAt
		c.FinishedAt = &t
	}
	return c
}
