//go:build windows

package tunnel

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// raisePriority starts frpc above normal priority, like the agent itself
// (resources_windows.go): the tunnel is part of the agent's job and must keep
// pace with it when apps load the machine.
func raisePriority(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.ABOVE_NORMAL_PRIORITY_CLASS
}
