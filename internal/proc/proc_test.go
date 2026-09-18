package proc

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// treeHelperModeEnv selects what a helper process does; see
// TestTreeHelperProcess.
const treeHelperModeEnv = "LIGHTAGENT_PROC_TREE_HELPER"

// TestTreeHelperProcess is not a real test: it provides the processes of the
// tree tests. The "root" mode spawns a "leaf" and reports its pid, so a test can
// tell a root-only kill (the leaf survives) from a tree kill.
func TestTreeHelperProcess(t *testing.T) {
	switch os.Getenv(treeHelperModeEnv) {
	case "root":
		child := exec.Command(os.Args[0], "-test.run=^TestTreeHelperProcess$")
		child.Env = append(os.Environ(), treeHelperModeEnv+"=leaf")
		// The leaf must not inherit the pipe the test reads the root's pid
		// from, or reading it would never reach the announcement.
		child.Stdout = io.Discard
		child.Stderr = io.Discard
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "spawn leaf:", err)
			os.Exit(1)
		}
		fmt.Printf("leaf=%d\n", child.Process.Pid)
		time.Sleep(30 * time.Second)
	case "leaf":
		time.Sleep(30 * time.Second)
	}
}

// startTreeHelper starts the root of a two-process tree and returns the command
// plus the pid of the process the root spawned. tree selects the ownership mode.
func startTreeHelper(t *testing.T, tree bool) (*exec.Cmd, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestTreeHelperProcess$")
	cmd.Env = append(os.Environ(), treeHelperModeEnv+"=root")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	start := StartProcess
	if tree {
		start = Start
	}
	if err := start(cmd); err != nil {
		t.Fatalf("start: %v", err)
	}
	leaf := readLeafPID(t, stdout)
	t.Cleanup(func() {
		_ = Kill(cmd)
		_ = cmd.Wait()
		Reap(cmd)
		// A process-managed root leaves its child alone, so the test ends it.
		if p, err := os.FindProcess(leaf); err == nil {
			_ = p.Kill()
		}
	})
	return cmd, leaf
}

// TestKillTerminatesTheWholeTree pins the tree contract: Kill reaches the
// processes the root spawned. Stopping only the root would leave the real
// program behind whenever a launcher sits in between (a shell, cmd.exe, npx).
func TestKillTerminatesTheWholeTree(t *testing.T) {
	skipWithoutLivenessCheck(t)
	cmd, leaf := startTreeHelper(t, true)

	if err := Kill(cmd); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	waitForProcessExit(t, leaf)
}

// TestKillProcessLeavesTheChildTree pins the other ownership mode: a
// process-managed command is stopped, while the children it started keep
// running — the stdio MCP shutdown relies on exactly that.
func TestKillProcessLeavesTheChildTree(t *testing.T) {
	skipWithoutLivenessCheck(t)
	cmd, leaf := startTreeHelper(t, false)

	if err := Kill(cmd); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	_ = cmd.Wait()
	Reap(cmd)
	if processAlive(cmd.Process.Pid) {
		t.Fatalf("the managed process %d is still running", cmd.Process.Pid)
	}
	if !processAlive(leaf) {
		t.Fatal("the child process was terminated although only the process was managed")
	}
}

// TestReapTerminatesLeftovers pins the other half of the tree contract: a root
// that exits while a process it spawned is still running must not leave that
// process behind, so a finished exec session leaks nothing.
func TestReapTerminatesLeftovers(t *testing.T) {
	skipWithoutLivenessCheck(t)
	cmd, leaf := startTreeHelper(t, true)

	// Stop the root only, the way a shell normally ends.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill root: %v", err)
	}
	_ = cmd.Wait()
	Reap(cmd)
	waitForProcessExit(t, leaf)
}

// TestShutdownTerminatesTrackedTrees pins the exit path: everything started
// through Start is terminated by Shutdown, which the signal watcher and the
// main process rely on.
func TestShutdownTerminatesTrackedTrees(t *testing.T) {
	skipWithoutLivenessCheck(t)
	_, leaf := startTreeHelper(t, true)

	Shutdown()
	waitForProcessExit(t, leaf)
}

// TestShutdownStopsProcessManagedCommands pins the exit path of the single
// process mode: the launched process is stopped (a stdio MCP server is closed)
// while its children keep running.
func TestShutdownStopsProcessManagedCommands(t *testing.T) {
	skipWithoutLivenessCheck(t)
	cmd, leaf := startTreeHelper(t, false)

	Shutdown()
	_ = cmd.Wait()
	Reap(cmd)
	if processAlive(cmd.Process.Pid) {
		t.Fatalf("the managed process %d is still running", cmd.Process.Pid)
	}
	if !processAlive(leaf) {
		t.Fatal("the child process was terminated although only the process was managed")
	}
}

// readLeafPID waits for the root helper's leaf announcement.
func readLeafPID(t *testing.T, stdout io.Reader) int {
	t.Helper()
	reader := bufio.NewReader(stdout)
	deadline := time.Now().Add(20 * time.Second)
	for {
		line, err := reader.ReadString('\n')
		if pid, ok := parseLeafPID(line); ok {
			return pid
		}
		if err != nil {
			t.Fatalf("the helper did not report its leaf: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the helper did not report its leaf in time")
		}
	}
}

// parseLeafPID extracts the pid from the helper's "leaf=<pid>" line.
func parseLeafPID(line string) (int, bool) {
	value, found := strings.CutPrefix(strings.TrimSpace(line), "leaf=")
	if !found {
		return 0, false
	}
	pid, err := strconv.Atoi(value)
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// waitForProcessExit polls until pid is gone, which proves the tree handling
// reached it.
func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d outlived its tree", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// skipWithoutLivenessCheck skips the tree tests where no pid liveness check
// exists.
func skipWithoutLivenessCheck(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "js" || runtime.GOOS == "plan9" {
		t.Skip("no process liveness check on this host")
	}
}
