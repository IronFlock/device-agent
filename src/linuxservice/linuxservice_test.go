package linuxservice

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnitStartsTheAgentWithEveryPathExplicit(t *testing.T) {
	unit := DefaultPaths().Unit()
	for _, want := range []string{
		"ExecStart=/opt/ironflock/device-agent/device-agent -config /opt/ironflock/device-agent/device-config.flock -agentDir /opt/ironflock/device-agent -appsDir /apps -dbFileName /opt/ironflock/device-agent/device-agent.db -logFile /var/log/ironflock/device-agent.log",
		"ConditionPathExists=/opt/ironflock/device-agent/enrolled",
		"Environment=IRONFLOCK_AGENT_SUPERVISOR=systemd",
		"ExecStartPre=-/opt/ironflock/device-agent/repair.sh",
		"StartLimitIntervalSec=0",
		"Restart=always",
		"SuccessExitStatus=75",
		"RuntimeDirectory=ironflock-device-agent",
	} {
		assert.Contains(t, unit, want)
	}
	assert.NotContains(t, unit, "PrivateTmp", "the single-instance locks live in /tmp")
}

func TestWriteFilesBelowRoot(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, WriteFiles(root, DefaultPaths()))

	unit, err := os.ReadFile(filepath.Join(root, UnitPath))
	require.NoError(t, err)
	assert.Equal(t, DefaultPaths().Unit(), string(unit))

	info, err := os.Stat(filepath.Join(root, DefaultPaths().Repair()))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
	assert.NoFileExists(t, filepath.Join(root, UnitPath)+".tmp")
}

// fakeBinary writes a shell script that answers -version like the agent, or
// fails when broken.
func fakeBinary(t *testing.T, path, version string, works bool) {
	t.Helper()
	body := "#!/bin/sh\necho " + version + "\n"
	if !works {
		body = "#!/bin/sh\nexit 1\n"
	}
	require.NoError(t, os.WriteFile(path, []byte(body), 0o755))
}

func runRepair(t *testing.T, p Paths) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "repair.sh")
	require.NoError(t, os.WriteFile(script, []byte(p.RepairScript()), 0o755))
	out, err := exec.Command("sh", script).CombinedOutput()
	require.NoError(t, err, "the repair script never fails the unit")
	return string(out)
}

func TestRepairScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	t.Run("leaves a working binary alone", func(t *testing.T) {
		p := Paths{AgentDir: t.TempDir()}
		fakeBinary(t, p.Binary(), "1.3.0", true)
		fakeBinary(t, filepath.Join(p.AgentDir, "device-agent-prev"), "1.2.0", true)
		assert.Empty(t, runRepair(t, p))
		out, _ := exec.Command(p.Binary(), "-version").Output()
		assert.Equal(t, "1.3.0", strings.TrimSpace(string(out)))
	})
	t.Run("restores prev when the active binary is missing", func(t *testing.T) {
		p := Paths{AgentDir: t.TempDir()}
		fakeBinary(t, filepath.Join(p.AgentDir, "device-agent-prev"), "1.2.0", true)
		assert.Contains(t, runRepair(t, p), "restored")
		out, err := exec.Command(p.Binary(), "-version").Output()
		require.NoError(t, err)
		assert.Equal(t, "1.2.0", strings.TrimSpace(string(out)))
	})
	t.Run("restores a download when active and prev are broken", func(t *testing.T) {
		p := Paths{AgentDir: t.TempDir()}
		fakeBinary(t, p.Binary(), "", false)
		fakeBinary(t, filepath.Join(p.AgentDir, "device-agent-prev"), "", false)
		fakeBinary(t, filepath.Join(p.AgentDir, "device-agent-v1.3.1"), "1.3.1", true)
		assert.Contains(t, runRepair(t, p), "restored")
		out, err := exec.Command(p.Binary(), "-version").Output()
		require.NoError(t, err)
		assert.Equal(t, "1.3.1", strings.TrimSpace(string(out)))
	})
	t.Run("reports when nothing works", func(t *testing.T) {
		p := Paths{AgentDir: t.TempDir()}
		assert.Contains(t, runRepair(t, p), "no working agent binary")
	})
}
