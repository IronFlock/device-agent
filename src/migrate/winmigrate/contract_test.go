package winmigrate

import (
	"os"
	"path/filepath"
	"reagent/winservice"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The behaviour table of the repair scripts, run against the fake's
// interpreter here (TestRepairScriptModel) and against the real scripts on
// Windows (contract_windows_test.go). Each case sets up the device after a
// power cut and says what the script must have done.
type repairScriptCase struct {
	name  string
	setup func(d *device)
	check func(t *testing.T, d *device)
}

func repairScriptCases() []repairScriptCase {
	return []repairScriptCase{
		{
			name:  "legacy script: the exe is present, the service starts",
			setup: func(d *device) {},
			check: func(t *testing.T, d *device) {
				assert.True(t, d.serviceRunning(winservice.LegacyServiceName))
				assert.Equal(t, testVersion, d.content(filepath.Join(d.legacyDir, "reagent.exe")))
			},
		},
		{
			name: "legacy script: a vacant exe is restored from prev",
			setup: func(d *device) {
				require.NoError(d.t, os.Remove(filepath.Join(d.legacyDir, "reagent.exe")))
			},
			check: func(t *testing.T, d *device) {
				assert.Equal(t, "1.3.8", d.content(filepath.Join(d.legacyDir, "reagent.exe")))
				assert.False(t, d.exists(filepath.Join(d.legacyDir, "reagent-prev.exe")))
				assert.True(t, d.serviceRunning(winservice.LegacyServiceName))
			},
		},
		{
			name: "legacy script: without prev, the newest download",
			setup: func(d *device) {
				require.NoError(d.t, os.Remove(filepath.Join(d.legacyDir, "reagent.exe")))
				require.NoError(d.t, os.Remove(filepath.Join(d.legacyDir, "reagent-prev.exe")))
			},
			check: func(t *testing.T, d *device) {
				assert.Equal(t, "1.3.8", d.content(filepath.Join(d.legacyDir, "reagent.exe")))
				assert.True(t, d.serviceRunning(winservice.LegacyServiceName))
			},
		},
		// The universal script is the task's from the prepare step on
		// (before, a power cut leaves the legacy script and service as they
		// were); these cases lose power right after prepare.
		{
			name: "universal script, journal present: the migrator is restored from reagent.exe and started",
			setup: func(d *device) {
				d.pausedAfter("prepare")
				require.NoError(d.t, os.Remove(Layout{Root: d.root}.MigratorExe()))
			},
			check: func(t *testing.T, d *device) {
				assert.Equal(t, testVersion, d.content(Layout{Root: d.root}.MigratorExe()))
				assert.True(t, d.serviceRunning(winservice.MigrateServiceName))
			},
		},
		{
			name: "universal script, journal present, migrator present: just started",
			setup: func(d *device) {
				d.pausedAfter("prepare")
			},
			check: func(t *testing.T, d *device) {
				assert.True(t, d.serviceRunning(winservice.MigrateServiceName))
				assert.False(t, d.serviceRunning(winservice.LegacyServiceName), "the script leaves the agents to the migrator")
			},
		},
		{
			name: "universal script, no journal: the enabled agent service is repaired from the move's prev and started",
			setup: func(d *device) {
				d.pausedAfter("prepare")
				require.NoError(d.t, os.Remove(Layout{Root: d.root}.Journal()))
				require.NoError(d.t, os.Remove(Layout{Root: d.root}.JournalBak()))
				// prepare dropped reagent-prev.exe and reagent-v1.3.8.exe;
				// device-agent-prev.exe (a copy of this version) is what is left
				require.NoError(d.t, os.Remove(filepath.Join(d.legacyDir, "reagent.exe")))
			},
			check: func(t *testing.T, d *device) {
				assert.Equal(t, testVersion, d.content(filepath.Join(d.legacyDir, "reagent.exe")))
				assert.True(t, d.exists(filepath.Join(d.legacyDir, "device-agent-prev.exe")), "copied, not renamed")
				assert.True(t, d.serviceRunning(winservice.LegacyServiceName))
				assert.False(t, d.serviceRunning(winservice.MigrateServiceName))
			},
		},
		{
			name: "universal script, no journal: a disabled service is left alone",
			setup: func(d *device) {
				d.pausedAfter("prepare")
				require.NoError(d.t, os.Remove(Layout{Root: d.root}.Journal()))
				require.NoError(d.t, os.Remove(Layout{Root: d.root}.JournalBak()))
				require.NoError(d.t, os.Remove(filepath.Join(d.legacyDir, "reagent.exe")))
				d.sys.services[winservice.LegacyServiceName].startType = winservice.StartDisabled
			},
			check: func(t *testing.T, d *device) {
				assert.False(t, d.serviceRunning(winservice.LegacyServiceName))
				assert.False(t, d.exists(filepath.Join(d.legacyDir, "reagent.exe")), "nothing restored")
			},
		},
		{
			name: "universal script, journal present, migrator and prev gone after rename-files: the migrator is restored from the renamed exe",
			setup: func(d *device) {
				d.pausedAfter("rename-files")
				require.NoError(d.t, os.Remove(Layout{Root: d.root}.MigratorExe()))
				require.NoError(d.t, os.Remove(filepath.Join(d.legacyDir, "device-agent-prev.exe")))
			},
			check: func(t *testing.T, d *device) {
				assert.Equal(t, testVersion, d.content(Layout{Root: d.root}.MigratorExe()))
				assert.True(t, d.serviceRunning(winservice.MigrateServiceName))
			},
		},
		{
			name: "universal script, journal present but the migrate service disabled (rolled back): the enabled agent service is repaired and started",
			setup: func(d *device) {
				// a real rollback: the journal says failed_at, the migrate
				// service is disabled, the legacy agent runs again
				e := d.env()
				e.Fault = "register-service"
				require.NoError(d.t, Check(e))
				require.NoError(d.t, Begin(e, d.selfExe()))
				require.Error(d.t, Run(e))
				d.migratorExits()
				require.True(d.t, d.journal().Failed())
				d.powerCut()
				require.NoError(d.t, os.Remove(filepath.Join(d.legacyDir, "reagent.exe")))
			},
			check: func(t *testing.T, d *device) {
				assert.Equal(t, testVersion, d.content(filepath.Join(d.legacyDir, "reagent.exe")))
				assert.True(t, d.serviceRunning(winservice.LegacyServiceName))
				assert.False(t, d.serviceRunning(winservice.MigrateServiceName))
			},
		},
		{
			name: "universal script, journal in progress after the move but the power cut took the services' configuration: the migrate service is enabled again and finishes the move",
			setup: func(d *device) {
				d.pausedAfter("move-folder")
				// what the canary device booted with (2026-10-24): the registry
				// as it was before the move (migrate service disabled by an
				// earlier rollback, reagent enabled, its executable moved away),
				// the flushed journal past move-folder
				d.sys.services[winservice.MigrateServiceName].startType = winservice.StartDisabled
				d.sys.services[winservice.LegacyServiceName].startType = winservice.StartAutomatic
			},
			check: func(t *testing.T, d *device) {
				require.Equal(t, winservice.StartAutomatic, d.sys.services[winservice.MigrateServiceName].startType)
				require.True(t, d.serviceRunning(winservice.MigrateServiceName))
				d.runMigratorAfterBoot()
				assert.True(t, d.journal().Done)
				assert.Equal(t, winservice.ServiceName, d.connectedService())
			},
		},
		{
			name: "universal script, journal present, the migrator torn after stop-legacy: replaced from the renamed exe and started",
			setup: func(d *device) {
				d.pausedAfter("stop-legacy")
				// the file is there but torn by a power cut: other bytes than the
				// staged ones (the recorded SHA-256), and it does not run
				require.NoError(d.t, os.WriteFile(Layout{Root: d.root}.MigratorExe(), []byte("torn"), 0o755))
				d.sys.brokenExe[Layout{Root: d.root}.MigratorExe()] = true
			},
			check: func(t *testing.T, d *device) {
				assert.Equal(t, testVersion, d.content(Layout{Root: d.root}.MigratorExe()))
				require.True(t, d.serviceRunning(winservice.MigrateServiceName))
				d.runMigratorAfterBoot()
				assert.True(t, d.journal().Done)
				assert.Equal(t, winservice.ServiceName, d.connectedService())
			},
		},
		{
			name: "universal script, journal present after the move: the migrator is restored from the new folder's exe and started",
			setup: func(d *device) {
				d.pausedAfter("move-folder")
				require.NoError(d.t, os.Remove(Layout{Root: d.root}.MigratorExe()))
				// the rollback target would serve too; without it only the exe can
				require.NoError(d.t, os.Remove(filepath.Join(d.newDir(), "device-agent-prev.exe")))
			},
			check: func(t *testing.T, d *device) {
				assert.Equal(t, testVersion, d.content(Layout{Root: d.root}.MigratorExe()))
				assert.True(t, d.serviceRunning(winservice.MigrateServiceName))
				assert.False(t, d.serviceRunning(winservice.LegacyServiceName))
			},
		},
		{
			name: "universal script, journal present, only a failed download left: no migrator is made",
			setup: func(d *device) {
				d.pausedAfter("prepare")
				require.NoError(d.t, os.Remove(Layout{Root: d.root}.MigratorExe()))
				require.NoError(d.t, os.Remove(filepath.Join(d.legacyDir, winservice.LegacyExeName)))
				require.NoError(d.t, os.Remove(filepath.Join(d.legacyDir, "device-agent-prev.exe")))
				d.file(filepath.Join(d.legacyDir, "reagent-failed-v1.3.9.exe"), "1.3.9", 0o755)
			},
			check: func(t *testing.T, d *device) {
				assert.False(t, d.exists(Layout{Root: d.root}.MigratorExe()), "a failed download is never the migrator")
				assert.False(t, d.serviceRunning(winservice.MigrateServiceName))
			},
		},
		{
			name: "legacy script after a rollback: a vacant reagent.exe is restored from the spare the rollback kept",
			setup: func(d *device) {
				e := d.env()
				e.Fault = "health"
				require.NoError(d.t, Check(e))
				require.NoError(d.t, Begin(e, d.selfExe()))
				require.Error(d.t, Run(e))
				d.powerCut()
				require.NoError(d.t, os.Remove(filepath.Join(d.legacyDir, winservice.LegacyExeName)))
			},
			check: func(t *testing.T, d *device) {
				assert.Equal(t, testVersion, d.content(filepath.Join(d.legacyDir, winservice.LegacyExeName)))
				assert.False(t, d.exists(filepath.Join(d.legacyDir, "reagent-prev.exe")), "renamed into place")
				assert.True(t, d.serviceRunning(winservice.LegacyServiceName))
			},
		},
		{
			name: "new script: a vacant exe is restored from prev",
			setup: func(d *device) {
				e := d.env()
				require.NoError(d.t, Check(e))
				require.NoError(d.t, Begin(e, d.selfExe()))
				require.NoError(d.t, Run(e))
				d.powerCut()
				require.NoError(d.t, os.Remove(filepath.Join(d.newDir(), "device-agent.exe")))
			},
			check: func(t *testing.T, d *device) {
				assert.Equal(t, testVersion, d.content(filepath.Join(d.newDir(), "device-agent.exe")))
				assert.True(t, d.serviceRunning(winservice.ServiceName))
			},
		},
	}
}
