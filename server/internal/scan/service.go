package scan

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"websitemapper/internal/discovery"
	"websitemapper/internal/fetch"
	"websitemapper/internal/results"
)

// ErrQueueFull is returned when no more scans can be queued.
var ErrQueueFull = errors.New("scan queue is full, try again shortly")

// ErrAuthorizationRequired is returned when a scan is requested without
// confirming permission to scan the domain.
var ErrAuthorizationRequired = errors.New("confirm that you own this domain or have permission to scan it")

// CreateRequest is a request to scan a target.
type CreateRequest struct {
	Target string
	// AuthorizationConfirmed must be true: the requester confirms they own
	// the domain or have permission to scan it.
	AuthorizationConfirmed bool
}

const (
	validateStep = "validate"
	finalizeStep = "finalize"
)

// Stage is one ordered phase of a scan, shown as one progress step. Its
// engines run one after another, each seeing everything found so far.
//
// The pipeline is an ordered list of stages, so new engines (robots.txt,
// sitemap, JavaScript analysis, ...) are added by inserting them into a
// stage or adding a stage, without changing the service.
type Stage struct {
	ID      string
	Label   string
	Engines []discovery.Engine
}

// Options configures a Service.
type Options struct {
	// Workers is the number of scans that run concurrently.
	Workers int
	// QueueSize is how many scans may wait for a worker.
	QueueSize int
	// ScanTimeout bounds the total duration of one scan.
	ScanTimeout time.Duration
	// ProgressInterval is how often live counts are persisted while running.
	ProgressInterval time.Duration
	// MaxURLsPerHost bounds how many URLs are recorded per host (0: no limit).
	MaxURLsPerHost int
	// MaxRequests bounds the outbound HTTP requests of one scan (0: no limit).
	MaxRequests int
	// RequestsPerSecond bounds one scan's overall request rate across all
	// hosts (0: no limit). Per-host rates are limited separately.
	RequestsPerSecond float64
}

// Service creates scans and runs them in the background.
//
// Scans are handed to a fixed pool of in-process workers through a channel.
// Replacing the channel with an external job queue later only changes how
// jobs are delivered to run.
type Service struct {
	repo   Repository
	stages []Stage
	opts   Options
	log    *slog.Logger

	queue chan job
	wg    sync.WaitGroup
}

type job struct {
	id     string
	target discovery.Target
}

// NewService creates a Service that runs the given stages in order.
func NewService(repo Repository, stages []Stage, opts Options, log *slog.Logger) *Service {
	if opts.Workers < 1 {
		opts.Workers = 1
	}
	if opts.QueueSize < 1 {
		opts.QueueSize = 1
	}
	if opts.ScanTimeout <= 0 {
		opts.ScanTimeout = 5 * time.Minute
	}
	if opts.ProgressInterval <= 0 {
		opts.ProgressInterval = time.Second
	}
	return &Service{
		repo:   repo,
		stages: stages,
		opts:   opts,
		log:    log,
		queue:  make(chan job, opts.QueueSize),
	}
}

// Start launches the workers. They stop when ctx is cancelled; running
// scans are cancelled too. Use Wait to block until they have exited.
func (s *Service) Start(ctx context.Context) {
	for i := 0; i < s.opts.Workers; i++ {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case j := <-s.queue:
					s.run(ctx, j)
				}
			}
		}()
	}
}

// Wait blocks until all workers have stopped.
func (s *Service) Wait() { s.wg.Wait() }

// Create checks the authorization confirmation, validates the target,
// stores a queued scan and enqueues it. It returns immediately; the scan
// runs in the background.
func (s *Service) Create(ctx context.Context, req CreateRequest) (Scan, error) {
	if !req.AuthorizationConfirmed {
		return Scan{}, ErrAuthorizationRequired
	}
	target, err := discovery.ParseTarget(req.Target)
	if err != nil {
		return Scan{}, err
	}
	id, err := newID()
	if err != nil {
		return Scan{}, err
	}
	sc := Scan{
		ID:                     id,
		Target:                 target.Input,
		Domain:                 target.Domain,
		StartURL:               target.StartURL,
		Status:                 StatusQueued,
		AuthorizationConfirmed: true,
		CreatedAt:              time.Now().UTC(),
		Steps:                  s.initialSteps(),
		Errors:                 []EngineError{},
	}
	if err := s.repo.Create(ctx, sc); err != nil {
		return Scan{}, err
	}

	select {
	case s.queue <- job{id: id, target: target}:
	default:
		sc.Status = StatusFailed
		sc.Error = ErrQueueFull.Error()
		_ = s.repo.Update(ctx, sc)
		return Scan{}, ErrQueueFull
	}
	s.log.Info("scan queued", "event", "scan_queued", "scan_id", id, "target", target.StartURL)
	return sc, nil
}

// Get returns a scan's current status.
func (s *Service) Get(ctx context.Context, id string) (Scan, error) {
	return s.repo.Get(ctx, id)
}

// Result returns a finished scan's result.
func (s *Service) Result(ctx context.Context, id string) (Result, error) {
	return s.repo.GetResult(ctx, id)
}

// initialSteps lists progress steps: target validation (already done when
// the scan is created), one step per stage, and finalization.
func (s *Service) initialSteps() []Step {
	steps := make([]Step, 0, len(s.stages)+2)
	steps = append(steps, Step{ID: validateStep, Label: "Validating target", Status: StepDone})
	for _, st := range s.stages {
		steps = append(steps, Step{ID: st.ID, Label: st.Label, Status: StepPending})
	}
	return append(steps, Step{ID: finalizeStep, Label: "Finalizing results", Status: StepPending})
}

// run executes one scan. Engine failures are recorded and the scan
// continues; the scan fails only if every engine run failed or it was
// cancelled.
func (s *Service) run(parent context.Context, j job) {
	log := s.log.With("scan_id", j.id)
	tr, err := newTracker(s.repo, j.id)
	if err != nil {
		log.Error("scan disappeared before it started", "event", "scan_lost", "error", err)
		return
	}

	started := time.Now()
	defer func() {
		if r := recover(); r != nil {
			log.Error("scan panicked", "event", "scan_failed", "panic", fmt.Sprint(r))
			tr.update(func(sc *Scan) { finish(sc, started, StatusFailed, "internal error while scanning") })
		}
	}()

	tr.update(func(sc *Scan) {
		sc.Status = StatusRunning
		t := started.UTC()
		sc.StartedAt = &t
	})
	log.Info("scan started", "event", "scan_started", "target", j.target.StartURL)

	ctx, cancel := context.WithTimeout(parent, s.opts.ScanTimeout)
	defer cancel()
	// Every request any engine makes for this scan draws from one budget.
	budget := fetch.NewBudget(s.opts.MaxRequests, s.opts.RequestsPerSecond)
	ctx = fetch.WithBudget(ctx, budget)

	agg := results.NewAggregator(j.target, s.opts.MaxURLsPerHost)
	// Every scan starts from the entered host and the apex domain, whether
	// or not any engine finds them.
	for _, h := range j.target.Hosts() {
		agg.Add(discovery.Finding{Host: h, Source: discovery.SourceTarget})
	}
	stopProgress := s.trackProgress(tr, agg, budget)

	runs, failed := 0, 0
	for i, st := range s.stages {
		step := i + 1 // step 0 is validation
		if ctx.Err() != nil {
			tr.update(func(sc *Scan) { sc.Steps[step].Status = StepSkipped })
			continue
		}
		tr.update(func(sc *Scan) { sc.Steps[step].Status = StepRunning })

		stageFailed, total := 0, 0
		for _, eng := range st.Engines {
			if ctx.Err() != nil {
				break
			}
			runs++
			count, err := s.runEngine(ctx, log, j.target, st, eng, agg)
			total += count
			if err != nil {
				failed++
				stageFailed++
				tr.update(func(sc *Scan) {
					sc.Errors = append(sc.Errors, EngineError{Stage: st.ID, Engine: eng.Name(), Message: engineMessage(err)})
				})
			}
		}
		counts := agg.Counts()
		tr.update(func(sc *Scan) {
			sc.Steps[step].Findings = total
			sc.Counts = counts
			sc.Requests = budget.Used()
			if len(st.Engines) > 0 && stageFailed == len(st.Engines) {
				sc.Steps[step].Status = StepFailed
			} else {
				sc.Steps[step].Status = StepDone
			}
		})
	}
	stopProgress()

	finalIdx := len(s.stages) + 1
	tr.update(func(sc *Scan) { sc.Steps[finalIdx].Status = StepRunning })

	res := agg.Result()
	status, msg := StatusCompleted, ""
	switch {
	case parent.Err() != nil:
		status, msg = StatusFailed, "scan was cancelled"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		// Engines stopped by the deadline did not fail; results are partial.
		tr.update(func(sc *Scan) {
			sc.Errors = append(sc.Errors, EngineError{Engine: "scan", Message: "scan time limit reached; results are partial"})
		})
	case runs > 0 && failed == runs:
		status, msg = StatusFailed, "every discovery step failed; see errors"
	}
	if s.opts.MaxRequests > 0 && budget.Used() >= s.opts.MaxRequests {
		tr.update(func(sc *Scan) {
			sc.Errors = append(sc.Errors, EngineError{Engine: "scan",
				Message: fmt.Sprintf("scan request limit (%d) reached; results are partial", s.opts.MaxRequests)})
		})
	}

	var finished Scan
	tr.update(func(sc *Scan) {
		sc.Counts = res.Counts()
		sc.Requests = budget.Used()
		sc.Steps[finalIdx].Status = StepDone
		finish(sc, started, status, msg)
		finished = sc.Clone()
	})

	err = s.repo.SaveResult(context.Background(), Result{
		ScanID: j.id,
		Status: finished.Status,
		Domain: Domain{
			Target:     finished.Target,
			Canonical:  finished.Domain,
			StartURL:   finished.StartURL,
			ScannedAt:  started.UTC(),
			DurationMs: finished.DurationMs,
		},
		Errors: finished.Errors,
		Counts: finished.Counts,
		Result: res,
	})
	if err != nil {
		log.Error("saving result failed", "event", "result_save_failed", "error", err)
	}

	c := finished.Counts
	log.Info("scan finished", "event", "scan_"+string(status), "duration", time.Since(started).Round(time.Millisecond),
		"hosts", c.Hosts, "hosts_resolved", c.HostsResolved, "hosts_reachable", c.HostsReachable,
		"urls", c.URLs, "requests", finished.Requests, "engine_errors", len(finished.Errors))
}

// runEngine runs one engine against the current state and returns how many
// findings it reported.
func (s *Service) runEngine(ctx context.Context, log *slog.Logger, t discovery.Target, st Stage, eng discovery.Engine, agg *results.Aggregator) (int, error) {
	var n atomic.Int64
	emit := func(f discovery.Finding) { n.Add(1); agg.Add(f) }
	start := time.Now()
	err := discover(ctx, eng, discovery.Input{Target: t, State: agg}, emit)
	count := int(n.Load())
	if err != nil {
		log.Warn("discovery failed", "event", "discovery_failed", "stage", st.ID, "engine", eng.Name(),
			"count", count, "error", err)
	} else {
		log.Info("discovery completed", "event", "discovery_completed", "stage", st.ID, "engine", eng.Name(),
			"count", count, "duration", time.Since(start).Round(time.Millisecond))
	}
	return count, err
}

// discover runs an engine, converting a panic into an error so one broken
// engine cannot take down the scan.
func discover(ctx context.Context, e discovery.Engine, in discovery.Input, emit discovery.Emit) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("engine panicked: %v", r)
		}
	}()
	return e.Discover(ctx, in, emit)
}

func engineMessage(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "stopped: scan time limit reached"
	case errors.Is(err, context.Canceled):
		return "stopped: scan was cancelled"
	}
	return err.Error()
}

// trackProgress periodically persists live counts until the returned stop
// function is called.
func (s *Service) trackProgress(tr *tracker, agg *results.Aggregator, budget *fetch.Budget) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(s.opts.ProgressInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				counts := agg.Counts()
				tr.update(func(sc *Scan) {
					sc.Counts = counts
					sc.Requests = budget.Used()
				})
			}
		}
	}()
	return func() { close(done); wg.Wait() }
}

func finish(sc *Scan, started time.Time, status Status, msg string) {
	now := time.Now().UTC()
	sc.Status = status
	sc.Error = msg
	sc.FinishedAt = &now
	sc.DurationMs = time.Since(started).Milliseconds()
}

// tracker serializes updates to one scan's status record.
type tracker struct {
	repo Repository
	mu   sync.Mutex
	scan Scan
}

func newTracker(repo Repository, id string) (*tracker, error) {
	sc, err := repo.Get(context.Background(), id)
	if err != nil {
		return nil, err
	}
	return &tracker{repo: repo, scan: sc}, nil
}

func (t *tracker) update(fn func(*Scan)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	fn(&t.scan)
	// Status writes are best effort; the in-memory store only fails if the
	// scan was evicted, in which case there is nobody left to report to.
	_ = t.repo.Update(context.Background(), t.scan)
}

func newID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
