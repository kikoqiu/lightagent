package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"lightagent/internal/proc"
)

// restartExitDelay is how long the replacement process is given to come up
// before this one ends. The reply of the request that asked for the restart is on
// its way to the browser, and the pause lets it arrive; the page then reconnects
// by itself (a lost connection is retried after at least a second anyway).
const restartExitDelay = 300 * time.Millisecond

// restartProgram starts a replacement of this program and ends the current
// process. It is what the web mirror's Restart button runs (see
// web.Server.SetRestarter): the mirror saves the conversation first, and the
// replacement resumes that session, so the browser ends up talking to a fresh
// process that continues the same conversation with the config.json that was just
// saved in effect.
//
// An error is returned only while the replacement is being created: nothing of
// this process is given up before the new one exists, so a restart that cannot
// start leaves the run — and the browser that asked for it — exactly as it was.
//
// The replacement is started from the same executable with the arguments this
// process was given plus --resume (see restartArgs) and inherits the standard
// streams, so a terminal session continues in the same console. The release
// callbacks are then called: the mirror gives up its listening address there,
// cheaply and without dropping the live connections, because the replacement
// binds that address again — which is what lets a browser reconnect to the page
// it already has (the new process still boots for a few milliseconds, so the port
// is free well before anything tries to bind it).
//
// The new process is this process's replacement, not its child: it is not
// registered with proc, so the shutdown below leaves it running.
func restartProgram(release ...func() error) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate the program: %w", err)
	}
	cmd := exec.Command(exe, restartArgs(os.Args[1:])...)
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

// restartArgs rebuilds the command line of the replacement process: the
// arguments this process was given, minus the ones it must not repeat, plus
// --resume.
//
// Two kinds are dropped. -C/--dir changed the working directory, and the
// replacement starts in the directory this process is in now, so repeating it
// would resolve the same path twice (a relative one against its own result).
// --no-save would keep the next exit from saving the conversation, which is the
// opposite of what a restart is for. A resume switch is dropped as well, because
// exactly one is appended at the end whichever spelling this process was given:
// that is what loads the saved session again. Everything else — the config file,
// the model overrides, an explicit --session path — travels as it is, so the
// replacement is the same run with the same settings.
func restartArgs(args []string) []string {
	out := make([]string, 0, len(args)+2)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		// Only switches are rewritten: everything else is a value (or a
		// positional argument) and travels verbatim.
		if !strings.HasPrefix(arg, "-") {
			out = append(out, arg)
			continue
		}
		name := strings.TrimLeft(arg, "-")
		hasValue := false
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name, hasValue = name[:eq], true
		}
		switch name {
		case "C", "dir":
			// The directory is the next argument unless it came as -dir=PATH.
			if !hasValue {
				i++
			}
		case "no-save", "r", "resume":
			// Dropped: a restart always resumes, and its exit always saves.
		default:
			out = append(out, arg)
		}
	}
	return append(out, "--resume")
}
