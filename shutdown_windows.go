//go:build windows

package main

import (
	"os"
	"syscall"
)

// watchedSignals are the signals that end the process. Windows delivers Ctrl+C
// (and Ctrl+Break) as os.Interrupt; SIGTERM cannot be raised here but is
// harmless to watch, and SIGBREAK is already reported as an interrupt by the
// Go runtime.
var watchedSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

// signalStatus maps a signal to the status shells report for it (128+N).
func signalStatus(sig os.Signal) int {
	if s, ok := sig.(syscall.Signal); ok {
		return 128 + int(s)
	}
	return 1
}
