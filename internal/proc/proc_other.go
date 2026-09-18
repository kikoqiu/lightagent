//go:build !unix && !windows

package proc

import (
	"errors"
	"os"
	"os/exec"
)

// prepareTree, adoptTree and releaseTree need no work on hosts without process
// groups or job objects: there is nothing to set up before or after starting the
// process.
func prepareTree(*exec.Cmd) {}

func adoptTree(*exec.Cmd) {}

func releaseTree(*exec.Cmd) {}

// killTree falls back to stopping the single process; these hosts offer no
// portable way to reach its descendants.
func killTree(cmd *exec.Cmd) error { return killProcess(cmd) }

// killProcess stops only the process that was launched.
func killProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}
