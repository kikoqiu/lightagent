//go:build unix

package main

import (
	"os"
	"syscall"
)

// watchedSignals are the signals that end the process. SIGHUP matters here:
// closing the terminal hangs up the session, and the child trees live in their
// own process groups, so nothing else would terminate them.
var watchedSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}

// signalStatus maps a signal to the status shells report for it (128+N).
func signalStatus(sig os.Signal) int {
	if s, ok := sig.(syscall.Signal); ok {
		return 128 + int(s)
	}
	return 1
}
