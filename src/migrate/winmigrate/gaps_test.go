package winmigrate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reagent/winservice"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The gaps the boundary provers found (2026-10-23), each with the test that
// fails without its fix.

// The legacy service gives "IronFlock Device Agent" up while the new one
// exists, and gets it back when a rollback removes the new one, so
// services.msc never lists the name twice. Cosmetic: a failed rename never
// fails the migration (TestAFailedDisplayNameRenameIsCosmetic).
func TestTheLegacyServiceGivesItsDisplayNameUpWhileTheNewOneExists(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	require.NoError(t, d.run(e))
	assert.True(t, d.journal().Done)
	assert.Equal(t, winservice.DisplayName, d.sys.services[winservice.ServiceName].spec.DisplayName)
	assert.False(t, d.serviceExists(winservice.LegacyServiceName))

	d = newDevice(t)
	e = d.env()
	e.Fault = "health"
	d.stage(e)
	require.ErrorContains(t, d.run(e), "health")
	assert.True(t, d.journal().Failed())
	assert.False(t, d.serviceExists(winservice.ServiceName))
	assert.Equal(t, winservice.DisplayName, d.sys.services[winservice.LegacyServiceName].spec.DisplayName)
	assert.Equal(t, winservice.LegacyServiceName, d.connectedService())
}

// A rename back refused for a while (an antivirus scanning the renamed
// executable) is retried, and the other files go back regardless. Nothing
// else can bring reagent.exe back here: no migrator copy, no rollback target.
func TestARefusedRenameBackIsRetried(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	e.Fault = "health"
	d.stage(e)
	exe := filepath.Join(d.legacyDir, winservice.ExeName)
	e.Logf = func(format string, args ...any) {
		if fmt.Sprintf(format, args...) == "migrate: undoing rename-files" {
			require.NoError(t, os.Remove(Layout{Root: d.root}.MigratorExe()))
			require.NoError(t, os.Remove(filepath.Join(d.legacyDir, "device-agent-prev.exe")))
			d.sys.holders[exe] = []string{"MsMpEng.exe"}
			d.sys.releaseAfter[exe] = 2
		}
	}
	require.ErrorContains(t, d.run(e), "health")
	assert.Equal(t, testVersion, d.content(filepath.Join(d.legacyDir, winservice.LegacyExeName)))
	assert.False(t, d.exists(exe))
	assert.Equal(t, "database", d.content(filepath.Join(d.legacyDir, "reagent.db")))
	assert.Equal(t, winservice.LegacyServiceName, d.connectedService())
}

// The executable lost during the attempt (quarantined after the move) and
// no migrator copy either: the rollback target device-agent-prev.exe, a copy
// of this version, becomes reagent.exe instead of being removed.
func TestARollbackKeepsTheRollbackTargetWhenTheExecutableIsLost(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	e.Logf = func(format string, args ...any) {
		if fmt.Sprintf(format, args...) == "migrate: move-folder" {
			require.NoError(t, os.Remove(filepath.Join(d.legacyDir, winservice.ExeName)))
			require.NoError(t, os.Remove(Layout{Root: d.root}.MigratorExe()))
		}
	}
	require.ErrorContains(t, d.run(e), "start-agent")
	j := d.journal()
	assert.True(t, j.Failed())
	assert.Equal(t, winservice.LegacyServiceName, j.EnsuredService, j.EnsureError)
	assert.Equal(t, testVersion, d.content(filepath.Join(d.legacyDir, winservice.LegacyExeName)))
	assert.False(t, d.exists(filepath.Join(d.legacyDir, "device-agent-prev.exe")))
	assert.Equal(t, winservice.LegacyServiceName, d.connectedService())
	d.boot()
	d.runMigratorAfterBoot()
	assert.Equal(t, winservice.LegacyServiceName, d.connectedService(), "still connected after a boot")
}

// A migrator lost before the repair task knew the migration (quarantined
// right after Begin) is staged again by the agent when the migrate service
// cannot start; the journal is kept.
func TestRestageReplacesALostMigrator(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	d.migratorExits()
	migrator := Layout{Root: d.root}.MigratorExe()
	require.NoError(t, os.Remove(migrator))
	require.Error(t, d.sys.StartService(winservice.MigrateServiceName))

	require.NoError(t, Restage(e, d.selfExe()))
	assert.Equal(t, testVersion, d.content(migrator))
	require.NoError(t, d.sys.StartService(winservice.MigrateServiceName))
	assert.True(t, d.journal().InProgressState(), "the journal is kept")
	assert.Equal(t, 0, d.journal().Step)

	require.NoError(t, d.run(e))
	assert.True(t, d.journal().Done)
	assert.Error(t, Restage(e, d.selfExe()), "nothing to stage again once done")
}

// ensure-agent that could start nothing is not an outcome: the migrate
// service asks for a restart (its recovery action) and the next run undoes
// again and tries again, until the legacy agent runs.
func TestNoAgentRunningAsksForARestart(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	e.Fault = "health"
	d.stage(e)
	d.sys.failOn["start "+winservice.LegacyServiceName] = errors.New("the service database is locked")
	require.ErrorContains(t, d.run(e), "health")
	j := d.journal()
	assert.True(t, j.Failed())
	assert.Equal(t, "", j.EnsuredService)
	assert.NotEqual(t, "", j.EnsureError)
	assert.Equal(t, uint32(1), exitCodeFor(e, errors.New("health")))
	assert.NotEqual(t, winservice.StartDisabled, d.sys.services[winservice.MigrateServiceName].startType, "the migrate service stays enabled")
	assert.Equal(t, "", d.connectedService())

	// the recovery action starts it again; the lock is gone
	delete(d.sys.failOn, "start "+winservice.LegacyServiceName)
	require.NoError(t, d.run(e))
	j = d.journal()
	assert.True(t, j.Failed())
	assert.Equal(t, winservice.LegacyServiceName, j.EnsuredService)
	assert.Equal(t, uint32(0), exitCodeFor(e, nil))
	assert.Equal(t, winservice.StartDisabled, d.sys.services[winservice.MigrateServiceName].startType)
	assert.Equal(t, winservice.LegacyServiceName, d.connectedService())
}

// -agentDir C:\…\Reagent\ (a trailing separator, kept verbatim in the
// ImagePath) must plan …\DeviceAgent\apps, not …\DeviceAgentapps.
func TestATrailingSeparatorInTheLegacyPathsIsIgnored(t *testing.T) {
	d := newDevice(t)
	s := d.sys.services[winservice.LegacyServiceName]
	for i := 1; i < len(s.spec.Args); i++ {
		if s.spec.Args[i-1] == "-agentDir" || s.spec.Args[i-1] == "-appsDir" {
			s.spec.Args[i] += string(os.PathSeparator)
		}
	}
	e := d.env()
	d.stage(e)
	require.NoError(t, d.run(e))
	j := d.journal()
	assert.True(t, j.Done)
	assert.Equal(t, d.legacyDir, j.Legacy.AgentDir)
	assert.Equal(t, filepath.Join(d.newDir(), "apps"), j.New.AppsDir)
	assert.Equal(t, winservice.ServiceName, d.connectedService())
}

func TestLegacyPathsTrimTrailingSeparators(t *testing.T) {
	cfg := ServiceConfig{
		Exe:  `C:\ProgramData\IronFlock\Reagent\reagent.exe`,
		Args: []string{"-agentDir", `C:\ProgramData\IronFlock\Reagent\`, "-appsDir", `C:\ProgramData\IronFlock\Reagent\apps\`},
	}
	p := legacyPaths(cfg)
	assert.Equal(t, `C:\ProgramData\IronFlock\Reagent`, p.AgentDir)
	assert.Equal(t, `C:\ProgramData\IronFlock\Reagent\apps`, p.AppsDir)
	assert.Equal(t, `C:\`, trimSeparators(`C:\`), "a drive root keeps its separator")
}

// Windows accepts a duplicate display name (TestContractDuplicateDisplayName),
// so a rename of the legacy service that fails only costs services.msc a
// clear listing; the move goes through.
func TestAFailedDisplayNameRenameIsCosmetic(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	d.sys.failOn["display-name "+winservice.LegacyServiceName] = errors.New("access denied")
	require.NoError(t, d.run(e))
	assert.True(t, d.journal().Done)
	assert.Equal(t, winservice.ServiceName, d.connectedService())
}
