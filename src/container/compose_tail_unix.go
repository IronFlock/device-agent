//go:build !windows

package container

import (
	"os/exec"
	"syscall"
)

// A compose log tail is a process tree: the docker CLI starts the compose
// plugin as a child that inherits the output pipe. A signal to the CLI alone
// leaves the plugin — the larger process of the two — running with the pipe
// open. The tail therefore gets its own process group, and the group is
// signalled as a whole.

func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func terminateTree(cmd *exec.Cmd) { signalGroup(cmd, syscall.SIGTERM) }

func killTree(cmd *exec.Cmd) { signalGroup(cmd, syscall.SIGKILL) }

func signalGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd.Process == nil {
		return
	}
	// A negative pid addresses the process group; its id is the leader's pid.
	_ = syscall.Kill(-cmd.Process.Pid, sig)
}
