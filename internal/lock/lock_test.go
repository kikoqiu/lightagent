package lock

import (
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
