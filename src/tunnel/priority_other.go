//go:build !windows

package tunnel

import "os/exec"

// raisePriority is a no-op off Windows: there frpc inherits the agent's unit
// (its CPU weight, nice value and OOM score) by being its child.
func raisePriority(_ *exec.Cmd) {}
