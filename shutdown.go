package main

import (
	"os"
	"os/signal"
	"sync"

	"lightagent/internal/proc"
)

// shutdownHooks are the callbacks a signal-triggered exit runs before leaving.
// They release state that the normal return path releases in a defer (the
// terminal, for instance), which a signal exit never reaches.
var (
	shutdownHooksMu sync.Mutex
	shutdownHooks   []func()
)

// onShutdown registers fn to run before the process exits on a signal.
func onShutdown(fn func()) {
	if fn == nil {
		return
	}
	shutdownHooksMu.Lock()
	shutdownHooks = append(shutdownHooks, fn)
	shutdownHooksMu.Unlock()
}

// watchShutdown ends the process cleanly when it is asked to stop: Ctrl+C
// outside the editor, SIGTERM from a supervisor, SIGHUP when the terminal goes
// away (see watchedSignals for what the host delivers). It runs the registered
// hooks, terminates every child process tree (the exec sessions and the stdio
// MCP servers would otherwise keep running) and exits with the conventional
// 128+signal status.
//
// The interactive editor does not depend on it: it reads Ctrl+C as a key press,
// so no signal is raised while it drives the console.
func watchShutdown() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, watchedSignals...)
	go func() {
		sig, ok := <-signals
		if !ok {
			return
		}
		runShutdownHooks()
		proc.Shutdown()
		os.Exit(signalStatus(sig))
	}()
}

// runShutdownHooks runs the registered hooks newest first, mirroring the LIFO
// order of the deferred teardown they stand in for.
func runShutdownHooks() {
	shutdownHooksMu.Lock()
	hooks := make([]func(), len(shutdownHooks))
	copy(hooks, shutdownHooks)
	shutdownHooksMu.Unlock()

	for i := len(hooks) - 1; i >= 0; i-- {
		hooks[i]()
	}
}
