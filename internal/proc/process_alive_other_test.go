//go:build !unix && !windows

package proc

// processAlive cannot be answered on this host; the tree tests skip themselves
// before asking (see skipWithoutLivenessCheck).
func processAlive(int) bool { return true }
