package config_test

import (
	"io/fs"
	"os"
	"syscall"
	"testing"
)

// writeFile plants a config file with an exact mode, bypassing Save so that
// Load can be tested against files Save would never have written.
func writeFile(t *testing.T, path, contents string, mode fs.FileMode) {
	t.Helper()

	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	// WriteFile obeys the umask, so the mode is set explicitly afterwards.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

// setUmask makes the process umask permissive for the duration of a test, so a
// permissions assertion proves Save sets the mode explicitly rather than
// inheriting a tight umask from the test runner. It returns a function that
// restores the previous value, and also restores it at cleanup.
func setUmask(t *testing.T, mask int) func() {
	t.Helper()

	previous := syscall.Umask(mask)
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		syscall.Umask(previous)
	}
	t.Cleanup(restore)
	return restore
}
