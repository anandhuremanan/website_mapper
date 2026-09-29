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
)

// StepStatus is the state of a single progress step.
type StepStatus string

const (
	StepPending StepStatus = "pending"
	StepRunning StepStatus = "running"
	StepDone    StepStatus = "done"
	StepFailed  StepStatus = "failed"
	StepSkipped StepStatus = "skipped"
)

// Scan is the status record of a scan. The (potentially large) result is
// stored separately so status polling stays cheap.
type Scan struct {
	ID       string `json:"id"`
	Target   string `json:"target"`
	Domain   string `json:"domain"`
	StartURL string `json:"startUrl"`
	Status   Status `json:"status"`
	// AuthorizationConfirmed records that the requester confirmed they own
	// the domain or have permission to scan it. Scans require it.
	AuthorizationConfirmed bool       `json:"authorizationConfirmed"`
	CreatedAt              time.Time  `json:"createdAt"`
	StartedAt              *time.Time `json:"startedAt,omitempty"`
	FinishedAt             *time.Time `json:"finishedAt,omitempty"`
	DurationMs             int64      `json:"durationMs,omitempty"`

	Steps  []Step         `json:"steps"`
	Counts results.Counts `json:"counts"`
	// Requests is the number of outbound HTTP requests made so far.
	Requests int `json:"requests"`
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
}

// Result is a completed scan's normalized result plus scan context.
type Result struct {
	ScanID string         `json:"scanId"`
	Status Status         `json:"status"`
	Domain Domain         `json:"domain"`
	Errors []EngineError  `json:"errors"`
	Counts results.Counts `json:"counts"`
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
