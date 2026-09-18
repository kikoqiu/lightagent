// Package proc starts child processes with an explicit ownership mode so that
// nothing the agent started outlives it and nothing it did not start is touched:
//
//   - Start owns a whole process tree: the process and everything it spawns
//     (exec sessions, which are terminated as a unit).
//   - StartProcess owns only the process it launched (a stdio MCP server: the
//     server itself is closed, while whatever it started keeps running).
//
// The tree mode uses the platform's grouping facility — a job object with the
// kill-on-close limit on Windows, a dedicated process group on Unix — while the
// process mode is a plain start plus a plain process kill. Every wait on that
// path is bounded, so an unresponsive process cannot hold up the exit.
package proc

import (
	"os/exec"
	"sync"
)

// mode records how a started process is managed.
type mode int

const (
	// modeTree owns the process and everything it spawns.
	modeTree mode = iota
	// modeProcess owns only the launched process.
	modeProcess
)

// live tracks the processes started through Start/StartProcess that have not
// been reaped yet, so Shutdown can stop everything that is still alive.
var (
	liveMu sync.Mutex
	live   = map[*exec.Cmd]mode{}
)

// Start launches cmd as the root of its own process tree: the whole tree is
// owned, so Kill ends the process and its descendants and the tree is
// registered for the shutdown.
func Start(cmd *exec.Cmd) error {
	return start(cmd, modeTree)
}

// StartProcess launches cmd without tree management: Kill stops the process
// itself, never the children it spawned, and the shutdown treats it the same
// way. Use it for processes whose descendants are none of our business.
func StartProcess(cmd *exec.Cmd) error {
	return start(cmd, modeProcess)
}

func start(cmd *exec.Cmd, m mode) error {
	if m == modeTree {
		prepareTree(cmd)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	if m == modeTree {
		adoptTree(cmd)
	}
	liveMu.Lock()
	live[cmd] = m
	liveMu.Unlock()
	return nil
}

// Kill stops what cmd owns: the whole tree for a tree-managed process (a shell's
// children, an npx launcher's node process, ...), the process itself for a
// process-managed one. It reports nil when there is nothing left to stop and
// returns as soon as the termination has been requested, so a process that
// refuses to die cannot block the caller.
func Kill(cmd *exec.Cmd) error {
	if cmd == nil {
		return nil
	}
	if managedMode(cmd) == modeTree {
		return killTree(cmd)
	}
	return killProcess(cmd)
}

// Reap releases the bookkeeping of a process whose root has been waited for;
// call it after cmd.Wait. A tree-managed root that exited may still have left
// processes behind (a program the shell started in the background, for
// example), so whatever is still alive in that tree is terminated here too. A
// process-managed command is only forgotten about.
func Reap(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	liveMu.Lock()
	m, ok := live[cmd]
	delete(live, cmd)
	liveMu.Unlock()
	if ok && m == modeTree {
		releaseTree(cmd)
	}
}

// Shutdown stops every process that has been started and not reaped, each in its
// own mode. The main process calls it on the way out and the signal watcher
// calls it when it is asked to stop, so no exec session is left running and
// every stdio MCP server we launched is closed.
func Shutdown() {
	liveMu.Lock()
	pending := make(map[*exec.Cmd]mode, len(live))
	for cmd, m := range live {
		pending[cmd] = m
	}
	liveMu.Unlock()

	// Stopping a tree may briefly wait for it to exit, so the independent
	// processes are stopped in parallel and the exit stays short.
	var wg sync.WaitGroup
	for cmd, m := range pending {
		wg.Add(1)
		go func(cmd *exec.Cmd, m mode) {
			defer wg.Done()
			if m == modeTree {
				_ = killTree(cmd)
				return
			}
			_ = killProcess(cmd)
		}(cmd, m)
	}
	wg.Wait()
}

// managedMode reports how a command is managed. A command that never went
// through this package is treated as a single process, the least destructive
// mode.
func managedMode(cmd *exec.Cmd) mode {
	liveMu.Lock()
	defer liveMu.Unlock()
	if m, ok := live[cmd]; ok {
		return m
	}
	return modeProcess
}
