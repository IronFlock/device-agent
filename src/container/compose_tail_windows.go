//go:build windows

package container

import (
	"os/exec"
	"strconv"
)

// A compose log tail is a process tree: the docker CLI starts the compose
// plugin as a child that inherits the output pipe. Windows has no process
// groups to signal, so taskkill walks the tree (/T) for us.

func setProcessGroup(_ *exec.Cmd) {}

func terminateTree(cmd *exec.Cmd) { killTree(cmd) }

func killTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run() //nolint:gosec // G204: fixed system tool, the pid of the agent's own child
	if err != nil {
		// taskkill missing or refused: at least the CLI goes.
		_ = cmd.Process.Kill()
	}
}
