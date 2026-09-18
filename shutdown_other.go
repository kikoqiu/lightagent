//go:build !unix && !windows

package main

import "os"

// watchedSignals are the signals that end the process; these hosts have no
// SIGTERM/SIGHUP to watch.
var watchedSignals = []os.Signal{os.Interrupt}

// signalStatus cannot decode a numeric signal here, so the exit status is the
// generic failure one.
func signalStatus(os.Signal) int { return 1 }
