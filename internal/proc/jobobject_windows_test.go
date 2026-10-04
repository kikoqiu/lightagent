//go:build windows

package proc

import (
	"syscall"
	"testing"
	"unsafe"
)

// TestJobStructureMatchesTheProcessLayout pins the size the kill-on-close job
// structure is filled with. SetInformationJobObject only accepts
// JOBOBJECT_EXTENDED_LIMIT_INFORMATION when its buffer matches this process's
// layout — 144 bytes with 8-byte pointers (64-bit), 112 with 4-byte pointers
// (32-bit) — and otherwise fails with ERROR_BAD_LENGTH, so no job is ever
// established and Reap cannot remove a tree the root left behind.
func TestJobStructureMatchesTheProcessLayout(t *testing.T) {
	pointer := unsafe.Sizeof(uintptr(0))
	want := uintptr(112)
	if pointer == 8 {
		want = 144
	}
	if got := jobExtendedLimitInformationSize(); got != want {
		t.Fatalf("jobExtendedLimitInformationSize() = %d, want %d for a %d-byte pointer", got, want, pointer)
	}

	// The API rejects a buffer of the wrong size, so a job that can be created
	// is the behavioural proof that the layout is the one Windows expects here.
	job, err := newJobObject()
	if err != nil {
		t.Fatalf("newJobObject: %v", err)
	}
	if err := syscall.CloseHandle(job); err != nil {
		t.Fatalf("CloseHandle: %v", err)
	}
}
