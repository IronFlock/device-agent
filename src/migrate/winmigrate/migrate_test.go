package winmigrate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reagent/winservice"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// assertMigrated checks the device after a successful move of the default
// folder: the new service runs the renamed files from DeviceAgent, the old
// service and folder are gone, and the agent found its recreate marker.
func assertMigrated(t *testing.T, d *device) {
	t.Helper()
	j := d.journal()
	assert.True(t, j.Done)
	assert.Equal(t, StrategyMove, j.Strategy)
	assert.Equal(t, winservice.ServiceName, j.EnsuredService)
	assert.Equal(t, winservice.ServiceName, d.connectedService(), "the new service is connected")
	assert.False(t, d.serviceExists(winservice.LegacyServiceName), "the reagent service is gone")
	assert.False(t, d.exists(d.legacyDir), "the old folder is gone")

	newDir := d.newDir()
	snap := d.snapshot()
	rel := func(name string) string { return filepath.ToSlash(filepath.Join(winservice.Folder, name)) }
	assert.Equal(t, testVersion, snap[rel("device-agent.exe")])
	assert.Equal(t, testVersion, snap[rel("device-agent-prev.exe")], "the rollback target is this version, not the 1.3.x prev")
	assert.Equal(t, "database", snap[rel("device-agent.db")])
	assert.Equal(t, "wal", snap[rel("device-agent.db-wal")])
	assert.Equal(t, "shm", snap[rel("device-agent.db-shm")])
	assert.Equal(t, "log", snap[rel("device-agent.log")])
	assert.Equal(t, "rotated", snap[rel("device-agent-2026-10-17T10-00-00.000.log")])
	assert.Equal(t, `{"secret":"s3cr3t"}`, snap[rel("device.flock")])
	assert.Equal(t, "frpc", snap[rel("frpc.exe")])
	assert.Equal(t, "HTTPS_PROXY=http://proxy:3128\n", snap[rel("proxy.env")])
	assert.Equal(t, "app data", snap[rel("apps/prod/app1/data.txt")])
	assert.Equal(t, "postgres", snap[rel("apps/prod/app2/pg/base")])
	assert.Equal(t, strings.ReplaceAll(winservice.RepairCmd(newDir), d.root, "<root>"), snap[rel("repair.cmd")])
	for _, gone := range []string{"reagent.exe", "reagent-prev.exe", "reagent-v1.3.8.exe", "reagent-failed-v1.3.9.exe", "reagent.db", "reagent.db-wal", "reagent.db-shm", "reagent.log", "downloads/reagent-v1.3.8"} {
		assert.NotContains(t, snap, rel(gone), gone)
	}
	assert.True(t, d.markerConsumed(newDir), "the agent recreated its containers once from the moved folder")

	s := d.sys.services[winservice.ServiceName]
	require.NotNil(t, s)
	assert.Equal(t, filepath.Join(newDir, "device-agent.exe"), s.spec.Exe)
	assert.Equal(t, []string{
		"-config", filepath.Join(newDir, "device.flock"),
		"-agentDir", newDir,
		"-appsDir", filepath.Join(newDir, "apps"),
		"-dbFileName", filepath.Join(newDir, "device-agent.db"),
		"-logFile", filepath.Join(newDir, "device-agent.log"),
		"-debug",
	}, s.spec.Args, "paths rewritten, other flags carried over")
	assert.Equal(t, []string{"HTTPS_PROXY=http://proxy:3128"}, s.spec.Env, "the legacy environment travels")
	assert.Equal(t, winservice.StartAutomatic, s.startType)
	assert.True(t, s.delayed)
	assert.Equal(t, []int{5, 30, 120}, s.spec.RestartDelays)
	assert.True(t, d.sys.events[winservice.ServiceName])
	assert.False(t, d.sys.events[winservice.LegacyServiceName])
	assert.True(t, d.sys.defender[filepath.Join(newDir, "frpc.exe")])
	assert.False(t, d.sys.defender[filepath.Join(d.legacyDir, "frpc.exe")])
	assert.Equal(t, filepath.Join(newDir, winservice.RepairName), d.sys.task.script, "the repair task points at the new folder")
	// the app containers run with their policies (recreated from the new
	// folder by the agent); the one not bound into the folder untouched
	assertPolicies(t, d, newDir)
}

// fixturePolicies are the restart policies the fixture's containers have
// when nothing holds them: the apps' configured ones.
var fixturePolicies = map[string]string{"prod_1_app1": "no", "prod_2_app2-db-1": "always", "other": "always"}

// assertPolicies checks the fixture's containers' policies, and that the
// app containers bind the folder the agent runs from.
func assertPolicies(t *testing.T, d *device, agentDir string) {
	t.Helper()
	policies := d.policies()
	for name, want := range fixturePolicies {
		assert.Equal(t, want, policies[name], name)
	}
	for _, c := range d.sys.containers {
		if _, fixture := fixturePolicies[c.Name]; fixture && c.Name != "other" {
			assert.True(t, bindsUnder(c.Container, agentDir), "%s binds %s", c.Name, agentDir)
		}
	}
}

// assertRolledBack checks the device after a rollback: the legacy service
// runs again from its folder with its original names and settings; the new
// service and folder are gone; the files are before (the snapshot from
// before the attempt, usually without the update files prepare drops).
func assertRolledBack(t *testing.T, d *device, before map[string]string) {
	t.Helper()
	j := d.journal()
	assert.True(t, j.Failed())
	assert.False(t, j.Done)
	assert.Equal(t, winservice.LegacyServiceName, j.EnsuredService, j.EnsureError)
	assert.Equal(t, winservice.LegacyServiceName, d.connectedService(), "the legacy service is connected again")
	assert.False(t, d.serviceExists(winservice.ServiceName), "no new service is left")
	assert.False(t, d.exists(d.newDir()), "no new folder is left")
	assert.Equal(t, before, d.snapshot())
	s := d.sys.services[winservice.LegacyServiceName]
	require.NotNil(t, s)
	assert.Equal(t, winservice.StartAutomatic, s.startType, "the start type is back")
	assert.True(t, s.delayed)
	assert.Equal(t, filepath.Join(d.legacyDir, winservice.RepairName), d.sys.task.script, "the repair task is back")
	assert.Equal(t, fmt.Sprintf(legacyRepair, d.legacyDir), d.content(filepath.Join(d.legacyDir, winservice.RepairName)))
	assert.Equal(t, winservice.StartDisabled, d.sys.services[winservice.MigrateServiceName].startType, "the migrate service waits for the next attempt")
	assertPolicies(t, d, d.legacyDir)
	if j.FailedStep > stepIndex("move-folder") && j.Strategy == StrategyMove {
		assert.True(t, d.markerConsumed(d.legacyDir) || d.exists(filepath.Join(d.legacyDir, winservice.RecreateMarkerName)),
			"after a move back the legacy agent recreates its containers")
	}
}

func TestMigrateMovesALegacyInstall(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	require.NoError(t, d.run(e))
	assertMigrated(t, d)

	j := d.journal()
	assert.Equal(t, 1, j.Runs)
	assert.Equal(t, 0, j.Attempts)
	assert.Equal(t, legacyPID, j.Legacy.PID)
	assert.Equal(t, "auto", j.Legacy.StartType)
	assert.True(t, j.CreatedService)
	assert.Equal(t, []ContainerRecord{{ID: "c1", Name: "prod_1_app1", RestartPolicy: "no", Dir: d.legacyDir, Restored: true}, {ID: "c2", Name: "prod_2_app2-db-1", RestartPolicy: "always", Dir: d.legacyDir, Restored: true}}, sortedRecords(j.Containers),
		"the old containers, gone since the agent recreated them, count as restored")
	assert.False(t, j.DockerUnavailable)

	// The order of the SCM operations the plan requires: reagent disabled
	// before it is stopped; the new service registered demand-start and
	// switched to delayed-auto only when it starts.
	calls := d.sys.calls
	assert.Less(t, index(calls, "start-type reagent disabled"), index(calls, "stop reagent"))
	assert.Less(t, index(calls, "create-or-update ironflock-device-agent"), index(calls, "start-type ironflock-device-agent auto"))
	assert.Less(t, index(calls, "start-type ironflock-device-agent auto"), index(calls, "start ironflock-device-agent"))
	assert.Less(t, index(calls, "start ironflock-device-agent"), index(calls, "delete reagent"), "the legacy service goes only after the new agent connected")

	// The leftovers go at the agent's next start.
	assert.True(t, d.serviceExists(winservice.MigrateServiceName))
	d.boot()
	assert.Equal(t, winservice.ServiceName, d.connectedService())
	assert.False(t, d.serviceExists(winservice.MigrateServiceName), "the agent removed the migrate service")
	assert.False(t, d.exists(e.Layout.MigrateDir()), "the agent removed .migrate")
	assert.True(t, d.exists(e.Layout.Journal()), "the journal stays")
	assert.False(t, d.migrateServiceRunning())
}

func sortedRecords(records []ContainerRecord) []ContainerRecord {
	out := append([]ContainerRecord(nil), records...)
	for i := range out {
		for k := i + 1; k < len(out); k++ {
			if out[k].ID < out[i].ID {
				out[i], out[k] = out[k], out[i]
			}
		}
	}
	return out
}

func index(calls []string, call string) int {
	for i, c := range calls {
		if c == call {
			return i
		}
	}
	return -1
}

func lastIndex(calls []string, call string) int {
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i] == call {
			return i
		}
	}
	return -1
}

// A custom -agentDir keeps its folder: only the names change.
func TestMigrateInPlaceKeepsACustomFolder(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "Users", "alice", "reagent")
	d := newDeviceIn(t, custom)
	e := d.env()
	d.stage(e)
	require.NoError(t, d.run(e))

	j := d.journal()
	assert.True(t, j.Done)
	assert.Equal(t, StrategyInPlace, j.Strategy)
	assert.Contains(t, j.StrategyReason, custom)
	assert.Equal(t, custom, j.New.AgentDir)
	assert.Equal(t, winservice.ServiceName, d.connectedService())
	assert.False(t, d.serviceExists(winservice.LegacyServiceName))
	assert.False(t, d.exists(d.newDir()), "nothing moved to DeviceAgent")
	assert.Equal(t, testVersion, d.content(filepath.Join(custom, "device-agent.exe")))
	assert.Equal(t, "database", d.content(filepath.Join(custom, "device-agent.db")))
	assert.Equal(t, "log", d.content(filepath.Join(custom, "device-agent.log")))
	assert.False(t, d.exists(filepath.Join(custom, "reagent.exe")))
	assert.False(t, d.exists(filepath.Join(custom, winservice.RecreateMarkerName)), "the bind mounts did not change")
	s := d.sys.services[winservice.ServiceName]
	assert.Equal(t, filepath.Join(custom, "device-agent.exe"), s.spec.Exe)
	assert.Equal(t, custom, flagValue(s.spec.Args, "-agentDir", ""))
	assert.Equal(t, filepath.Join(custom, "apps"), flagValue(s.spec.Args, "-appsDir", ""))
	assert.True(t, d.sys.defender[filepath.Join(custom, "frpc.exe")], "the exclusion names the folder the agent still runs in")
	assert.Equal(t, filepath.Join(custom, winservice.RepairName), d.sys.task.script)
}

// The other reasons for staying in place.
func TestStrategy(t *testing.T) {
	cases := map[string]struct {
		setup  func(d *device)
		want   string
		reason string
	}{
		"default":                     {func(d *device) {}, StrategyMove, "the default folder"},
		"DeviceAgent in use":          {func(d *device) { d.file(filepath.Join(d.newDir(), "x"), "", 0o644) }, StrategyInPlace, "not empty"},
		"another volume":              {func(d *device) { d.sys.otherVolume[d.legacyDir] = true }, StrategyInPlace, "another volume"},
		"Hyper-V backend, not shared": {func(d *device) { d.sys.hyperV, d.sys.shared = true, []string{filepath.Join(d.root, "Other")} }, StrategyInPlace, "file sharing"},
		"Hyper-V backend, shared":     {func(d *device) { d.sys.hyperV, d.sys.shared = true, []string{d.root} }, StrategyMove, "the default folder"},
		"WSL backend":                 {func(d *device) { d.sys.hyperV, d.sys.shared = false, nil }, StrategyMove, "the default folder"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			d := newDevice(t)
			c.setup(d)
			e := d.env()
			d.stage(e)
			require.NoError(t, d.run(e))
			j := d.journal()
			assert.True(t, j.Done)
			assert.Equal(t, c.want, j.Strategy)
			assert.Contains(t, j.StrategyReason, c.reason)
			assert.Equal(t, winservice.ServiceName, d.connectedService())
		})
	}
}

// An empty DeviceAgent folder (a failed earlier attempt, a curious operator)
// is removed by Check and does not change the strategy.
func TestCheckRemovesAnEmptyNewFolder(t *testing.T) {
	d := newDevice(t)
	require.NoError(t, os.MkdirAll(d.newDir(), 0o755))
	e := d.env()
	d.stage(e)
	assert.False(t, d.exists(d.newDir()))
	require.NoError(t, d.run(e))
	assert.Equal(t, StrategyMove, d.journal().Strategy)
}

// A failure at any step (the fault fires before the step runs, so even a
// fault "at cleanup" comes before Done) brings the device back exactly as it
// was, with the legacy service connected again. prepare drops the legacy
// update files for good; a fault before prepare ran leaves even those.
func TestMigrateRollsBackAtEveryStep(t *testing.T) {
	for _, name := range StepNames() {
		t.Run(name, func(t *testing.T) {
			d := newDevice(t)
			before := d.snapshot()
			if name != "prepare" {
				before = d.withoutDropped(before)
			}
			e := d.env()
			d.stage(e)
			e.Fault = name

			err := d.run(e)
			require.ErrorContains(t, err, "fault injected at "+name)
			assertRolledBack(t, d, before)
			j := d.journal()
			assert.Contains(t, j.Error, name)
			assert.Contains(t, LastError(e.Layout), name)
			assert.Equal(t, 1, j.Attempts)
			// The new service never stayed enabled next to the legacy one,
			// and its undo disables it before stopping it: recovery must not
			// restart it mid-rollback.
			if i := index(d.sys.calls, "start-type ironflock-device-agent auto"); i >= 0 {
				assert.Greater(t, lastIndex(d.sys.calls, "start-type ironflock-device-agent disabled"), i)
				assert.Greater(t, lastIndex(d.sys.calls, "delete ironflock-device-agent"), i)
				undo := d.sys.calls[index(d.sys.calls, "start ironflock-device-agent"):]
				assert.Less(t, index(undo, "start-type ironflock-device-agent disabled"), index(undo, "stop ironflock-device-agent"), "disabled first")
			}
		})
	}
}

// An error inside the cleanup tail (after Done) does not undo anything: the
// move is done, the tail only logs, and it runs again at the migrate
// service's next start until everything it does has worked.
func TestAnErrorAfterDoneIsNotRolledBack(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	d.sys.failOn["delete reagent"] = errors.New("the service is busy")
	require.NoError(t, d.run(e))
	j := d.journal()
	assert.True(t, j.Done)
	assert.False(t, j.Failed())
	assert.True(t, d.serviceExists(winservice.LegacyServiceName), "the reagent service could not be deleted this time")
	assert.Equal(t, winservice.StartDisabled, d.sys.services[winservice.LegacyServiceName].startType)
	assert.Equal(t, winservice.ServiceName, d.connectedService())

	// the migrate service's next start reruns the tail
	delete(d.sys.failOn, "delete reagent")
	require.NoError(t, d.sys.startService(winservice.MigrateServiceName))
	require.NoError(t, d.run(e))
	assert.True(t, d.journal().Done)
	assert.False(t, d.serviceExists(winservice.LegacyServiceName))
}

// finalState is the device after a migration that was never interrupted.
func finalState(t *testing.T) map[string]string {
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	require.NoError(t, d.run(e))
	return d.snapshot()
}

// A power cut after any call that changes something: the boot runs the
// repair task and the automatic services, the migrate service resumes, and
// the device ends where an uninterrupted migration ends — or, after a
// rollback, with the legacy service connected. At every instant after the
// boot an agent is connected before the migrator is done.
func TestMigrateSurvivesAPowerCutAtEveryCall(t *testing.T) {
	want := finalState(t)

	reference := newDevice(t)
	reference.stage(reference.env())
	require.NoError(t, reference.run(reference.env()))
	total := len(reference.sys.calls)
	require.Greater(t, total, 50)

	for n := 1; n <= total; n++ {
		t.Run(fmt.Sprintf("call %d %s", n, reference.sys.calls[n-1]), func(t *testing.T) {
			d := newDevice(t)
			d.stage(d.env())
			d.sys.crashAt = n
			require.PanicsWithValue(t, errCrash, func() { _ = Run(d.env()) })
			d.sys.crashAt = 0

			d.boot()
			d.runMigratorAfterBoot()

			j := d.journal()
			require.True(t, j.Done || j.Failed(), "the migration ends done or rolled back: %+v", j)
			connected := d.connectedService()
			require.NotEmpty(t, connected, "an agent is connected after the boot")
			if j.Done {
				assert.Equal(t, winservice.ServiceName, connected)
				assert.Equal(t, want, d.snapshot())
				assert.True(t, d.markerConsumed(d.newDir()), "the agent recreated its containers from the moved folder")
				assertPolicies(t, d, d.newDir())
			} else {
				assert.Equal(t, winservice.LegacyServiceName, connected)
			}
		})
	}
}

// A power cut while the migrator is paused after a step (the journal says
// the step is done): the resume ends where an uninterrupted migration ends.
func TestMigrateResumesAfterEveryStep(t *testing.T) {
	want := finalState(t)
	for _, name := range StepNames() {
		t.Run("after "+name, func(t *testing.T) {
			d := newDevice(t)
			e := d.env()
			d.stage(e)
			// the pause sleeps an hour at a time: cut the power there
			e.Pause = name
			e.Sys = &crashOnLongSleep{d.sys}
			require.PanicsWithValue(t, errCrash, func() { _ = Run(e) })
			d.boot()
			d.runMigratorAfterBoot()
			assert.True(t, d.journal().Done)
			assert.Equal(t, want, d.snapshot())
			assert.Equal(t, winservice.ServiceName, d.connectedService())
		})
	}
}

// crashOnLongSleep cuts the power when the test hook's pause sleeps.
type crashOnLongSleep struct{ *fakeSystem }

func (c *crashOnLongSleep) Sleep(d time.Duration) {
	if d == time.Hour {
		panic(errCrash)
	}
	c.fakeSystem.Sleep(d)
}

// A power cut in the middle of a rollback (from every step): the boot
// continues the rollback, even when the fault that started it is gone by
// then (it never runs the migration forward over half-undone steps). The
// rollback is a fact once the journal says so: a cut before that write
// resumes forward, as the fault was transient, which
// TestMigrateSurvivesAPowerCutAtEveryCall covers. Once the undo of
// stop-legacy gave the legacy service its start type back, it boots on its
// own, before the migrator resumes: the device is connected at every boot
// from there on.
func TestMigrateContinuesARollbackAfterAPowerCut(t *testing.T) {
	for _, fault := range StepNames() {
		probe := newDevice(t)
		pe := probe.env()
		probe.stage(pe)
		rollbackFrom, undoPrepareAt := -1, -1
		pe.Logf = func(format string, args ...any) {
			line := fmt.Sprintf(format, args...)
			if rollbackFrom < 0 && strings.Contains(line, "rolling back") {
				rollbackFrom = len(probe.sys.calls)
			}
			if undoPrepareAt < 0 && line == "migrate: undoing prepare" {
				undoPrepareAt = len(probe.sys.calls)
			}
		}
		pe.Fault = fault
		require.Error(t, probe.run(pe))
		require.GreaterOrEqual(t, rollbackFrom, 0)
		require.GreaterOrEqual(t, undoPrepareAt, 0)
		// the journal write that records the rollback
		rollbackSaved := -1
		for n := rollbackFrom; n < len(probe.sys.calls); n++ {
			if probe.sys.calls[n] == "write "+winservice.JournalName {
				rollbackSaved = n + 1
				break
			}
		}
		require.Greater(t, rollbackSaved, 0)
		// the undo of stop-legacy gave the legacy service its start type
		// back before the undo of prepare begins: from a cut inside the latter
		// on, the legacy service boots by itself
		legacyAuto := undoPrepareAt
		for n := rollbackSaved + 1; n <= len(probe.sys.calls); n++ {
			t.Run(fmt.Sprintf("%s call %d %s", fault, n, probe.sys.calls[n-1]), func(t *testing.T) {
				d := newDevice(t)
				before := d.snapshot()
				if fault != "prepare" {
					before = d.withoutDropped(before)
				}
				e := d.env()
				d.stage(e)
				e.Fault = fault
				d.sys.crashAt = n
				require.PanicsWithValue(t, errCrash, func() { _ = Run(e) })
				d.sys.crashAt = 0
				d.boot() // the boot, without the fault
				if n > legacyAuto && stepIndex(fault) > stepIndex("stop-legacy") {
					require.Equal(t, winservice.LegacyServiceName, d.connectedService(), "the legacy service boots on its own, before the migrator resumes")
				}
				d.runMigratorAfterBoot()
				assertRolledBack(t, d, before)
			})
		}
	}
}

// The migrate service is asked to stop (sc stop) while a step runs: the
// step in flight completes, the next one does not start, the journal
// persists and the next start resumes.
func TestAStoppedMigratorResumes(t *testing.T) {
	want := finalState(t)
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	stopped := false
	steps := 0
	e.Cancelled = func() bool { return stopped }
	e.Logf = func(format string, args ...any) {
		if format == "migrate: %s" {
			steps++
			stopped = steps == 4 // during rename-files
		}
	}
	err := d.run(e)
	require.ErrorIs(t, err, ErrCancelled)
	j := d.journal()
	assert.True(t, j.InProgressState())
	assert.Equal(t, 4, j.Step)

	stopped = false
	require.NoError(t, d.sys.startService(winservice.MigrateServiceName))
	require.NoError(t, d.run(e))
	assert.True(t, d.journal().Done)
	assert.Equal(t, want, d.snapshot())
}

// A new agent that never connects is rolled back and the legacy one
// reconnects; a connected file that is not this process's (stale, another
// service name, a reused pid) cannot pass the health check.
func TestMigrateRollsBackAnAgentThatNeverConnects(t *testing.T) {
	cases := map[string]func(d *device){
		"never connects": func(d *device) { d.sys.connectFails[winservice.ServiceName] = true },
		"a stale file of an earlier process": func(d *device) {
			d.sys.connectFails[winservice.ServiceName] = true
			d.sys.agentKeepsStaleConnected = true
			d.file(filepath.Join(d.legacyDir, winservice.ConnectedName), "999 "+testVersion+" "+winservice.ServiceName+"\n", 0o644)
		},
		"the legacy name in the file": func(d *device) {
			d.sys.connectFails[winservice.ServiceName] = true
			d.sys.agentKeepsStaleConnected = true
			d.file(filepath.Join(d.legacyDir, winservice.ConnectedName), fmt.Sprintf("%d %s reagent\n", d.sys.nextPID+1, testVersion), 0o644)
		},
		"another version": func(d *device) {
			d.sys.connectFails[winservice.ServiceName] = true
			d.sys.agentKeepsStaleConnected = true
			d.file(filepath.Join(d.legacyDir, winservice.ConnectedName), fmt.Sprintf("%d 1.3.8 %s\n", d.sys.nextPID+1, winservice.ServiceName), 0o644)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			d := newDevice(t)
			setup(d)
			before := d.withoutDropped(d.snapshot())
			e := d.env()
			d.stage(e)
			err := d.run(e)
			require.ErrorContains(t, err, "health")
			assertRolledBack(t, d, before)
		})
	}
}

// The agent removes the connected file at its start: a stale file naming
// the pid the service gets again (pid reuse) would otherwise pass. This is
// the model of service_windows.go's removal; without it the check passes.
func TestAStaleConnectedFileWithAReusedPIDPassesOnlyWithoutTheRemoval(t *testing.T) {
	stale := func(d *device) {
		d.sys.connectFails[winservice.ServiceName] = true
		d.file(filepath.Join(d.legacyDir, winservice.ConnectedName), fmt.Sprintf("%d %s %s\n", d.sys.nextPID+1, testVersion, winservice.ServiceName), 0o644)
	}
	t.Run("the agent removes it", func(t *testing.T) {
		d := newDevice(t)
		stale(d)
		e := d.env()
		d.stage(e)
		require.ErrorContains(t, d.run(e), "health")
		assert.True(t, d.journal().Failed())
	})
	t.Run("an agent that keeps it would pass", func(t *testing.T) {
		d := newDevice(t)
		stale(d)
		d.sys.agentKeepsStaleConnected = true
		e := d.env()
		d.stage(e)
		require.NoError(t, d.run(e))
		assert.True(t, d.journal().Done)
	})
}

// An agent that restarts during its first minute has not proven itself,
// even though recovery starts it again and it connects again.
func TestMigrateRollsBackAnAgentThatRestarts(t *testing.T) {
	d := newDevice(t)
	before := d.withoutDropped(d.snapshot())
	e := d.env()
	d.stage(e)
	e.Logf = func(format string, args ...any) {
		if strings.HasPrefix(fmt.Sprintf(format, args...), "migrate: "+winservice.ServiceName+" connected as pid") {
			// the new agent crashes right after it connected; recovery
			// starts it again with a new pid
			s := d.sys.services[winservice.ServiceName]
			_ = (&uncounted{d.sys}).KillProcess(s.pid)
		}
	}
	require.ErrorContains(t, d.run(e), "restarted within")
	assertRolledBack(t, d, before)
}

// Every attempt that rolls back is retried a day later, without end.
func TestMigrateRetriesForever(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	e.Fault = "health"
	for attempt := 1; attempt <= 10; attempt++ {
		due, err := Due(e)
		require.NoError(t, err)
		require.True(t, due, "attempt %d", attempt)
		d.stage(e)
		require.Error(t, d.run(e))
		assert.Equal(t, attempt, d.journal().Attempts)
		assert.Equal(t, winservice.LegacyServiceName, d.connectedService())
		due, err = Due(e)
		require.NoError(t, err)
		require.False(t, due, "not again the same day")
		d.sys.now = d.sys.now.Add(retryAfterFailure)
	}
	assert.Contains(t, LastError(e.Layout), "attempt 10; retried at the agent's next start")

	e.Fault = ""
	d.stage(e)
	require.NoError(t, d.run(e))
	assert.True(t, d.journal().Done)
	assert.Equal(t, 10, d.journal().Attempts)
}

// A clock behind the last failure's stamp does not hold the retry off.
func TestDueWithTheClockBehind(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	e.Fault = "health"
	d.stage(e)
	require.Error(t, d.run(e))
	d.sys.now = d.sys.now.Add(-48 * time.Hour)
	due, err := Due(e)
	require.NoError(t, err)
	assert.True(t, due)
}

func TestDue(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	due, err := Due(e)
	require.NoError(t, err)
	assert.True(t, due, "never tried")

	d.stage(e)
	due, err = Due(e)
	require.NoError(t, err)
	assert.False(t, due, "in progress: the migrate service resumes it")
	assert.True(t, InProgress(e.Layout))

	require.NoError(t, d.run(e))
	due, err = Due(e)
	require.NoError(t, err)
	assert.False(t, due, "done")
	assert.False(t, InProgress(e.Layout))
	assert.Empty(t, LastError(e.Layout))
}

// A process that keeps dying (the watchdog, a crash) is not restarted
// forever: the attempt is rolled back after maxRuns. Every start of the
// migrate service counts a run.
func TestMigrateRollsBackAfterTooManyRuns(t *testing.T) {
	d := newDevice(t)
	before := d.withoutDropped(d.snapshot())
	e := d.env()
	d.stage(e)
	// the watchdog (45 min without finishing) exits the process while it
	// hangs after move-folder; the SCM's recovery starts it again
	e.Pause = "move-folder"
	e.Sys = &crashOnLongSleep{d.sys}
	require.PanicsWithValue(t, errCrash, func() { _ = Run(e) })
	assert.Equal(t, 1, d.journal().Runs)
	d.migratorExits()

	// four more starts that die the same way
	j := d.journal()
	j.Runs = maxRuns
	require.NoError(t, d.env().saveJournal(j))
	require.NoError(t, d.sys.startService(winservice.MigrateServiceName))
	err := d.run(d.env())
	require.ErrorContains(t, err, "stopped 5 times")
	assertRolledBack(t, d, before)
}

// An unreadable journal falls back to its backup, and the attempt is rolled
// back (one more step may have run); when both are unreadable, the migrator
// can only make sure an agent service runs.
func TestCorruptJournal(t *testing.T) {
	t.Run("the journal, with a backup", func(t *testing.T) {
		d := newDevice(t)
		before := d.withoutDropped(d.snapshot())
		e := d.env()
		d.stage(e)
		e.Pause = "rename-files"
		e.Sys = &crashOnLongSleep{d.sys}
		require.PanicsWithValue(t, errCrash, func() { _ = Run(e) })
		d.file(e.Layout.Journal(), "{garbage", 0o644)
		d.boot()
		d.runMigratorAfterBoot()
		j := d.journal()
		assert.True(t, j.Failed())
		assert.Contains(t, j.Error, "restored from its backup")
		assertRolledBack(t, d, before)
	})
	t.Run("both", func(t *testing.T) {
		d := newDevice(t)
		e := d.env()
		d.stage(e)
		e.Pause = "rename-files"
		e.Sys = &crashOnLongSleep{d.sys}
		require.PanicsWithValue(t, errCrash, func() { _ = Run(e) })
		d.file(e.Layout.Journal(), "{garbage", 0o644)
		d.file(e.Layout.JournalBak(), "", 0o644)
		d.boot()
		err := d.run(d.env())
		require.ErrorIs(t, err, ErrJournalUnreadable)
		// The files were renamed (step 3 done) and the legacy service
		// disabled: ensure-agent renames them back and starts it. The
		// migration itself needs a hand (the journal is unreadable) — but
		// the device is connected, and nothing was deleted.
		assert.Equal(t, winservice.LegacyServiceName, d.connectedService())
		assert.Equal(t, testVersion, d.content(filepath.Join(d.legacyDir, "reagent.exe")))
		assert.Equal(t, "database", d.content(filepath.Join(d.legacyDir, "reagent.db")))
		assert.Equal(t, "log", d.content(filepath.Join(d.legacyDir, "reagent.log")))
		assert.False(t, d.exists(filepath.Join(d.legacyDir, "device-agent.exe")))
		assert.True(t, d.exists(filepath.Join(d.legacyDir, "device.flock")))
		d.boot()
		d.runMigratorAfterBoot()
		assert.Equal(t, winservice.LegacyServiceName, d.connectedService(), "and after a boot")
	})
	t.Run("both, after register-service", func(t *testing.T) {
		d := newDevice(t)
		d.pausedAfter("register-service")
		d.file(Layout{Root: d.root}.Journal(), "{garbage", 0o644)
		d.file(Layout{Root: d.root}.JournalBak(), "", 0o644)
		d.boot()
		err := d.run(d.env())
		require.ErrorIs(t, err, ErrJournalUnreadable)
		// The new service exists with its files: ensure-agent starts it.
		assert.Equal(t, winservice.ServiceName, d.connectedService())
	})
	t.Run("both, before anything moved", func(t *testing.T) {
		d := newDevice(t)
		e := d.env()
		d.stage(e)
		d.file(e.Layout.Journal(), "{garbage", 0o644)
		d.file(e.Layout.JournalBak(), "", 0o644)
		d.boot()
		err := d.run(d.env())
		require.ErrorIs(t, err, ErrJournalUnreadable)
		assert.Equal(t, winservice.LegacyServiceName, d.connectedService(), "the legacy service still runs")
	})
}

// Defender quarantined the migrator's copy: the migrate service cannot
// start at boot, the repair task restores the copy from an agent binary and
// starts it.
func TestQuarantinedMigratorIsRestoredByTheRepairTask(t *testing.T) {
	want := finalState(t)
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	e.Pause = "rename-files"
	e.Sys = &crashOnLongSleep{d.sys}
	require.PanicsWithValue(t, errCrash, func() { _ = Run(e) })
	require.NoError(t, os.Remove(e.Layout.MigratorExe()))
	d.boot()
	require.True(t, d.exists(e.Layout.MigratorExe()), "restored by the repair task")
	require.True(t, d.migrateServiceRunning())
	d.runMigratorAfterBoot()
	assert.True(t, d.journal().Done)
	assert.Equal(t, want, d.snapshot())
}

// Docker is down for the whole move (Docker Desktop before anyone logs
// in): the containers keep their policies, the move goes through, and the
// agent recreates the containers once Docker is back.
func TestMigrateWithDockerDown(t *testing.T) {
	d := newDevice(t)
	d.sys.dockerDown = true
	e := d.env()
	d.stage(e)
	require.NoError(t, d.run(e))
	j := d.journal()
	assert.True(t, j.Done)
	assert.True(t, j.DockerUnavailable)
	assert.Empty(t, j.Containers)
	assert.Equal(t, winservice.ServiceName, d.connectedService())
	assert.True(t, d.exists(filepath.Join(d.newDir(), winservice.RecreateMarkerName)))
}

// A container with a restart policy that Docker started again at boot
// (after a power cut mid-move) recreated the old folder under its bind
// mount: the resume moves the stray aside and goes on; a rollback moves it
// aside before the folder goes back.
func TestAStrayFolderDockerRecreated(t *testing.T) {
	t.Run("resume", func(t *testing.T) {
		d := newDevice(t)
		d.sys.dockerDown = true // stop-apps records nothing, the policies stay
		e := d.env()
		d.stage(e)
		e.Pause = "move-folder"
		e.Sys = &crashOnLongSleep{d.sys}
		require.PanicsWithValue(t, errCrash, func() { _ = Run(e) })
		d.sys.dockerDown = false
		d.boot() // Docker starts c2 (always) and recreates Reagent\apps\prod\app2
		require.True(t, d.exists(filepath.Join(d.legacyDir, "apps", "prod", "app2")), "the stray folder")
		d.runMigratorAfterBoot()
		j := d.journal()
		assert.True(t, j.Done, j.Error)
		assert.Equal(t, winservice.ServiceName, d.connectedService())
		strays, _ := filepath.Glob(d.legacyDir + ".stray-*")
		assert.Len(t, strays, 1, "the stray was moved aside, never deleted")
	})
	t.Run("rollback", func(t *testing.T) {
		d := newDevice(t)
		d.sys.dockerDown = true
		before := d.withoutDropped(d.snapshot())
		e := d.env()
		d.stage(e)
		e.Pause = "start-agent"
		e.Sys = &crashOnLongSleep{d.sys}
		require.PanicsWithValue(t, errCrash, func() { _ = Run(e) })
		d.sys.dockerDown = false
		d.sys.connectFails[winservice.ServiceName] = true // the new agent never connects: rollback
		d.boot()
		require.True(t, d.exists(filepath.Join(d.legacyDir, "apps", "prod", "app2")), "the stray folder")
		d.runMigratorAfterBoot()
		j := d.journal()
		assert.True(t, j.Failed())
		strays, _ := filepath.Glob(d.legacyDir + ".stray-*")
		require.Len(t, strays, 1)
		require.NoError(t, os.RemoveAll(strays[0]))
		assertRolledBack(t, d, before)
	})
}

// The folder cannot be moved back (something holds it, the stray cannot be
// moved aside): the legacy service has no folder to run from, so
// ensure-agent re-creates the new service, whose folder is complete, and
// starts it; the agent adopts the journal once it connects.
func TestAFailedMoveBackEndsWithTheNewServiceAdopted(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	e.Fault = "health"
	// the new folder is held open by something once it exists
	e.Logf = func(format string, args ...any) {
		if fmt.Sprintf(format, args...) == "migrate: health" {
			d.sys.holders[d.newDir()] = []string{"explorer.exe"}
		}
	}
	err := d.run(e)
	require.ErrorContains(t, err, "fault injected at health")
	j := d.journal()
	assert.Equal(t, winservice.ServiceName, j.EnsuredService, j.EnsureError)
	assert.True(t, d.exists(d.newDir()))
	assert.False(t, d.exists(d.legacyDir))
	assert.Equal(t, winservice.ServiceName, d.connectedService())
	// The agent connected while the migrate service was still watching it:
	// it adopts the journal once that service has stopped (its retry).
	assert.True(t, j.Failed(), "not adopted under the running migrator")
	d.agentRetriesAdoption()
	j = d.journal()
	assert.True(t, j.Done)
	assert.Contains(t, j.Adopted, "rolled back at health")
	assert.False(t, d.serviceExists(winservice.LegacyServiceName))
	assert.False(t, InProgress(e.Layout))
	due, err := Due(e)
	require.NoError(t, err)
	assert.False(t, due)
}

// An open handle on the folder (an Explorer window) blocks the rename for a
// while: the retries wait it out; one that never goes names the holder, the
// attempt rolls back, and the next one stays in place.
func TestAHeldFolder(t *testing.T) {
	t.Run("released", func(t *testing.T) {
		d := newDevice(t)
		d.sys.holders[d.legacyDir] = []string{"explorer.exe"}
		d.sys.releaseAfter[d.legacyDir] = 3
		e := d.env()
		d.stage(e)
		require.NoError(t, d.run(e))
		assert.True(t, d.journal().Done)
		assert.Equal(t, StrategyMove, d.journal().Strategy)
	})
	t.Run("never released", func(t *testing.T) {
		d := newDevice(t)
		before := d.withoutDropped(d.snapshot())
		d.sys.holders[d.legacyDir] = []string{"explorer.exe", "MsMpEng.exe"}
		e := d.env()
		d.stage(e)
		err := d.run(e)
		require.ErrorContains(t, err, "held open by MsMpEng.exe, explorer.exe")
		assert.Contains(t, LastError(e.Layout), "explorer.exe")
		j := d.journal()
		assert.True(t, j.PreferInPlace)
		delete(d.sys.holders, d.legacyDir)
		assertRolledBack(t, d, before)

		// the next attempt keeps the folder in place
		d.sys.now = d.sys.now.Add(retryAfterFailure)
		d.stage(e)
		require.NoError(t, d.run(e))
		j = d.journal()
		assert.True(t, j.Done)
		assert.Equal(t, StrategyInPlace, j.Strategy)
		assert.Contains(t, j.StrategyReason, "earlier attempt")
		assert.Equal(t, winservice.ServiceName, d.connectedService())
		assert.Equal(t, filepath.Join(d.legacyDir, "device-agent.exe"), d.sys.services[winservice.ServiceName].spec.Exe)
	})
}

// A file of the new name that exists already (an earlier attempt that could
// not rename it back, an operator's copy) is never overwritten: it is moved
// aside, unless it is the executable at the right version.
func TestRenamesNeverOverwrite(t *testing.T) {
	d := newDevice(t)
	d.file(filepath.Join(d.legacyDir, "device-agent.db"), "older copy", 0o644)
	d.file(filepath.Join(d.legacyDir, "device-agent.exe"), testVersion, 0o755)
	e := d.env()
	d.stage(e)
	require.NoError(t, d.run(e))
	snap := d.snapshot()
	assert.Equal(t, "database", snap[winservice.Folder+"/device-agent.db"], "the live database won")
	stale := 0
	for path, content := range snap {
		if strings.Contains(path, "device-agent.db.stale-") {
			stale++
			assert.Equal(t, "older copy", content)
		}
	}
	assert.Equal(t, 1, stale, "the other copy was moved aside, not deleted")
	assert.Equal(t, testVersion, snap[winservice.Folder+"/device-agent.exe"])
}

// A device to move: elevated, a registered reagent service with its files;
// a hand-installed new service or an update under way stops the move. That
// the process is the reagent service is the automatic trigger's condition,
// not Check's: `migrate-layout start` runs from a console while the service
// runs (or after it stopped).
func TestCheck(t *testing.T) {
	cases := map[string]struct {
		setup func(d *device)
		want  string
	}{
		"legacy service": {func(d *device) {}, ""},
		"from a console, the service stopped": {func(d *device) {
			require.NoError(t, (&uncounted{d.sys}).StopService(winservice.LegacyServiceName))
		}, ""},
		"not elevated":       {func(d *device) { d.sys.elevated = false }, "not elevated"},
		"no reagent service": {func(d *device) { delete(d.sys.services, winservice.LegacyServiceName) }, "no reagent service"},
		"no reagent.exe": {func(d *device) {
			require.NoError(t, (&uncounted{d.sys}).StopService(winservice.LegacyServiceName))
			d.sys.now = d.sys.now.Add(time.Minute)
			require.NoError(t, os.Remove(filepath.Join(d.legacyDir, "reagent.exe")))
		}, "executable"},
		"no device.flock": {func(d *device) { require.NoError(t, os.Remove(filepath.Join(d.legacyDir, winservice.ConfigName))) }, "no "},
		"update swapping": {func(d *device) {
			d.file(filepath.Join(d.legacyDir, "update-state.json"), `{"phase":"swapping","new_version":"1.4.1"}`, 0o644)
		}, "an agent update is in swapping"},
		"update in probation": {func(d *device) {
			d.file(filepath.Join(d.legacyDir, "update-state.json"), `{"phase":"probation","new_version":"1.4.1"}`, 0o644)
		}, "an agent update is in probation"},
		"update judged": {func(d *device) {
			d.file(filepath.Join(d.legacyDir, "update-state.json"), `{"phase":"rolledback","new_version":"1.4.1"}`, 0o644)
		}, ""},
		"new service by hand": {func(d *device) {
			d.sys.services[winservice.ServiceName] = &fakeService{spec: winservice.Spec{Name: winservice.ServiceName}, state: StateStopped}
		}, "exists already"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			d := newDevice(t)
			c.setup(d)
			err := Check(d.env())
			if c.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, ErrNotEligible)
			assert.ErrorContains(t, err, c.want)
		})
	}
}

// A new service this journal created and a rollback could not delete does
// not stop the next attempt: Check allows it, and register-service takes it
// over (open or create).
func TestCheckAllowsTheServiceThisJournalCreated(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	e.Fault = "health"
	d.sys.failOn["delete "+winservice.ServiceName] = errors.New("access denied")
	require.Error(t, d.run(e))
	j := d.journal()
	require.True(t, j.Failed())
	require.True(t, j.CreatedService, "still ours")
	require.True(t, d.serviceExists(winservice.ServiceName))
	assert.Equal(t, winservice.StartDisabled, d.sys.services[winservice.ServiceName].startType)
	assert.Equal(t, winservice.LegacyServiceName, d.connectedService())

	require.NoError(t, Check(d.env()))
	delete(d.sys.failOn, "delete "+winservice.ServiceName)
	d.sys.now = d.sys.now.Add(retryAfterFailure)

	// The next attempt rolls back before register-service (whose undo would
	// delete the service): it owns the service all the same (CreatedService
	// travels into its journal), so the attempt after that is not refused as
	// "installed by hand" and takes the service over.
	e.Fault = "stop-apps"
	d.stage(e)
	require.True(t, d.journal().CreatedService, "carried into the new attempt")
	require.ErrorContains(t, d.run(e), "stop-apps")
	assert.True(t, d.serviceExists(winservice.ServiceName), "left for the next attempt")
	assert.True(t, d.journal().CreatedService, "still owned")
	assert.Equal(t, winservice.LegacyServiceName, d.connectedService())
	d.sys.now = d.sys.now.Add(retryAfterFailure)

	require.NoError(t, Check(d.env()))
	e.Fault = ""
	d.stage(e)
	require.NoError(t, d.run(e))
	assertMigrated(t, d)
}

func TestBeginRecordsAFailedStart(t *testing.T) {
	d := newDevice(t)
	d.sys.failOn["start "+winservice.MigrateServiceName] = errors.New("the service did not respond to the start request in a timely fashion")
	e := d.env()
	require.NoError(t, Check(e))
	require.Error(t, Begin(e, d.selfExe()))
	j := d.journal()
	assert.True(t, j.Failed(), "retried tomorrow, not resumed at boot")
	assert.Equal(t, 1, j.Attempts)
	assert.Equal(t, winservice.StartDisabled, d.sys.services[winservice.MigrateServiceName].startType)
	assert.Equal(t, winservice.LegacyServiceName, d.connectedService())
}

// Stage hardens the root first, removes what the agent did not make, keeps
// a verified copy of the running executable as the migrator and registers
// the migrate service with the legacy environment and the test hooks; the
// journal comes last.
func TestStage(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	e.Fault = "health"
	d.sys.owners[e.Layout.MigrateDir()] = "user"
	d.file(filepath.Join(e.Layout.MigrateDir(), "device-agent.exe"), "planted", 0o755)
	d.sys.owners[d.newDir()] = "user"
	d.file(filepath.Join(d.newDir(), "planted.txt"), "", 0o644)
	require.NoError(t, Check(e))
	d.sys.counting = true
	require.NoError(t, Stage(e, d.selfExe()))

	assert.Equal(t, []string{d.root}, d.sys.hardened)
	assert.Equal(t, testVersion, d.content(e.Layout.MigratorExe()), "the planted migrator was replaced by a copy of the running agent")
	assert.False(t, d.exists(d.newDir()), "a DeviceAgent folder someone else made is gone")
	m := d.sys.services[winservice.MigrateServiceName]
	require.NotNil(t, m)
	assert.Equal(t, e.Layout.MigratorExe(), m.spec.Exe)
	assert.Equal(t, []string{"migrate-layout"}, m.spec.Args)
	assert.Equal(t, winservice.StartAutomatic, m.startType)
	assert.False(t, m.delayed)
	assert.Equal(t, []string{"HTTPS_PROXY=http://proxy:3128", FaultEnv + "=health"}, m.spec.Env)
	assert.Equal(t, []int{30}, m.spec.RestartDelays)
	assert.True(t, d.sys.events[winservice.MigrateServiceName])
	j := d.journal()
	assert.True(t, j.InProgressState())
	assert.Equal(t, testVersion, j.Version)
	assert.Equal(t, 0, j.Step)
	// the journal was the last thing written
	assert.Equal(t, "write "+winservice.JournalName, d.sys.calls[len(d.sys.calls)-1])
}

// A rollback whose import of the saved task definition fails (schtasks
// refusing the exported XML) still points the repair task back at the
// legacy folder's script: the boot would otherwise run the universal script
// forever.
func TestRollbackRestoresTheTaskWhenTheImportFails(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	e.Fault = "stop-apps"
	d.sys.failOn["task-import"] = errors.New("the task XML contains a value which is incorrectly formatted")
	require.Error(t, d.run(e))
	assert.True(t, d.journal().Failed())
	assert.Equal(t, filepath.Join(d.legacyDir, winservice.RepairName), d.sys.task.script, "registered anew")
	assert.Equal(t, winservice.LegacyServiceName, d.connectedService())
}

// The test hooks a test sets in the reagent service's Environment (where
// the e2e harness puts them, and removes them for a retry) reach the
// migrate service, and only it: the new agent's service never carries them.
func TestStageTakesTheHooksFromTheLegacyServiceEnvironment(t *testing.T) {
	d := newDevice(t)
	legacy := d.sys.services[winservice.LegacyServiceName]
	legacy.spec.Env = append(legacy.spec.Env, FaultEnv+"=health", PauseEnv+"=move-folder")
	e := d.env() // no hooks in this process's environment
	require.NoError(t, Check(e))
	require.NoError(t, Stage(e, d.selfExe()))
	assert.Equal(t, []string{"HTTPS_PROXY=http://proxy:3128", FaultEnv + "=health", PauseEnv + "=move-folder"},
		d.sys.services[winservice.MigrateServiceName].spec.Env)

	// the process environment wins when both are set
	e.Fault = "cleanup"
	require.NoError(t, Stage(e, d.selfExe()))
	assert.Equal(t, []string{"HTTPS_PROXY=http://proxy:3128", FaultEnv + "=cleanup", PauseEnv + "=move-folder"},
		d.sys.services[winservice.MigrateServiceName].spec.Env)

	// the hooks the harness removed are gone at the next attempt
	legacy.spec.Env = []string{"HTTPS_PROXY=http://proxy:3128"}
	e.Fault = ""
	require.NoError(t, Stage(e, d.selfExe()))
	assert.Equal(t, []string{"HTTPS_PROXY=http://proxy:3128"}, d.sys.services[winservice.MigrateServiceName].spec.Env)

	// the new agent's service gets the environment without hooks (the fake
	// engine does not read the migrate service's environment, so the run
	// goes through)
	legacy.spec.Env = append(legacy.spec.Env, FaultEnv+"=health")
	d.stage(e)
	require.NoError(t, d.run(e))
	assert.Equal(t, []string{"HTTPS_PROXY=http://proxy:3128"}, d.sys.services[winservice.ServiceName].spec.Env)
}

// A migrator copy that does not run (a damaged copy, a quarantine) never
// becomes the migrate service.
func TestStageRefusesABrokenMigrator(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	d.sys.brokenExe[e.Layout.MigratorExe()] = true
	require.NoError(t, Check(e))
	err := Stage(e, d.selfExe())
	require.ErrorContains(t, err, "does not run")
	assert.False(t, d.exists(e.Layout.Journal()), "no journal: nothing to resume")
}

// The migrate service is counted (not the Now/Stat/Read calls), and a crash
// at the first counted call of Run already needs a resume.
func TestStepNamesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, name := range StepNames() {
		assert.False(t, seen[name], name)
		seen[name] = true
	}
	assert.Equal(t, []string{"prepare", "stop-legacy", "stop-apps", "rename-files", "move-folder", "register-service", "start-agent", "health", "cleanup"}, StepNames())
}

// The journal's backup: every save keeps the previous file, so a corrupted
// write leaves the state before it.
func TestJournalBackup(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	require.False(t, d.exists(e.Layout.JournalBak()), "nothing to back up at the first write")
	e.Pause = "stop-legacy"
	e.Sys = &crashOnLongSleep{d.sys}
	require.PanicsWithValue(t, errCrash, func() { _ = Run(e) })
	require.True(t, d.exists(e.Layout.JournalBak()))
	j, err := LoadJournal(e.Layout)
	require.NoError(t, err)
	assert.Equal(t, 2, j.Step)
	assert.False(t, j.RestoredFromBackup)

	require.NoError(t, os.Remove(e.Layout.Journal()))
	j, err = LoadJournal(e.Layout)
	require.NoError(t, err)
	assert.True(t, j.RestoredFromBackup)
	assert.LessOrEqual(t, j.Step, 2)
}

// Bind sources as the engines report them map onto the host folder.
func TestHostPathOf(t *testing.T) {
	assert.Equal(t, "c:/programdata/ironflock/reagent/apps", hostPathOf(`C:\ProgramData\IronFlock\Reagent\apps`))
	assert.Equal(t, "c:/programdata/ironflock/reagent/apps", hostPathOf("/mnt/c/ProgramData/IronFlock/Reagent/apps"))
	assert.Equal(t, "c:/programdata/ironflock/reagent/apps", hostPathOf("/run/desktop/mnt/host/c/ProgramData/IronFlock/Reagent/apps/"))
	assert.Equal(t, "c:/programdata/ironflock/reagent/apps", hostPathOf("/host_mnt/c/ProgramData/IronFlock/Reagent/apps"), "Docker Desktop's Hyper-V backend")
	assert.True(t, underDir("/host_mnt/c/ProgramData/IronFlock/Reagent/apps/prod/app1", `C:\ProgramData\IronFlock\Reagent`))
	assert.Equal(t, "d:/data", hostPathOf("/mnt/d/data"))
	assert.Equal(t, "/mnt/wsl/docker-desktop", hostPathOf("/mnt/wsl/docker-desktop"), "not a drive")
	assert.Equal(t, "/var/lib/x", hostPathOf("/var/lib/x"))
	assert.True(t, underDir("/mnt/c/ProgramData/IronFlock/Reagent/apps/prod/x", `C:\ProgramData\IronFlock\Reagent`))
	assert.True(t, underDir(`C:\ProgramData\IronFlock\Reagent`, `c:\programdata\ironflock\reagent\`))
	assert.False(t, underDir(`C:\ProgramData\IronFlock\ReagentX\apps`, `C:\ProgramData\IronFlock\Reagent`))
	assert.False(t, underDir(`C:\ProgramData\IronFlock\DeviceAgent\apps`, `C:\ProgramData\IronFlock\Reagent`))
}

// The legacy ImagePath's flags other than the paths travel to the new
// service; the paths are the new folder's.
func TestExtraArgs(t *testing.T) {
	assert.Equal(t, []string{"-debug", "-env", "production", "-remoteUpdateURL=https://x/dl"},
		extraArgs([]string{"-config", `C:\a\device.flock`, "-agentDir", `C:\a`, "-debug", "-appsDir", `C:\a\apps`, "-env", "production", "-dbFileName", `C:\a\reagent.db`, "-logFile=C:\\a\\reagent.log", "-remoteUpdateURL=https://x/dl"}))
	assert.Nil(t, extraArgs([]string{"-config", "x"}))
	assert.Equal(t, `C:\a\apps`, flagValue([]string{"-appsDir=C:\\a\\apps"}, "-appsDir", ""))
	assert.Equal(t, "dflt", flagValue([]string{"-other", "x"}, "-appsDir", "dflt"))
}

func TestDroppedLegacyFiles(t *testing.T) {
	for name, dropped := range map[string]bool{
		"reagent-prev.exe":          true,
		"reagent-failed-v1.3.9.exe": true,
		"reagent-v1.3.8.exe":        true,
		"reagent-v1.4.0.exe":        false,
		"reagent-v1.4.1.exe":        false,
		"reagent-vgarbage.exe":      true,
		"reagent.exe":               false,
		"reagent.db":                false,
		"device-agent-prev.exe":     false,
		"frpc.exe":                  false,
	} {
		assert.Equal(t, dropped, droppedLegacyFile(name), name)
	}
}

// A newer download that was not activated yet keeps travelling under the
// new name; the new agent's update check activates what it finds.
func TestANewerDownloadIsRenamed(t *testing.T) {
	d := newDevice(t)
	d.file(filepath.Join(d.legacyDir, "reagent-v1.4.1.exe"), "1.4.1", 0o755)
	e := d.env()
	d.stage(e)
	require.NoError(t, d.run(e))
	assert.Equal(t, "1.4.1", d.content(filepath.Join(d.newDir(), "device-agent-v1.4.1.exe")))
	assert.False(t, d.exists(filepath.Join(d.newDir(), "reagent-v1.4.1.exe")))
}

// The repair scripts the fake interprets behave like the real ones: the
// behaviour table (also run against the real scripts on Windows).
func TestRepairScriptModel(t *testing.T) {
	for _, c := range repairScriptCases() {
		t.Run(c.name, func(t *testing.T) {
			d := newDevice(t)
			d.powerCut()
			c.setup(d)
			d.runRepairTask()
			c.check(t, d)
		})
	}
}
