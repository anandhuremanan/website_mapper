// Package scan owns the scan lifecycle: creating scans, running discovery
// engines in the background, tracking progress and storing results.
package scan

import (
	"context"
	"errors"
	"time"

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

// Phase is what a scan is doing right now, for progress display.
type Phase string

const (
	PhaseQueued     Phase = "queued"
	PhaseSubdomains Phase = "discovering_subdomains"
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
	Status     Status     `json:"status"`
	CreatedAt  time.Time  `json:"createdAt"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	DurationMs int64      `json:"durationMs,omitempty"`

	Phase Phase `json:"phase"`
	// QueuePosition is the 1-based position while queued.
	QueuePosition int `json:"queuePosition,omitempty"`
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

// Result is a completed scan's normalized result plus scan context.
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
	Target     string    `json:"target"`
	Canonical  string    `json:"canonical"`
	StartURL   string    `json:"startUrl"`
	ScannedAt  time.Time `json:"scannedAt"`
	DurationMs int64     `json:"durationMs"`
}

// ErrNotFound is returned when a scan or result does not exist.
var ErrNotFound = errors.New("not found")

// Repository persists scans and results. The in-memory implementation is
// used for V1; a database-backed one can replace it without changing the
// service. Implementations must return copies that callers may modify.
type Repository interface {
	Create(ctx context.Context, s Scan) error
	Get(ctx context.Context, id string) (Scan, error)
	Update(ctx context.Context, s Scan) error
	SaveResult(ctx context.Context, r Result) error
	GetResult(ctx context.Context, id string) (Result, error)
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
