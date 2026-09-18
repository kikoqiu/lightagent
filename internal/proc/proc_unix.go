//go:build unix

package proc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// killGrace is how long a tree is given to honour SIGTERM before it is killed
// outright. Programs get the chance to flush and clean up, but nothing that
// ignores the signal can hold up the shutdown for long.
const killGrace = 300 * time.Millisecond

// groupPollInterval is how often the grace wait re-checks whether the group is
// empty.
const groupPollInterval = 10 * time.Millisecond

// prepareTree gives the child its own process group, so the shell and everything
// it spawns can be signalled as one unit.
func prepareTree(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// adoptTree needs no work on Unix: the group is established by prepareTree,
// before the child exists.
func adoptTree(*exec.Cmd) {}

// killTree stops the whole group: SIGTERM lets the processes exit on their own,
// SIGKILL ends the ones that are still around after the grace period. The wait
// for the grace period is bounded, so a process that ignores both cannot hold up
// the caller.
func killTree(cmd *exec.Cmd) error {
	pgid, ok := processGroup(cmd)
	if !ok || !groupAlive(pgid) {
		return nil
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	if waitGroupExit(pgid, killGrace) {
		return nil
	}
	return signalGroup(pgid, syscall.SIGKILL)
}

// releaseTree terminates what the root process left behind, so a finished
// session cannot leak a background program.
func releaseTree(cmd *exec.Cmd) {
	pgid, ok := processGroup(cmd)
	if !ok || !groupAlive(pgid) {
		return
	}
	_ = signalGroup(pgid, syscall.SIGKILL)
}

// killProcess stops only the process that was launched; the children it started
// are left alone.
func killProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

// signalGroup signals a whole process group, treating "the group is gone"
// (ESRCH) as success: the outcome is what the caller asked for.
func signalGroup(pgid int, sig syscall.Signal) error {
	if err := syscall.Kill(-pgid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// processGroup returns the group id of a started command. The child leads its
// own group (see prepare), so its pid is the group id.
func processGroup(cmd *exec.Cmd) (int, bool) {
	if cmd == nil || cmd.Process == nil {
		return 0, false
	}
	return cmd.Process.Pid, true
}

// groupAlive reports whether the group still has a member. A group whose
// processes are all gone reports ESRCH, which keeps a stale entry from
// signalling a recycled pid.
func groupAlive(pgid int) bool {
	return syscall.Kill(-pgid, 0) == nil
}

// waitGroupExit polls until the group is empty or the timeout elapses.
func waitGroupExit(pgid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !groupAlive(pgid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(groupPollInterval)
	}
}
