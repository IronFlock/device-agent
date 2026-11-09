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

func TestHostShellOpensInTheIronFlockFolder(t *testing.T) {
	assert.Equal(t, "/opt/ironflock", hostShellStartDir)

	for name, binDirHasSystemdRun := range map[string]bool{"plain bash": false, "scope": true} {
		t.Run(name, func(t *testing.T) {
			binDir := t.TempDir()
			if binDirHasSystemdRun {
				require.NoError(t, os.WriteFile(binDir+"/systemd-run", []byte("#!/bin/sh\nexit 0\n"), 0o755))
			}
			t.Setenv("PATH", binDir)
			scopeProbe.once = sync.Once{}
			scopeProbe.ok = false
			t.Cleanup(func() { scopeProbe.once = sync.Once{}; scopeProbe.ok = false })

			folder := t.TempDir()
			original := hostShellStartDir
			hostShellStartDir = folder
			t.Cleanup(func() { hostShellStartDir = original })

			assert.Equal(t, folder, hostShellCommand().Dir)
		})
	}
}

func TestHostShellOpensAnywhereWhenTheFolderCannotBeUsed(t *testing.T) {
	file := t.TempDir() + "/a-file"
	require.NoError(t, os.WriteFile(file, nil, 0o644))

	assert.Empty(t, hostShellDir(t.TempDir()+"/missing"), "a host with the legacy layout has no such folder")
	assert.Empty(t, hostShellDir(file), "not a folder")
}

func TestHostShellOpensAnywhereWhenTheFolderCannotBeEntered(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can enter any folder")
	}
	folder := t.TempDir()
	require.NoError(t, os.Chmod(folder, 0o600))
	t.Cleanup(func() { _ = os.Chmod(folder, 0o755) })

	assert.Empty(t, hostShellDir(folder))
}

// The reset only shows where the parent runs with raised values, which takes
// root with CAP_SYS_NICE and CAP_SYS_RESOURCE: a Linux device or VM, or
// `docker run --cap-add SYS_NICE --cap-add SYS_RESOURCE`. Elsewhere (CI's
// runner user, a plain container) it skips.
func TestHostShellRunsAtNormalPriority(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("needs root on Linux: only root can raise its own priority to then see the shell's reset")
	}
	if err := syscall.Setpriority(syscall.PRIO_PROCESS, 0, -5); err != nil {
		t.Skipf("cannot raise this process's priority (%v): needs CAP_SYS_NICE", err)
	}
	if err := os.WriteFile("/proc/self/oom_score_adj", []byte("-900"), 0o600); err != nil {
		t.Skipf("cannot lower this process's OOM score (%v): needs CAP_SYS_RESOURCE", err)
	}
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
