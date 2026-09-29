// Package store contains scan.Repository implementations.
package store

import (
	"context"
	"fmt"
	"sync"

	"websitemapper/internal/scan"
)

// Memory is an in-memory scan.Repository. When more than limit scans are
// stored, the oldest scans (and their results) are evicted.
type Memory struct {
	limit int

	mu      sync.RWMutex
	scans   map[string]scan.Scan
	results map[string]scan.Result
	order   []string
}

// NewMemory creates a Memory repository keeping at most limit scans.
func NewMemory(limit int) *Memory {
	if limit < 1 {
		limit = 1
	}
	return &Memory{
		limit:   limit,
		scans:   make(map[string]scan.Scan),
		results: make(map[string]scan.Result),
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
	for len(m.order) > m.limit {
		oldest := m.order[0]
		m.order = m.order[1:]
		delete(m.scans, oldest)
		delete(m.results, oldest)
	}
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
	if _, ok := m.scans[s.ID]; !ok {
		return scan.ErrNotFound
	}
	m.scans[s.ID] = s.Clone()
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
	m.results[r.ScanID] = r
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
