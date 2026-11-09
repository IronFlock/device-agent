//go:build !windows

package terminal

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// The agent runs with a raised CPU weight, a nice value of -16 and a
// protected OOM score. A device terminal's shell must not: whatever a user
// runs in it would outrank every app and use the agent's own share. Under
// systemd the shell gets its own scope in system.slice (its own cgroup, the
// normal share); either way its nice value and OOM score are reset.

// scopeProbe remembers once whether `systemd-run --scope` works here: not
// without systemd, not as a user without the right to talk to PID 1, not
// before systemd 236 (--collect).
var scopeProbe struct {
	once sync.Once
	ok   bool
}

var scopeArgs = []string{"--scope", "--quiet", "--collect", "--slice=system.slice", "--"}

func systemdScopeWorks() bool {
	scopeProbe.once.Do(func() {
		if _, err := exec.LookPath("systemd-run"); err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		args := append(append([]string{}, scopeArgs...), "true")
		scopeProbe.ok = exec.CommandContext(ctx, "systemd-run", args...).Run() == nil //nolint:gosec // G204: fixed systemd-run invocation, fixed arguments
	})
	return scopeProbe.ok
}

// hostShellStartDir is where a device terminal opens. A variable for the tests.
var hostShellStartDir = "/opt/ironflock"

// hostShellDir returns dir if the shell can start there, or "" for the agent's
// own directory. A directory the shell cannot enter would keep the terminal
// from opening at all, and a host with the legacy layout does not have this one.
func hostShellDir(dir string) string {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() || unix.Access(dir, unix.X_OK) != nil {
		return ""
	}
	return dir
}

// hostShellCommand is the shell of a device terminal. systemd-run --scope
// execs the shell in place once the scope exists, so the pid the agent holds
// is the shell's, and the shell keeps the directory it is started in.
func hostShellCommand() *exec.Cmd {
	cmd := exec.Command("bash")
	if systemdScopeWorks() {
		args := append(append([]string{}, scopeArgs...), "bash")
		cmd = exec.Command("systemd-run", args...) //nolint:gosec // G204: fixed systemd-run invocation, fixed arguments
	}
	cmd.Dir = hostShellDir(hostShellStartDir)
	return cmd
}

// resetPriority gives the shell the values a login shell has. Processes its
// rc files start in the first microseconds keep the agent's; that is fine.
func resetPriority(pid int) {
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, pid, 0)
	_ = os.WriteFile(fmt.Sprintf("/proc/%d/oom_score_adj", pid), []byte("0"), 0o600)
}
