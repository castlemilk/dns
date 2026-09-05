//go:build unix

package config

import (
	"os"
	"syscall"
)

// sameFilesystem reports whether two directories live on the same device. The
// second return value is false when the platform or the filesystem cannot
// answer, in which case the caller must not treat the check as a failure.
func sameFilesystem(first, second string) (bool, bool) {
	firstInfo, err := os.Stat(first)
	if err != nil {
		return false, false
	}
	secondInfo, err := os.Stat(second)
	if err != nil {
		return false, false
	}
	firstStat, ok := firstInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return false, false
	}
	secondStat, ok := secondInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return false, false
	}
	return firstStat.Dev == secondStat.Dev, true
}
