package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// restartExitDelay is how long the reply of the request that asked for the
// restart is given to reach the browser before the run is given up. The page
// reconnects by itself afterwards (a lost connection is retried after at least a
// second anyway), and the pause is what lets it show "restarting…" instead of a
// network error.
const restartExitDelay = 300 * time.Millisecond

// restartEnv marks the run a restart starts. A replacement process is created
// while this process still holds the directory lock, so its startup waits for
// the hand-over instead of reporting the instance conflict that is already
// ending (see lockWaitForThisRun).
const restartEnv = "LIGHTAGENT_RESTART"

// restartLockWait is how long a restarted run waits for the directory lock of the
// run it replaces. The hand-over happens within milliseconds — it is the tail of
// the request that asked for the restart — and the old process ends
// restartExitDelay after the replacement was created at the latest, an exit that
// drops the lock with it. The deadline only has to outlast a slow release path,
// while a directory that is genuinely held still ends up reported.
const restartLockWait = 3 * time.Second

// restartProgram restarts this run: the same program, started with the arguments
// this process was given plus --resume (see restartArgs) and with this process's
// own environment (see restartEnvFor). It is what the web mirror's Restart button
// runs (see web.Server.SetRestarter): the mirror saves the conversation first, and
// the restarted run resumes that session, so the browser ends up talking to a
// fresh process that continues the same conversation with the config.json that
// was just saved in effect.
//
// An error is returned only while the hand-over is still being prepared: nothing
// of this process is given up before that, so a restart that cannot happen leaves
// the run — and the browser that asked for it — exactly as it was.
//
// The release callbacks are called once the run is on its way out: the mirror
// gives up its listening address there, cheaply and without dropping the live
// connections, because the restarted run binds that address again — which is
// what lets a browser reconnect to the page it already has (the new run still
// boots for a few milliseconds, so the port is free well before anything tries
// to bind it). The shutdown hooks that follow restore the terminal and release
// the directory lock. What a restart must not leak into the run that replaces it
// are the exec sessions and stdio MCP servers started here; those are the child
// processes proc.Shutdown terminates before the hand-over completes.
//
// How the run continues differs per host (see handOverRun): on Unix the image of
// this process is replaced in place, which keeps its PID, its session and its
// terminal; on the other hosts a replacement process takes over and this one
// ends.
func restartProgram(release ...func() error) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate the program: %w", err)
	}
	return handOverRun(exe, restartArgs(os.Args[1:]), restartEnvFor(os.Environ()), release...)
}

// restartArgs rebuilds the command line of the restarted run: the arguments this
// process was given, minus the ones it must not repeat, plus --resume.
//
// Two kinds are dropped. -C/--dir changed the working directory, and the
// restarted run starts in the directory this process is in now, so repeating it
// would resolve the same path twice (a relative one against its own result).
// --no-save would keep the next exit from saving the conversation, which is the
// opposite of what a restart is for. A resume switch is dropped as well, because
// exactly one is appended at the end whichever spelling this process was given:
// that is what loads the saved session again. Everything else — the config file,
// the model overrides, an explicit --session path — travels as it is, so the
// restarted run is the same run with the same settings.
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

// restartEnvFor returns the environment of the restarted run: the environment of
// this process with the restart marker set exactly once. An inherited copy is
// dropped first, so the value the restarted run reads is the one this restart put
// there, whatever this process was started with.
func restartEnvFor(environ []string) []string {
	out := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		if strings.HasPrefix(kv, restartEnv+"=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, restartEnv+"=1")
}

// lockWaitForThisRun is how long the directory lock may take at startup: a run a
// restart started waits for the hand-over of the run it replaces (see
// restartLockWait), every other run fails at once on a directory another instance
// holds. The marker is cleared on the way, so the processes this one starts are
// ordinary instances again.
func lockWaitForThisRun() time.Duration {
	if strings.TrimSpace(os.Getenv(restartEnv)) == "" {
		return 0
	}
	_ = os.Unsetenv(restartEnv)
	return restartLockWait
}
