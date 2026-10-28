//go:build !unix

package main

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"lightagent/internal/proc"
)

// handOverRun starts a replacement process and ends this one. These hosts cannot
// replace the running image, so the run continues in a child of this process
// that inherits the standard streams.
//
// The replacement is created before anything of this run is given up, so a
// restart that cannot start leaves the run — and the browser that asked for it —
// exactly as it was. It is also marked as the restarted run (see restartEnv): it
// is created before this process gives anything up, so it cannot take the
// directory lock right away, and the marker is what makes its startup wait for
// the hand-over below instead of failing with an instance conflict.
//
// The new process is this process's replacement, not its child: it is not
// registered with proc, so the shutdown below leaves it running. What a
// replacement process cannot keep on Unix is the terminal — the shell takes it
// back as soon as the process it waits for exits, which leaves the replacement in
// an orphaned background process group whose reads fail with EIO — so those hosts
// replace the running image instead (see restart_unix.go).
func handOverRun(exe string, args, env []string, release ...func() error) error {
	cmd := exec.Command(exe, args...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", exe, err)
	}
	for _, fn := range release {
		if fn != nil {
			_ = fn()
		}
	}
	// The console is handed over right away: the raw editor is switched off now,
	// while the replacement is still in its own startup, so a restore afterwards
	// cannot undo the terminal setup it makes for itself. The same shutdown hooks
	// release the shared browser, which the replacement starts again when it needs
	// one.
	runShutdownHooks()
	// What is left is what a restart must not leak into the run that replaces it:
	// the exec sessions and stdio MCP servers started here. The shutdown hooks
	// already ran, so the exit is proc.Shutdown plus os.Exit rather than the whole
	// shutdown path again.
	time.AfterFunc(restartExitDelay, func() {
		proc.Shutdown()
		os.Exit(0)
	})
	return nil
}
