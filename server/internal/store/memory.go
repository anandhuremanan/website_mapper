// Package store contains scan.Repository implementations.
package store

import (
	"context"
	"fmt"
	"sync"

	"websitemapper/internal/scan"
)

// Memory is an in-memory scan.Repository.
//
// Queued and running scans are never evicted. Finished scans are evicted
// oldest first when there are more than MaxScans of them, or when their
// stored results together hold more than MaxURLs URLs. The URL count is the
// memory measure: a stored result costs roughly 0.6 KB per URL, while status
// records are a few KB each. The most recently saved result is always kept,
// even if it alone exceeds MaxURLs.
type Memory struct {
	limits Limits

	mu         sync.RWMutex
	scans      map[string]scan.Scan
	results    map[string]scan.Result
	resultURLs map[string]int
	storedURLs int
	order      []string // creation order
}

// Limits bound what Memory retains. Zero means no limit.
type Limits struct {
	// MaxScans bounds finished scans kept (status and result).
	MaxScans int
	// MaxURLs bounds the URLs held by stored results in total.
	MaxURLs int
}

// NewMemory creates a Memory repository.
func NewMemory(limits Limits) *Memory {
	return &Memory{
		limits:     limits,
		scans:      make(map[string]scan.Scan),
		results:    make(map[string]scan.Result),
		resultURLs: make(map[string]int),
	}
}

var _ scan.Repository = (*Memory)(nil)

func (m *Memory) Create(_ context.Context, s scan.Scan) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.scans[s.ID]; exists {
		return fmt.Errorf("scan %s already exists", s.ID)
	}
	m.scans[s.ID] = s.Clone()
	m.order = append(m.order, s.ID)
	return nil
}

func (m *Memory) Get(_ context.Context, id string) (scan.Scan, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.scans[id]
	if !ok {
		return scan.Scan{}, scan.ErrNotFound
	}
	return s.Clone(), nil
}

func (m *Memory) Update(_ context.Context, s scan.Scan) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.scans[s.ID]
	if !ok {
		return scan.ErrNotFound
	}
	m.scans[s.ID] = s.Clone()
	if s.Status.Finished() && !old.Status.Finished() {
		m.evictLocked(s.ID)
	}
	return nil
}

// SaveResult stores a result. Results are treated as immutable once saved,
// so they are shared rather than deep-copied.
func (m *Memory) SaveResult(_ context.Context, r scan.Result) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.scans[r.ScanID]; !ok {
		return scan.ErrNotFound
	}
	m.storedURLs -= m.resultURLs[r.ScanID]
	m.results[r.ScanID] = r
	m.resultURLs[r.ScanID] = r.Counts.URLs
	m.storedURLs += r.Counts.URLs
	m.evictLocked(r.ScanID)
	return nil
}

func (m *Memory) GetResult(_ context.Context, id string) (scan.Result, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.results[id]
	if !ok {
		return scan.Result{}, scan.ErrNotFound
	}
	return r, nil
}

// evictLocked removes the oldest finished scans until the limits hold,
// never removing keep or any unfinished scan. Caller holds mu.
func (m *Memory) evictLocked(keep string) {
	finished := 0
	for _, s := range m.scans {
		if s.Status.Finished() {
			finished++
		}
	}
	over := func() bool {
		return (m.limits.MaxScans > 0 && finished > m.limits.MaxScans) ||
			(m.limits.MaxURLs > 0 && m.storedURLs > m.limits.MaxURLs)
	}
	if !over() {
		return
	}
	kept := m.order[:0]
	for _, id := range m.order {
		s := m.scans[id]
		if over() && id != keep && s.Status.Finished() {
			delete(m.scans, id)
			delete(m.results, id)
			m.storedURLs -= m.resultURLs[id]
			delete(m.resultURLs, id)
			finished--
			continue
		}
		kept = append(kept, id)
	}
	m.order = kept
}

// Stats reports what the repository holds.
type Stats struct {
	Scans      int `json:"scans"`
	Results    int `json:"results"`
	StoredURLs int `json:"storedUrls"`
}

// Stats returns a snapshot of the repository's size.
func (m *Memory) Stats() Stats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return Stats{Scans: len(m.scans), Results: len(m.results), StoredURLs: m.storedURLs}
}
