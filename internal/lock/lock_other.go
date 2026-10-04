//go:build !windows && (!unix || aix)

package lock

import "os"

// lockFile falls back to an exclusive create on hosts without an advisory lock
// primitive: only one process can create the file, which is enough to keep a
// directory to one instance. A holder that dies without releasing leaves the
// file behind, which this fallback cannot tell from a live holder.
func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if os.IsExist(err) {
		return nil, errHeld
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

// unlockFile needs no work here: closing the file is what ends the exclusion.
func unlockFile(*os.File) error { return nil }
