//go:build windows

package proc

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Job-object constants. jobInfoClassExtendedLimits is the JOBOBJECTINFOCLASS
// value that accepts the extended limit structure, and
// jobLimitKillOnJobClose (JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE) makes the OS
// terminate every member of the job once its last handle is closed.
const (
	jobInfoClassExtendedLimits = 9
	jobLimitKillOnJobClose     = 0x00002000
)

// processSetQuota is PROCESS_SET_QUOTA, the access right
// AssignProcessToJobObject asks for besides PROCESS_TERMINATE (the syscall
// package only declares the latter).
const processSetQuota = 0x0100

var (
	jobKernel32 = syscall.NewLazyDLL("kernel32.dll")

	procCreateJobObjectW         = jobKernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = jobKernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = jobKernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject       = jobKernel32.NewProc("TerminateJobObject")
)

// jobExtendedLimitInformationSize is the size SetInformationJobObject expects
// for JOBOBJECT_EXTENDED_LIMIT_INFORMATION. The structure follows the pointer
// size of the caller, and not only in its size_t fields: its LARGE_INTEGER and
// ULONGLONG members force 8-byte alignment, so its C size is padded up to 8
// (144 bytes on 64-bit: 64-byte basic limits + 48-byte IO counters + 4
// pointers; 112 bytes on 32-bit: 44-byte basic limits padded to 48 for the
// 8-aligned IO counters + 48-byte IO counters + 4 four-byte size_t fields).
//
// Go's ABI pads int64/uint64 to 4 bytes on 32-bit, so the structure cannot be
// spelled as a portable Go struct — unsafe.Sizeof would give 108 there and the
// API rejects it with ERROR_BAD_LENGTH (only 144 succeeds on 64-bit and only
// 112 on 32-bit). It is therefore filled by size, below.
func jobExtendedLimitInformationSize() uintptr {
	if unsafe.Sizeof(uintptr(0)) == 8 {
		return 144
	}
	return 112
}

// jobLimitFlagsOffset is where LimitFlags sits inside
// JOBOBJECT_EXTENDED_LIMIT_INFORMATION: right after the two LARGE_INTEGER
// user-time limits, which is the same offset in both layouts. It is the only
// field this program sets, every other one staying zero.
const jobLimitFlagsOffset = 16

// jobs maps a started command to the job object that owns its tree. The handle
// is kept open for the lifetime of the tree, so a hard termination of this
// process (crash, taskkill) still removes the whole tree: closing the handle
// triggers the kill-on-close limit.
var (
	jobsMu sync.Mutex
	jobs   = map[*exec.Cmd]syscall.Handle{}
)

// prepareTree is a no-op on Windows: a job is joined after the process exists
// (see adoptTree).
func prepareTree(*exec.Cmd) {}

// adoptTree assigns the fresh process to a new job. Assignment fails when this
// process itself runs inside a job that refuses nesting, in which case the tree
// simply stays unmanaged and killTree falls back to taskkill.
func adoptTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	job, err := newJobObject()
	if err != nil {
		return
	}
	handle, err := syscall.OpenProcess(
		processSetQuota|syscall.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = syscall.CloseHandle(job)
		return
	}
	err = assignProcessToJob(job, handle)
	_ = syscall.CloseHandle(handle)
	if err != nil {
		_ = syscall.CloseHandle(job)
		return
	}
	jobsMu.Lock()
	jobs[cmd] = job
	jobsMu.Unlock()
}

// killTree terminates the tree through its job. Only the direct child could be
// stopped by os.Process.Kill; a launcher such as cmd.exe or npm would leave the
// real program (and any sibling it started) running.
func killTree(cmd *exec.Cmd) error {
	if job, ok := jobFor(cmd); ok {
		if err := terminateJob(job); err == nil {
			return nil
		}
	}
	return killTreeFallback(cmd)
}

// releaseTree drops the job handle of a tree whose root process has been waited
// for. Closing the handle makes the kill-on-close limit remove whatever the
// root left behind, so a finished session cannot leak a background program. A
// tree that never made it into a job (the assignment can be refused when this
// process already runs inside a job) has no handle to close, so taskkill is the
// fallback, the way killTree falls back on it.
func releaseTree(cmd *exec.Cmd) {
	job, ok := takeJob(cmd)
	if ok {
		_ = syscall.CloseHandle(job)
		return
	}
	releaseTreeFallback(cmd)
}

// killProcess stops only the process that was launched (TerminateProcess); the
// children it started keep running, which is what a stdio MCP server shutdown
// asks for.
func killProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

// jobFor returns the job of a tracked tree.
func jobFor(cmd *exec.Cmd) (syscall.Handle, bool) {
	if cmd == nil {
		return 0, false
	}
	jobsMu.Lock()
	defer jobsMu.Unlock()
	job, ok := jobs[cmd]
	return job, ok
}

// takeJob removes and returns the job of a tracked tree, if it has one.
func takeJob(cmd *exec.Cmd) (syscall.Handle, bool) {
	if cmd == nil {
		return 0, false
	}
	jobsMu.Lock()
	defer jobsMu.Unlock()
	job, ok := jobs[cmd]
	delete(jobs, cmd)
	return job, ok
}

// newJobObject creates a job whose members are terminated once the last handle
// to it is closed.
func newJobObject() (syscall.Handle, error) {
	r, _, callErr := procCreateJobObjectW.Call(0, 0)
	if r == 0 {
		return 0, callErr
	}
	job := syscall.Handle(r)
	if err := setJobInformation(job); err != nil {
		_ = syscall.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

// setJobInformation applies the extended limits carrying the kill-on-close flag.
// Only LimitFlags is meaningful, so the structure is built as a zeroed buffer of
// the size this process's layout uses (see jobExtendedLimitInformationSize)
// with that one field written in.
func setJobInformation(job syscall.Handle) error {
	size := jobExtendedLimitInformationSize()
	info := make([]byte, size)
	binary.LittleEndian.PutUint32(info[jobLimitFlagsOffset:], jobLimitKillOnJobClose)
	r, _, callErr := procSetInformationJobObject.Call(
		uintptr(job),
		uintptr(jobInfoClassExtendedLimits),
		uintptr(unsafe.Pointer(&info[0])),
		size,
	)
	if r == 0 {
		return callErr
	}
	return nil
}

// assignProcessToJob adds a process to a job.
func assignProcessToJob(job, process syscall.Handle) error {
	r, _, callErr := procAssignProcessToJobObject.Call(uintptr(job), uintptr(process))
	if r == 0 {
		return callErr
	}
	return nil
}

// terminateJob kills every process in a job.
func terminateJob(job syscall.Handle) error {
	r, _, callErr := procTerminateJobObject.Call(uintptr(job), 1)
	if r == 0 {
		return callErr
	}
	return nil
}

// taskkillTimeout bounds the fallback helper, so a stuck taskkill cannot hold up
// the shutdown.
const taskkillTimeout = 5 * time.Second

// taskkillTree asks taskkill to stop a whole process tree.
func taskkillTree(pid int) error {
	ctx, cancel := context.WithTimeout(context.Background(), taskkillTimeout)
	defer cancel()
	killer := exec.CommandContext(ctx, "taskkill.exe", "/T", "/F", "/PID", strconv.Itoa(pid))
	// A console would otherwise flash a window for the helper.
	killer.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return killer.Run()
}

// killTreeFallback stops a tree that could not be assigned to a job: taskkill
// walks the child tree, and stopping the direct process is the last resort.
func killTreeFallback(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := taskkillTree(cmd.Process.Pid); err != nil {
		return killProcess(cmd)
	}
	return nil
}

// releaseTreeFallback stops what a tree that could not be put in a job left
// behind, matching the effort killTreeFallback makes. The root has already been
// waited for by the time Reap runs, so taskkill usually cannot find it any more;
// this covers the window in which it still exists.
func releaseTreeFallback(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = taskkillTree(cmd.Process.Pid)
}
