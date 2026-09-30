package scan

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"websitemapper/internal/cache"
	"websitemapper/internal/discovery"
	"websitemapper/internal/resource"
	"websitemapper/internal/results"
)

var (
	// ErrQueueFull is returned when no more scans can be queued.
	ErrQueueFull = errors.New("scan queue is full, try again shortly")
	// ErrShuttingDown is returned when the server no longer accepts scans.
	ErrShuttingDown = errors.New("server is shutting down")
	// ErrFinished is returned when cancelling a scan that already ended.
	ErrFinished = errors.New("scan has already finished")
	// ErrShared is returned when cancelling a scan shared by several
	// requesters without saying which subscription to release.
	ErrShared = errors.New("this scan is shared with other requesters; cancel it with your subscriptionId")
	// ErrNotSubscribed is returned for an unknown or already released
	// subscription.
	ErrNotSubscribed = errors.New("no such active subscription for this scan")
)

// Cancellation causes, recorded on each scan's context so the scan can tell
// why it stopped.
var (
	errUserCancel = errors.New("cancelled by user")
	errShutdown   = errors.New("server shutting down")
	errTimeout    = errors.New("scan time limit reached")
)

const (
	validateStep = "validate"
	finalizeStep = "finalize"

	// globalWaitNotice is the average wait for shared capacity above which
	// the wait is reported to the user.
	globalWaitNotice = 250 * time.Millisecond
)

// Stage is one ordered phase of a scan, shown as one progress step. Its
// engines run one after another, each seeing everything found so far.
//
// The pipeline is an ordered list of stages, so new engines (for example
// JavaScript analysis) are added by inserting them into a stage or adding a
// stage, without changing the service.
type Stage struct {
	ID      string
	Label   string
	Engines []discovery.Engine
	// Modes are the scan modes the stage runs in; empty means every mode.
	Modes []Mode
}

// runsIn reports whether the stage is part of a scan in mode m.
func (st Stage) runsIn(m Mode) bool {
	if len(st.Modes) == 0 {
		return true
	}
	for _, x := range st.Modes {
		if x == m {
			return true
		}
	}
	return false
}

// CreateRequest is a request to scan a target.
type CreateRequest struct {
	Target string
	// Mode is the scan depth; empty means the service's default.
	Mode Mode
}

// Options configures a Service.
type Options struct {
	// DefaultMode is used when a request does not choose a mode.
	DefaultMode Mode
	// MaxRunning is the number of scans that run at the same time. Further
	// scans wait in the queue.
	MaxRunning int
	// QueueSize is how many scans may wait; beyond it Create fails.
	QueueSize int
	// ScanTimeout bounds the total duration of one scan.
	ScanTimeout time.Duration
	// ProgressInterval is how often live counts are persisted while running.
	ProgressInterval time.Duration
	// MaxRequests bounds each scan's outbound HTTP requests (0: no limit).
	MaxRequests int
	// MaxDownloadBytes bounds the bytes each scan downloads (0: no limit).
	MaxDownloadBytes int64
	// Limits bound each scan's recorded results.
	Limits results.Limits
	// Pools are the shared resource pools, reported by Stats.
	Pools []*resource.Pool
	// Bandwidth is the shared download limiter, reported by Stats.
	Bandwidth *resource.Bandwidth
	// Caches are the shared discovery caches, reported by Stats.
	Caches []interface{ Stats() cache.Stats }
}

// Service creates scans and runs them in the background.
//
// It is an in-process scheduler: scans wait in a FIFO queue and at most
// MaxRunning run at once, each in its own goroutine. Work inside scans is
// further bounded server-wide by the shared resource pools used by the HTTP
// client and DNS engine. Replacing the in-memory queue with an external job
// queue later only changes how jobs reach run.
type Service struct {
	repo   Repository
	stages []Stage
	opts   Options
	log    *slog.Logger

	mu      sync.Mutex
	base    context.Context // set by Start; parent of every scan context
	queue   []*job          // waiting scans, FIFO
	jobs    map[string]*job // queued and running scans, by scan ID
	active  map[string]*job // queued and running scans, by equivalence key
	joined  int64           // requests that joined an existing scan
	running int
	closed  bool
	stopped chan struct{} // closed when the service stops accepting scans
	wg      sync.WaitGroup
}

type job struct {
	id     string
	key    string
	target discovery.Target
	mode   Mode
	stages []Stage // the pipeline's stages that run in mode
	// cancel is set once the job runs.
	cancel context.CancelCauseFunc
	// subscribers are the active subscriptions (requesters) of this scan.
	subscribers map[string]bool
}

// optionsVersion identifies the scan options that affect discovery. Scans
// take no per-request options yet, so it is constant; when they do, the
// options must become part of the equivalence key.
const optionsVersion = "v1"

// scanKey defines equivalent scans: the same normalized start URL (scheme,
// host and path, which decide where crawling starts) with the same mode and
// options. "example.com" and "https://example.com/" are equivalent;
// "www.example.com", "http://example.com" or another mode are not.
func scanKey(t discovery.Target, m Mode) string {
	return t.StartURL + "|" + string(m) + "|" + optionsVersion
}

// NewService creates a Service that runs the given stages in order.
func NewService(repo Repository, stages []Stage, opts Options, log *slog.Logger) *Service {
	if opts.DefaultMode == "" {
		opts.DefaultMode = ModeLight
	}
	opts.MaxRunning = max(opts.MaxRunning, 1)
	opts.QueueSize = max(opts.QueueSize, 1)
	if opts.ScanTimeout <= 0 {
		opts.ScanTimeout = 30 * time.Minute
	}
	if opts.ProgressInterval <= 0 {
		opts.ProgressInterval = time.Second
	}
	return &Service{
		repo:    repo,
		stages:  stages,
		opts:    opts,
		log:     log,
		jobs:    make(map[string]*job),
		active:  make(map[string]*job),
		stopped: make(chan struct{}),
	}
}

// Start begins running queued scans. When ctx is cancelled the service
// stops: queued scans are cancelled and running scans are interrupted.
// Use Shutdown for a graceful stop and Wait to block until it is complete.
func (s *Service) Start(ctx context.Context) {
	s.mu.Lock()
	s.base = ctx
	s.dispatchLocked()
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		select {
		case <-ctx.Done():
			s.stop()
		case <-s.stopped:
		}
	}()
}

// Shutdown stops accepting scans, cancels queued scans, interrupts running
// scans (which still save their partial results) and waits for them to
// finish until ctx is done.
func (s *Service) Shutdown(ctx context.Context) error {
	s.stop()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("scans still running after shutdown grace period: %w", ctx.Err())
	}
}

// Wait blocks until the service has stopped and every scan goroutine exited.
func (s *Service) Wait() { s.wg.Wait() }

// stop closes the service to new scans, cancels queued scans and
// interrupts running ones. It is idempotent.
func (s *Service) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.stopped)
	for _, j := range s.queue {
		delete(s.jobs, j.id)
		s.forgetLocked(j)
		s.markCancelledLocked(j.id, StopShutdown, "the server shut down before the scan started")
	}
	s.queue = nil
	for _, j := range s.jobs {
		if j.cancel != nil {
			j.cancel(errShutdown)
		}
	}
	s.log.Info("scan service stopping", "event", "service_stopping", "running", s.running)
}

// Create validates the target and returns immediately. If an equivalent
// scan is already queued or running, the request joins it (same scan ID, a
// new subscription) instead of starting another. Otherwise a new scan is
// queued and runs when capacity is available.
func (s *Service) Create(ctx context.Context, req CreateRequest) (Scan, error) {
	target, err := discovery.ParseTarget(req.Target)
	if err != nil {
		return Scan{}, err
	}
	mode, err := ParseMode(string(req.Mode), s.opts.DefaultMode)
	if err != nil {
		return Scan{}, err
	}
	id, err := newID()
	if err != nil {
		return Scan{}, err
	}

	token, err := newID()
	if err != nil {
		return Scan{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Scan{}, ErrShuttingDown
	}
	key := scanKey(target, mode)
	if j, ok := s.active[key]; ok {
		// Join: no new job, no queue slot, no worker.
		j.subscribers[token] = true
		s.joined++
		sc, err := s.viewLocked(ctx, j.id)
		if err != nil {
			delete(j.subscribers, token)
			return Scan{}, err
		}
		sc.SubscriptionID, sc.Coalesced = token, true
		s.log.Info("scan request joined an equivalent scan", "event", "scan_coalesced", "scan_id", j.id,
			"target", target.StartURL, "subscribers", len(j.subscribers))
		return sc, nil
	}
	if len(s.queue) >= s.opts.QueueSize {
		return Scan{}, ErrQueueFull
	}
	sc := Scan{
		ID:        id,
		Target:    target.Input,
		Domain:    target.Domain,
		StartURL:  target.StartURL,
		Status:    StatusQueued,
		Phase:     PhaseQueued,
		CreatedAt: time.Now().UTC(),
		Mode:      mode,
		Steps:     initialSteps(s.stagesFor(mode)),
		Errors:    []EngineError{},
		Limits:    []LimitNotice{},
	}
	if err := s.repo.Create(ctx, sc); err != nil {
		return Scan{}, err
	}
	j := &job{id: id, key: key, target: target, mode: mode, stages: s.stagesFor(mode), subscribers: map[string]bool{token: true}}
	s.jobs[id] = j
	s.active[key] = j
	s.queue = append(s.queue, j)
	sc.QueuePosition = len(s.queue)
	s.log.Info("scan queued", "event", "scan_queued", "scan_id", id, "target", target.StartURL,
		"queue_position", sc.QueuePosition, "running", s.running)
	s.dispatchLocked()
	if j.cancel != nil { // capacity was free: it started right away
		sc.Status, sc.Phase, sc.QueuePosition = StatusRunning, PhaseSubdomains, 0
	}
	sc.SubscriptionID, sc.Subscribers = token, 1
	return sc, nil
}

// dispatchLocked starts queued scans while capacity allows. Caller holds mu.
func (s *Service) dispatchLocked() {
	for s.base != nil && !s.closed && s.running < s.opts.MaxRunning && len(s.queue) > 0 {
		j := s.queue[0]
		s.queue[0] = nil
		s.queue = s.queue[1:]
		ctx, cancel := context.WithCancelCause(s.base)
		j.cancel = cancel
		s.running++

		// Mark the scan running now, so status reads never show a scan that
		// has left the queue as still queued.
		if sc, err := s.repo.Get(context.Background(), j.id); err == nil {
			sc.Status, sc.Phase = StatusRunning, PhaseSubdomains
			t := time.Now().UTC()
			sc.StartedAt = &t
			_ = s.repo.Update(context.Background(), sc)
		}

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.run(ctx, j)
			cancel(nil)
			s.mu.Lock()
			s.running--
			delete(s.jobs, j.id)
			s.forgetLocked(j)
			s.dispatchLocked()
			s.mu.Unlock()
		}()
	}
}

// Cancel releases a subscription to a queued or running scan.
//
// A scan can be shared by several requesters (see Create). subscription
// names the requester's subscription; releasing it detaches that requester
// only, and the scan continues while other subscriptions remain. When the
// last subscription is released the scan itself is cancelled, since nobody
// is waiting for it any more: a queued scan is removed from the queue
// immediately; a running scan is interrupted, releases its shared
// resources, and saves the partial results collected so far.
//
// An empty subscription cancels an unshared scan (the behaviour before
// scans could be shared) and returns ErrShared if others share it.
func (s *Service) Cancel(ctx context.Context, id, subscription string) (Scan, error) {
	s.mu.Lock()
	j, ok := s.jobs[id]
	if !ok {
		s.mu.Unlock()
		sc, err := s.repo.Get(ctx, id)
		if err != nil {
			return Scan{}, err
		}
		return sc, ErrFinished
	}
	switch {
	case subscription == "" && len(j.subscribers) > 1:
		s.mu.Unlock()
		return Scan{}, ErrShared
	case subscription == "":
		clear(j.subscribers)
	case !j.subscribers[subscription]:
		s.mu.Unlock()
		return Scan{}, ErrNotSubscribed
	default:
		delete(j.subscribers, subscription)
	}
	if n := len(j.subscribers); n > 0 {
		sc, err := s.viewLocked(ctx, id)
		s.mu.Unlock()
		sc.Detached = true
		s.log.Info("subscriber left a shared scan", "event", "scan_unsubscribed", "scan_id", id, "subscribers", n)
		return sc, err
	}

	if j.cancel == nil {
		for i, q := range s.queue {
			if q == j {
				s.queue = append(s.queue[:i], s.queue[i+1:]...)
				break
			}
		}
		delete(s.jobs, id)
		s.forgetLocked(j)
		s.markCancelledLocked(id, StopCancel, "")
		s.mu.Unlock()
		s.log.Info("scan cancelled while queued", "event", "scan_cancelled", "scan_id", id)
		return s.repo.Get(ctx, id)
	}
	j.cancel(errUserCancel)
	s.mu.Unlock()
	s.log.Info("scan cancellation requested", "event", "scan_cancel_requested", "scan_id", id)
	return s.repo.Get(ctx, id)
}

// forgetLocked stops new requests from joining j. Caller holds mu.
func (s *Service) forgetLocked(j *job) {
	if s.active[j.key] == j {
		delete(s.active, j.key)
	}
}

// markCancelledLocked records a scan that was cancelled before it ran.
func (s *Service) markCancelledLocked(id, reason, msg string) {
	sc, err := s.repo.Get(context.Background(), id)
	if err != nil {
		return
	}
	now := time.Now().UTC()
	sc.Status, sc.Phase, sc.StopReason, sc.Error = StatusCancelled, PhaseDone, reason, msg
	sc.FinishedAt = &now
	for i := range sc.Steps {
		if sc.Steps[i].Status == StepPending {
			sc.Steps[i].Status = StepSkipped
		}
	}
	_ = s.repo.Update(context.Background(), sc)
}

// Get returns a scan's current status, with its queue position if queued
// and its subscriber count while it is active.
func (s *Service) Get(ctx context.Context, id string) (Scan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.viewLocked(ctx, id)
}

// viewLocked reads a scan and adds the scheduler's live details. Caller
// holds mu.
func (s *Service) viewLocked(ctx context.Context, id string) (Scan, error) {
	sc, err := s.repo.Get(ctx, id)
	if err != nil {
		return sc, err
	}
	if j, ok := s.jobs[id]; ok && !sc.Status.Finished() {
		sc.Subscribers = len(j.subscribers)
		if sc.Status == StatusQueued {
			for i, q := range s.queue {
				if q == j {
					sc.QueuePosition = i + 1
					break
				}
			}
		}
	}
	return sc, nil
}

// Result returns a finished scan's result.
func (s *Service) Result(ctx context.Context, id string) (Result, error) {
	return s.repo.GetResult(ctx, id)
}

// Stats describes the scheduler and shared resources.
type Stats struct {
	Running    int              `json:"running"`
	Queued     int              `json:"queued"`
	MaxRunning int              `json:"maxRunning"`
	QueueSize  int              `json:"queueSize"`
	Pools      []resource.Stats `json:"pools"`
	// CoalescedRequests counts scan requests that joined an equivalent
	// active scan instead of starting a new one.
	CoalescedRequests int64                   `json:"coalescedRequests"`
	Caches            []cache.Stats           `json:"caches"`
	Bandwidth         resource.BandwidthStats `json:"bandwidth"`
}

// Stats returns a snapshot of the scheduler and resource pools.
func (s *Service) Stats() Stats {
	s.mu.Lock()
	st := Stats{Running: s.running, Queued: len(s.queue), MaxRunning: s.opts.MaxRunning, QueueSize: s.opts.QueueSize,
		CoalescedRequests: s.joined, Caches: []cache.Stats{}, Bandwidth: s.opts.Bandwidth.Stats()}
	s.mu.Unlock()
	for _, p := range s.opts.Pools {
		st.Pools = append(st.Pools, p.Stats())
	}
	for _, c := range s.opts.Caches {
		st.Caches = append(st.Caches, c.Stats())
	}
	return st
}

// initialSteps lists progress steps: target validation (already done when
// the scan is created), one step per stage, and finalization.
// stagesFor returns the pipeline's stages that run in mode m.
func (s *Service) stagesFor(m Mode) []Stage {
	var out []Stage
	for _, st := range s.stages {
		if st.runsIn(m) {
			out = append(out, st)
		}
	}
	return out
}

func initialSteps(stages []Stage) []Step {
	steps := make([]Step, 0, len(stages)+2)
	steps = append(steps, Step{ID: validateStep, Label: "Validating target", Status: StepDone})
	for _, st := range stages {
		steps = append(steps, Step{ID: st.ID, Label: st.Label, Status: StepPending})
	}
	return append(steps, Step{ID: finalizeStep, Label: "Finalizing results", Status: StepPending})
}

// run executes one scan. Engine failures are recorded and the scan
// continues. The scan's context carries its resource account (request
// budget and wait accounting) and ends on cancellation, shutdown or the
// scan time limit; in every case the results collected so far are saved.
func (s *Service) run(ctx context.Context, j *job) {
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

	acct := resource.NewAccount(j.id, s.opts.MaxRequests).WithDownloadBudget(s.opts.MaxDownloadBytes)
	ctx = resource.WithAccount(ctx, acct)
	ctx, cancel := context.WithTimeoutCause(ctx, s.opts.ScanTimeout, errTimeout)
	defer cancel()

	tr.update(func(sc *Scan) {
		sc.Status = StatusRunning
		if sc.StartedAt == nil {
			t := started.UTC()
			sc.StartedAt = &t
		}
	})
	log.Info("scan started", "event", "scan_started", "target", j.target.StartURL)

	agg := results.NewAggregator(j.target, s.opts.Limits)
	// Every scan starts from the entered host and the apex domain, whether
	// or not any engine finds them.
	for _, h := range j.target.Hosts() {
		agg.Add(discovery.Finding{Host: h, Source: discovery.SourceTarget})
	}
	var phase atomic.Value // Phase
	phase.Store(PhaseSubdomains)
	stopProgress := s.trackProgress(tr, agg, acct, &phase)

	runs, failed := 0, 0
	for i, st := range j.stages {
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
			p := phaseFor(eng.Name())
			phase.Store(p)
			counts := agg.Counts()
			tr.update(func(sc *Scan) { sc.Phase, sc.Progress = p, phaseProgress(p, counts) })

			runs++
			count, err := s.runEngine(ctx, log, j.target, st, eng, agg)
			total += count
			var partial *discovery.PartialError
			switch {
			case err == nil, ctx.Err() != nil:
				// Being stopped by cancellation or the time limit is not an
				// engine failure; the stop reason explains it.
			case errors.As(err, &partial):
				tr.update(func(sc *Scan) {
					sc.Errors = append(sc.Errors, EngineError{Stage: st.ID, Engine: eng.Name(), Message: err.Error(), Partial: true})
				})
			default:
				failed++
				stageFailed++
				tr.update(func(sc *Scan) {
					sc.Errors = append(sc.Errors, EngineError{Stage: st.ID, Engine: eng.Name(), Message: err.Error()})
				})
			}
		}
		counts := agg.Counts()
		interrupted := ctx.Err() != nil
		tr.update(func(sc *Scan) {
			sc.Steps[step].Findings = total
			sc.Counts = counts
			switch {
			case interrupted:
				sc.Steps[step].Status = StepStopped
			case len(st.Engines) > 0 && stageFailed == len(st.Engines):
				sc.Steps[step].Status = StepFailed
			default:
				sc.Steps[step].Status = StepDone
			}
		})
	}
	stopProgress()

	finalIdx := len(j.stages) + 1
	tr.update(func(sc *Scan) {
		sc.Steps[finalIdx].Status = StepRunning
		sc.Phase, sc.Progress = PhaseFinalizing, nil
	})

	status, stop, msg := s.outcome(ctx, runs, failed)
	res := agg.Result()
	counts := res.Counts()
	limits := s.limitNotices(counts, acct, stop, true)

	var finished Scan
	tr.update(func(sc *Scan) {
		sc.Counts = counts
		sc.Limits = limits
		sc.Resources = resources(acct)
		sc.StopReason = stop
		sc.Phase = PhaseDone
		sc.Steps[finalIdx].Status = StepDone
		finish(sc, started, status, msg)
		finished = sc.Clone()
	})

	err = s.repo.SaveResult(context.Background(), Result{
		ScanID:     j.id,
		Status:     finished.Status,
		StopReason: stop,
		Domain: Domain{
			Mode:       finished.Mode,
			Target:     finished.Target,
			Canonical:  finished.Domain,
			StartURL:   finished.StartURL,
			ScannedAt:  started.UTC(),
			DurationMs: finished.DurationMs,
		},
		Errors: finished.Errors,
		Limits: finished.Limits,
		Counts: counts,
		Result: res,
	})
	if err != nil {
		log.Error("saving result failed", "event", "result_save_failed", "error", err)
	}

	event := "scan_" + string(status)
	if stop == StopTimeout {
		event = "scan_timed_out"
	}
	log.Info("scan finished", "event", event, "stop_reason", stop, "duration", time.Since(started).Round(time.Millisecond),
		"hosts", counts.Hosts, "hosts_resolved", counts.HostsResolved, "hosts_reachable", counts.HostsReachable,
		"urls", counts.URLs, "requests", acct.Requests(), "limits", len(limits), "engine_errors", len(finished.Errors))
}

// outcome decides a finished scan's status from why its context ended.
func (s *Service) outcome(ctx context.Context, runs, failed int) (Status, string, string) {
	if ctx.Err() != nil {
		switch cause := context.Cause(ctx); {
		case errors.Is(cause, errUserCancel):
			return StatusCancelled, StopCancel, "cancelled; results collected before cancellation are kept"
		case errors.Is(cause, errTimeout):
			// Timed-out scans complete with partial results and say so.
			return StatusCompleted, StopTimeout, ""
		default:
			return StatusCancelled, StopShutdown, "the server shut down while the scan was running"
		}
	}
	if runs > 0 && failed == runs {
		return StatusFailed, "", "every discovery step failed; see errors"
	}
	return StatusCompleted, "", ""
}

// limitNotices explains which budgets shaped the result. final is true once
// the pipeline has ended, when hosts still waiting will not be processed.
func (s *Service) limitNotices(c results.Counts, acct *resource.Account, stop string, final bool) []LimitNotice {
	notices := []LimitNotice{}
	add := func(code, format string, args ...any) {
		notices = append(notices, LimitNotice{Code: code, Message: fmt.Sprintf(format, args...)})
	}
	l := c.Limits
	if stop == StopTimeout {
		add(LimitScanTimeout, "The scan reached its time limit (%s) and stopped; results are partial.", s.opts.ScanTimeout)
	}
	if acct.RequestsExhausted() {
		add(LimitRequestBudget, "The scan used its budget of %d requests; remaining probing and crawling was skipped.", acct.MaxRequests())
	}
	if acct.DownloadExhausted() {
		add(LimitDownload, "The scan downloaded its limit of %d MB; remaining probing and crawling was skipped.", acct.MaxBytes()>>20)
	}
	if l.HostsOmitted > 0 {
		add(LimitHostBudget, "%d more hostnames were discovered after the limit of %d hosts per scan; they are not listed.",
			l.HostsOmitted, s.opts.Limits.MaxHosts)
	}
	if n := l.ResolveSkipped + l.ProbeSkipped + l.CrawlSkipped; n > 0 {
		add(LimitHostBudget, "Per-scan limits left hosts unprocessed: %d not resolved, %d not probed, %d not crawled.",
			l.ResolveSkipped, l.ProbeSkipped, l.CrawlSkipped)
	}
	if l.CrawlLimited > 0 {
		add(LimitCrawl, "%d hosts had more pages than the per-host request limit; their crawls stopped early.", l.CrawlLimited)
	}
	if final && stop == "" && c.HostsResolvePending > 0 {
		// The pipeline runs one follow-up round for hosts found while
		// crawling; hosts first seen in that round are listed unchecked.
		add(LimitDiscoveryRounds, "%d hosts were first found in the last crawl round; they are listed but were not resolved, probed or crawled.",
			c.HostsResolvePending)
	}
	if l.URLsOmitted > 0 {
		add(LimitURLBudget, "%d URLs were seen but not recorded because of the recorded-URL limits.", l.URLsOmitted)
	}
	// Waits are reported per operation: parallel workers wait at the same
	// time, so a total would overstate the delay.
	for _, name := range sortedKeys(acct.Usage()) {
		u := acct.Usage()[name]
		if u.Delayed == 0 {
			continue
		}
		if avg := u.Waited / time.Duration(u.Delayed); avg >= globalWaitNotice {
			add(LimitGlobalResource, "%d of %d %s operations waited for the server's shared capacity (average wait %s). "+
				"The server limits how much work all scans do at once; this made the scan slower but did not reduce what it found.",
				u.Delayed, u.Acquired, poolNoun(name), avg.Round(10*time.Millisecond))
		}
	}
	return notices
}

// runEngine runs one engine against the current state and returns how many
// findings it reported.
func (s *Service) runEngine(ctx context.Context, log *slog.Logger, t discovery.Target, st Stage, eng discovery.Engine, agg *results.Aggregator) (int, error) {
	var n atomic.Int64
	emit := func(f discovery.Finding) { n.Add(1); agg.Add(f) }
	start := time.Now()
	err := discover(ctx, eng, discovery.Input{Target: t, State: agg}, emit)
	count := int(n.Load())
	switch {
	case err != nil && ctx.Err() != nil:
		log.Info("discovery stopped", "event", "discovery_stopped", "stage", st.ID, "engine", eng.Name(),
			"count", count, "reason", context.Cause(ctx))
	case err != nil:
		log.Warn("discovery failed", "event", "discovery_failed", "stage", st.ID, "engine", eng.Name(),
			"count", count, "error", err)
	default:
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

// phaseFor maps an engine to the phase shown while it runs.
func phaseFor(engine string) Phase {
	switch engine {
	case "subdomains":
		return PhaseSubdomains
	case "dns":
		return PhaseResolving
	case "http":
		return PhaseProbing
	case "archive":
		return PhaseArchive
	case "sitemap":
		return PhaseSitemaps
	case "html":
		return PhaseCrawling
	}
	return Phase(engine)
}

// phaseProgress measures host-based phases from aggregate counters.
func phaseProgress(p Phase, c results.Counts) *PhaseProgress {
	var done, pending int
	switch p {
	case PhaseResolving:
		pending = c.HostsResolvePending
		done = c.Hosts - pending - c.Limits.ResolveSkipped
	case PhaseProbing:
		done, pending = c.HostsProbed, c.HostsProbePending
	case PhaseCrawling:
		done, pending = c.HostsCrawled, c.HostsCrawlPending
	default:
		return nil
	}
	return &PhaseProgress{Total: done + pending, Completed: done, Pending: pending}
}

func resources(acct *resource.Account) Resources {
	r := Resources{Requests: acct.Requests(), MaxRequests: acct.MaxRequests(),
		DownloadedBytes: acct.Bytes(), MaxDownloadBytes: acct.MaxBytes()}
	if usage := acct.Usage(); len(usage) > 0 {
		r.Pools = map[string]PoolUsage{}
		for name, u := range usage {
			pu := PoolUsage{Operations: u.Acquired, Delayed: u.Delayed}
			if u.Delayed > 0 {
				pu.AvgWaitMs = (u.Waited / time.Duration(u.Delayed)).Milliseconds()
			}
			r.Pools[name] = pu
		}
	}
	return r
}

func poolNoun(pool string) string {
	switch pool {
	case "http":
		return "HTTP request"
	case "dns":
		return "DNS lookup"
	case "certificate-transparency":
		return "certificate log query"
	}
	return pool
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// trackProgress periodically persists live counters until the returned stop
// function is called. Each update writes only fixed-size aggregates.
func (s *Service) trackProgress(tr *tracker, agg *results.Aggregator, acct *resource.Account, phase *atomic.Value) (stop func()) {
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
				p := phase.Load().(Phase)
				res := resources(acct)
				limits := s.limitNotices(counts, acct, "", false)
				tr.update(func(sc *Scan) {
					sc.Counts, sc.Progress, sc.Resources, sc.Limits = counts, phaseProgress(p, counts), res, limits
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
