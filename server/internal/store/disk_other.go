//go:build !linux

package store

// diskFree is only implemented for Linux, where the server is deployed;
// elsewhere the free-space check is skipped.
func diskFree(string) (uint64, bool) { return 0, false }
