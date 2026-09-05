//go:build unix

package hosting

import "syscall"

// freeBytes reports the space available to an unprivileged writer in dir. The
// second result is false when the platform cannot answer, in which case the
// caller must not refuse the upload on that ground.
func freeBytes(dir string) (uint64, bool) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return 0, false
	}
	if stat.Bsize <= 0 {
		return 0, false
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), true
}
