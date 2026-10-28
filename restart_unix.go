//go:build unix

package main

import (
	"fmt"
	"os"
	"syscall"
	"time"

	"lightagent/internal/proc"
)

// handOverRun continues the run in place: the image of this process is replaced
// with a fresh one (syscall.Exec), so the PID, the session, the process group and
// the controlling terminal stay exactly as they are.
//
// A replacement *process* cannot keep the terminal here. The shell that started
// this run is waiting for it, and it takes the terminal back (tcsetpgrp to
// itself) the moment that process exits, so a child left behind ends up in a
// background process group — one that is orphaned as well, since its parent is
// gone. A read from the terminal in an orphaned background group is not stopped
// with SIGTTIN; the kernel fails it with EIO instead. A restarted CLI could not
// read a single key: every startup reported "lightagent: read /dev/stdin:
// input/output error" (the fallback line scanner's read of stdin) and the
// terminal session of the run was lost. Replacing the image keeps the process the
// shell waits for alive, so job control, the terminal and the exit status carry
// on as if nothing had happened — which is also what a supervisor such as systemd
// expects from a restart.
//
// The arguments are the ones restartArgs rebuilt and the environment is the one
// restartEnvFor built: the new image parses the same run, with --resume appended.
func handOverRun(exe string, args, env []string, release ...func() error) error {
	// Replacing the image cannot report its failure back to the caller anymore —
	// by then everything below has happened — so the reasons it would fail are
	// ruled out while the run is still intact.
	if err := checkExecutable(exe); err != nil {
		return err
	}
	// The hand-over waits a moment so the reply of the request that asked for the
	// restart reaches the browser first: replacing the image drops every live
	// connection. Once it starts it is all over at once — the mirror gives up its
	// listening address (only the listener, so nothing else is cut off), the
	// shutdown hooks restore the terminal and release the directory lock, and the
	// image is replaced — so neither the port nor the lock is free for longer than
	// the swap itself.
	time.AfterFunc(restartExitDelay, func() {
		for _, fn := range release {
			if fn != nil {
				_ = fn()
			}
		}
		// The raw editor is switched off before the new image starts, so the
		// terminal setup it makes for itself cannot be undone afterwards; the same
		// hooks release the shared browser, which the new image starts again when it
		// needs one, and the directory lock, which the new image takes over.
		runShutdownHooks()
		// What a restart must not leak into the run that replaces it: the exec
		// sessions and stdio MCP servers started here. They are children of this
		// PID, so the new image would neither know them nor reap them.
		proc.Shutdown()
		argv := append([]string{os.Args[0]}, args...)
		if err := syscall.Exec(exe, argv, env); err != nil {
			// exec only returns when it failed, and this process is all that is
			// left of the run by now: there is nothing to fall back to.
			fmt.Fprintf(os.Stderr, "lightagent: restart: %v\n", err)
			os.Exit(1)
		}
	})
	return nil
}

// checkExecutable reports whether exe is still a file the image can be replaced
// with.
func checkExecutable(exe string) error {
	info, err := os.Stat(exe)
	if err != nil {
		return fmt.Errorf("locate the program: %w", err)
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return fmt.Errorf("start %s: not an executable file", exe)
	}
	return nil
}
