//go:build integration && unix

package testreap

import (
	"errors"
	"os"
	"syscall"
)

// running reports whether a process with this ID exists. Signal 0 checks
// without signalling; EPERM means it exists but belongs to another user.
func running(pid int) bool {
	if pid == os.Getpid() {
		return true
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
