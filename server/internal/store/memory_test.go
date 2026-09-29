package store

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"websitemapper/internal/scan"
)

func TestMemoryCRUD(t *testing.T) {
	ctx := context.Background()
	m := NewMemory(Limits{MaxScans: 10})
	s := scan.Scan{ID: "a", Status: scan.StatusQueued, Steps: []scan.Step{{ID: "html", Status: scan.StepPending}}}
	if err := m.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := m.Create(ctx, s); err == nil {
		t.Error("duplicate create should fail")
	}

	got, err := m.Get(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	// Mutating the returned copy must not affect the store.
	got.Steps[0].Status = scan.StepDone
	again, _ := m.Get(ctx, "a")
	if again.Steps[0].Status != scan.StepPending {
		t.Error("Get returned shared state")
	}

	got.Status = scan.StatusRunning
	if err := m.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	again, _ = m.Get(ctx, "a")
	if again.Status != scan.StatusRunning {
		t.Error("update not applied")
	}

	if _, err := m.GetResult(ctx, "a"); !errors.Is(err, scan.ErrNotFound) {
		t.Errorf("GetResult before save: %v", err)
	}
	if err := m.SaveResult(ctx, scan.Result{ScanID: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.GetResult(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(ctx, "missing"); !errors.Is(err, scan.ErrNotFound) {
		t.Errorf("Get missing: %v", err)
	}
}

// finish creates a scan and moves it to a finished state with a result of
// the given number of URLs, as the service does.
func finish(t *testing.T, m *Memory, id string, urls int) {
	t.Helper()
	ctx := context.Background()
	if err := m.Create(ctx, scan.Scan{ID: id, Status: scan.StatusRunning}); err != nil {
		t.Fatal(err)
	}
	if err := m.Update(ctx, scan.Scan{ID: id, Status: scan.StatusCompleted}); err != nil {
		t.Fatal(err)
	}
	r := scan.Result{ScanID: id}
	r.Counts.URLs = urls
	if err := m.SaveResult(ctx, r); err != nil {
		t.Fatal(err)
	}
}

func exists(m *Memory, id string) bool {
	_, err := m.Get(context.Background(), id)
	return err == nil
}

func TestMemoryNeverEvictsActiveScans(t *testing.T) {
	ctx := context.Background()
	m := NewMemory(Limits{MaxScans: 2})
	// Far more queued/running scans than MaxScans: none may disappear, or
	// their progress would be lost and polls would return 404.
	for i := 0; i < 10; i++ {
		st := scan.StatusQueued
		if i < 3 {
			st = scan.StatusRunning
		}
		if err := m.Create(ctx, scan.Scan{ID: fmt.Sprint("active", i), Status: st}); err != nil {
			t.Fatal(err)
		}
	}
	finish(t, m, "done1", 1)
	finish(t, m, "done2", 1)
	finish(t, m, "done3", 1)
	for i := 0; i < 10; i++ {
		if !exists(m, fmt.Sprint("active", i)) {
			t.Errorf("active scan %d was evicted", i)
		}
	}
	if exists(m, "done1") || !exists(m, "done2") || !exists(m, "done3") {
		t.Error("expected only the oldest finished scan to be evicted")
	}
}

func TestMemoryEvictsByStoredURLs(t *testing.T) {
	m := NewMemory(Limits{MaxScans: 100, MaxURLs: 1000})
	finish(t, m, "a", 400)
	finish(t, m, "b", 400)
	finish(t, m, "c", 400) // 1200 > 1000: the oldest (a) goes
	if exists(m, "a") || !exists(m, "b") || !exists(m, "c") {
		t.Errorf("after c: a=%v b=%v c=%v", exists(m, "a"), exists(m, "b"), exists(m, "c"))
	}
	if st := m.Stats(); st.StoredURLs != 800 || st.Results != 2 {
		t.Errorf("stats = %+v", st)
	}
	// A single result larger than the budget is still kept (the newest
	// result is never evicted); everything older makes room for it.
	finish(t, m, "huge", 5000)
	if !exists(m, "huge") || exists(m, "b") || exists(m, "c") {
		t.Errorf("huge kept = %v, b = %v, c = %v", exists(m, "huge"), exists(m, "b"), exists(m, "c"))
	}
	if _, err := m.GetResult(context.Background(), "huge"); err != nil {
		t.Error(err)
	}
}
