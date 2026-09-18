//go:build windows

package proc

import "syscall"

// stillActive is STILL_ACTIVE, the exit code a running process reports.
const stillActive = 259

// processAlive reports whether a pid is still running. Opening the process
// succeeds for a terminated process whose kernel object is still referenced, so
// the exit code decides: a terminated process reports its exit code instead.
func processAlive(pid int) bool {
	h, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}
