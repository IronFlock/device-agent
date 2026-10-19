package migrate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reagent/layout"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The cases of the pre-release review of the migration.

// A config copied in as a file (no link into /boot) is the only copy of the
// device secret: a rollback must keep it, and a migration makes it 0600.
func TestMigrateKeepsAConfigThatIsAFile(t *testing.T) {
	setup := func(t *testing.T) *device {
		d := newDevice(t)
		require.NoError(t, os.Remove(d.p("/opt/reagent/device-config.flock")))
		require.NoError(t, os.Remove(d.p("/boot/demo_swarm.flock")))
		d.file("/opt/reagent/device-config.flock", `{"secret":"only-copy"}`, 0o644)
		return d
	}
	for _, name := range StepNames() {
		t.Run("rollback at "+name, func(t *testing.T) {
			d := setup(t)
			before := d.snapshot()
			e := d.env()
			d.stage(e)
			e.Fault = name
			require.Error(t, Run(e))
			assert.Equal(t, before, d.snapshot())
		})
	}
	t.Run("migrated", func(t *testing.T) {
		d := setup(t)
		e := d.env()
		d.stage(e)
		require.NoError(t, Run(e))
		assert.Equal(t, `-rw------- {"secret":"only-copy"}`, d.snapshot()[layout.AgentDir+"/device-config.flock"])
	})
}

// Docker's data-root on a partition of its own stays mounted when Docker
// stops: such a device moves by link, without stopping Docker.
func TestADataRootOnItsOwnPartitionMovesByLink(t *testing.T) {
	d := newDevice(t)
	d.mounts = []string{"/", "/boot", "/opt/reagent/docker-apps", "/opt/reagent/docker-apps/overlay2/abc/merged"}
	e := d.env()
	d.stage(e)
	require.NoError(t, Run(e))
	j := d.journal()
	assert.Equal(t, StrategyLink, j.Strategy)
	assert.Contains(t, j.StrategyReason, "/opt/reagent/docker-apps is a mount point")
	assert.Equal(t, -1, index(d.calls, "stop docker.service"))
	assert.Equal(t, layout.Current, layout.Detect(d.root).Kind)
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
		require.Error(t, Run(e))
		assert.Equal(t, attempt, d.journal().Attempts)
		due, err = Due(e)
		require.NoError(t, err)
		require.False(t, due, "not again the same day")
		d.now = d.now.Add(retryAfterFailure)
	}
	assert.Contains(t, LastError(d.root), "attempt 10; retried at the agent's next start")
}

// A power cut in the middle of a rollback: the boot continues the rollback,
// even when the fault that started it is gone by then (it never runs the
// migration forward over half-undone steps).
func TestMigrateContinuesARollbackAfterAPowerCut(t *testing.T) {
	for _, fault := range []string{"move-folder", "start-agent", "health"} {
		probe := newDevice(t)
		pe := probe.env()
		probe.stage(pe)
		rollbackFrom := -1
		pe.Logf = func(format string, args ...any) {
			if rollbackFrom < 0 && strings.Contains(fmt.Sprintf(format, args...), "rolling back") {
				rollbackFrom = len(probe.calls)
			}
		}
		pe.Fault = fault
		require.Error(t, Run(pe))
		require.GreaterOrEqual(t, rollbackFrom, 0)
		for n := rollbackFrom + 1; n <= len(probe.calls); n++ {
			t.Run(fmt.Sprintf("%s call %d %s", fault, n, probe.calls[n-1]), func(t *testing.T) {
				d := newDevice(t)
				before := d.snapshot()
				e := d.env()
				d.stage(e)
				e.Fault = fault
				d.crashAt = n
				require.PanicsWithValue(t, errCrash, func() { _ = Run(e) })
				d.crashAt = 0
				d.agentPID = 0
				_ = Run(d.env()) // the boot, without the fault
				require.True(t, d.journal().Failed())
				assert.Equal(t, before, d.snapshot())
			})
		}
	}
}

// A process that keeps dying is not restarted forever: the attempt is rolled
// back after maxRuns.
func TestMigrateRollsBackAfterTooManyRuns(t *testing.T) {
	d := newDevice(t)
	before := d.snapshot()
	e := d.env()
	d.stage(e)
	e.Pause = "move-folder"
	e.Sleep = func(dur time.Duration) {
		if dur == time.Hour {
			panic(errCrash)
		}
		d.now = d.now.Add(dur)
	}
	require.PanicsWithValue(t, errCrash, func() { _ = Run(e) })
	j := d.journal()
	j.Runs = maxRuns
	require.NoError(t, d.env().saveJournal(j))

	err := Run(d.env())
	require.ErrorContains(t, err, "stopped 5 times")
	assert.Equal(t, before, d.snapshot())
}

// The boot started the new unit (enabled, crash before the journal said so)
// before the migration got to start-agent: its connected file is kept.
func TestStartAgentKeepsTheReadyFileOfARunningAgent(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	e.Pause = "unblock-docker"
	e.Sleep = func(dur time.Duration) {
		if dur == time.Hour {
			panic(errCrash)
		}
		d.now = d.now.Add(dur)
	}
	require.PanicsWithValue(t, errCrash, func() { _ = Run(e) })

	d.agentPID = 4242
	d.started = d.now
	require.NoError(t, os.MkdirAll(filepath.Dir(d.p(connectedFile)), 0o755))
	require.NoError(t, os.WriteFile(d.p(connectedFile), []byte("4242 "+testVersion+"\n"), 0o644))
	require.NoError(t, Run(d.env()))
	assert.True(t, d.journal().Done)
}

// Installs without the manager (or a Docker without docker.socket): units
// systemd does not know are left alone, and a rollback starts the agent
// itself.
func TestMigrateWithoutTheManagerOrADockerSocket(t *testing.T) {
	d := newDevice(t)
	d.notLoaded[legacyManagerUnit] = true
	d.notLoaded["docker.socket"] = true
	before := d.snapshot()
	e := d.env()
	d.stage(e)
	e.Fault = "health"
	require.Error(t, Run(e))
	assert.Equal(t, before, d.snapshot())
	assert.Equal(t, "start "+legacyAgentUnit, d.calls[len(d.calls)-2])

	e.Fault = ""
	d.now = d.now.Add(retryAfterFailure)
	d.stage(e)
	require.NoError(t, Run(e))
	assert.True(t, d.journal().Done)
}

func TestInProgress(t *testing.T) {
	d := newDevice(t)
	assert.False(t, InProgress(d.root))
	e := d.env()
	d.stage(e)
	assert.True(t, InProgress(d.root))
	require.NoError(t, Run(e))
	assert.False(t, InProgress(d.root))
}

// The cleanup deletes the device key the legacy installer extracted to /root.
func TestMigrateRemovesTheRootKeyCopy(t *testing.T) {
	d := newDevice(t)
	d.file(rootKeyCopy, "PRIVATE KEY", 0o600)
	d.file(rootPubCopy, "ssh-rsa AAA", 0o644)
	e := d.env()
	d.stage(e)
	require.NoError(t, Run(e))
	assert.NoFileExists(t, d.p(rootKeyCopy))
	assert.NoFileExists(t, d.p(rootPubCopy))
}

// dockerd died earlier and left overlay mounts behind: they are stale once
// it stopped, and detached; the move goes through.
func TestStaleDockerMountsAreDetached(t *testing.T) {
	d := newDevice(t)
	d.mountsAfterDockerStop = []string{"/", "/opt/reagent/docker-apps/overlay2/abc/merged"}
	e := d.env()
	d.stage(e)
	require.NoError(t, Run(e))
	assert.Contains(t, d.calls, "umount /opt/reagent/docker-apps/overlay2/abc/merged")
	assert.True(t, d.journal().Done)
}

// Mounts that cannot be freed (live-restore keeps containers running): the
// attempt rolls back, and the next one leaves /opt/reagent in place.
func TestAfterMountsThatStayTheNextAttemptLinks(t *testing.T) {
	d := newDevice(t)
	d.mountsAfterDockerStop = []string{"/", "/opt/reagent/docker-apps/overlay2/abc/merged"}
	e := d.env()
	e.Unmount = func(string) error { return errors.New("busy") }
	d.stage(e)
	require.ErrorContains(t, Run(e), "still mounted")
	d.now = d.now.Add(retryAfterFailure)
	d.mounts = []string{"/", "/boot", "/opt/reagent/docker-apps/overlay2/abc/merged"}
	d.stage(e)
	require.NoError(t, Run(e))
	j := d.journal()
	assert.Equal(t, StrategyLink, j.Strategy)
	assert.True(t, j.Done)
}

// A clock behind the last failure's stamp does not hold the retry off.
func TestDueWithTheClockBehind(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	e.Fault = "health"
	d.stage(e)
	require.Error(t, Run(e))
	d.now = d.now.Add(-48 * time.Hour)
	due, err := Due(e)
	require.NoError(t, err)
	assert.True(t, due)
}

// Another enabled unit still runs a script in /opt/reagent (a legacy boot
// script that relinks the config from /boot, or removes it when /boot has
// none): the /boot copies stay.
func TestBootCopiesStayWhileAUnitUsesTheLegacyFolder(t *testing.T) {
	d := newDevice(t)
	// (A unit inside reswarmify/ goes with it in the same step, so cannot.)
	d.file("/etc/systemd/system/reswarm.service", "[Service]\nExecStart=/opt/reagent/scripts/reswarm.sh\n", 0o644)
	d.link("/etc/systemd/system/multi-user.target.wants/reswarm.service", d.p("/etc/systemd/system/reswarm.service"))
	e := d.env()
	d.stage(e)
	require.NoError(t, Run(e))
	assert.FileExists(t, d.p("/boot/demo_swarm.flock"))
	assert.FileExists(t, d.p(bootDeviceINI))
	assert.FileExists(t, d.p("/boot/demo_demo_swarm_test45.flock"))
}

// A failed attempt starts Docker again even when it did not stop it (a boot
// during the switch blocked it).
func TestARollbackStartsDocker(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	e.Fault = "stop-docker"
	require.Error(t, Run(e))
	assert.Contains(t, d.calls, "start docker.service")
}
