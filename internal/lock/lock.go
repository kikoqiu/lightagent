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
	"runtime"
	"strings"
	"sync"
	"time"
)

// LockFileName is the file the directory lock lives in.
const LockFileName = ".lock"

// acquireRetryInterval is the pause between the attempts of AcquireWait. A
// hand-over takes milliseconds, so a few attempts per 100ms are plenty and cost
// nothing while the wait is not needed.
const acquireRetryInterval = 10 * time.Millisecond

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
func Acquire(dir string) (*Lock, error) { return AcquireWait(dir, 0) }

// AcquireWait is Acquire with a deadline: it keeps trying for at most wait while
// another holder owns the directory, and fails at once on anything else. It is
// for the one process that expects to inherit a lock instead of competing for
// it — the replacement process a restart starts on the hosts that cannot replace
// their running image, which is created while the process it replaces still holds
// the lock (see the program's restartProgram). That process gives the lock up a
// moment later, so the replacement waits for the hand-over rather than reporting
// the instance conflict that is already ending; a directory that is genuinely
// taken is still reported at the deadline. wait <= 0 makes it Acquire.
func AcquireWait(dir string, wait time.Duration) (*Lock, error) {
	deadline := time.Now().Add(wait)
	for {
		lk, err := acquire(dir)
		if err == nil {
			return lk, nil
		}
		// The hand-over on the other side is two steps — unlock, then remove the
		// lock file and, when it was the last thing in it, the state directory —
		// so an attempt can land in between and find the file or the directory
		// gone. That is the same race as the lock being held and is retried the
		// same way.
		retry := errors.Is(err, errHeld) || os.IsNotExist(err)
		if !retry || wait <= 0 || !time.Now().Before(deadline) {
			if errors.Is(err, errHeld) {
				err = fmt.Errorf("another lightagent instance is already running in %s", filepath.Dir(dir))
			}
			return nil, err
		}
		time.Sleep(acquireRetryInterval)
	}
}

// acquire is one attempt at the lock of dir. It reports a racing holder as
// errHeld, so AcquireWait can tell it from an I/O failure.
func acquire(dir string) (*Lock, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create state directory %s: %w", dir, err)
	}
	path := filepath.Join(dir, LockFileName)
	f, err := lockFile(path)
	if errors.Is(err, errHeld) {
		return nil, errHeld
	}
	if err != nil {
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	// A holder hands the lock over by unlocking and then removing the lock file
	// (see Release), so the flock just taken can be on the very file that was
	// removed: it guards nothing, because the next instance creates a new file
	// and locks that while this process holds the old one. An attempt that lost
	// that race is reported as a conflict, which is what it is.
	if !lockFileStillIs(f, path) {
		_ = unlockFile(f)
		_ = f.Close()
		return nil, errHeld
	}
	return &Lock{file: f, path: path, dir: dir}, nil
}

// lockFileStillIs reports whether path still names the file that was locked.
func lockFileStillIs(f *os.File, path string) bool {
	held, err := f.Stat()
	if err != nil {
		return false
	}
	onDisk, err := os.Stat(path)
	if err != nil {
		return false
	}
	return os.SameFile(held, onDisk)
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

// Holder is the one directory lock a running instance holds. The lock follows
// the working directory a /cd moves to (see Move), so an instance keeps
// guarding the directory it actually runs in — and refuses to move into one
// another instance already guards — instead of leaving a stale lock behind.
type Holder struct {
	// stateDirName is the directory a working directory keeps its state in
	// (".lightagent"), so Move can find the state directory of the directory
	// the process moves to.
	stateDirName string

	mu sync.Mutex
	l  *Lock
}

// NewHolder creates an empty holder. stateDirName is the state directory below
// a working directory (store.DirName), which Move locks.
func NewHolder(stateDirName string) *Holder {
	return &Holder{stateDirName: stateDirName}
}

// Hold takes the lock of the state directory dir, releasing a lock held before
// only after the new one is in hand. A directory another instance already holds
// fails here and leaves the previous lock — and everything else — as it was.
func (h *Holder) Hold(dir string) error { return h.HoldWait(dir, 0) }

// HoldWait is Hold with the deadline of AcquireWait: it waits up to wait for a
// holder that is giving the directory up, which is what a restart replacement
// needs (see AcquireWait). A /cd never waits: the directory is either free or
// the move is refused.
func (h *Holder) HoldWait(dir string, wait time.Duration) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	next, err := AcquireWait(dir, wait)
	if err != nil {
		return err
	}
	h.swapLocked(next)
	return nil
}

// Move takes the state-directory lock of workdir, enters that directory through
// enter and only then gives up the previous lock: the instance guards exactly
// the directory it runs in, and the directory it left is free again. Nothing
// changes when another instance already holds the new directory — enter is not
// called and the previous lock stays — and a failed enter hands the new lock
// back, leaving no trace behind when its state directory had to be created for
// the attempt. It returns the absolute directory the process now runs in.
func (h *Holder) Move(workdir string, enter func(dir string) (string, error)) (string, error) {
	abs, err := filepath.Abs(workdir)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", workdir, err)
	}
	// The state directory is created below the target, so that directory has to
	// exist first: a mistyped path must fail here instead of being created just
	// to hold the lock.
	if info, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("enter %s: %w", abs, err)
	} else if !info.IsDir() {
		return "", fmt.Errorf("enter %s: not a directory", abs)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.l != nil && sameDirectory(h.l.dir, filepath.Join(abs, h.stateDirName)) {
		// Moving to the directory already guarded needs no new lock: taking it
		// again would conflict with this very process.
		return enter(abs)
	}
	next, err := Acquire(filepath.Join(abs, h.stateDirName))
	if err != nil {
		return "", err
	}
	now, err := enter(abs)
	if err != nil {
		// The process did not move: hand the new lock back, so the directory
		// stays as free as it was found.
		_ = next.Release()
		return "", err
	}
	h.swapLocked(next)
	return now, nil
}

// Release gives up the held lock. It is safe to call more than once, from the
// exit path and a signal exit at the same time.
func (h *Holder) Release() error {
	h.mu.Lock()
	prev := h.l
	h.l = nil
	h.mu.Unlock()
	return prev.Release()
}

// swapLocked makes next the held lock and releases the previous one. The caller
// must hold h.mu.
func (h *Holder) swapLocked(next *Lock) {
	prev := h.l
	h.l = next
	_ = prev.Release()
}

// sameDirectory reports whether two paths name the same directory: both are
// asked for their file identity when they exist, so a working directory reached
// through a symbolic link counts as the directory itself. A path that does not
// exist falls back to the cleaned spelling (folded on Windows, where paths are
// case-insensitive).
func sameDirectory(a, b string) bool {
	ai, aerr := os.Stat(a)
	bi, berr := os.Stat(b)
	if aerr == nil && berr == nil {
		return os.SameFile(ai, bi)
	}
	return samePath(a, b)
}

// samePath reports whether two cleaned paths spell the same directory. Windows
// paths are case-insensitive, so they compare folded there.
func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
