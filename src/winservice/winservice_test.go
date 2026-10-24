package winservice

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func crlfLines(t *testing.T, content string) []string {
	t.Helper()
	assert.True(t, strings.HasSuffix(content, "\r\n"), "batch files want CRLF endings")
	lines := strings.Split(content, "\r\n")
	for _, line := range lines {
		assert.NotContains(t, line, "\n", "no bare LF inside lines")
	}
	return lines
}

func TestRepairCmd(t *testing.T) {
	content := RepairCmd(`C:\ProgramData\IronFlock\DeviceAgent`)
	crlfLines(t, content)

	assert.Contains(t, content, `set "AGENTDIR=C:\ProgramData\IronFlock\DeviceAgent"`)
	assert.Contains(t, content, `if exist "%AGENTDIR%\device-agent.exe" goto start`)
	assert.Contains(t, content, `ren "%AGENTDIR%\device-agent-prev.exe" "device-agent.exe"`)
	assert.Contains(t, content, `device-agent-v*.exe`)
	assert.Contains(t, content, "sc start ironflock-device-agent\r\n")
	assert.NotContains(t, content, "reagent", "the installed agent has only the current names")
}

// The universal script of a migration must point at the right places at every
// instant: the journal, the migrator's folder, both agent folders and both
// services.
func TestUniversalRepairCmd(t *testing.T) {
	root := `C:\ProgramData\IronFlock`
	content := UniversalRepairCmd(root, root+`\Reagent`, root+`\DeviceAgent`)
	lines := crlfLines(t, content)

	assert.Contains(t, lines, `set "ROOT=C:\ProgramData\IronFlock"`)
	assert.Contains(t, lines, `set "MIGRATE=C:\ProgramData\IronFlock\.migrate"`)
	assert.Contains(t, lines, `set "NEWDIR=C:\ProgramData\IronFlock\DeviceAgent"`)
	assert.Contains(t, lines, `set "OLDDIR=C:\ProgramData\IronFlock\Reagent"`)
	// in progress: a migrator with the recorded SHA-256 is kept, another one
	// (a torn copy) is replaced from an agent binary of this move with that
	// hash (else the first one there), copied through a temporary name, and
	// the migrate service is started. Nothing is run to check a binary:
	// cmd.exe blocks for good on a truncated or empty executable.
	assert.Contains(t, lines, `if not exist "%ROOT%\.device-agent-migration.json" goto agents`)
	assert.Contains(t, lines, `if not exist "%MIGRATE%\device-agent.exe" goto restore`)
	assert.Contains(t, lines, `call :verified "%MIGRATE%\device-agent.exe"`)
	assert.Contains(t, lines, `if defined OK goto migrate`)
	assert.NotContains(t, content, "-version", "a binary is never run to check it")
	assert.Contains(t, content, `certutil -hashfile "%~1" SHA256`)
	assert.Contains(t, lines, `set /p WANT=<"%MIGRATE%\device-agent.exe.sha256"`)
	assert.Contains(t, lines, `if defined FALLBACK call :install "%FALLBACK%"`)
	assert.Contains(t, lines, `del /f /q "%MIGRATE%\device-agent.exe" >nul 2>&1`)
	assert.Contains(t, content, `copy /y "%~1" "%MIGRATE%\device-agent.exe.tmp" >nul && move /y "%MIGRATE%\device-agent.exe.tmp" "%MIGRATE%\device-agent.exe" >nul`)
	assert.Contains(t, content, `"%OLDDIR%\reagent.exe"`)
	assert.Contains(t, content, `"%NEWDIR%\device-agent.exe"`)
	assert.NotContains(t, content, `"%OLDDIR%\reagent-prev.exe"`, "the legacy spare is from before 1.4.0: not a migrator")
	assert.Contains(t, content, `dir /b /o-d "%NEWDIR%\device-agent-v*.exe"`)
	assert.Contains(t, content, `dir /b /o-d "%OLDDIR%\reagent-v*.exe"`)
	assert.NotContains(t, content, `*-v*.exe`, "a failed download (-failed-v) is never a migrator")
	assert.Contains(t, lines, "sc start ironflock-device-agent-migrate")
	// otherwise: whichever service is enabled
	assert.Contains(t, lines, `call :repair ironflock-device-agent "%NEWDIR%" device-agent`)
	assert.Contains(t, lines, `call :repair reagent "%OLDDIR%" reagent`)
	assert.Contains(t, lines, `if /i "%START%"=="0x4" exit /b 0`)
	assert.Contains(t, lines, `  ren "%DIR%\%NAME%-prev.exe" "%NAME%.exe"`)
	assert.Contains(t, lines, `  copy /y "%DIR%\device-agent-prev.exe" "%DIR%\%NAME%.exe" >nul`, "prepare drops reagent-prev.exe; its own prev serves the legacy service too")
	assert.Contains(t, lines, "sc start %SVC%")

	// a custom legacy folder (in-place strategy) keeps its own path
	custom := UniversalRepairCmd(root, `C:\Users\alice\reagent`, `C:\Users\alice\reagent`)
	assert.Contains(t, custom, `set "OLDDIR=C:\Users\alice\reagent"`)
	assert.Contains(t, custom, `set "NEWDIR=C:\Users\alice\reagent"`)
}

func TestAgentSpecPinsEveryPath(t *testing.T) {
	// Host paths (filepath.Join): the agent dir is a real folder on the
	// machine the spec is made on; the tests run on any OS.
	agentDir := filepath.Join(t.TempDir(), "DeviceAgent")
	appsDir := filepath.Join(agentDir, "apps")
	spec := AgentSpec(agentDir, appsDir, []string{"-debug"}, []string{"HTTPS_PROXY=http://proxy:3128"})

	assert.Equal(t, "ironflock-device-agent", spec.Name)
	assert.Equal(t, "IronFlock Device Agent", spec.DisplayName)
	assert.Equal(t, filepath.Join(agentDir, "device-agent.exe"), spec.Exe)
	assert.Equal(t, []string{
		"-config", filepath.Join(agentDir, "device.flock"),
		"-agentDir", agentDir,
		"-appsDir", appsDir,
		"-dbFileName", filepath.Join(agentDir, "device-agent.db"),
		"-logFile", filepath.Join(agentDir, "device-agent.log"),
		"-debug",
	}, spec.Args)
	assert.Equal(t, StartAutomatic, spec.StartType)
	assert.True(t, spec.Delayed)
	assert.Equal(t, []int{5, 30, 120}, spec.RestartDelays)
	assert.True(t, spec.NonCrashRecovery)
	assert.Equal(t, []string{"HTTPS_PROXY=http://proxy:3128"}, spec.Env)
}

func TestMigrateSpecResumesAtBoot(t *testing.T) {
	spec := MigrateSpec(`C:\ProgramData\IronFlock\.migrate\device-agent.exe`, []string{"DOCKER_HOST=tcp://127.0.0.1:2376", "IRONFLOCK_MIGRATE_FAULT=health"})
	assert.Equal(t, "ironflock-device-agent-migrate", spec.Name)
	assert.Equal(t, []string{"migrate-layout"}, spec.Args)
	assert.Equal(t, StartAutomatic, spec.StartType, "a power cut resumes the move at boot")
	assert.False(t, spec.Delayed)
	assert.Equal(t, []int{30}, spec.RestartDelays, "restarted forever, every 30 s")
	assert.True(t, spec.NonCrashRecovery)
	assert.Contains(t, spec.Env, "DOCKER_HOST=tcp://127.0.0.1:2376")
}
