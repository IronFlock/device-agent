package winmigrate

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reagent/winservice"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The boundaries of the steps stop-legacy, stop-apps, rename-files and
// move-folder and of their undos.

// --- stop-legacy ------------------------------------------------------------

// hungStop is a legacy service whose stop hangs: the service control manager
// reports stop-pending until the process is terminated.
type hungStop struct {
	*fakeSystem
}

func (h *hungStop) StopService(name string) error {
	if name != winservice.LegacyServiceName {
		return h.fakeSystem.StopService(name)
	}
	h.call("stop %s", name)
	s := h.services[name]
	if s.state != StateRunning {
		return nil
	}
	s.state = StateStopPending
	return fmt.Errorf("service %s did not stop within %s", name, 2*time.Minute)
}

// StartService: ERROR_SERVICE_ALREADY_RUNNING counts as success (the real
// adapter), a stop-pending service included.
func (h *hungStop) StartService(name string) error {
	if s := h.services[name]; name == winservice.LegacyServiceName && s != nil && s.state == StateStopPending {
		h.call("start %s", name)
		return nil
	}
	return h.fakeSystem.StartService(name)
}

// KillProcess of the hung process ends the hang.
func (h *hungStop) KillProcess(pid int) error {
	err := h.fakeSystem.KillProcess(pid)
	if s := h.services[winservice.LegacyServiceName]; s != nil && s.state == StateStopPending && s.pid == pid {
		s.state, s.pid = StateStopped, 0
	}
	return err
}

// A legacy agent whose stop hangs (or is refused: the service is still
// starting) is terminated like any process that outlives its stop; the move
// goes on. Without that the attempt would roll back onto a service that
// never stops and leave the device offline until the next boot.
func TestAHungLegacyStopIsTerminated(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	e.Sys = &hungStop{d.sys}
	d.stage(e)
	require.NoError(t, d.run(e))
	assert.True(t, d.journal().Done)
	calls := d.sys.calls
	assert.Less(t, index(calls, "stop reagent"), index(calls, fmt.Sprintf("kill %d", legacyPID)), "terminated after the stop")
	assert.GreaterOrEqual(t, index(calls, fmt.Sprintf("kill %d", legacyPID+1)), 0, "frpc too")
	assert.Equal(t, winservice.ServiceName, d.connectedService())
}

// A plain error from the stop (ERROR_SERVICE_CANNOT_ACCEPT_CTRL while the
// agent is still starting) is handled the same way.
func TestARefusedLegacyStopIsTerminated(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	d.sys.failOn["stop reagent"] = errors.New("the service cannot accept control messages at this time")
	d.stage(e)
	require.NoError(t, d.run(e))
	assert.True(t, d.journal().Done)
	assert.Equal(t, winservice.ServiceName, d.connectedService())
	assert.False(t, d.serviceExists(winservice.LegacyServiceName))
}

// flakyStartType refuses the first n non-disabling start-type changes of the
// legacy service (the service database locked by an installer).
type flakyStartType struct {
	*fakeSystem
	remaining int
}

func (f *flakyStartType) SetStartType(name string, start winservice.StartType, delayed bool) error {
	if name == winservice.LegacyServiceName && start != winservice.StartDisabled && f.remaining > 0 {
		f.remaining--
		f.call("start-type %s %s", name, start)
		return errors.New("the service database is locked")
	}
	return f.fakeSystem.SetStartType(name, start, delayed)
}

// A transient failure of the service control manager that spans both
// restores of the legacy start type (the rollback's and ensure-agent's)
// does not end the retries: the migrate service stays enabled and asks for a
// restart; its next run undoes again and enables the legacy service.
func TestATransientSCMFailureInTheRollbackIsRetried(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	flaky := &flakyStartType{fakeSystem: d.sys, remaining: 2}
	e.Sys = flaky
	e.Fault = "stop-apps"
	d.stage(e)
	require.ErrorContains(t, d.run(e), "stop-apps")
	j := d.journal()
	assert.True(t, j.Failed())
	assert.Equal(t, "", j.EnsuredService)
	assert.Contains(t, j.EnsureError, "database is locked")
	assert.Equal(t, winservice.StartDisabled, d.sys.services[winservice.LegacyServiceName].startType)
	assert.NotEqual(t, winservice.StartDisabled, d.sys.services[winservice.MigrateServiceName].startType, "the migrate service stays enabled")
	assert.Equal(t, uint32(1), exitCodeFor(e, nil), "and asks for a restart")
	assert.Equal(t, "", d.connectedService())

	// the recovery action, 30 s later; the database is free
	d.sys.now = d.sys.now.Add(30 * time.Second)
	require.NoError(t, d.sys.startService(winservice.MigrateServiceName))
	require.NoError(t, d.run(e))
	j = d.journal()
	assert.Equal(t, winservice.LegacyServiceName, j.EnsuredService)
	assert.Equal(t, winservice.LegacyServiceName, d.connectedService())
	assert.Equal(t, winservice.StartDisabled, d.sys.services[winservice.MigrateServiceName].startType)
	assert.Equal(t, uint32(0), exitCodeFor(e, nil))
}

// A process that outlives its service's stop (the agent, frpc) is
// terminated after the wait, and a power cut around the termination resumes
// to the same end.
func TestStopLegacyTerminatesLingeringProcesses(t *testing.T) {
	want := finalState(t)
	d := newDevice(t)
	d.sys.stopLinger = 10 * time.Minute
	e := d.env()
	d.stage(e)
	require.NoError(t, d.run(e))
	assert.True(t, d.journal().Done)
	calls := d.sys.calls
	kill := index(calls, fmt.Sprintf("kill %d", legacyPID))
	require.Greater(t, kill, index(calls, "stop reagent"))
	require.GreaterOrEqual(t, index(calls, fmt.Sprintf("kill %d", legacyPID+1)), 0, "frpc")
	assert.Equal(t, winservice.ServiceName, d.connectedService())
	assert.Equal(t, want, d.snapshot())

	// the power goes after every call from the disable up to the journal
	// write after the kills
	from := index(calls, "start-type reagent disabled") + 1
	to := kill + 1
	for ; to < len(calls) && calls[to] != "write "+winservice.JournalName; to++ {
	}
	for n := from + 1; n <= to+2; n++ {
		t.Run(fmt.Sprintf("call %d %s", n, calls[n-1]), func(t *testing.T) {
			d := newDevice(t)
			d.sys.stopLinger = 10 * time.Minute
			d.stage(d.env())
			d.sys.crashAt = n
			require.PanicsWithValue(t, errCrash, func() { _ = Run(d.env()) })
			d.sys.crashAt = 0
			d.boot()
			d.runMigratorAfterBoot()
			assert.True(t, d.journal().Done)
			assert.Equal(t, winservice.ServiceName, d.connectedService())
			assert.Equal(t, want, d.snapshot())
		})
	}
}

// --- prepare ------------------------------------------------------------------

// A failure inside prepare: at the task switch nothing was dropped yet and
// the rollback brings everything back; at a drop, the task is universal and
// the rollback target written, both undone, and the next attempt succeeds.
func TestPrepareFailsMidway(t *testing.T) {
	t.Run("at the task switch", func(t *testing.T) {
		d := newDevice(t)
		before := d.snapshot()
		e := d.env()
		d.stage(e)
		d.sys.failOn["task .migrate/repair.cmd"] = errors.New("schtasks /Create: access is denied")
		require.ErrorContains(t, d.run(e), "pointing the repair task")
		assertRolledBack(t, d, before)
		assert.True(t, d.exists(filepath.Join(d.legacyDir, "reagent-prev.exe")))
		assert.Equal(t, "1.3.8", d.content(filepath.Join(d.legacyDir, "reagent-prev.exe")), "nothing dropped, nothing replaced")
		assert.False(t, d.exists(filepath.Join(d.legacyDir, "device-agent-prev.exe")))
		assert.Equal(t, 1, d.journal().Attempts)
	})
	t.Run("at a drop", func(t *testing.T) {
		d := newDevice(t)
		before := d.snapshot()
		e := d.env()
		d.stage(e)
		d.sys.failOn["remove Reagent/reagent-prev.exe"] = errors.New("the process cannot access the file because it is being used by another process")
		require.ErrorContains(t, d.run(e), "prepare:")
		j := d.journal()
		assert.True(t, j.Failed())
		assert.Equal(t, winservice.LegacyServiceName, d.connectedService())
		assert.Equal(t, filepath.Join(d.legacyDir, winservice.RepairName), d.sys.task.script, "the task is back")
		assert.Equal(t, fmt.Sprintf(legacyRepair, d.legacyDir), d.content(filepath.Join(d.legacyDir, winservice.RepairName)))
		assert.False(t, d.exists(filepath.Join(d.legacyDir, "device-agent-prev.exe")), "the rollback target is removed: the legacy spare is still there")
		assert.False(t, d.exists(filepath.Join(d.legacyDir, "reagent-failed-v1.3.9.exe")), "dropped before the failure")
		assert.Equal(t, "1.3.8", d.content(filepath.Join(d.legacyDir, "reagent-prev.exe")), "the failing drop")
		assert.True(t, d.exists(filepath.Join(d.legacyDir, "reagent-v1.3.8.exe")), "not reached")
		assert.Equal(t, winservice.StartDisabled, d.sys.services[winservice.MigrateServiceName].startType)
		_ = before

		delete(d.sys.failOn, "remove Reagent/reagent-prev.exe")
		d.sys.now = d.sys.now.Add(retryAfterFailure)
		d.stage(e)
		require.NoError(t, d.run(e))
		assertMigrated(t, d)
	})
}

// --- rename-files -------------------------------------------------------------

// One rename back that keeps failing (the WAL held by a scanner) does not
// keep the other files from their names: the executable and the database
// go back, the legacy service connects.
func TestARollbackWhoseWalRenameBackFailsStillRenamesTheRest(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	e.Fault = "health"
	d.stage(e)
	d.sys.failOn["rename Reagent/device-agent.db-wal"] = errors.New("sharing violation")
	require.ErrorContains(t, d.run(e), "health")
	assert.Equal(t, testVersion, d.content(filepath.Join(d.legacyDir, "reagent.exe")))
	assert.Equal(t, "database", d.content(filepath.Join(d.legacyDir, "reagent.db")))
	assert.Equal(t, "log", d.content(filepath.Join(d.legacyDir, "reagent.log")))
	assert.True(t, d.exists(filepath.Join(d.legacyDir, "device-agent.db-wal")), "the one that could not go back")
	assert.Equal(t, winservice.LegacyServiceName, d.connectedService())
}

// The files the new agent wrote during health (a download, a failed
// version, a rotated log) go back under the legacy names with the rest.
func TestFilesTheNewAgentWroteDuringHealthComeBackUnderTheLegacyNames(t *testing.T) {
	for name, dir := range map[string]string{"move": "", "in place": filepath.Join(t.TempDir(), "Users", "alice", "reagent")} {
		t.Run(name, func(t *testing.T) {
			d := newDeviceIn(t, dir)
			d.sys.connectFails[winservice.ServiceName] = true
			e := d.env()
			d.stage(e)
			e.Logf = func(format string, args ...any) {
				if fmt.Sprintf(format, args...) == "migrate: health" {
					newDir := d.journal().New.AgentDir
					d.file(filepath.Join(newDir, "device-agent-v1.4.1.exe"), "1.4.1", 0o755)
					d.file(filepath.Join(newDir, "device-agent-failed-v1.4.2.exe"), "1.4.2", 0o755)
					d.file(filepath.Join(newDir, "device-agent-2026-10-23T12-30-00.000.log"), "rotated by the new agent", 0o644)
				}
			}
			require.ErrorContains(t, d.run(e), "health")
			assert.Equal(t, "1.4.1", d.content(filepath.Join(d.legacyDir, "reagent-v1.4.1.exe")))
			assert.Equal(t, "1.4.2", d.content(filepath.Join(d.legacyDir, "reagent-failed-v1.4.2.exe")))
			assert.Equal(t, "rotated by the new agent", d.content(filepath.Join(d.legacyDir, "reagent-2026-10-23T12-30-00.000.log")))
			for _, left := range []string{"device-agent-v", "device-agent-failed", "device-agent-2026"} {
				matches, _ := filepath.Glob(filepath.Join(d.legacyDir, left+"*"))
				assert.Empty(t, matches, left)
			}
			assert.Equal(t, winservice.LegacyServiceName, d.connectedService())
		})
	}
}

// Files of the new name that exist already are moved aside, never
// overwritten — also when the power goes between the moving aside and the
// rename.
func TestRenamesNeverOverwriteSurvivesAPowerCut(t *testing.T) {
	setup := func(t *testing.T) *device {
		d := newDevice(t)
		d.file(filepath.Join(d.legacyDir, "device-agent.db"), "older copy", 0o644)
		d.file(filepath.Join(d.legacyDir, "device-agent.exe"), testVersion, 0o755)
		return d
	}
	ref := setup(t)
	ref.stage(ref.env())
	require.NoError(t, ref.run(ref.env()))
	var cuts []int
	for i, c := range ref.sys.calls {
		if strings.HasPrefix(c, "rename Reagent/") {
			cuts = append(cuts, i+1, i+2) // before and after each rename
		}
	}
	require.NotEmpty(t, cuts)
	for _, n := range cuts {
		t.Run(fmt.Sprintf("call %d %s", n, ref.sys.calls[n-1]), func(t *testing.T) {
			d := setup(t)
			d.stage(d.env())
			d.sys.crashAt = n
			require.PanicsWithValue(t, errCrash, func() { _ = Run(d.env()) })
			d.sys.crashAt = 0
			d.boot()
			d.runMigratorAfterBoot()
			require.True(t, d.journal().Done)
			assert.Equal(t, winservice.ServiceName, d.connectedService())
			snap := d.snapshot()
			assert.Equal(t, "database", snap[winservice.Folder+"/device-agent.db"], "the live database won")
			stale := 0
			for path := range snap {
				if strings.Contains(path, ".stale-") {
					stale++
				}
			}
			assert.Equal(t, 2, stale, "both pre-existing files were moved aside, not deleted: %v", snap)
		})
	}
}

// failOnce refuses one rename of a path with a sharing violation (an
// antivirus scanning the file).
type failOnce struct {
	*fakeSystem
	path   string
	failed bool
}

func (f *failOnce) Rename(from, to string) error {
	if !f.failed && from == f.path {
		f.failed = true
		f.call("rename %s %s", f.rel(from), f.rel(to))
		return errors.New("sharing violation")
	}
	return f.fakeSystem.Rename(from, to)
}

// A rename refused once mid-step is retried and the move goes through.
func TestARenameThatFailsOnceIsRetried(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	once := &failOnce{fakeSystem: d.sys, path: filepath.Join(d.legacyDir, "reagent.db")}
	e.Sys = once
	d.stage(e)
	require.NoError(t, d.run(e))
	assert.True(t, once.failed)
	assertMigrated(t, d)
	first := index(d.sys.calls, "rename Reagent/reagent.db Reagent/device-agent.db")
	require.GreaterOrEqual(t, first, 0)
	assert.Equal(t, "sleep 1s", d.sys.calls[first+1], "retried after a wait")
}

// --- move-folder ----------------------------------------------------------------

// Docker was down during stop-apps (the compose container keeps restart
// always); the power goes after the folder rename; at the boot Docker starts
// the container and recreates the old folder under its bind mount. The
// resume finds both folders: the one Docker made is the stray (no
// device.flock), goes aside, and the move completes — it is not demoted to
// in place.
func TestAStrayAtAResumeInsideMoveFolderIsMovedAside(t *testing.T) {
	ref := newDevice(t)
	ref.sys.dockerDown = true
	ref.stage(ref.env())
	require.NoError(t, ref.run(ref.env()))
	renameIdx := index(ref.sys.calls, "rename Reagent DeviceAgent")
	require.Greater(t, renameIdx, 0)
	for offset := 2; offset <= 6; offset++ {
		n := renameIdx + offset
		t.Run(fmt.Sprintf("call %d %s", n, ref.sys.calls[n-1]), func(t *testing.T) {
			d := newDevice(t)
			d.sys.dockerDown = true
			d.stage(d.env())
			d.sys.crashAt = n
			require.PanicsWithValue(t, errCrash, func() { _ = Run(d.env()) })
			d.sys.crashAt = 0
			d.sys.dockerDown = false
			d.boot()
			require.True(t, d.exists(filepath.Join(d.legacyDir, "apps", "prod", "app2")), "the stray folder")
			require.True(t, d.exists(d.newDir()), "the moved folder")
			d.runMigratorAfterBoot()
			j := d.journal()
			require.True(t, j.Done, "%+v", j)
			assert.Equal(t, StrategyMove, j.Strategy)
			assert.False(t, j.PreferInPlace, "the folder WAS moved")
			strays, _ := filepath.Glob(d.legacyDir + ".stray-*")
			assert.Len(t, strays, 1)
			assert.Equal(t, winservice.ServiceName, d.connectedService())
		})
	}
}

// A foreign, non-empty DeviceAgent appears between prepare and move-folder:
// the move refuses ("both exist"), and the undo never mistakes the agent's
// own folder for the stray.
func TestAForeignNewFolderAtMoveFolderIsNeverTheStray(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	e.Logf = func(format string, args ...any) {
		if fmt.Sprintf(format, args...) == "migrate: move-folder" {
			d.file(filepath.Join(d.newDir(), "foreign.txt"), "someone else's", 0o644)
		}
	}
	require.ErrorContains(t, d.run(e), "both")
	j := d.journal()
	assert.True(t, j.Failed())
	assert.True(t, j.PreferInPlace)
	strays, _ := filepath.Glob(d.legacyDir + ".stray-*")
	assert.Empty(t, strays, "the agent's own folder is never the stray")
	assert.Equal(t, testVersion, d.content(filepath.Join(d.legacyDir, "reagent.exe")))
	assert.Equal(t, "someone else's", d.content(filepath.Join(d.newDir(), "foreign.txt")), "untouched")
	assert.Equal(t, winservice.LegacyServiceName, d.connectedService())
	d.boot()
	d.runMigratorAfterBoot()
	assert.Equal(t, winservice.LegacyServiceName, d.connectedService())
}

// The new agent consumed the recreate marker (it removed a container and
// recorded its volumes) and was stopped before recreating it: the undo
// keeps the recorded volumes and only asks the legacy agent to list again.
func TestTheUndoKeepsAConsumedRecreateMarker(t *testing.T) {
	run := func(t *testing.T, consumed string) string {
		d := newDevice(t)
		e := d.env()
		d.stage(e)
		e.Fault = "health"
		marker := ""
		e.Logf = func(format string, args ...any) {
			switch fmt.Sprintf(format, args...) {
			case "migrate: health":
				if consumed != "" {
					d.file(filepath.Join(d.newDir(), winservice.RecreateMarkerName), consumed, 0o644)
				}
			case "migrate: undoing stop-apps": // after the folder went back, before the legacy agent reads it
				data, err := os.ReadFile(filepath.Join(d.legacyDir, winservice.RecreateMarkerName))
				require.NoError(t, err)
				marker = string(data)
			}
		}
		require.ErrorContains(t, d.run(e), "health")
		require.True(t, d.journal().Failed())
		require.Equal(t, winservice.LegacyServiceName, d.connectedService())
		return marker
	}
	marker := run(t, `{"listed":true,"containers":{"prod_1_app1":{"volumes":[{"name":"7f3a","target":"/var/lib/postgresql/data"}]}}}`)
	assert.Contains(t, marker, "7f3a", "the recorded volume survives the move back")
	assert.Contains(t, marker, `"listed":false`, "the legacy agent lists again")
	assert.Empty(t, strings.TrimSpace(run(t, "")), "an untouched marker stays an empty file")
}

// An empty DeviceAgent that appeared after prepare is removed by
// move-folder, also with a power cut right after the removal.
func TestAnEmptyNewFolderAtMoveFolderIsRemoved(t *testing.T) {
	makeEmpty := func(d *device, e *Env) {
		e.Logf = func(format string, args ...any) {
			if fmt.Sprintf(format, args...) == "migrate: move-folder" {
				require.NoError(d.t, os.MkdirAll(d.newDir(), 0o755))
			}
		}
	}
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	makeEmpty(d, &e)
	require.NoError(t, d.run(e))
	assert.Equal(t, StrategyMove, d.journal().Strategy)
	assertMigrated(t, d)
	removal := index(d.sys.calls, "remove-all DeviceAgent")
	require.Greater(t, removal, 0, "the RemoveAll branch ran")

	d2 := newDevice(t)
	e2 := d2.env()
	d2.stage(e2)
	makeEmpty(d2, &e2)
	d2.sys.crashAt = removal + 2 // the cut right after the removal
	require.PanicsWithValue(t, errCrash, func() { _ = Run(e2) })
	d2.sys.crashAt = 0
	d2.boot()
	d2.runMigratorAfterBoot()
	assertMigrated(t, d2)
}

// A folder rename refused for the whole retry period sets PreferInPlace and
// rolls back; the power goes right after that save, before the journal
// says rolling back: the resume goes FORWARD (the fault was transient), and
// with the holder gone at the boot the move completes.
func TestARefusedMoveResumesForwardOnceTheHolderIsGone(t *testing.T) {
	ref := newDevice(t)
	ref.sys.holders[ref.legacyDir] = []string{"explorer.exe"}
	ref.stage(ref.env())
	require.Error(t, ref.run(ref.env()))
	require.True(t, ref.journal().PreferInPlace)
	last := lastIndex(ref.sys.calls, "rename Reagent DeviceAgent")
	require.Greater(t, last, 0)
	// after the last refused rename: the backup copy and the write of
	// PreferInPlace, then the backup copy and the write of rolling back
	require.Equal(t, "write "+winservice.JournalName, ref.sys.calls[last+2])
	require.Equal(t, "write "+winservice.JournalName, ref.sys.calls[last+4])

	d := newDevice(t)
	d.sys.holders[d.legacyDir] = []string{"explorer.exe"}
	d.stage(d.env())
	d.sys.crashAt = last + 4 // the power goes before the rolling-back write
	require.PanicsWithValue(t, errCrash, func() { _ = Run(d.env()) })
	d.sys.crashAt = 0
	j := d.journal()
	require.True(t, j.InProgressState())
	require.True(t, j.PreferInPlace)
	delete(d.sys.holders, d.legacyDir)
	d.boot()
	d.runMigratorAfterBoot()
	j = d.journal()
	assert.True(t, j.Done, "%+v", j)
	assert.Equal(t, StrategyMove, j.Strategy, "planned as a move, moved")
	assert.Equal(t, winservice.ServiceName, d.connectedService())
}

// The rollback that moves a Docker-made stray aside before the folder goes
// back, with a power cut after every call of the resumed run and Docker
// recreating the stray again at every boot.
func TestAStrayRollbackSurvivesAPowerCutAtEveryCall(t *testing.T) {
	setup := func(t *testing.T) *device {
		d := newDevice(t)
		d.sys.dockerDown = true
		e := d.env()
		d.stage(e)
		e.Pause = "start-agent"
		e.Sys = &crashOnLongSleep{d.sys}
		require.PanicsWithValue(t, errCrash, func() { _ = Run(e) })
		d.sys.dockerDown = false
		d.sys.connectFails[winservice.ServiceName] = true
		d.boot()
		require.True(t, d.exists(filepath.Join(d.legacyDir, "apps", "prod", "app2")), "the stray folder")
		return d
	}
	ref := setup(t)
	ref.runMigratorAfterBoot()
	require.True(t, ref.journal().Failed())
	strayIdx := -1
	for i, c := range ref.sys.calls {
		if strings.HasPrefix(c, "rename Reagent Reagent.stray-") {
			strayIdx = i
		}
	}
	require.Greater(t, strayIdx, 0)
	for n := strayIdx; n <= len(ref.sys.calls); n++ {
		t.Run(fmt.Sprintf("call %d %s", n, ref.sys.calls[n-1]), func(t *testing.T) {
			d := setup(t)
			d.sys.crashAt = n
			require.PanicsWithValue(t, errCrash, func() { _ = Run(d.env()) })
			d.sys.crashAt = 0
			d.boot()
			d.runMigratorAfterBoot()
			j := d.journal()
			require.True(t, j.Failed(), "%+v", j)
			assert.Equal(t, winservice.LegacyServiceName, d.connectedService())
			assert.Equal(t, "database", d.content(filepath.Join(d.legacyDir, "reagent.db")))
			assert.True(t, d.exists(filepath.Join(d.legacyDir, "device.flock")))
		})
	}
}

// A container the new agent made under the new folder (a compose app a user
// stopped during health: compose does not bring it up again) keeps its
// restart policy taken away by the rollback: given back, Docker would start
// it at the next boot and recreate the new folder under its bind mount,
// which forces every later attempt in place.
func TestARollbackLeavesTheNewFoldersContainersWithoutRestart(t *testing.T) {
	d := newDevice(t)
	before := d.withoutDropped(d.snapshot())
	e := d.env()
	d.stage(e)
	e.Fault = "health"
	e.Logf = func(format string, args ...any) {
		if fmt.Sprintf(format, args...) == "migrate: health" {
			d.sys.containers = append(d.sys.containers, &fakeContainer{
				Container:  Container{ID: "c4", Name: "prod_4_app4-web-1", RestartPolicy: "always", Binds: []string{filepath.Join(d.newDir(), "apps", "prod", "app4")}, Running: true},
				configured: "always", wanted: false,
			})
		}
	}
	require.ErrorContains(t, d.run(e), "health")
	j := d.journal()
	require.True(t, j.Failed())
	var c4 *ContainerRecord
	for i := range j.Containers {
		if j.Containers[i].ID == "c4" {
			c4 = &j.Containers[i]
		}
	}
	require.NotNil(t, c4, "recorded by the undo of start-agent")
	assert.Equal(t, "always", c4.RestartPolicy)
	assert.Equal(t, d.newDir(), c4.Dir)
	assert.True(t, c4.Restored, "left to the agent")
	assert.Equal(t, "no", d.policies()["prod_4_app4-web-1"])
	d.boot()
	d.runMigratorAfterBoot()
	assertRolledBack(t, d, before)
}

// --- in place -------------------------------------------------------------------

// snapshotOf is the files below dir (the in-place device's folder lies
// outside the root the snapshot walks).
func snapshotOf(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Name() == winservice.ConnectedName || entry.Name() == winservice.RecreateMarkerName {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		out[filepath.ToSlash(rel)] = strings.ReplaceAll(string(data), dir, "<dir>")
		return nil
	})
	require.NoError(t, err)
	return out
}

func inPlaceDir(t *testing.T) string { return filepath.Join(t.TempDir(), "Users", "alice", "reagent") }

// The in-place strategy under a power cut after every call: the resume ends
// with the new service connected from the custom folder, or rolled back.
func TestInPlaceSurvivesAPowerCutAtEveryCall(t *testing.T) {
	ref := newDeviceIn(t, inPlaceDir(t))
	ref.stage(ref.env())
	require.NoError(t, ref.run(ref.env()))
	require.Equal(t, StrategyInPlace, ref.journal().Strategy)
	total := len(ref.sys.calls)
	for n := 1; n <= total; n++ {
		t.Run(fmt.Sprintf("call %d %s", n, ref.sys.calls[n-1]), func(t *testing.T) {
			dir := inPlaceDir(t)
			d := newDeviceIn(t, dir)
			d.stage(d.env())
			d.sys.crashAt = n
			require.PanicsWithValue(t, errCrash, func() { _ = Run(d.env()) })
			d.sys.crashAt = 0
			d.boot()
			d.runMigratorAfterBoot()
			j := d.journal()
			require.True(t, j.Done || j.Failed(), "%+v", j)
			connected := d.connectedService()
			require.NotEmpty(t, connected)
			if j.Done {
				assert.Equal(t, winservice.ServiceName, connected)
				assert.Equal(t, testVersion, d.content(filepath.Join(dir, "device-agent.exe")))
				assert.Equal(t, "database", d.content(filepath.Join(dir, "device-agent.db")))
			} else {
				assert.Equal(t, winservice.LegacyServiceName, connected)
			}
		})
	}
}

// A rollback at every step of an in-place migration brings the custom folder
// back as it was (but for the dropped legacy update files and the spare the
// rollback keeps), with the legacy service connected.
func TestInPlaceRollsBackAtEveryStep(t *testing.T) {
	for _, name := range StepNames() {
		t.Run(name, func(t *testing.T) {
			dir := inPlaceDir(t)
			d := newDeviceIn(t, dir)
			before := snapshotOf(t, dir)
			if name != "prepare" {
				for path := range before {
					if droppedLegacyFile(path) || strings.HasPrefix(path, "downloads/reagent-") {
						delete(before, path)
					}
				}
				before["reagent-prev.exe"] = testVersion
			}
			e := d.env()
			d.stage(e)
			e.Fault = name
			require.ErrorContains(t, d.run(e), "fault injected at "+name)
			j := d.journal()
			assert.True(t, j.Failed())
			assert.Equal(t, winservice.LegacyServiceName, j.EnsuredService, j.EnsureError)
			assert.Equal(t, winservice.LegacyServiceName, d.connectedService())
			assert.False(t, d.serviceExists(winservice.ServiceName))
			assert.False(t, d.exists(d.newDir()))
			assert.Equal(t, before, snapshotOf(t, dir))
			assert.Equal(t, filepath.Join(dir, winservice.RepairName), d.sys.task.script)
			assertPolicies(t, d, dir)
			d.boot()
			d.runMigratorAfterBoot()
			assert.Equal(t, winservice.LegacyServiceName, d.connectedService(), "after a boot")
		})
	}
}

// The resume after a power cut past move-folder finds the old folder
// recreated by Docker and moves it aside; a power cut right after that
// rename resumes to done.
func TestAStrayAtTheResumeSurvivesACutAfterItsRename(t *testing.T) {
	setup := func(t *testing.T) *device {
		d := newDevice(t)
		d.sys.dockerDown = true
		e := d.env()
		d.stage(e)
		e.Pause = "move-folder"
		e.Sys = &crashOnLongSleep{d.sys}
		require.PanicsWithValue(t, errCrash, func() { _ = Run(e) })
		d.sys.dockerDown = false
		d.boot()
		require.True(t, d.exists(filepath.Join(d.legacyDir, "apps", "prod", "app2")), "the stray folder")
		return d
	}
	ref := setup(t)
	ref.runMigratorAfterBoot()
	require.True(t, ref.journal().Done)
	strayIdx := -1
	for i, c := range ref.sys.calls {
		if strings.HasPrefix(c, "rename Reagent Reagent.stray-") {
			strayIdx = i
		}
	}
	require.GreaterOrEqual(t, strayIdx, 0)
	for _, n := range []int{strayIdx + 1, strayIdx + 2} {
		t.Run(fmt.Sprintf("call %d %s", n, ref.sys.calls[n-1]), func(t *testing.T) {
			d := setup(t)
			d.sys.crashAt = n
			require.PanicsWithValue(t, errCrash, func() { _ = Run(d.env()) })
			d.sys.crashAt = 0
			d.boot()
			d.runMigratorAfterBoot()
			assert.True(t, d.journal().Done, "%+v", d.journal())
			assert.Equal(t, winservice.ServiceName, d.connectedService())
			strays, _ := filepath.Glob(d.legacyDir + ".stray-*")
			assert.GreaterOrEqual(t, len(strays), 1)
			assert.False(t, d.exists(d.legacyDir))
		})
	}
}
