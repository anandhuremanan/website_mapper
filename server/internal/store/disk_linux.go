package store

import "syscall"

// diskFree returns the space available to the server on dir's file system.
func diskFree(dir string) (uint64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, false
	}
	return st.Bavail * uint64(st.Bsize), true
}
