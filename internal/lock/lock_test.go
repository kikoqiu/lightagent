package lock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestAcquireExcludesASecondHolder pins the core promise: while a directory is
// locked, a second Acquire on the same directory fails immediately instead of
// waiting for the first holder to exit.
func TestAcquireExcludesASecondHolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".lightagent")
	first, err := Acquire(dir)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer func() { _ = first.Release() }()

	if _, err := Acquire(dir); err == nil {
		t.Fatal("a second Acquire must fail while the directory is locked")
	}

	// Once the holder releases, the directory can be locked again.
	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	second, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
	_ = second.Release()
}

// TestReleaseRemovesTheEmptyDirectory pins the cleanup: the lock file goes with
// the lock, and so does the state directory when the lock was all it held.
func TestReleaseRemovesTheEmptyDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".lightagent")
	lk, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, LockFileName)); err != nil {
		t.Fatalf("the lock file should exist while the lock is held: %v", err)
	}
	if err := lk.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the empty state directory should be removed, stat err = %v", err)
	}
}

// TestReleaseKeepsADirectoryWithState pins the other half of the cleanup: a
// directory that still holds a session file survives the lock, only the lock
// file goes.
func TestReleaseKeepsADirectoryWithState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".lightagent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(dir, "session.json")
	if err := os.WriteFile(session, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lk, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := lk.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Stat(session); err != nil {
		t.Fatalf("the session file must survive the lock: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, LockFileName)); !os.IsNotExist(err) {
		t.Fatalf("the lock file should be gone, stat err = %v", err)
	}
}

// TestReleaseIsIdempotent pins that the exit path and a signal exit may both
// ask for the release without the second call doing any damage.
func TestReleaseIsIdempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".lightagent")
	lk, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := lk.Release(); err != nil {
		t.Fatalf("first Release: %v", err)
	}
	if err := lk.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
}

// TestHolderMoveMovesTheLockWithTheDirectory pins a /cd: the held lock moves to
// the state directory of the new working directory, so the new directory is
// guarded afterwards and the one left behind can be locked again by another
// instance.
func TestHolderMoveMovesTheLockWithTheDirectory(t *testing.T) {
	base := t.TempDir()
	first := filepath.Join(base, "first")
	second := filepath.Join(base, "second")
	for _, dir := range []string{first, second} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h := NewHolder(".lightagent")
	if err := h.Hold(filepath.Join(first, ".lightagent")); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	defer func() { _ = h.Release() }()

	entered := ""
	got, err := h.Move(second, func(dir string) (string, error) {
		entered = dir
		return dir, nil
	})
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	if entered != second || got != second {
		t.Fatalf("entered %q and returned %q, want %q for both", entered, got, second)
	}
	// The new directory is guarded ...
	if secondLock, err := Acquire(filepath.Join(second, ".lightagent")); err == nil {
		_ = secondLock.Release()
		t.Fatal("the new directory must be locked")
	}
	// ... and the one left behind is free, so another instance can start there.
	freed, err := Acquire(filepath.Join(first, ".lightagent"))
	if err != nil {
		t.Fatalf("the directory left behind must be free again: %v", err)
	}
	_ = freed.Release()
}

// TestHolderMoveRefusesADirectoryAnotherInstanceHolds pins the conflict rule: a
// /cd into a directory a second instance runs in fails before anything changes —
// the process is not entered and the lock the instance holds stays.
func TestHolderMoveRefusesADirectoryAnotherInstanceHolds(t *testing.T) {
	base := t.TempDir()
	first := filepath.Join(base, "first")
	taken := filepath.Join(base, "taken")
	h := NewHolder(".lightagent")
	if err := h.Hold(filepath.Join(first, ".lightagent")); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	defer func() { _ = h.Release() }()
	other, err := Acquire(filepath.Join(taken, ".lightagent"))
	if err != nil {
		t.Fatalf("lock the other instance's directory: %v", err)
	}
	defer func() { _ = other.Release() }()

	entered := false
	if _, err := h.Move(taken, func(dir string) (string, error) {
		entered = true
		return dir, nil
	}); err == nil {
		t.Fatal("a /cd into a held directory must fail")
	}
	if entered {
		t.Fatal("the process must not enter a directory it cannot lock")
	}
	// The lock the instance holds is untouched.
	if own, err := Acquire(filepath.Join(first, ".lightagent")); err == nil {
		_ = own.Release()
		t.Fatal("the previous lock must stay after a refused move")
	}
}

// TestHolderMoveGivesTheNewLockBackWhenEnteringFails pins the aborted move: a
// directory that cannot be entered leaves no lock — and no state directory that
// had to be created for the attempt — behind, while the previous lock stays.
func TestHolderMoveGivesTheNewLockBackWhenEnteringFails(t *testing.T) {
	base := t.TempDir()
	first := filepath.Join(base, "first")
	broken := filepath.Join(base, "broken")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	h := NewHolder(".lightagent")
	if err := h.Hold(filepath.Join(first, ".lightagent")); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	defer func() { _ = h.Release() }()

	failed := errors.New("cannot enter the directory")
	if _, err := h.Move(broken, func(dir string) (string, error) {
		return "", failed
	}); !errors.Is(err, failed) {
		t.Fatalf("Move = %v, want the enter error", err)
	}
	if _, err := os.Stat(filepath.Join(broken, ".lightagent")); !os.IsNotExist(err) {
		t.Fatalf("the aborted attempt left its state directory behind, stat err = %v", err)
	}
	if own, err := Acquire(filepath.Join(first, ".lightagent")); err == nil {
		_ = own.Release()
		t.Fatal("the previous lock must stay after a failed move")
	}
}

// TestHolderMoveRefusesAMissingDirectory pins that a mistyped /cd fails instead
// of being created: the state directory would be made below it, and creating
// the working directory is not what a refused /cd may do.
func TestHolderMoveRefusesAMissingDirectory(t *testing.T) {
	base := t.TempDir()
	first := filepath.Join(base, "first")
	missing := filepath.Join(base, "missing")
	h := NewHolder(".lightagent")
	if err := h.Hold(filepath.Join(first, ".lightagent")); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	defer func() { _ = h.Release() }()

	entered := false
	if _, err := h.Move(missing, func(dir string) (string, error) {
		entered = true
		return dir, nil
	}); err == nil {
		t.Fatal("a /cd into a missing directory must fail")
	}
	if entered {
		t.Fatal("the process must not enter a directory that does not exist")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("the missing directory must not be created, stat err = %v", err)
	}
	if own, err := Acquire(filepath.Join(first, ".lightagent")); err == nil {
		_ = own.Release()
		t.Fatal("the previous lock must stay after a refused move")
	}
}

// TestHolderMoveTreatsASymlinkedDirectoryAsTheSame pins the identity check: a
// working directory reached through a symbolic link is the directory the
// instance already guards, so entering it is not a conflict with its own lock.
func TestHolderMoveTreatsASymlinkedDirectoryAsTheSame(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("symbolic links are not available here: %v", err)
	}
	h := NewHolder(".lightagent")
	if err := h.Hold(filepath.Join(dir, ".lightagent")); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	defer func() { _ = h.Release() }()

	entered := ""
	if _, err := h.Move(link, func(d string) (string, error) {
		entered = d
		return d, nil
	}); err != nil {
		t.Fatalf("Move through a symbolic link: %v", err)
	}
	if entered == "" {
		t.Fatal("the process must enter the linked directory")
	}
}

// TestHolderMoveToTheGuardedDirectory pins that a /cd into the directory the
// instance already guards is not a conflict with its own lock.
func TestHolderMoveToTheGuardedDirectory(t *testing.T) {
	dir := t.TempDir()
	h := NewHolder(".lightagent")
	if err := h.Hold(filepath.Join(dir, ".lightagent")); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	defer func() { _ = h.Release() }()

	got, err := h.Move(dir, func(d string) (string, error) { return d, nil })
	if err != nil {
		t.Fatalf("Move to the guarded directory: %v", err)
	}
	if got != dir {
		t.Fatalf("Move returned %q, want %q", got, dir)
	}
	if own, err := Acquire(filepath.Join(dir, ".lightagent")); err == nil {
		_ = own.Release()
		t.Fatal("the directory must still be guarded")
	}
	if err := h.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := h.Release(); err != nil {
		t.Fatalf("a second Release must be a no-op: %v", err)
	}
}
