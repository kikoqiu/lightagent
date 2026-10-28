//go:build unix

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCheckExecutableRejectsWhatExecCannotStart pins the check that runs while the
// run is still intact: the image is only replaced with a file that exists and can
// be executed, so a directory or a file without the execute bit is refused instead
// of being handed over to.
func TestCheckExecutableRejectsWhatExecCannotStart(t *testing.T) {
	dir := t.TempDir()
	if err := checkExecutable(dir); err == nil {
		t.Error("a directory must not be accepted as the program")
	}
	file := filepath.Join(dir, "lightagent")
	if err := os.WriteFile(file, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkExecutable(file); err == nil {
		t.Error("a file without the execute bit must be refused")
	}
	if err := os.Chmod(file, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkExecutable(file); err != nil {
		t.Errorf("checkExecutable of an executable file: %v", err)
	}
}
