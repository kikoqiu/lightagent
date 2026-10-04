//go:build windows

package lock

import (
	"os"
	"syscall"
	"unsafe"
)

// LockFileEx flags: an exclusive lock that fails at once instead of waiting for
// the current holder to release it.
const (
	lockfileFailImmediately = 0x00000001
	lockfileExclusiveLock   = 0x00000002
)

// errorLockViolation is ERROR_LOCK_VIOLATION: the byte range is already locked.
const errorLockViolation = syscall.Errno(33)

var (
	lockKernel32 = syscall.NewLazyDLL("kernel32.dll")

	procLockFileEx   = lockKernel32.NewProc("LockFileEx")
	procUnlockFileEx = lockKernel32.NewProc("UnlockFileEx")
)

// lockFile opens (creating when needed) path and takes an exclusive byte-range
// lock over its first byte without blocking, so a second holder fails instead
// of waiting for the first one to exit. Every holder locks the same byte, which
// is what makes the file a single directory lock.
func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	var overlapped syscall.Overlapped
	r, _, callErr := procLockFileEx.Call(
		f.Fd(),
		uintptr(lockfileExclusiveLock|lockfileFailImmediately),
		0,
		1,
		0,
		uintptr(unsafe.Pointer(&overlapped)),
	)
	if r == 0 {
		_ = f.Close()
		if errno, ok := callErr.(syscall.Errno); ok && errno == errorLockViolation {
			return nil, errHeld
		}
		return nil, callErr
	}
	return f, nil
}

// unlockFile releases the byte-range lock taken by lockFile.
func unlockFile(f *os.File) error {
	var overlapped syscall.Overlapped
	r, _, callErr := procUnlockFileEx.Call(
		f.Fd(),
		0,
		1,
		0,
		uintptr(unsafe.Pointer(&overlapped)),
	)
	if r == 0 {
		return callErr
	}
	return nil
}
