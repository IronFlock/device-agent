//go:build !windows

package terminal

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHostShellIsPlainBashWithoutSystemdRun(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no systemd-run here
	scopeProbe.once = sync.Once{}
	scopeProbe.ok = false
	t.Cleanup(func() { scopeProbe.once = sync.Once{}; scopeProbe.ok = false })

	assert.Equal(t, []string{"bash"}, hostShellCommand().Args)
}

func TestHostShellUsesAScopeWhereSystemdRunWorks(t *testing.T) {
	binDir := t.TempDir()
	// A systemd-run that accepts the probe.
	require.NoError(t, os.WriteFile(binDir+"/systemd-run", []byte("#!/bin/sh\nexit 0\n"), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	scopeProbe.once = sync.Once{}
	scopeProbe.ok = false
	t.Cleanup(func() { scopeProbe.once = sync.Once{}; scopeProbe.ok = false })

	cmd := hostShellCommand()
	assert.Equal(t, "systemd-run", cmd.Args[0])
	assert.Equal(t, []string{"--scope", "--quiet", "--collect", "--slice=system.slice", "--", "bash"}, cmd.Args[1:])
}

// The reset only shows where the parent runs with raised values, which takes
// root: run as root on a Linux device or VM.
func TestHostShellRunsAtNormalPriority(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("needs root on Linux: only root can raise its own priority to then see the shell's reset")
	}
	require.NoError(t, syscall.Setpriority(syscall.PRIO_PROCESS, 0, -5))
	require.NoError(t, os.WriteFile("/proc/self/oom_score_adj", []byte("-900"), 0o644))
	t.Cleanup(func() {
		_ = syscall.Setpriority(syscall.PRIO_PROCESS, 0, 0)
		_ = os.WriteFile("/proc/self/oom_score_adj", []byte("0"), 0o644)
	})

	shell, err := startHostShell()
	require.NoError(t, err)
	t.Cleanup(func() { _ = shell.Close() })
	pid := shell.(*unixShell).cmd.Process.Pid

	require.Eventually(t, func() bool {
		score, err := os.ReadFile(fmt.Sprintf("/proc/%d/oom_score_adj", pid))
		return err == nil && strings.TrimSpace(string(score)) == "0"
	}, 2*time.Second, 20*time.Millisecond, "the shell must not inherit the agent's OOM score")

	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	require.NoError(t, err)
	// Field 19 (1-based) is the nice value; the command name may hold spaces,
	// so count from the closing parenthesis.
	stat := string(raw)
	fields := strings.Fields(stat[strings.LastIndexByte(stat, ')')+1:])
	nice, err := strconv.Atoi(fields[16])
	require.NoError(t, err)
	assert.Equal(t, 0, nice, "the shell must not inherit the agent's nice value")
}
