package winmigrate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reagent/winservice"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The boundaries of Begin (the legacy agent's side: harden, the migrator
// copy, the migrate service, the journal last, the start) and of the paths
// that reach it (the automatic trigger, `migrate-layout start`).

// beginVariant prepares the device before the Begin under test.
type beginVariant struct {
	name  string
	setup func(t *testing.T, d *device)
}

func beginVariants() []beginVariant {
	return []beginVariant{
		{"fresh", func(t *testing.T, d *device) {}},
		{"after a rollback", func(t *testing.T, d *device) {
			e := d.env()
			e.Fault = "health"
			d.stage(e)
			require.Error(t, d.run(e))
			require.True(t, d.journal().Failed())
			d.sys.counting = false
			d.sys.calls = nil
			d.sys.now = d.sys.now.Add(retryAfterFailure)
		}},
		{"the start fails", func(t *testing.T, d *device) {
			d.sys.failOn["start "+winservice.MigrateServiceName] = errors.New("the service did not respond to the start request in a timely fashion")
		}},
	}
}

// A power cut after every counted call of Check+Begin: after the boot an
// agent is connected, and the device is either migrated (a journal written
// before the cut resumes) or still legacy with a clean retry ahead.
func TestBeginSurvivesAPowerCutAtEveryCall(t *testing.T) {
	for _, v := range beginVariants() {
		ref := newDevice(t)
		v.setup(t, ref)
		ref.sys.counting = true
		require.NoError(t, Check(ref.env()))
		err := Begin(ref.env(), ref.selfExe())
		if v.name == "the start fails" {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
		}
		total := len(ref.sys.calls)
		require.Greater(t, total, 5, v.name)
		for n := 1; n <= total; n++ {
			t.Run(fmt.Sprintf("%s/call %d %s", v.name, n, ref.sys.calls[n-1]), func(t *testing.T) {
				d := newDevice(t)
				v.setup(t, d)
				d.sys.counting = true
				d.sys.crashAt = n
				require.NoError(t, Check(d.env()))
				require.PanicsWithValue(t, errCrash, func() { _ = Begin(d.env(), d.selfExe()) })
				d.sys.crashAt = 0
				delete(d.sys.failOn, "start "+winservice.MigrateServiceName)

				d.boot()
				d.runMigratorAfterBoot()
				connected := d.connectedService()
				require.NotEmpty(t, connected, "an agent is connected after the boot")
				j, err := LoadJournal(d.env().Layout)
				require.NoError(t, err)
				if j != nil && j.Done {
					assert.Equal(t, winservice.ServiceName, connected)
					return
				}
				assert.Equal(t, winservice.LegacyServiceName, connected)
				if j != nil {
					require.True(t, j.Failed(), "a journal neither done nor failed would never be retried: %+v", j)
					d.sys.now = d.sys.now.Add(retryAfterFailure)
				}
				due, err := Due(d.env())
				require.NoError(t, err)
				require.True(t, due, "the legacy agent's next trigger stages again")
				d.sys.counting = false
				require.NoError(t, Check(d.env()))
				require.NoError(t, Begin(d.env(), d.selfExe()))
				d.sys.counting = true
				require.NoError(t, d.run(d.env()))
				assert.True(t, d.journal().Done)
				assert.Equal(t, winservice.ServiceName, d.connectedService())
			})
		}
	}
}

// The migrator's copy is lost (quarantined) after a power cut before
// prepare pointed the repair task at the universal script: the boot's legacy
// script knows nothing of it and the migrate service cannot start; the
// legacy agent, connected again, stages the migrator anew when it finds the
// service cannot start (startMigrateService: Restage), and the move ends
// where an uninterrupted one ends.
func TestAMigratorLostBeforeTheTaskSwitchIsRestagedByTheLegacyAgent(t *testing.T) {
	want := finalState(t)
	ref := newDevice(t)
	ref.stage(ref.env())
	require.NoError(t, ref.run(ref.env()))
	taskSwitch := index(ref.sys.calls, "task .migrate/repair.cmd") + 1
	require.Greater(t, taskSwitch, 1)
	for n := 1; n <= taskSwitch+1; n++ {
		t.Run(fmt.Sprintf("call %d %s", n, ref.sys.calls[n-1]), func(t *testing.T) {
			d := newDevice(t)
			d.stage(d.env())
			d.sys.crashAt = n
			require.PanicsWithValue(t, errCrash, func() { _ = Run(d.env()) })
			d.sys.crashAt = 0
			require.NoError(t, os.Remove(d.env().Layout.MigratorExe()), "the antivirus took the migrator")
			d.boot()
			require.Equal(t, winservice.LegacyServiceName, d.connectedService(), "the legacy agent is connected after the boot")
			if !d.migrateServiceRunning() {
				// what the legacy agent's trigger does for a journal in
				// progress: start the migrate service; stage it again when
				// that fails
				require.Error(t, (&uncounted{d.sys}).StartService(winservice.MigrateServiceName))
				require.NoError(t, Restage(d.env(), d.selfExe()))
				require.NoError(t, (&uncounted{d.sys}).StartService(winservice.MigrateServiceName))
			}
			d.runMigratorAfterBoot()
			assert.True(t, d.journal().Done, "%+v", d.journal())
			assert.Equal(t, winservice.ServiceName, d.connectedService())
			assert.Equal(t, want, d.snapshot())
		})
	}
}

// beginDuringWatch races a Begin (a Studio click, `migrate-layout start`)
// against the previous migrator's ensure-agent watch after its rollback: the
// journal says failed, the legacy agent is connected, the migrate service
// still runs.
type beginDuringWatch struct {
	*fakeSystem
	d        *device
	raced    bool
	beginErr error
}

func (b *beginDuringWatch) Sleep(dur time.Duration) {
	if !b.raced && dur == 5*time.Second {
		legacy := b.services[winservice.LegacyServiceName]
		if j, _ := LoadJournal(b.layout); j != nil && j.Failed() && legacy != nil && legacy.state == StateRunning {
			b.raced = true
			e2 := b.d.env()
			e2.Sys = &uncounted{b.fakeSystem}
			if err := Check(e2); err != nil {
				b.beginErr = fmt.Errorf("check: %w", err)
			} else {
				b.beginErr = Begin(e2, b.d.selfExe())
			}
		}
	}
	b.fakeSystem.Sleep(dur)
}

// A Begin while the previous attempt's migrator is still finishing (its
// ensure-agent watch lasts a minute after the journal says failed) is
// refused with a clear reason: staged under a running migrator, the fresh
// journal would be marked ensured and the service disabled — a migration in
// progress that nothing resumes. Later, the retry runs to done.
func TestBeginIsRefusedWhileTheMigrateServiceRuns(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	e.Fault = "health"
	race := &beginDuringWatch{fakeSystem: d.sys, d: d}
	e.Sys = race
	require.ErrorContains(t, d.run(e), "health")
	require.True(t, race.raced, "the racing Begin ran")
	require.ErrorContains(t, race.beginErr, "still running")
	j := d.journal()
	assert.True(t, j.Failed())
	assert.Equal(t, winservice.LegacyServiceName, j.EnsuredService)
	assert.Equal(t, 1, j.Attempts)
	assert.Equal(t, winservice.StartDisabled, d.sys.services[winservice.MigrateServiceName].startType)
	assert.Equal(t, winservice.LegacyServiceName, d.connectedService())

	// the retry a day later, the migrator gone
	d.sys.now = d.sys.now.Add(retryAfterFailure)
	e.Fault = ""
	d.stage(e)
	require.NoError(t, d.run(e))
	assert.True(t, d.journal().Done)
	assert.Equal(t, winservice.ServiceName, d.connectedService())
}

// The migrate service's executable cannot be removed while it runs (Windows
// keeps a mapped image; the fake models it): a Stage under a running
// migrator would fail there, opaquely, if the status check did not refuse
// it first.
func TestARunningMigratorsExecutableCannotBeRemoved(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	require.True(t, d.migrateServiceRunning())
	require.ErrorContains(t, d.sys.Remove(e.Layout.MigratorExe()), "Access is denied")
	d.migratorExits()
	require.NoError(t, d.sys.Remove(e.Layout.MigratorExe()))
}

// A journal (and a migrator) a local user planted before the root was
// hardened (a legacy install never hardens it; the first Stage does) is
// removed before anything reads it: planted in progress, it would otherwise
// stop the move and every self-update for good; planted failed with
// prefer_in_place, it would steer the next attempt.
func TestAPlantedJournalIsRemovedBeforeTheTriggerReadsIt(t *testing.T) {
	t.Run("in progress", func(t *testing.T) {
		d := newDevice(t)
		e := d.env()
		d.sys.owners[e.Layout.Journal()] = "user"
		d.file(e.Layout.Journal(), `{"version":"1.4.0","step":0,"started_at":"2026-10-23T12:00:00Z","updated_at":"2026-10-23T12:00:00Z"}`, 0o644)
		d.sys.owners[e.Layout.MigrateDir()] = "user"
		d.file(e.Layout.MigratorExe(), "planted", 0o755)
		require.True(t, InProgress(e.Layout), "the planted journal reads as in progress")

		// what the trigger does first
		require.NoError(t, RemoveForeign(e))
		assert.False(t, InProgress(e.Layout))
		assert.False(t, d.exists(e.Layout.Journal()))
		assert.False(t, d.exists(e.Layout.MigratorExe()))
		due, err := Due(e)
		require.NoError(t, err)
		assert.True(t, due)
		d.stage(e)
		require.NoError(t, d.run(e))
		assertMigrated(t, d)
	})
	t.Run("failed, steering the strategy", func(t *testing.T) {
		d := newDevice(t)
		e := d.env()
		d.sys.owners[e.Layout.Journal()] = "user"
		d.file(e.Layout.Journal(), `{"version":"1.4.0","step":0,"failed_at":"2026-10-01T12:00:00Z","attempts":7,"prefer_in_place":true,"started_at":"2026-10-01T12:00:00Z","updated_at":"2026-10-01T12:00:00Z"}`, 0o644)
		d.stage(e) // Stage removes it before it reads the previous attempt
		require.NoError(t, d.run(e))
		j := d.journal()
		assert.Equal(t, StrategyMove, j.Strategy, "not steered in place")
		assert.Equal(t, 0, j.Attempts)
		assert.False(t, j.PreferInPlace)
	})
}

// A link at the migration's own files — what a local user can make there
// before the root is hardened, a junction needing no privilege (Go reports it
// on Windows as irregular, not as a symbolic link) — goes whoever owns it.
// Here the owner read reports Administrators for it, as one that follows the
// link to the folder it points at does: kept, the link would have Stage copy
// the migrator through it and register the migrate service's executable
// behind it, and the engine read a journal through it. What a link points at
// keeps its content: the link goes, never its target.
func TestALinkAtTheMigrationsFilesIsRemovedWhoeverOwnsIt(t *testing.T) {
	for _, link := range []struct {
		name string
		typ  os.FileMode
	}{{"junction", os.ModeIrregular}, {"symbolic link", os.ModeSymlink}} {
		t.Run(link.name, func(t *testing.T) {
			d := newDevice(t)
			e := d.env()
			folder := filepath.Join(d.root, "..", "administrators")
			d.file(filepath.Join(folder, winservice.ExeName), "planted", 0o755)
			journal := filepath.Join(d.root, "..", "administrators.json")
			d.file(journal, `{"version":"1.4.0","step":0,"started_at":"2026-10-23T12:00:00Z","updated_at":"2026-10-23T12:00:00Z"}`, 0o644)
			d.sys.linkTypes = map[string]os.FileMode{}
			links := map[string]string{e.Layout.MigrateDir(): folder, e.Layout.Journal(): journal, e.Layout.JournalBak(): journal}
			for path, target := range links {
				require.NoError(t, os.Symlink(target, path))
				d.sys.linkTypes[path] = link.typ
				owned, err := d.sys.OwnedByAdministrators(path)
				require.NoError(t, err)
				require.True(t, owned)
			}
			require.True(t, InProgress(e.Layout), "the journal behind the link reads as in progress")

			require.NoError(t, RemoveForeign(e))
			for path := range links {
				assert.False(t, d.exists(path), "%s removed", path)
			}
			assert.Equal(t, "planted", d.content(filepath.Join(folder, winservice.ExeName)), "the folder the link pointed at is untouched")
			assert.FileExists(t, journal)

			// The move stages into a folder of its own and completes.
			d.stage(e)
			info, err := os.Lstat(e.Layout.MigrateDir())
			require.NoError(t, err)
			assert.True(t, info.IsDir(), "a folder, not a link")
			assert.Equal(t, testVersion, d.content(e.Layout.MigratorExe()))
			require.NoError(t, d.run(e))
			assertMigrated(t, d)
			assert.Equal(t, "planted", d.content(filepath.Join(folder, winservice.ExeName)))
		})
	}
}

// The new folder is judged by the owner of the entry itself, a link's own
// (winservice.OwnedByAdministrators reads it so): a link a local user made
// goes — the link, not what it points at — while one an administrator made
// stays: they may have linked DeviceAgent to another drive (or the move
// renamed a linked Reagent to it), and removing it would cut the agent off
// its folder.
func TestALinkedNewFolderIsJudgedByItsOwnOwner(t *testing.T) {
	for _, c := range []struct {
		owner   string
		removed bool
	}{{"user", true}, {"administrators", false}} {
		t.Run(c.owner, func(t *testing.T) {
			d := newDevice(t)
			e := d.env()
			drive := filepath.Join(d.root, "..", "other-drive")
			d.file(filepath.Join(drive, "data"), "kept", 0o644)
			require.NoError(t, os.Symlink(drive, d.newDir()))
			d.sys.linkTypes = map[string]os.FileMode{d.newDir(): os.ModeIrregular}
			d.sys.owners[d.newDir()] = c.owner

			require.NoError(t, RemoveForeign(e))
			assert.Equal(t, !c.removed, d.exists(d.newDir()))
			assert.Equal(t, "kept", d.content(filepath.Join(drive, "data")), "what the link points at is untouched")
		})
	}
}

// The console's `migrate-layout` with no verb runs the engine in this
// process (runMigrationHere: ForegroundCheck, then Run), possibly before any
// Stage hardened the root: a journal and a migrator a local user planted go
// first. Read, a planted journal in progress would have the engine carry out
// the move without Check and copy the planted migrator into the new folder
// as its rollback binary; a planted done one, naming a new folder the user
// made, would delete reagent and point the repair task — and the new
// service — into the user's folder.
func TestAConsoleRunRemovesAPlantedJournalBeforeItReadsIt(t *testing.T) {
	for _, c := range []struct {
		name    string
		journal func(d *device) Journal
	}{
		{"in progress", func(d *device) Journal {
			return Journal{Version: testVersion, StartedAt: "2026-10-23T12:00:00Z", UpdatedAt: "2026-10-23T12:00:00Z"}
		}},
		{"done, into a folder the user made", func(d *device) Journal {
			mine := filepath.Join(d.root, "mine")
			d.file(filepath.Join(mine, winservice.ExeName), "planted", 0o755)
			d.file(filepath.Join(mine, winservice.ConfigName), "{}", 0o600)
			d.file(filepath.Join(mine, "device-agent.db"), "database", 0o644)
			d.sys.owners[mine] = "user"
			return Journal{
				Version: testVersion, Step: len(steps), Done: true, Planned: true, Strategy: StrategyInPlace,
				Legacy:    LegacyService{AgentDir: d.legacyDir, Exe: d.selfExe()},
				New:       NewService{AgentDir: mine, AppsDir: filepath.Join(mine, "apps")},
				StartedAt: "2026-10-23T12:00:00Z", UpdatedAt: "2026-10-23T12:00:00Z",
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := newDevice(t)
			e := d.env()
			data, err := json.Marshal(c.journal(d))
			require.NoError(t, err)
			d.sys.owners[e.Layout.Journal()] = "user"
			d.file(e.Layout.Journal(), string(data), 0o644)
			d.sys.owners[e.Layout.MigrateDir()] = "user"
			d.file(e.Layout.MigratorExe(), "planted", 0o755)
			repairTask := d.sys.task.script

			// runMigrationHere, run from a copy outside the agent folder
			require.NoError(t, ForegroundCheck(e, filepath.Join(d.root, "..", "copy", winservice.ExeName)))
			require.NoError(t, Run(e))

			assert.False(t, d.exists(e.Layout.Journal()), "the planted journal is gone")
			assert.False(t, d.exists(e.Layout.MigrateDir()), "the planted migrator is gone")
			assert.Empty(t, d.sys.hardened, "nothing staged")
			assert.True(t, d.serviceExists(winservice.LegacyServiceName))
			assert.False(t, d.serviceExists(winservice.ServiceName))
			assert.Equal(t, winservice.LegacyServiceName, d.connectedService())
			assert.Equal(t, repairTask, d.sys.task.script)
			assert.FileExists(t, d.selfExe(), "the legacy folder stays where it is")
			assert.False(t, d.exists(d.newDir()))
		})
	}
}

// Begin's start of the migrate service fails: the journal says failed, the
// service is disabled. A power cut between those two writes leaves an
// enabled migrate service with a failed journal: at the boot it disables
// itself once it has made sure the legacy agent runs.
func TestABeginThatFailsToStartIsDisabledAtTheNextBoot(t *testing.T) {
	ref := newDevice(t)
	ref.sys.failOn["start "+winservice.MigrateServiceName] = errors.New("timeout")
	require.NoError(t, Check(ref.env()))
	ref.sys.counting = true
	require.Error(t, Begin(ref.env(), ref.selfExe()))
	disable := lastIndex(ref.sys.calls, "start-type "+winservice.MigrateServiceName+" disabled")
	require.Greater(t, disable, lastIndex(ref.sys.calls, "write "+winservice.JournalName), "the failed journal is saved, then the service disabled")

	d := newDevice(t)
	d.sys.failOn["start "+winservice.MigrateServiceName] = errors.New("timeout")
	require.NoError(t, Check(d.env()))
	d.sys.counting = true
	d.sys.crashAt = disable + 1 // the power goes before the disable
	require.PanicsWithValue(t, errCrash, func() { _ = Begin(d.env(), d.selfExe()) })
	d.sys.crashAt = 0
	delete(d.sys.failOn, "start "+winservice.MigrateServiceName)
	require.True(t, d.journal().Failed())
	require.NotEqual(t, winservice.StartDisabled, d.sys.services[winservice.MigrateServiceName].startType)

	d.boot()
	d.runMigratorAfterBoot()
	j := d.journal()
	assert.True(t, j.Failed())
	assert.Equal(t, winservice.LegacyServiceName, j.EnsuredService)
	assert.Equal(t, winservice.StartDisabled, d.sys.services[winservice.MigrateServiceName].startType)
	assert.Equal(t, winservice.LegacyServiceName, d.connectedService())
}

// The engine runs in a console process only when no migrate service runs
// and the executable is outside the folder the move renames: its own image
// would block the rename and every attempt would end in place.
func TestForegroundCheck(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	require.NoError(t, ForegroundCheck(e, filepath.Join(d.root, "..", "copy", "reagent.exe")))
	err := ForegroundCheck(e, d.selfExe())
	require.ErrorContains(t, err, "runs from the agent folder")
	assert.ErrorContains(t, err, "copy outside")

	d.stage(e) // the migrate service runs
	err = ForegroundCheck(e, filepath.Join(d.root, "..", "copy", "reagent.exe"))
	require.ErrorContains(t, err, "still running")
}

// One journal, one engine: a second Run (the migrate service started by the
// legacy agent's trigger next to a foreground run) is refused by the
// machine-wide lock and touches nothing.
func TestOnlyOneEngineRunsAtATime(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	reached := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	e.Logf = func(format string, args ...any) {
		if fmt.Sprintf(format, args...) == "migrate: stop-apps" {
			once.Do(func() { close(reached); <-release })
		}
	}
	first := make(chan error, 1)
	go func() { first <- Run(e) }()
	<-reached

	second := d.env()
	second.Logf = nil
	err := Run(second)
	require.ErrorContains(t, err, "another migration process is running")
	assert.Equal(t, uint32(1), exitCodeFor(second, err), "the migrate service asks for a restart and tries again")

	close(release)
	require.NoError(t, <-first)
	d.migratorExits()
	j := d.journal()
	assert.True(t, j.Done)
	assert.Equal(t, 1, j.Runs, "the refused engine counted no run")
	assert.Equal(t, 1, countCalls(d.sys.calls, "rename Reagent DeviceAgent"), "the move ran once")
	assert.Equal(t, winservice.ServiceName, d.connectedService())
}

func countCalls(calls []string, call string) int {
	n := 0
	for _, c := range calls {
		if c == call {
			n++
		}
	}
	return n
}

// prepare looks at update-state.json once more: Check passed before the
// journal existed, and a swap that began just then may still be renaming
// the executables. The attempt rolls back before it touched anything.
func TestPrepareRefusesAnUpdateUnderWay(t *testing.T) {
	d := newDevice(t)
	before := d.snapshot()
	e := d.env()
	d.stage(e)
	d.file(filepath.Join(d.legacyDir, "update-state.json"), `{"phase":"swapping","new_version":"1.4.1"}`, 0o644)
	before[filepath.ToSlash(filepath.Join(winservice.LegacyFolder, "update-state.json"))] = `{"phase":"swapping","new_version":"1.4.1"}`
	err := d.run(e)
	require.ErrorContains(t, err, "an agent update is in swapping")
	j := d.journal()
	assert.True(t, j.Failed())
	assert.Equal(t, 0, j.FailedStep)
	assertRolledBack(t, d, before)
}

// The live agent folder is never swept, whoever owns it: on Windows 10/11
// the legacy folder (and so the moved one) belongs to the administrator
// account whose elevated `service install` made it, not to the
// Administrators group. A console `migrate-layout start` on a migrated
// device must not take device.flock and the apps' data with it. A folder
// that is no agent's is still removed.
func TestTheLiveAgentFolderIsNeverSwept(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	require.NoError(t, d.run(e))
	require.True(t, d.journal().Done)
	d.sys.owners[d.newDir()] = "user"

	require.NoError(t, RemoveForeign(e))
	assert.True(t, d.exists(filepath.Join(d.newDir(), winservice.ConfigName)), "device.flock stays")
	assert.Equal(t, "app data", d.content(filepath.Join(d.newDir(), "apps", "prod", "app1", "data.txt")))
	assert.Equal(t, winservice.ServiceName, d.connectedService())

	// a registered service runs from it although its configuration is gone
	require.NoError(t, os.Remove(filepath.Join(d.newDir(), winservice.ConfigName)))
	require.NoError(t, RemoveForeign(e))
	assert.True(t, d.exists(d.newDir()), "the folder a service runs from stays")

	// no agent's folder: removed
	d = newDevice(t)
	e = d.env()
	d.file(filepath.Join(d.newDir(), "planted.exe"), "x", 0o755)
	d.sys.owners[d.newDir()] = "user"
	require.NoError(t, RemoveForeign(e))
	assert.False(t, d.exists(d.newDir()))
}
