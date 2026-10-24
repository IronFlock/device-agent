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

// The containers' restart policies (stop-apps, its undo, the cleanup tail),
// the leftovers the agent removes, and the agent's side of a done journal.

// A compose app the user stopped keeps its old container after the move
// (compose only brings up what should run): the cleanup leaves it without a
// restart policy, since with one Docker would start it against a recreated
// old folder at every daemon start — a zombie folder nobody moves aside once
// the leftovers are gone.
func TestCleanupLeavesAnOldFolderContainerWithoutRestart(t *testing.T) {
	for name, dockerDownAtStopApps := range map[string]bool{"recorded by stop-apps": false, "Docker down at stop-apps": true} {
		t.Run(name, func(t *testing.T) {
			d := newDevice(t)
			for _, c := range d.sys.containers {
				if c.ID == "c2" {
					c.Running, c.wanted = false, false
				}
			}
			d.sys.dockerDown = dockerDownAtStopApps
			e := d.env()
			d.stage(e)
			if dockerDownAtStopApps {
				e.Logf = func(format string, args ...any) {
					if fmt.Sprintf(format, args...) == "migrate: cleanup" {
						d.sys.dockerDown = false // Docker Desktop started at a logon meanwhile
					}
				}
			}
			require.NoError(t, d.run(e))
			j := d.journal()
			require.True(t, j.Done)
			assert.Equal(t, "no", d.policies()["prod_2_app2-db-1"], "left to the agent, which recreates it with its own policy")
			for _, c := range j.Containers {
				assert.True(t, c.Restored, c.Name)
			}
			strays, _ := filepath.Glob(d.legacyDir + ".stray-*")
			for boot := 1; boot <= 2; boot++ {
				d.boot()
				d.runMigratorAfterBoot()
				assert.False(t, d.exists(d.legacyDir), "no zombie folder after boot %d", boot)
				assert.Equal(t, winservice.ServiceName, d.connectedService())
			}
			after, _ := filepath.Glob(d.legacyDir + ".stray-*")
			assert.Equal(t, len(strays), len(after), "no stray made at a boot")
			assert.False(t, d.serviceExists(winservice.MigrateServiceName), "the leftovers went")
			assert.Equal(t, "no", d.policies()["prod_2_app2-db-1"])
		})
	}
}

// Docker is unreachable from the cleanup on and still at the agent's first
// start (Docker Desktop starts at a logon): the leftovers stay until the
// policies are restored, the agent settles them once Docker is reachable
// (afterDockerReady: RemoveLeftovers), and only then removes the leftovers.
func TestLeftoversStayUntilThePoliciesAreRestored(t *testing.T) {
	d := newDeviceIn(t, inPlaceDir(t))
	e := d.env()
	d.stage(e)
	e.Logf = func(format string, args ...any) {
		if fmt.Sprintf(format, args...) == "migrate: cleanup" {
			d.sys.dockerDown = true
		}
	}
	require.NoError(t, d.run(e))
	require.True(t, d.journal().Done)
	require.Equal(t, "no", d.policies()["prod_2_app2-db-1"])

	d.boot() // Docker still down at the agent's start
	d.runMigratorAfterBoot()
	assert.True(t, d.serviceExists(winservice.MigrateServiceName), "the leftovers stay while a policy is owed")
	assert.True(t, d.exists(e.Layout.MigrateDir()))
	assert.Equal(t, "no", d.policies()["prod_2_app2-db-1"])

	d.sys.dockerDown = false
	RemoveLeftovers(d.sys.env(), winservice.ServiceName) // the agent, once Docker is reachable
	assert.Equal(t, "always", d.policies()["prod_2_app2-db-1"], "the policy is back")
	assert.False(t, d.serviceExists(winservice.MigrateServiceName), "and the leftovers went")
	assert.False(t, d.exists(e.Layout.MigrateDir()))
	for _, c := range d.journal().Containers {
		assert.True(t, c.Restored, c.Name)
	}
	assert.Equal(t, winservice.ServiceName, d.connectedService())
}

// A rollback that could not give a policy back (Docker unreachable) owes it
// to the next attempt: the record travels into the fresh journal, so the
// attempt keeps the original policy (not the "no" it finds) and restores it
// at its end.
func TestAnUnrestoredPolicySurvivesTheNextAttempt(t *testing.T) {
	d := newDeviceIn(t, inPlaceDir(t))
	e := d.env()
	e.Fault = "health"
	d.stage(e)
	e.Logf = func(format string, args ...any) {
		if fmt.Sprintf(format, args...) == "migrate: undoing stop-apps" {
			d.sys.dockerDown = true
		}
	}
	require.ErrorContains(t, d.run(e), "health")
	j := d.journal()
	require.True(t, j.Failed())
	require.Equal(t, winservice.LegacyServiceName, d.connectedService())
	var unrestored []string
	for _, c := range j.Containers {
		if !c.Restored {
			unrestored = append(unrestored, c.Name)
		}
	}
	require.Contains(t, unrestored, "prod_2_app2-db-1", "owed")
	require.Equal(t, "no", d.policies()["prod_2_app2-db-1"])

	d.sys.dockerDown = false
	d.sys.now = d.sys.now.Add(retryAfterFailure)
	e.Fault = ""
	e.Logf = nil
	d.stage(e)
	j = d.journal()
	require.Len(t, j.Containers, 2, "the owed records travel into the next attempt: %+v", j.Containers)
	require.NoError(t, d.run(e))
	assert.True(t, d.journal().Done)
	assert.Equal(t, "always", d.policies()["prod_2_app2-db-1"], "restored from the record, not from what stop-apps found")
	assert.Equal(t, winservice.ServiceName, d.connectedService())
}

// Docker was down for the whole migration (its containers kept their
// policies) and comes up at the boot after every power cut: it starts the
// compose container and recreates the old folder under its bind mount. The
// device ends migrated without a zombie folder (the container the agent did
// not recreate loses its policy, the folder goes aside), or rolled back.
func TestDockerDownDuringTheMoveThenUpAtEveryBoot(t *testing.T) {
	ref := newDevice(t)
	ref.sys.dockerDown = true
	ref.stage(ref.env())
	require.NoError(t, ref.run(ref.env()))
	total := len(ref.sys.calls)
	for n := 1; n <= total; n++ {
		t.Run(fmt.Sprintf("call %d %s", n, ref.sys.calls[n-1]), func(t *testing.T) {
			d := newDevice(t)
			d.sys.dockerDown = true
			d.stage(d.env())
			d.sys.crashAt = n
			require.PanicsWithValue(t, errCrash, func() { _ = Run(d.env()) })
			d.sys.crashAt = 0
			d.sys.dockerDown = false
			for boot := 1; boot <= 2; boot++ {
				d.boot()
				d.runMigratorAfterBoot()
			}
			j := d.journal()
			require.True(t, j.Done || j.Failed(), "%+v", j)
			if j.Done {
				assert.Equal(t, winservice.ServiceName, d.connectedService())
				assert.False(t, d.exists(d.legacyDir), "no zombie old folder")
				assert.False(t, d.serviceExists(winservice.MigrateServiceName), "the leftovers went")
			} else {
				assert.Equal(t, winservice.LegacyServiceName, d.connectedService())
			}
		})
	}
}

// Docker was unreachable at stop-apps and came up before move-folder (Docker
// Desktop at a logon): the containers that run from the folder are stopped
// (and recorded) before the rename, which they would otherwise block.
func TestMoveFolderStopsContainersThatCameUpAfterStopApps(t *testing.T) {
	d := newDevice(t)
	d.sys.dockerDown = true
	e := d.env()
	d.stage(e)
	e.Logf = func(format string, args ...any) {
		if fmt.Sprintf(format, args...) == "migrate: move-folder" {
			d.sys.dockerDown = false
		}
	}
	require.NoError(t, d.run(e))
	j := d.journal()
	require.True(t, j.Done)
	assert.True(t, j.DockerUnavailable)
	assert.Equal(t, []ContainerRecord{
		{ID: "c1", Name: "prod_1_app1", RestartPolicy: "no", Dir: d.legacyDir, Restored: true},
		{ID: "c2", Name: "prod_2_app2-db-1", RestartPolicy: "always", Dir: d.legacyDir, Restored: true},
	}, sortedRecords(j.Containers), "recorded at move-folder")
	calls := d.sys.calls
	assert.Less(t, index(calls, "restart-policy c2 no"), index(calls, "rename Reagent DeviceAgent"))
	assert.Less(t, index(calls, "stop-container c2"), index(calls, "rename Reagent DeviceAgent"))
	assertMigrated(t, d)
}

// on-failure keeps its retry count through the journal.
func TestRestartPolicyRoundTrip(t *testing.T) {
	for policy, want := range map[string]struct {
		name  string
		count int
	}{
		"no":           {"no", 0},
		"always":       {"always", 0},
		"on-failure":   {"on-failure", 0},
		"on-failure:3": {"on-failure", 3},
		"":             {"no", 0},
	} {
		name, count := parseRestartPolicy(policy)
		assert.Equal(t, want.name, name, policy)
		assert.Equal(t, want.count, count, policy)
	}
	assert.Equal(t, "on-failure:3", restartPolicyString("on-failure", 3))
	assert.Equal(t, "on-failure", restartPolicyString("on-failure", 0))
	assert.Equal(t, "always", restartPolicyString("always", 5), "the count means nothing to the other policies")
	assert.Equal(t, "no", restartPolicyString("", 0))
}

// Docker dies in the middle of stop-apps: the step records what it saw,
// gives up on the container it could not change, and the move goes on (the
// rollback restores from the record; the agent recreates the container).
func TestStopAppsGoesOnWhenDockerDiesMidStep(t *testing.T) {
	for _, call := range []string{"restart-policy c2", "stop-container c2"} {
		t.Run(call, func(t *testing.T) {
			d := newDevice(t)
			e := d.env()
			d.stage(e)
			d.sys.failOn[call] = errors.New("error during connect: the pipe has been ended")
			require.NoError(t, d.run(e))
			j := d.journal()
			assert.True(t, j.Done)
			assert.True(t, j.DockerUnavailable)
			var c2 *ContainerRecord
			for i := range j.Containers {
				if j.Containers[i].ID == "c2" {
					c2 = &j.Containers[i]
				}
			}
			require.NotNil(t, c2, "recorded before the failing call")
			assert.Equal(t, "always", c2.RestartPolicy, "the original policy")
			assert.Equal(t, winservice.ServiceName, d.connectedService())
		})
	}
}

// The undo sees the containers the new agent made: a record whose container
// is gone (recreated) counts as restored; the undo of start-agent records,
// disarms and stops the new agent's containers before the folder moves back,
// and the legacy agent recreates them from its folder.
func TestTheUndoSeesTheRecreatedContainers(t *testing.T) {
	t.Run("done", func(t *testing.T) {
		d := newDevice(t)
		e := d.env()
		d.stage(e)
		require.NoError(t, d.run(e))
		assertMigrated(t, d)
		ids := map[string]bool{}
		for _, c := range d.sys.containers {
			ids[c.ID] = true
		}
		assert.Equal(t, map[string]bool{"c1'": true, "c2'": true, "c3": true}, ids, "the app containers were recreated under new ids")
		for _, c := range d.journal().Containers {
			assert.True(t, c.Restored, "%s: gone, so restored", c.Name)
		}
	})
	t.Run("rolled back", func(t *testing.T) {
		d := newDevice(t)
		before := d.withoutDropped(d.snapshot())
		e := d.env()
		e.Fault = "health"
		d.stage(e)
		require.ErrorContains(t, d.run(e), "health")
		calls := d.sys.calls
		stopped := index(calls, "stop-container c2'")
		require.GreaterOrEqual(t, stopped, 0, "the new agent's compose container was stopped")
		assert.Less(t, index(calls, "restart-policy c2' no"), stopped)
		assert.Less(t, stopped, index(calls, "rename DeviceAgent Reagent"), "before the folder went back")
		j := d.journal()
		recorded := map[string]ContainerRecord{}
		for _, c := range j.Containers {
			recorded[c.ID] = c
		}
		assert.Equal(t, "always", recorded["c2'"].RestartPolicy)
		assert.Equal(t, d.newDir(), recorded["c2'"].Dir)
		assert.True(t, recorded["c2'"].Restored, "left to the legacy agent")
		assert.True(t, recorded["c2"].Restored, "gone: recreated by the new agent")
		assertRolledBack(t, d, before)
		ids := map[string]bool{}
		for _, c := range d.sys.containers {
			ids[c.ID] = true
		}
		assert.Equal(t, map[string]bool{"c1''": true, "c2''": true, "c3": true}, ids, "recreated once more by the legacy agent")
	})
}

// schtasks fails when the cleanup points the repair task at the new folder's
// script, in the migrator and again at the agent's start: the leftovers stay
// (the universal script the task still names lives in .migrate) until the
// task is re-pointed; the boot-time repair keeps working.
func TestLeftoversStayWhileTheRepairTaskNamesTheUniversalScript(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	d.sys.failOn["task DeviceAgent/repair.cmd"] = errors.New("ERROR: The task scheduler service is not available")
	require.NoError(t, d.run(e))
	require.True(t, d.journal().Done)
	require.Equal(t, e.Layout.RepairCmd(), d.sys.task.script, "the tail could not re-point the task")
	d.powerCut()

	RemoveLeftovers(d.sys.env(), winservice.ServiceName) // the agent's start, schtasks still failing
	assert.True(t, d.serviceExists(winservice.MigrateServiceName), "kept")
	assert.True(t, d.exists(e.Layout.MigrateDir()), "kept: the task names its script")
	assert.Equal(t, e.Layout.RepairCmd(), d.sys.task.script)

	delete(d.sys.failOn, "task DeviceAgent/repair.cmd")
	RemoveLeftovers(d.sys.env(), winservice.ServiceName) // the next start
	assert.Equal(t, filepath.Join(d.newDir(), winservice.RepairName), d.sys.task.script, "re-pointed")
	assert.False(t, d.serviceExists(winservice.MigrateServiceName))
	assert.False(t, d.exists(e.Layout.MigrateDir()))

	// a torn self-update later: the boot's repair restores the executable
	require.NoError(t, os.Remove(filepath.Join(d.newDir(), winservice.ExeName)))
	d.boot()
	assert.Equal(t, winservice.ServiceName, d.connectedService())
	assert.Equal(t, testVersion, d.content(filepath.Join(d.newDir(), winservice.ExeName)))
}

// Done, with the repair task still universal (schtasks failed at the
// cleanup), and the new executable lost since: the universal script's
// migrate branch repairs no agent, so the migrate service, which runs
// before the delayed agent at a boot, restores the executable itself; the
// agent starts, and the task is re-pointed.
func TestADoneJournalWithTheUniversalTaskRestoresAVacantExe(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	d.sys.failOn["task DeviceAgent/repair.cmd"] = errors.New("ERROR: The task scheduler service is not available")
	require.NoError(t, d.run(e))
	j := d.journal()
	require.True(t, j.Done)
	require.Equal(t, winservice.ServiceName, j.EnsuredService)
	require.Equal(t, e.Layout.RepairCmd(), d.sys.task.script)
	d.powerCut()
	require.NoError(t, os.Remove(filepath.Join(d.newDir(), winservice.ExeName)))
	delete(d.sys.failOn, "task DeviceAgent/repair.cmd")

	d.bootRealOrder()
	assert.Equal(t, winservice.ServiceName, d.connectedService(), "connected after this very boot")
	assert.Equal(t, testVersion, d.content(filepath.Join(d.newDir(), winservice.ExeName)))
	assert.Equal(t, filepath.Join(d.newDir(), winservice.RepairName), d.sys.task.script)
}

// A power cut after every call of the agent's RemoveLeftovers at its start
// after a done migration: the next boot ends with the new agent connected
// and, one start later, without leftovers.
func TestRemoveLeftoversSurvivesAPowerCutAtEveryCall(t *testing.T) {
	done := func(t *testing.T) *device {
		d := newDevice(t)
		e := d.env()
		d.stage(e)
		require.NoError(t, d.run(e))
		d.powerCut()
		d.sys.calls = nil
		d.sys.counting = true
		return d
	}
	ref := done(t)
	RemoveLeftovers(ref.env(), winservice.ServiceName)
	total := len(ref.sys.calls)
	require.Greater(t, total, 5)
	for n := 1; n <= total; n++ {
		t.Run(fmt.Sprintf("call %d %s", n, ref.sys.calls[n-1]), func(t *testing.T) {
			d := done(t)
			d.sys.crashAt = n
			require.PanicsWithValue(t, errCrash, func() { RemoveLeftovers(d.env(), winservice.ServiceName) })
			d.sys.crashAt = 0
			d.boot()
			d.runMigratorAfterBoot()
			require.True(t, d.journal().Done)
			require.Equal(t, winservice.ServiceName, d.connectedService(), "after the first boot")
			d.boot()
			d.runMigratorAfterBoot()
			assert.Equal(t, winservice.ServiceName, d.connectedService(), "after the second boot")
			assert.False(t, d.serviceExists(winservice.MigrateServiceName))
			assert.False(t, d.serviceExists(winservice.LegacyServiceName))
			assert.False(t, d.exists(Layout{Root: d.root}.MigrateDir()))
			assert.Equal(t, filepath.Join(d.newDir(), winservice.RepairName), d.sys.task.script)
		})
	}
}

// The adopted rollback (a failed move-back): a power cut after every call
// Adopt makes; the boot ends done with the new agent connected, and the
// leftovers go.
func TestAdoptSurvivesAPowerCutAtEveryCall(t *testing.T) {
	setup := func(t *testing.T) *device {
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
		require.ErrorContains(t, d.run(e), "fault injected at health")
		j := d.journal()
		require.True(t, j.Failed())
		require.Equal(t, winservice.ServiceName, j.EnsuredService, j.EnsureError)
		require.Equal(t, winservice.ServiceName, d.connectedService())
		d.sys.calls = nil
		d.sys.counting = true
		return d
	}
	ref := setup(t)
	require.True(t, Adopt(ref.env()))
	total := len(ref.sys.calls)
	require.Greater(t, total, 3)
	require.True(t, ref.journal().Done)
	for n := 1; n <= total; n++ {
		t.Run(fmt.Sprintf("call %d %s", n, ref.sys.calls[n-1]), func(t *testing.T) {
			d := setup(t)
			d.sys.crashAt = n
			require.PanicsWithValue(t, errCrash, func() { Adopt(d.env()) })
			d.sys.crashAt = 0
			d.sys.adopts = true
			d.boot()
			d.runMigratorAfterBoot()
			j := d.journal()
			assert.True(t, j.Done, "%+v", j)
			assert.Equal(t, winservice.ServiceName, d.connectedService(), "after the first boot")
			d.boot()
			d.runMigratorAfterBoot()
			assert.False(t, d.serviceExists(winservice.MigrateServiceName))
			assert.False(t, d.serviceExists(winservice.LegacyServiceName))
			assert.False(t, d.exists(Layout{Root: d.root}.MigrateDir()))
			assert.Equal(t, winservice.ServiceName, d.connectedService(), "after the second boot")
		})
	}
}

// Run's done branch finishes ensure-agent when its outcome was never
// recorded (a power cut in its watch): at a boot the migrate service runs
// before the delayed agent and starts it.
func TestTheDoneBranchEnsuresTheAgent(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	require.NoError(t, d.run(e))
	j := d.journal()
	j.EnsuredService = ""
	require.NoError(t, e.saveJournal(j))
	assert.Equal(t, uint32(1), exitCodeFor(e, nil), "not ensured: the service asks for a restart")
	d.powerCut()
	require.NoError(t, d.sys.startService(winservice.MigrateServiceName))
	d.sys.calls = nil
	d.sys.counting = true
	require.NoError(t, d.run(e))
	assert.GreaterOrEqual(t, index(d.sys.calls, "start ironflock-device-agent"), 0, "ensure-agent started the agent")
	assert.Equal(t, winservice.ServiceName, d.journal().EnsuredService)
	assert.Equal(t, winservice.ServiceName, d.connectedService())
	assert.Equal(t, uint32(0), exitCodeFor(e, nil))
}

// Every power cut after the journal says done, booted in the real order
// (the migrate service's run before the delayed agent starts): done, the
// agent ensured and connected, no leftovers after its start.
func TestADoneJournalSurvivesAPowerCutInTheRealBootOrder(t *testing.T) {
	ref := newDevice(t)
	ref.stage(ref.env())
	require.NoError(t, ref.run(ref.env()))
	total := len(ref.sys.calls)
	// the done write is the journal write right before the cleanup tail
	// disables the legacy service
	doneAt := lastIndex(ref.sys.calls, "start-type reagent disabled")
	for doneAt > 0 && ref.sys.calls[doneAt] != "write "+winservice.JournalName {
		doneAt--
	}
	require.Greater(t, doneAt, 0)
	for n := doneAt + 2; n <= total; n++ {
		t.Run(fmt.Sprintf("call %d %s", n, ref.sys.calls[n-1]), func(t *testing.T) {
			d := newDevice(t)
			d.stage(d.env())
			d.sys.crashAt = n
			require.PanicsWithValue(t, errCrash, func() { _ = Run(d.env()) })
			d.sys.crashAt = 0
			require.True(t, d.journal().Done, "cut after done")
			d.bootRealOrder()
			d.runMigratorAfterBoot()
			j := d.journal()
			assert.True(t, j.Done)
			assert.Equal(t, winservice.ServiceName, j.EnsuredService)
			assert.Equal(t, winservice.ServiceName, d.connectedService())
			d.boot()
			d.runMigratorAfterBoot()
			assert.False(t, d.serviceExists(winservice.MigrateServiceName))
			assert.False(t, d.serviceExists(winservice.LegacyServiceName))
			assert.Equal(t, winservice.ServiceName, d.connectedService())
		})
	}
}

// After done only the new service is ensured: a fallback to the legacy
// service would re-create it from the journal — in place, a second service
// running from the same folder on a fresh database. A failed ensure is left
// to the migrate service's recovery.
func TestAfterDoneOnlyTheNewServiceIsEnsured(t *testing.T) {
	dir := inPlaceDir(t)
	d := newDeviceIn(t, dir)
	e := d.env()
	d.stage(e)
	require.NoError(t, d.run(e))
	j := d.journal()
	require.True(t, j.Done)
	require.False(t, d.serviceExists(winservice.LegacyServiceName))
	j.EnsuredService = ""
	require.NoError(t, e.saveJournal(j))
	d.powerCut()
	d.sys.failOn["start "+winservice.ServiceName] = errors.New("the service database is locked")
	require.NoError(t, d.sys.startService(winservice.MigrateServiceName))
	require.NoError(t, d.run(e))
	j = d.journal()
	assert.Equal(t, "", j.EnsuredService)
	assert.Contains(t, j.EnsureError, "database is locked")
	assert.False(t, d.serviceExists(winservice.LegacyServiceName), "no second service")
	assert.False(t, d.exists(filepath.Join(dir, "reagent.exe")), "no second executable")
	assert.Equal(t, uint32(1), exitCodeFor(e, nil), "retried by the recovery action")

	delete(d.sys.failOn, "start "+winservice.ServiceName)
	require.NoError(t, d.sys.startService(winservice.MigrateServiceName))
	require.NoError(t, d.run(e))
	assert.Equal(t, winservice.ServiceName, d.journal().EnsuredService)
	assert.Equal(t, winservice.ServiceName, d.connectedService())
	assert.Equal(t, uint32(0), exitCodeFor(e, nil))
}
