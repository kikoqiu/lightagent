//go:build windows

package proc

import (
	"context"
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

// jobObjectBasicLimitInformation mirrors JOBOBJECT_BASIC_LIMIT_INFORMATION.
type jobObjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

// jobObjectIOCounters mirrors IO_COUNTERS.
type jobObjectIOCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

// jobExtendedLimitInformation mirrors
// JOBOBJECT_EXTENDED_LIMIT_INFORMATION: the trailing memory limits are only
// there to match the layout the API expects.
type jobExtendedLimitInformation struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IOCounters            jobObjectIOCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

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
// root left behind, so a finished session cannot leak a background program.
func releaseTree(cmd *exec.Cmd) {
	jobsMu.Lock()
	job, ok := jobs[cmd]
	delete(jobs, cmd)
	jobsMu.Unlock()
	if ok {
		_ = syscall.CloseHandle(job)
	}
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

// newJobObject creates a job whose members are terminated once the last handle
// to it is closed.
func newJobObject() (syscall.Handle, error) {
	r, _, callErr := procCreateJobObjectW.Call(0, 0)
	if r == 0 {
		return 0, callErr
	}
	job := syscall.Handle(r)
	info := jobExtendedLimitInformation{}
	info.BasicLimitInformation.LimitFlags = jobLimitKillOnJobClose
	if err := setJobInformation(job, &info); err != nil {
		_ = syscall.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

// setJobInformation applies the extended limits carrying the kill-on-close flag.
func setJobInformation(job syscall.Handle, info *jobExtendedLimitInformation) error {
	r, _, callErr := procSetInformationJobObject.Call(
		uintptr(job),
		uintptr(jobInfoClassExtendedLimits),
		uintptr(unsafe.Pointer(info)),
		uintptr(unsafe.Sizeof(*info)),
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

// killTreeFallback stops a tree that could not be assigned to a job: taskkill
// walks the child tree, and stopping the direct process is the last resort.
func killTreeFallback(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), taskkillTimeout)
	defer cancel()
	killer := exec.CommandContext(ctx, "taskkill.exe", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	// A console would otherwise flash a window for the helper.
	killer.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := killer.Run(); err != nil {
		return killProcess(cmd)
	}
	return nil
}
