// Package storetest opens result stores for tests.
package storetest

import (
	"io"
	"log/slog"
	"testing"

	"websitemapper/internal/store"
)

// New opens a store in a temporary directory that is removed, after the
// store is closed, when the test ends. opts.Dir is ignored.
func New(t testing.TB, opts store.Options) *store.Store {
	t.Helper()
	opts.Dir = t.TempDir()
	if opts.Log == nil {
		opts.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s, err := store.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
