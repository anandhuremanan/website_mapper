// Package store keeps scans and their results on disk, one SQLite database
// file per scan.
//
// Results live on disk rather than in memory so that the server's memory
// does not grow with how much scans find. One file per scan keeps scans
// independent: they never contend for a writer, a damaged file loses one
// result, and removing an old result is deleting its file, which returns
// the space at once (a shared database would have to be compacted).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"websitemapper/internal/scan"
)

// Options configures a Store. Zero limits mean no limit.
type Options struct {
	// Dir holds the scan databases. It is created if missing.
	Dir string
	// MaxScans bounds the finished scans kept.
	MaxScans int
	// MaxBytes bounds the disk space finished scans use together.
	MaxBytes int64
	// MaxAge is how long a finished scan is kept.
	MaxAge time.Duration
	// MinFreeBytes is the free disk space below which no scan is created.
	MinFreeBytes int64
	Log          *slog.Logger
}

// Store is a scan.Repository on disk.
//
// Status records are also held in memory (they are small and polled
// constantly) and written to disk when a scan is created and when it
// finishes. Queued and running scans are never removed. Finished scans are
// removed oldest first when there are more than MaxScans of them, when
// they use more than MaxBytes together, or once they are older than
// MaxAge; the scan that just finished is always kept.
//
// After a restart, finished scans are available again. Scans that were
// queued or running when the server stopped without finishing them are
// marked as failed and their partial data is dropped.
type Store struct {
	opts Options
	log  *slog.Logger

	mu     sync.Mutex
	scans  map[string]*entry
	order  []string // creation order
	bytes  int64    // disk used by finished scans
	closed bool
	// orphans are files that could not be deleted yet (for example because
	// a reader still had one open); they are retried.
	orphans []string

	stop chan struct{}
	done chan struct{}
}

type entry struct {
	scan scan.Scan
	// w is open while the scan records URLs.
	w *writer
	// hasResult is true once the scan has something to read: its final
	// result, or a snapshot saved while it runs.
	hasResult bool
	size      int64
}

// janitorInterval is how often expired scans are removed.
const janitorInterval = 10 * time.Minute

const dbExt = ".db"

// validID guards the file names built from scan IDs.
var validID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Open opens the store in opts.Dir, loading the scans already there.
func Open(opts Options) (*Store, error) {
	if opts.Dir == "" {
		return nil, errors.New("store: no data directory configured")
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if err := os.MkdirAll(opts.Dir, 0o750); err != nil {
		return nil, fmt.Errorf("store: creating data directory: %w", err)
	}
	s := &Store{opts: opts, log: opts.Log, scans: make(map[string]*entry), stop: make(chan struct{}), done: make(chan struct{})}
	if err := s.load(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.evictLocked("")
	s.mu.Unlock()
	go s.janitor()
	return s, nil
}

var _ scan.Repository = (*Store)(nil)

func (s *Store) path(id string) string { return filepath.Join(s.opts.Dir, id+dbExt) }

// load indexes the databases in the data directory. Files that cannot be
// read as a scan are deleted: they are leftovers, not results.
func (s *Store) load() error {
	files, err := os.ReadDir(s.opts.Dir)
	if err != nil {
		return fmt.Errorf("store: reading data directory: %w", err)
	}
	interrupted := 0
	for _, f := range files {
		name := f.Name()
		id, isDB := strings.CutSuffix(name, dbExt)
		if f.IsDir() || !isDB || !validID.MatchString(id) {
			continue // not ours (SQLite side files are removed with their database)
		}
		path := s.path(id)
		e, err := loadEntry(path)
		if err != nil || e.scan.ID != id {
			s.log.Warn("removing unreadable scan database", "event", "store_file_dropped", "file", name, "error", err)
			_ = removeDB(path)
			continue
		}
		if !e.scan.Status.Finished() {
			// The server stopped while this scan was queued or running.
			interrupted++
			markInterrupted(&e.scan)
			if err := removeDB(path); err == nil {
				err = createDB(path, e.scan)
			}
			if err != nil {
				s.log.Warn("could not rewrite an interrupted scan", "scan_id", id, "error", err)
				_ = removeDB(path)
				continue
			}
			e.hasResult = false
		}
		e.size = fileSize(path)
		s.scans[id] = e
		s.order = append(s.order, id)
		s.bytes += e.size
	}
	sort.SliceStable(s.order, func(i, j int) bool {
		return s.scans[s.order[i]].scan.CreatedAt.Before(s.scans[s.order[j]].scan.CreatedAt)
	})
	s.log.Info("result store ready", "event", "store_opened", "dir", s.opts.Dir, "scans", len(s.scans),
		"interrupted", interrupted, "bytes", s.bytes)
	return nil
}

func loadEntry(path string) (*entry, error) {
	ctx := context.Background()
	db, err := openDB(path, "query_only(1)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var version []byte
	if err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, metaSchema).Scan(&version); err != nil {
		return nil, err
	}
	if string(version) != schemaVersion {
		return nil, fmt.Errorf("schema version %q, want %q", version, schemaVersion)
	}
	e := &entry{}
	if found, err := getJSON(ctx, db, metaScan, &e.scan); err != nil || !found {
		return nil, fmt.Errorf("no status record: %w", err)
	}
	var one int
	switch err := db.QueryRowContext(ctx, `SELECT 1 FROM meta WHERE key = ?`, metaResult).Scan(&one); {
	case err == nil:
		e.hasResult = true
	case !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}
	return e, nil
}

// markInterrupted turns an unfinished status record into a failed one.
func markInterrupted(sc *scan.Scan) {
	now := time.Now().UTC()
	sc.Status, sc.Phase, sc.Progress = scan.StatusFailed, scan.PhaseDone, nil
	sc.StopReason = scan.StopShutdown
	sc.Error = "the server restarted before this scan finished; please run it again"
	sc.FinishedAt = &now
	sc.QueuePosition, sc.Subscribers = 0, 0
	for i := range sc.Steps {
		switch sc.Steps[i].Status {
		case scan.StepPending:
			sc.Steps[i].Status = scan.StepSkipped
		case scan.StepRunning:
			sc.Steps[i].Status = scan.StepStopped
		}
	}
}

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

func (s *Store) Create(_ context.Context, sc scan.Scan) error {
	if !validID.MatchString(sc.ID) {
		return fmt.Errorf("store: invalid scan ID %q", sc.ID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errClosed
	}
	if _, exists := s.scans[sc.ID]; exists {
		return fmt.Errorf("scan %s already exists", sc.ID)
	}
	if free, ok := diskFree(s.opts.Dir); ok && s.opts.MinFreeBytes > 0 && free < uint64(s.opts.MinFreeBytes) {
		s.log.Warn("refusing a scan: the disk is nearly full", "event", "store_full", "free_bytes", free)
		return scan.ErrStorageFull
	}
	path := s.path(sc.ID)
	_ = removeDB(path) // a leftover of an earlier, unindexed scan with this ID
	if err := createDB(path, sc); err != nil {
		_ = removeDB(path)
		return fmt.Errorf("store: creating scan database: %w", err)
	}
	s.scans[sc.ID] = &entry{scan: sc.Clone()}
	s.order = append(s.order, sc.ID)
	return nil
}

func (s *Store) Get(_ context.Context, id string) (scan.Scan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.scans[id]
	if !ok {
		return scan.Scan{}, scan.ErrNotFound
	}
	return e.scan.Clone(), nil
}

// Update replaces a scan's status record. Progress updates stay in memory;
// the record is written to disk when the scan finishes.
func (s *Store) Update(_ context.Context, sc scan.Scan) error {
	s.mu.Lock()
	e, ok := s.scans[sc.ID]
	if !ok {
		s.mu.Unlock()
		return scan.ErrNotFound
	}
	finished := sc.Status.Finished() && !e.scan.Status.Finished()
	e.scan = sc.Clone()
	w := e.w
	s.mu.Unlock()
	if !finished {
		return nil
	}

	var err error
	if w != nil {
		err = w.putStatus(sc)
	} else {
		err = updateStatus(s.path(sc.ID), sc)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.w == nil {
		// No result will follow: the file is as large as it gets.
		s.setSizeLocked(e, sc.ID)
	}
	s.evictLocked(sc.ID)
	return err
}

func (s *Store) OpenURLs(_ context.Context, id string) (scan.URLStore, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errClosed
	}
	e, ok := s.scans[id]
	if !ok {
		return nil, scan.ErrNotFound
	}
	if e.w != nil {
		return nil, fmt.Errorf("scan %s is already recording URLs", id)
	}
	w, err := openWriter(s.path(id))
	if err != nil {
		return nil, fmt.Errorf("store: opening scan database: %w", err)
	}
	e.w = w
	return &scanURLs{writer: w, store: s, id: id}, nil
}

// scanURLs is the scan.URLStore handed to a scan. Closing it also tells
// the store the scan is no longer writing.
type scanURLs struct {
	*writer
	store *Store
	id    string
}

func (u *scanURLs) Close() error {
	err := u.writer.Close()
	u.store.released(u.id, u.writer)
	return err
}

// released records that a scan's writer was closed.
func (s *Store) released(id string, w *writer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.scans[id]
	if !ok || e.w != w {
		return
	}
	e.w = nil
	if e.scan.Status.Finished() {
		s.setSizeLocked(e, id)
		s.evictLocked(id)
	}
}

// setSizeLocked measures a finished scan's file. Caller holds mu.
func (s *Store) setSizeLocked(e *entry, id string) {
	s.bytes -= e.size
	e.size = fileSize(s.path(id))
	s.bytes += e.size
}

// writerOf returns the open writer of a scan.
func (s *Store) writerOf(id string) (*writer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.scans[id]
	if !ok {
		return nil, scan.ErrNotFound
	}
	if e.w == nil {
		return nil, fmt.Errorf("scan %s is not recording URLs", id)
	}
	return e.w, nil
}

func (s *Store) markReadable(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.scans[id]; ok {
		e.hasResult = true
	}
}

// SaveSnapshot stores the result of a scan that is still running, so that
// it can be read while the scan continues.
func (s *Store) SaveSnapshot(_ context.Context, r scan.Result) error {
	w, err := s.writerOf(r.ScanID)
	if err != nil {
		return err
	}
	if err := w.save(r); err != nil {
		return fmt.Errorf("store: saving snapshot: %w", err)
	}
	s.markReadable(r.ScanID)
	return nil
}

func (s *Store) SaveResult(_ context.Context, r scan.Result) error {
	w, err := s.writerOf(r.ScanID)
	if err != nil {
		return err
	}
	if err := w.finish(r); err != nil {
		return fmt.Errorf("store: saving result: %w", err)
	}
	s.markReadable(r.ScanID)
	s.released(r.ScanID, w)
	return nil
}

func (s *Store) OpenResult(ctx context.Context, id string) (scan.ResultReader, error) {
	s.mu.Lock()
	e, ok := s.scans[id]
	ready := ok && e.hasResult
	s.mu.Unlock()
	if !ready {
		return nil, scan.ErrNotFound
	}
	path := s.path(id)
	if _, err := os.Stat(path); err != nil {
		return nil, scan.ErrNotFound // removed since the check above
	}
	return openReader(ctx, path)
}

// evictLocked removes finished scans that are past the store's limits,
// oldest first, never removing keep, a scan that is not finished, or one
// still being written. Caller holds mu.
func (s *Store) evictLocked(keep string) {
	s.retryOrphansLocked()
	finished := 0
	for _, e := range s.scans {
		if e.scan.Status.Finished() {
			finished++
		}
	}
	over := func() bool {
		return (s.opts.MaxScans > 0 && finished > s.opts.MaxScans) ||
			(s.opts.MaxBytes > 0 && s.bytes > s.opts.MaxBytes)
	}
	expired := func(e *entry) bool {
		return s.opts.MaxAge > 0 && e.scan.FinishedAt != nil && time.Since(*e.scan.FinishedAt) > s.opts.MaxAge
	}
	kept := s.order[:0]
	for _, id := range s.order {
		e := s.scans[id]
		if id != keep && e.w == nil && e.scan.Status.Finished() && (over() || expired(e)) {
			delete(s.scans, id)
			s.bytes -= e.size
			finished--
			if err := removeDB(s.path(id)); err != nil {
				s.orphans = append(s.orphans, s.path(id))
			}
			continue
		}
		kept = append(kept, id)
	}
	s.order = kept
}

func (s *Store) retryOrphansLocked() {
	left := s.orphans[:0]
	for _, path := range s.orphans {
		if err := removeDB(path); err != nil {
			left = append(left, path)
		}
	}
	s.orphans = left
}

func (s *Store) janitor() {
	defer close(s.done)
	t := time.NewTicker(janitorInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.mu.Lock()
			s.evictLocked("")
			s.mu.Unlock()
		}
	}
}

// Close stops the store and closes every open scan database. Scans still
// writing fail from then on.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	var writers []*writer
	for _, e := range s.scans {
		if e.w != nil {
			writers = append(writers, e.w)
		}
	}
	s.mu.Unlock()
	close(s.stop)
	<-s.done
	var err error
	for _, w := range writers {
		if cerr := w.Close(); err == nil {
			err = cerr
		}
	}
	return err
}

// Stats implements the scan service's storage report.
func (s *Store) Stats() scan.StorageStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := scan.StorageStats{Scans: len(s.scans), Bytes: s.bytes, MaxBytes: s.opts.MaxBytes}
	for _, e := range s.scans {
		if e.hasResult {
			st.Results++
		}
	}
	if free, ok := diskFree(s.opts.Dir); ok {
		st.FreeBytes = int64(free)
	}
	return st
}

var errClosed = errors.New("store: closed")
