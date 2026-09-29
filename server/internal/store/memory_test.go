package store

import (
	"context"
	"errors"
	"testing"

	"websitemapper/internal/scan"
)

func TestMemoryCRUD(t *testing.T) {
	ctx := context.Background()
	m := NewMemory(10)
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

func TestMemoryEvictsOldest(t *testing.T) {
	ctx := context.Background()
	m := NewMemory(2)
	for _, id := range []string{"a", "b", "c"} {
		if err := m.Create(ctx, scan.Scan{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Get(ctx, "a"); !errors.Is(err, scan.ErrNotFound) {
		t.Error("oldest scan should be evicted")
	}
	if _, err := m.Get(ctx, "c"); err != nil {
		t.Error("newest scan should remain")
	}
}
