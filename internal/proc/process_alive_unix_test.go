//go:build unix

package proc

import "syscall"

// processAlive reports whether a pid is still running; a reaped orphan reports
// ESRCH.
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
