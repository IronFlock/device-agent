package winmigrate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reagent/winservice"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The engine's own boundaries: the journal and its backup, fail/rollback,
// ensure-agent's fallback, the agent-side writers.

// A corrupt primary journal with a good backup: the resumed migrator must
// not copy the corrupt file over the backup before it writes. With the power
// going between that copy and the write both files would be unreadable and,
// after rename-files, no agent could start.
func TestACorruptJournalNeverOverwritesItsBackup(t *testing.T) {
	d := newDevice(t)
	before := d.withoutDropped(d.snapshot())
	e := d.env()
	d.stage(e)
	e.Pause = "rename-files"
	e.Sys = &crashOnLongSleep{d.sys}
	require.PanicsWithValue(t, errCrash, func() { _ = Run(e) })
	d.file(e.Layout.Journal(), "{garbage", 0o644) // the primary is unreadable, the backup good
	d.boot()
	d.sys.calls = nil
	d.sys.crashAt = 1 // the resumed migrator's first write (it must not be a copy of the garbage)
	require.PanicsWithValue(t, errCrash, func() { _ = Run(d.env()) })
	d.sys.crashAt = 0
	require.Equal(t, "write "+winservice.JournalName, d.sys.calls[0], "no backup copy of an unreadable journal")
	d.boot()
	d.runMigratorAfterBoot()
	_, err := LoadJournal(e.Layout)
	require.NoError(t, err, "one of the two journal files stays readable")
	assert.Equal(t, winservice.LegacyServiceName, d.connectedService())
	assertRolledBack(t, d, before)
}

// The save of done inside cleanup fails once (a scanner holding the file):
// a proven-healthy migration is not rolled back over it; the tail saves
// again, the journal ends done and not failed.
func TestAFailedDoneSaveDoesNotRollBack(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	armed := false
	e.Logf = func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		if armed { // the failure is transient: one write
			delete(d.sys.failOn, "write "+winservice.JournalName)
			armed = false
		}
		if line == "migrate: cleanup" {
			d.sys.failOn["write "+winservice.JournalName] = errors.New("sharing violation")
			armed = true
		}
	}
	require.NoError(t, d.run(e))
	j := d.journal()
	assert.True(t, j.Done)
	assert.False(t, j.Failed())
	assert.Equal(t, winservice.ServiceName, j.EnsuredService)
	assertMigrated(t, d)
}

// An error after done (anything in the tail) never undoes the move: fail
// is a no-op on a done journal.
func TestFailAfterDoneUndoesNothing(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	require.NoError(t, d.run(e))
	j := d.journal()
	calls := len(d.sys.calls)
	err := e.fail(j, stepIndex("cleanup"), errors.New("late"))
	assert.ErrorContains(t, err, "late")
	assert.Equal(t, calls, len(d.sys.calls), "nothing done")
	assert.True(t, d.journal().Done)
	assert.False(t, d.journal().Failed())
}

// The rollback could not move the folder back and ensure-agent fell back to
// the new service, which never connects (no adoption): at the next boot the
// migrate service undoes again, the holder is gone by then, and the legacy
// service ends connected.
func TestAFallbackThatNeverConnectsIsUndoneAtTheNextBoot(t *testing.T) {
	d := newDevice(t)
	before := d.withoutDropped(d.snapshot())
	e := d.env()
	d.stage(e)
	e.Fault = "health"
	d.sys.connectFails[winservice.ServiceName] = true
	e.Logf = func(format string, args ...any) {
		if fmt.Sprintf(format, args...) == "migrate: health" {
			d.sys.holders[d.newDir()] = []string{"explorer.exe"}
		}
	}
	require.Error(t, d.run(e))
	j := d.journal()
	require.True(t, j.Failed())
	require.Equal(t, winservice.ServiceName, j.EnsuredService, j.EnsureError)
	require.Equal(t, "", d.connectedService(), "the new agent runs but never connects")
	require.NotEqual(t, winservice.StartDisabled, d.sys.services[winservice.MigrateServiceName].startType, "the migrate service stays enabled")

	delete(d.sys.holders, d.newDir())
	d.boot()
	d.runMigratorAfterBoot()
	assertRolledBack(t, d, before)
}

// The failed-move-back state booted with the holder still there (the task
// still runs the universal script, the journal says failed, the new service
// is the last resort): the new agent is connected again after the boot;
// once the holder is gone a boot completes the rollback.
func TestAFailedMoveBackIsCompletedAtABootWithoutTheHolder(t *testing.T) {
	d := newDevice(t)
	d.sys.adopts = false
	e := d.env()
	d.stage(e)
	e.Fault = "health"
	e.Logf = func(format string, args ...any) {
		if fmt.Sprintf(format, args...) == "migrate: health" {
			d.sys.holders[d.newDir()] = []string{"explorer.exe"}
		}
	}
	require.Error(t, d.run(e))
	j := d.journal()
	require.True(t, j.Failed())
	require.Equal(t, winservice.ServiceName, j.EnsuredService, j.EnsureError)
	require.Equal(t, Layout{Root: d.root}.RepairCmd(), d.sys.task.script, "the task is still the universal script")

	d.boot() // the holder is still there
	d.runMigratorAfterBoot()
	assert.Equal(t, winservice.ServiceName, d.connectedService(), "the new agent is connected again after the boot")

	delete(d.sys.holders, d.newDir())
	d.boot()
	d.runMigratorAfterBoot()
	assert.Equal(t, winservice.LegacyServiceName, d.connectedService(), "the rollback completed at a boot without the holder")
	assert.Equal(t, winservice.StartDisabled, d.sys.services[winservice.MigrateServiceName].startType)
	assert.Equal(t, filepath.Join(d.legacyDir, winservice.RepairName), d.sys.task.script, "the task is back")
	assert.False(t, d.exists(d.newDir()))
	assert.Equal(t, "database", d.content(filepath.Join(d.legacyDir, "reagent.db")))
}

// failedMoveBack sets a device up for the rollback whose move-back fails:
// a fault at health, the new folder held open from then on.
func failedMoveBack(d *device, e *Env) {
	e.Fault = "health"
	e.Logf = func(format string, args ...any) {
		if fmt.Sprintf(format, args...) == "migrate: health" {
			d.sys.holders[d.newDir()] = []string{"explorer.exe"}
		}
	}
}

// ensure-agent's fallback re-creates the new service; a power cut right
// after that, before the journal said it created it, must not leave a
// service the journal disowns: at the next boot the undo would skip it and
// Check would refuse every later attempt.
func TestACutAfterTheFallbackCreateDoesNotOrphanTheNewService(t *testing.T) {
	ref := newDevice(t)
	re := ref.env()
	ref.stage(re)
	failedMoveBack(ref, &re)
	require.Error(t, ref.run(re))
	require.Equal(t, winservice.ServiceName, ref.journal().EnsuredService)
	k := lastIndex(ref.sys.calls, "create-or-update "+winservice.ServiceName)
	require.Greater(t, k, 0)
	require.Equal(t, "write "+winservice.JournalName, ref.sys.calls[k-1], "CreatedService is saved before the service is created")

	d := newDevice(t)
	e := d.env()
	d.stage(e)
	failedMoveBack(d, &e)
	d.sys.crashAt = k + 2 // the power goes right after the create
	require.PanicsWithValue(t, errCrash, func() { _ = Run(e) })
	d.sys.crashAt = 0
	require.True(t, d.serviceExists(winservice.ServiceName), "created before the cut")
	require.True(t, d.journal().CreatedService, "and owned")

	delete(d.sys.holders, d.newDir())
	// a real boot: the migrator disables the new service before its delayed
	// start, so it never connects or adopts
	d.sys.connectFails[winservice.ServiceName] = true
	d.boot()
	d.runMigratorAfterBoot()
	assert.Equal(t, winservice.LegacyServiceName, d.connectedService())
	assert.False(t, d.serviceExists(winservice.ServiceName), "no orphan")
	assert.Equal(t, winservice.DisplayName, d.sys.services[winservice.LegacyServiceName].spec.DisplayName)
	assert.NoError(t, Check(d.env()), "the next attempt is possible")
}

// A power cut after every call of the rollback's fallback (take the display
// name, re-create the new service, save, start, watch, record): the boot
// ends with an agent connected and a final journal — rolled back onto the
// legacy service once the holder is gone, or done with the new service when
// its agent connects and adopts.
func TestTheRollbackFallbackSurvivesAPowerCutAtEveryCall(t *testing.T) {
	ref := newDevice(t)
	re := ref.env()
	ref.stage(re)
	failedMoveBack(ref, &re)
	fallbackFrom := -1
	logf := re.Logf
	re.Logf = func(format string, args ...any) {
		logf(format, args...)
		if fallbackFrom < 0 && fmt.Sprintf(format, args...) == "migrate: re-creating service "+winservice.ServiceName {
			fallbackFrom = len(ref.sys.calls)
		}
	}
	require.Error(t, ref.run(re))
	require.GreaterOrEqual(t, fallbackFrom, 0)
	total := len(ref.sys.calls)
	for _, variant := range []string{"the agent never connects", "the agent adopts"} {
		for n := fallbackFrom + 1; n <= total; n++ {
			t.Run(fmt.Sprintf("%s/call %d %s", variant, n, ref.sys.calls[n-1]), func(t *testing.T) {
				d := newDevice(t)
				e := d.env()
				d.stage(e)
				failedMoveBack(d, &e)
				d.sys.crashAt = n
				require.PanicsWithValue(t, errCrash, func() { _ = Run(e) })
				d.sys.crashAt = 0
				delete(d.sys.holders, d.newDir())
				if variant == "the agent never connects" {
					d.sys.connectFails[winservice.ServiceName] = true
				}
				d.boot()
				d.runMigratorAfterBoot()
				j := d.journal()
				connected := d.connectedService()
				require.NotEmpty(t, connected, "an agent is connected: %+v", j)
				require.True(t, j.Done || j.Failed(), "%+v", j)
				if j.Done {
					assert.Equal(t, winservice.ServiceName, connected)
					assert.Contains(t, j.Adopted, "rolled back")
				} else {
					assert.Equal(t, winservice.LegacyServiceName, connected)
					assert.Equal(t, winservice.LegacyServiceName, j.EnsuredService)
					assert.False(t, d.serviceExists(winservice.ServiceName))
					assert.NoError(t, Check(d.env()), "the next attempt is possible")
				}
			})
		}
	}
}

// The agent and the migrator both write the journal: a temporary file of
// one name shared by two writers would land one writer's head and the
// other's tail in the file. The fake's WriteFile has the real one's shape.
func TestJournalWritersNeverShareATempFile(t *testing.T) {
	d := newDevice(t)
	path := filepath.Join(d.root, "probe.json")
	a := strings.Repeat("a", 64<<10)
	b := strings.Repeat("b", 48<<10)
	mixed := 0
	for i := 0; i < 300; i++ {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = d.sys.WriteFile(path, []byte(a), 0o644) }()
		go func() { defer wg.Done(); _ = d.sys.WriteFile(path, []byte(b), 0o644) }()
		wg.Wait()
		got, err := os.ReadFile(path)
		require.NoError(t, err)
		if string(got) != a && string(got) != b {
			mixed++
		}
	}
	assert.Equal(t, 0, mixed, "a reader only ever sees one writer's content")
	leftovers, _ := filepath.Glob(filepath.Join(d.root, "probe.json.*"))
	assert.Empty(t, leftovers, "no temporary file left")
}

// The migrate service reruns a rollback at every start until the legacy
// agent runs; once the folder went back, the rerun leaves the recreate
// marker alone (the legacy agent may have consumed it meanwhile).
func TestTheRerunOfARollbackLeavesTheMarkerAlone(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	e.Fault = "health"
	d.stage(e)
	d.sys.failOn["start "+winservice.LegacyServiceName] = errors.New("the service database is locked")
	require.ErrorContains(t, d.run(e), "health")
	j := d.journal()
	require.True(t, j.Failed())
	require.Equal(t, "", j.EnsuredService)
	require.False(t, j.Moved, "moved back")
	marker := filepath.Join(d.legacyDir, winservice.RecreateMarkerName)
	consumed := `{"listed":true,"containers":{"prod_1_app1":{"volumes":[{"name":"7f3a","target":"/data"}]}}}`
	d.file(marker, consumed, 0o644) // the legacy agent consumed it meanwhile
	d.sys.dockerDown = true         // and does not consume it again in this run

	// the recovery action starts the migrate service again; the lock is gone
	delete(d.sys.failOn, "start "+winservice.LegacyServiceName)
	require.NoError(t, d.sys.startService(winservice.MigrateServiceName))
	require.NoError(t, d.run(e))
	assert.Equal(t, winservice.LegacyServiceName, d.journal().EnsuredService)
	assert.Equal(t, consumed, d.content(marker), "left alone")
}

// The new folder's executable vanishes (quarantined) once the new service
// exists: at start-agent (the start fails) and during health (the agent
// dies and cannot be restarted). The rollback brings the legacy service back
// with an executable, and it stays after a boot.
func TestARollbackRestoresAVanishedExeFromTheMigrator(t *testing.T) {
	for _, at := range []string{"migrate: start-agent", "migrate: health"} {
		t.Run(at, func(t *testing.T) {
			d := newDevice(t)
			e := d.env()
			d.stage(e)
			e.Logf = func(format string, args ...any) {
				if fmt.Sprintf(format, args...) == at {
					require.NoError(t, os.Remove(filepath.Join(d.newDir(), winservice.ExeName)))
					if s := d.sys.services[winservice.ServiceName]; s.state == StateRunning {
						_ = (&uncounted{d.sys}).KillProcess(s.pid) // the quarantine kills the process too
					}
				}
			}
			require.Error(t, d.run(e))
			j := d.journal()
			assert.True(t, j.Failed())
			assert.Equal(t, winservice.LegacyServiceName, j.EnsuredService, j.EnsureError)
			assert.Equal(t, winservice.LegacyServiceName, d.connectedService())
			assert.Equal(t, testVersion, d.content(filepath.Join(d.legacyDir, winservice.LegacyExeName)))
			d.boot()
			d.runMigratorAfterBoot()
			assert.Equal(t, winservice.LegacyServiceName, d.connectedService(), "still connected after a boot")
		})
	}
}

// The new agent's process outlives its service's stop during the rollback:
// it is terminated before the folder goes back (an open handle would block
// the rename), and the rollback completes.
func TestRollbackTerminatesANewAgentThatOutlivesItsStop(t *testing.T) {
	d := newDevice(t)
	before := d.withoutDropped(d.snapshot())
	d.sys.stopLinger = 2 * time.Minute
	e := d.env()
	e.Fault = "health"
	d.stage(e)
	require.ErrorContains(t, d.run(e), "health")
	calls := d.sys.calls
	stop := index(calls, "stop "+winservice.ServiceName) // the undo of start-agent
	del := lastIndex(calls, "delete "+winservice.ServiceName)
	require.Greater(t, stop, 0)
	require.Greater(t, del, stop)
	killed := 0
	for _, c := range calls[stop:del] {
		if strings.HasPrefix(c, "kill ") {
			killed++
		}
	}
	assert.GreaterOrEqual(t, killed, 1, "the lingering agent (and its frpc) terminated between the stop and the delete")
	assertRolledBack(t, d, before)
}

// A service of the new name still marked for deletion (an earlier attempt's
// delete while a handle was held): register-service waits until it is gone,
// then creates the service.
func TestRegisterServiceWaitsOutAMarkedForDeleteService(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	// the earlier attempt: rolled back, its delete of the new service refused
	e.Fault = "health"
	d.stage(e)
	d.sys.failOn["delete "+winservice.ServiceName] = errors.New("access denied")
	require.Error(t, d.run(e))
	require.True(t, d.journal().CreatedService)
	delete(d.sys.failOn, "delete "+winservice.ServiceName)
	// the service is running (a handle on it) and marked for deletion
	s := d.sys.services[winservice.ServiceName]
	s.startType, s.state, s.pid, s.markedForDelete = winservice.StartManual, StateRunning, 4242, true
	d.sys.processes[4242] = &fakeProcess{pid: 4242, exe: filepath.Join(d.newDir(), winservice.ExeName), service: winservice.ServiceName}
	d.sys.now = d.sys.now.Add(retryAfterFailure)
	e.Fault = ""
	d.stage(e)
	retries := 0
	e.Logf = func(format string, args ...any) {
		if strings.Contains(fmt.Sprintf(format, args...), "marked for deletion") {
			retries++
			if retries == 2 { // the handle is released: the service goes
				require.NoError(t, (&uncounted{d.sys}).StopService(winservice.ServiceName))
				delete(d.sys.processes, 4242)
			}
		}
	}
	require.NoError(t, d.run(e))
	assert.Equal(t, 2, retries)
	creates := 0
	sleeps := 0
	for i, c := range d.sys.calls {
		if c == "create-or-update "+winservice.ServiceName {
			creates++
		}
		if c == "sleep 5s" && i > index(d.sys.calls, "create-or-update "+winservice.ServiceName) && creates < 2 {
			sleeps++
		}
	}
	assert.GreaterOrEqual(t, creates, 2, "created once the deletion went through")
	assert.GreaterOrEqual(t, sleeps, 2)
	assertMigrated(t, d)

	// never released: the attempt rolls back onto the legacy service
	d2 := newDevice(t)
	e2 := d2.env()
	before := d2.withoutDropped(d2.snapshot())
	d2.sys.services[winservice.ServiceName] = &fakeService{spec: winservice.Spec{Name: winservice.ServiceName, DisplayName: "x", Exe: filepath.Join(d2.newDir(), winservice.ExeName)}, startType: winservice.StartManual, state: StateRunning, pid: 4242, markedForDelete: true}
	d2.sys.processes[4242] = &fakeProcess{pid: 4242, exe: filepath.Join(d2.newDir(), winservice.ExeName), service: winservice.ServiceName}
	require.NoError(t, e2.saveJournal(&Journal{Version: testVersion, StartedAt: "2026-10-22T12:00:00Z", FailedAt: "2026-10-22T12:00:00Z", Error: "x", CreatedService: true, Attempts: 1}))
	d2.stage(e2)
	require.ErrorContains(t, d2.run(e2), "marked for deletion")
	delete(d2.sys.processes, 4242)
	delete(d2.sys.services, winservice.ServiceName)
	assertRolledBack(t, d2, before)
}

// After a rollback the legacy folder keeps a spare binary: the move's
// rollback target (this version) becomes reagent-prev.exe, which the
// restored legacy repair script and a self-update rollback look for. A
// later loss of reagent.exe is still repaired at boot.
func TestARolledBackDeviceKeepsASpareBinary(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	e.Fault = "health"
	d.stage(e)
	require.ErrorContains(t, d.run(e), "health")
	require.Equal(t, winservice.LegacyServiceName, d.connectedService())
	assert.Equal(t, testVersion, d.content(filepath.Join(d.legacyDir, "reagent-prev.exe")), "the spare")
	assert.False(t, d.exists(filepath.Join(d.legacyDir, "device-agent-prev.exe")))

	d.powerCut()
	require.NoError(t, os.Remove(filepath.Join(d.legacyDir, winservice.LegacyExeName))) // quarantined later
	d.boot()
	d.runMigratorAfterBoot()
	assert.Equal(t, winservice.LegacyServiceName, d.connectedService())
	assert.Equal(t, testVersion, d.content(filepath.Join(d.legacyDir, winservice.LegacyExeName)))
	assert.Equal(t, filepath.Join(d.legacyDir, winservice.RepairName), d.sys.task.script)
}

// Both the legacy executable and the move's rollback target are gone by the
// time the rollback ends (quarantined): ensure-agent restores the legacy
// executable from the migrator, the same verified binary; without the
// migrator copy nothing could, and the migrate service asks for a restart.
func TestEnsureAgentRestoresTheLegacyExecutableFromTheMigrator(t *testing.T) {
	for _, migratorToo := range []bool{false, true} {
		t.Run(fmt.Sprintf("migrator gone too: %v", migratorToo), func(t *testing.T) {
			d := newDevice(t)
			e := d.env()
			e.Fault = "health"
			d.stage(e)
			restored := false
			e.Logf = func(format string, args ...any) {
				line := fmt.Sprintf(format, args...)
				if line == "migrate: undoing prepare" { // right before the undo of prepare runs
					require.NoError(t, os.Remove(filepath.Join(d.legacyDir, winservice.LegacyExeName)))
					require.NoError(t, os.Remove(filepath.Join(d.legacyDir, "device-agent-prev.exe")))
					if migratorToo {
						require.NoError(t, os.Remove(e.Layout.MigratorExe()))
					}
				}
				if strings.Contains(line, "was missing; restored from the migrator") {
					restored = true
				}
			}
			require.ErrorContains(t, d.run(e), "health")
			j := d.journal()
			if migratorToo {
				assert.Equal(t, "", j.EnsuredService)
				assert.Contains(t, j.EnsureError, "no ")
				assert.Equal(t, uint32(1), exitCodeFor(e, nil), "retried by the recovery action")
				assert.False(t, restored)
				return
			}
			assert.True(t, restored, "the log says so")
			assert.Equal(t, winservice.LegacyServiceName, j.EnsuredService, j.EnsureError)
			assert.Equal(t, testVersion, d.content(filepath.Join(d.legacyDir, winservice.LegacyExeName)))
			assert.Equal(t, winservice.LegacyServiceName, d.connectedService())
		})
	}
}

// A rollback whose ensure of the legacy service fails (and so looks at the
// new service) never drops an executable into an empty new folder: that
// would make it a folder in use, and every later attempt in place.
func TestARollbackNeverDropsAnExecutableIntoAnEmptyNewFolder(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	flaky := &flakyStartType{fakeSystem: d.sys, remaining: 2}
	e.Sys = flaky
	e.Fault = "stop-apps"
	d.stage(e)
	e.Logf = func(format string, args ...any) {
		if fmt.Sprintf(format, args...) == "migrate: stop-apps" {
			require.NoError(t, os.MkdirAll(d.newDir(), 0o755)) // someone made the folder meanwhile
		}
	}
	require.ErrorContains(t, d.run(e), "stop-apps")
	j := d.journal()
	require.Equal(t, "", j.EnsuredService, "both ensures failed this time")
	assert.False(t, d.exists(filepath.Join(d.newDir(), winservice.ExeName)), "nothing dropped into the empty folder")

	d.sys.now = d.sys.now.Add(30 * time.Second)
	require.NoError(t, d.sys.startService(winservice.MigrateServiceName))
	require.NoError(t, d.run(e))
	assert.Equal(t, winservice.LegacyServiceName, d.connectedService())
	d.sys.now = d.sys.now.Add(retryAfterFailure)
	e.Fault = ""
	e.Logf = nil
	d.stage(e)
	require.NoError(t, d.run(e))
	assert.Equal(t, StrategyMove, d.journal().Strategy, "the next attempt still moves")
}
