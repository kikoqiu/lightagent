// Package lock guards a state directory so that a directory holds at most one
// running lightagent instance. The guard is an advisory lock on
// <state directory>/.lock taken with the platform's own primitive — flock on
// Unix, LockFileEx on Windows — so no external library is involved.
//
// The lock file (and the directory around it) is created when the lock is
// taken and removed again on release; a state directory that held nothing but
// the lock is removed with it, so a run that never wrote a session leaves no
// trace on disk (see Release).
package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// LockFileName is the file the directory lock lives in.
const LockFileName = ".lock"

// errHeld reports that another holder already owns the lock. The platform
// implementations return it so Acquire can tell a conflict from an I/O failure.
var errHeld = errors.New("state directory is locked")

// Lock is a held directory lock.
type Lock struct {
	file *os.File
	path string
	dir  string

	once sync.Once
}

// Acquire takes the exclusive lock for the state directory dir, creating the
// directory and the lock file when they do not exist yet. A second holder for
// the same directory fails here immediately instead of waiting for the first
// one to exit, so a directory runs at most one instance at a time.
func Acquire(dir string) (*Lock, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create state directory %s: %w", dir, err)
	}
	path := filepath.Join(dir, LockFileName)
	f, err := lockFile(path)
	if errors.Is(err, errHeld) {
		return nil, fmt.Errorf("another lightagent instance is already running in %s", filepath.Dir(dir))
	}
	if err != nil {
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return &Lock{file: f, path: path, dir: dir}, nil
}

// Release gives up the lock: it unlocks and closes the lock file, removes it
// and, when the state directory holds nothing else, the directory itself. It is
// safe to call more than once, because the exit path and a signal exit may both
// ask for it.
func (l *Lock) Release() error {
	if l == nil {
		return nil
	}
	var err error
	l.once.Do(func() {
		if uerr := unlockFile(l.file); uerr != nil {
			err = uerr
		}
		if cerr := l.file.Close(); err == nil {
			err = cerr
		}
		l.file = nil
		if rerr := os.Remove(l.path); rerr != nil && !os.IsNotExist(rerr) && err == nil {
			err = rerr
		}
		l.removeDirIfEmpty()
	})
	return err
}

// removeDirIfEmpty removes the state directory when the lock file was the last
// thing in it, so a run that never wrote a session leaves no trace on disk.
func (l *Lock) removeDirIfEmpty() {
	entries, err := os.ReadDir(l.dir)
	if err != nil || len(entries) > 0 {
		return
	}
	_ = os.Remove(l.dir)
}
