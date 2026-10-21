package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// agedTempFile writes a file and gives it the given last-access and last-write
// times.
func agedTempFile(t *testing.T, path string, accessed, written time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chtimes(path, accessed, written); err != nil {
		t.Fatalf("set times on %s: %v", path, err)
	}
}

// tempPath names a file below the model's temp folder in workdir.
func tempPath(t *testing.T, workdir string, elems ...string) string {
	t.Helper()
	return filepath.Join(append([]string{workdir, DirName, TempDirName}, elems...)...)
}

// TestPruneTempDeletesOnlyFilesOldInBothTimes pins the rule: a file goes only
// when its last write and its last access are both older than the limit, so a
// result written long ago but still read, or touched but not read, survives.
// Nested files are walked too.
func TestPruneTempDeletesOnlyFilesOldInBothTimes(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	old := now.Add(-TempMaxAge - 24*time.Hour)
	fresh := now.Add(-time.Hour)

	oldFile := tempPath(t, dir, "old.txt")
	nested := tempPath(t, dir, "sub", "old.log")
	freshFile := tempPath(t, dir, "fresh.txt")
	recentlyRead := tempPath(t, dir, "recently-read.txt")
	recentlyWritten := tempPath(t, dir, "recently-written.txt")

	agedTempFile(t, oldFile, old, old)
	agedTempFile(t, nested, old, old)
	agedTempFile(t, freshFile, fresh, fresh)
	agedTempFile(t, recentlyRead, fresh, old)
	agedTempFile(t, recentlyWritten, old, fresh)

	removed, err := PruneTemp(dir, now, TempMaxAge)
	if err != nil {
		t.Fatalf("PruneTemp: %v", err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed = %v, want the two files old in both times", removed)
	}
	gone := map[string]bool{oldFile: true, nested: true}
	for _, path := range removed {
		if !gone[path] {
			t.Fatalf("removed %q, want only %v", path, gone)
		}
	}
	for _, kept := range []string{freshFile, recentlyRead, recentlyWritten} {
		if _, err := os.Stat(kept); err != nil {
			t.Fatalf("kept file %s: %v", kept, err)
		}
	}
	for _, path := range []string{oldFile, nested} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("old file %s still present, stat err = %v", path, err)
		}
	}
}

// TestPruneTempWithoutTempDir verifies a run that never wrote a temp file is a
// no-op rather than an error.
func TestPruneTempWithoutTempDir(t *testing.T) {
	removed, err := PruneTemp(t.TempDir(), time.Now(), TempMaxAge)
	if err != nil || len(removed) != 0 {
		t.Fatalf("PruneTemp = (%v, %v), want nothing and no error", removed, err)
	}
	// An unknown working directory and a disabled limit do nothing either.
	if removed, err := PruneTemp("", time.Now(), TempMaxAge); err != nil || len(removed) != 0 {
		t.Fatalf("PruneTemp with no workdir = (%v, %v)", removed, err)
	}
	if removed, err := PruneTemp(t.TempDir(), time.Now(), 0); err != nil || len(removed) != 0 {
		t.Fatalf("PruneTemp with no limit = (%v, %v)", removed, err)
	}
}
