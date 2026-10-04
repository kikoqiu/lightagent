//go:build unix && !aix

package lock

import (
	"errors"
	"os"
	"syscall"
)

// lockFile opens (creating when needed) path and takes an exclusive advisory
// lock on it without blocking, so a second holder fails instead of waiting for
// the first one to exit. The flock is tied to the open file description, so a
// second open — even in this process — conflicts with it.
func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errHeld
		}
		return nil, err
	}
	return f, nil
}

// unlockFile releases the advisory lock. Closing the file releases it as well,
// but the explicit unlock keeps the intent clear.
func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
