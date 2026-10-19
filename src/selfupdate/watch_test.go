package selfupdate

import (
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fleetDevice runs the state machine the way the service does: binaries are
// files whose content is their version, a "process" is OnServiceStart plus
// Watch steps, and behaviour says what a version does once started.
type fleetDevice struct {
	t   *testing.T
	m   *Manager
	now time.Time
	// behaviour of each version: crashes at start, connects, gets ready.
	crashes   map[string]bool
	noConnect map[string]bool
	notReady  map[string]bool
	// network: false is an outage (nothing connects).
	network bool
	// restarts counts process starts.
	restarts int
	// proc is the running process (nil: none yet, or it just exited).
	proc *watcher
}

func newFleetDevice(t *testing.T, version string) *fleetDevice {
	m := newTestManagerWith(t, LinuxNames)
	require.NoError(t, os.WriteFile(m.activePath(), []byte(version), 0o755))
	d := &fleetDevice{t: t, m: m, now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC),
		crashes: map[string]bool{}, noConnect: map[string]bool{}, notReady: map[string]bool{}, network: true}
	m.now = func() time.Time { return d.now }
	return d
}

func (d *fleetDevice) running() string { return fileContent(d.t, d.m.activePath()) }

// release publishes version and has the running agent activate it, as the
// update check does.
func (d *fleetDevice) release(version string) error {
	writeDownload(d.t, d.m, version)
	if d.m.IsVersionBlocked(version) {
		return ErrVersionBlocked
	}
	if err := d.m.Activate(version, d.running()); err != nil {
		return err
	}
	d.restart()
	return nil
}

// runFor runs the device for duration, continuing the running process: a
// start goes through OnServiceStart (a rollback restarts at once), a
// crashing version dies right away, the others are watched every 5 s until
// they exit for a restart (a rollback).
func (d *fleetDevice) runFor(duration time.Duration) {
	end := d.now.Add(duration)
	for d.now.Before(end) {
		if d.proc == nil {
			d.restarts++
			version := d.running()
			if d.m.OnServiceStart(version).RollbackPerformed {
				continue
			}
			if d.crashes[version] {
				d.now = d.now.Add(5 * time.Second) // systemd's RestartSec
				continue
			}
			d.proc = &watcher{m: d.m, start: d.now, cfg: WatchConfig{
				Version:       version,
				Ready:         func() bool { return !d.notReady[version] },
				Connected:     func() bool { return d.network && !d.noConnect[version] },
				HealthyUptime: DefaultHealthyUptime,
				Deadline:      DefaultDeadline,
			}}
		}
		if reason := d.proc.step(d.now); reason != "" {
			d.proc = nil
			continue
		}
		d.now = d.now.Add(5 * time.Second)
	}
}

// restart is what the update's restart request does: the process exits.
func (d *fleetDevice) restart() { d.proc = nil }

func (d *fleetDevice) phase() phase {
	marker, err := d.m.readMarker()
	require.NoError(d.t, err)
	if marker == nil {
		return ""
	}
	return marker.Phase
}

func TestAGoodUpdatePassesOnceItConnects(t *testing.T) {
	d := newFleetDevice(t, "1.3.0")
	require.NoError(t, d.release("1.3.1"))
	d.runFor(time.Minute)
	assert.Equal(t, phaseProbation, d.phase(), "connected, but not up long enough")
	d.runFor(2 * time.Minute)
	assert.Equal(t, "1.3.1", d.running())
	assert.Equal(t, phase(""), d.phase())
	assert.Equal(t, "1.3.0", fileContent(t, d.m.prevPath()), "the proven version stays the rollback target")
	_, ok := d.m.LastConnected()
	assert.True(t, ok)
}

// failsTwice offers version twice (the device runs each attempt for
// window) and checks that both end on old, the first leaving the release a
// second try, the second blocking it.
func failsTwice(t *testing.T, d *fleetDevice, version, old string, window time.Duration) {
	t.Helper()
	require.NoError(t, d.release(version))
	d.runFor(window)
	require.Equal(t, old, d.running())
	require.Equal(t, phaseStruck, d.phase(), "one failure: tried once more")
	require.False(t, d.m.IsVersionBlocked(version))

	require.NoError(t, d.release(version), "the next update (a start, or a user's) tries it again")
	d.runFor(window)
	require.Equal(t, old, d.running())
	require.Equal(t, phaseRolledBack, d.phase(), "a second failure while the old one connects: blocked")
	require.ErrorIs(t, d.release(version), ErrVersionBlocked)
}

func TestACrashingUpdateRollsBackAndTheOldVersionConnects(t *testing.T) {
	d := newFleetDevice(t, "1.3.0")
	d.crashes["1.3.1"] = true
	failsTwice(t, d, "1.3.1", "1.3.0", 15*time.Minute)
}

func TestAnUpdateThatNeverConnectsRollsBack(t *testing.T) {
	d := newFleetDevice(t, "1.3.0")
	d.noConnect["1.3.1"] = true
	require.NoError(t, d.release("1.3.1"))
	d.runFor(9 * time.Minute)
	assert.Equal(t, "1.3.1", d.running(), "still within its deadline")
	d.runFor(6 * time.Minute)
	assert.Equal(t, "1.3.0", d.running())
	assert.Equal(t, phaseStruck, d.phase())

	e := newFleetDevice(t, "1.3.0")
	e.noConnect["1.3.1"] = true
	failsTwice(t, e, "1.3.1", "1.3.0", 15*time.Minute)
}

func TestAnUpdateThatNeverGetsReadyRollsBack(t *testing.T) {
	d := newFleetDevice(t, "1.3.0")
	d.notReady["1.3.1"] = true
	failsTwice(t, d, "1.3.1", "1.3.0", 15*time.Minute)
}

// An outage during an update: the new version cannot connect, neither can the
// old one after the rollback. That proves nothing against the new version: it
// may be tried again, and passes once the network is back.
func TestAnOutageDoesNotBlockAGoodUpdate(t *testing.T) {
	d := newFleetDevice(t, "1.3.0")
	d.network = false
	require.NoError(t, d.release("1.3.1"))
	d.runFor(12 * time.Minute)
	assert.Equal(t, "1.3.0", d.running(), "rolled back: it did not connect")
	assert.Equal(t, phaseSuspect, d.phase())
	d.runFor(12 * time.Minute)
	assert.Equal(t, phase(""), d.phase(), "the old one did not connect either: not the new version's fault")
	assert.False(t, d.m.IsVersionBlocked("1.3.1"))

	d.network = true
	require.NoError(t, d.release("1.3.1"))
	d.runFor(5 * time.Minute)
	assert.Equal(t, "1.3.1", d.running())
	assert.Equal(t, phase(""), d.phase())
}

// Two broken releases one after the other, broken differently: the device
// stays on the version that connects, both end blocked, a good third one
// goes through.
func TestTwoBrokenReleasesInARow(t *testing.T) {
	d := newFleetDevice(t, "1.3.0")
	d.crashes["1.3.1"] = true
	d.noConnect["1.3.2"] = true
	failsTwice(t, d, "1.3.1", "1.3.0", 15*time.Minute)
	failsTwice(t, d, "1.3.2", "1.3.0", 25*time.Minute)

	require.NoError(t, d.release("1.3.3"))
	d.runFor(5 * time.Minute)
	assert.Equal(t, "1.3.3", d.running())
	assert.Equal(t, phase(""), d.phase())
	assert.Equal(t, "1.3.0", fileContent(t, d.m.prevPath()))
}

// An outage that ends while the old version is being judged — the new
// version started just before it and could not connect, the old one can:
// one failure, so it is tried again (and passes) rather than blocked.
func TestAnOutageEndingDuringTheJudgementDoesNotBlock(t *testing.T) {
	d := newFleetDevice(t, "1.3.0")
	d.network = false
	require.NoError(t, d.release("1.3.1"))
	d.runFor(10*time.Minute + 30*time.Second)
	require.Equal(t, "1.3.0", d.running())
	d.network = true
	d.runFor(5 * time.Minute)
	assert.Equal(t, phaseStruck, d.phase())
	assert.False(t, d.m.IsVersionBlocked("1.3.1"))
	require.NoError(t, d.release("1.3.1"))
	d.runFor(5 * time.Minute)
	assert.Equal(t, "1.3.1", d.running())
	assert.Equal(t, phase(""), d.phase())
}

// The old version's session comes up but it does not stay (a backend outage
// failing its start after the session is up): that is no proof against the
// new one.
func TestAJudgeThatDoesNotStayUpProvesNothing(t *testing.T) {
	d := newFleetDevice(t, "1.3.0")
	d.noConnect["1.3.1"] = true
	require.NoError(t, d.release("1.3.1"))
	d.runFor(10*time.Minute + 30*time.Second)
	require.Equal(t, phaseSuspect, d.phase())
	for i := 0; i < 3; i++ { // connected, but restarted every minute
		d.runFor(time.Minute)
		d.restart()
	}
	assert.Equal(t, phaseSuspect, d.phase(), "no verdict from a version that does not stay up")
}

// The old version, back after a rollback, keeps dying before it proves
// anything: the device alternates between the two until one connects,
// never stopping on a binary that cannot.
func TestTheDeviceAlternatesUntilAVersionConnects(t *testing.T) {
	d := newFleetDevice(t, "1.3.0")
	d.noConnect["1.3.1"] = true
	require.NoError(t, d.release("1.3.1"))
	d.runFor(10*time.Minute + 10*time.Second)
	require.Equal(t, "1.3.0", d.running())
	require.Equal(t, phaseSuspect, d.phase())
	d.crashes["1.3.0"] = true // e.g. 1.3.1 changed something 1.3.0 cannot read
	d.restart()
	d.runFor(time.Minute)
	assert.Equal(t, "1.3.1", d.running(), "the old one does not run either: the new one again")
	assert.Equal(t, phaseProbation, d.phase())

	d.noConnect["1.3.1"] = false // e.g. the network is back
	d.runFor(5 * time.Minute)
	assert.Equal(t, "1.3.1", d.running())
	assert.Equal(t, phase(""), d.phase())
}

// Two activations at once (the periodic check and a platform request) must
// not lose the rollback target.
func TestConcurrentActivationsKeepTheRollbackTarget(t *testing.T) {
	d := newFleetDevice(t, "1.3.0")
	writeDownload(t, d.m, "1.3.1")
	inProbe := make(chan struct{})
	release := make(chan struct{})
	probe := d.m.execVersion
	var once sync.Once
	d.m.execVersion = func(path string) (string, error) {
		blocked := false
		once.Do(func() { blocked = true })
		if blocked {
			close(inProbe)
			<-release
		}
		return probe(path)
	}
	first := make(chan error, 1)
	go func() { first <- d.m.Activate("1.3.1", "1.3.0") }()
	<-inProbe
	second := make(chan error, 1)
	go func() { second <- d.m.Activate("1.3.1", "1.3.0") }()
	time.Sleep(50 * time.Millisecond)
	close(release)
	require.NoError(t, <-first)
	assert.Error(t, <-second, "the second one waits, then finds the first one's probation")
	assert.Equal(t, "1.3.0", fileContent(t, d.m.prevPath()))
	assert.Equal(t, "1.3.1", fileContent(t, d.m.activePath()))
	assert.Equal(t, phaseProbation, d.phase())
}

// A board whose clock is behind the rollback's stamp (no RTC, a stale saved
// clock): the judging process's own uptime still ends the wait.
func TestAJudgeWithTheClockBehindTheStamp(t *testing.T) {
	d := newFleetDevice(t, "1.3.0")
	d.noConnect["1.3.1"] = true
	d.network = false
	require.NoError(t, d.release("1.3.1"))
	d.runFor(10*time.Minute + 30*time.Second)
	require.Equal(t, phaseSuspect, d.phase())
	d.now = d.now.Add(-time.Hour) // the clock went back at a reboot
	d.restart()
	d.runFor(11 * time.Minute)
	assert.Equal(t, phase(""), d.phase())
}

func TestNoActivationDuringProbationOrForASuspect(t *testing.T) {
	d := newFleetDevice(t, "1.3.0")
	d.noConnect["1.3.1"] = true
	require.NoError(t, d.release("1.3.1"))
	d.runFor(time.Minute)
	assert.Error(t, d.release("1.3.2"), "1.3.1 is in probation")
	d.network = false // so the old version cannot settle it at once
	d.runFor(10 * time.Minute)
	require.Equal(t, phaseSuspect, d.phase())
	assert.ErrorIs(t, d.release("1.3.1"), ErrVersionBlocked, "not again while it is suspect")
}

func TestActivationHold(t *testing.T) {
	d := newFleetDevice(t, "1.3.0")
	assert.Empty(t, d.m.ActivationHold(d.now), "never connected: nothing to lose")
	d.m.RecordConnected(d.now)
	assert.Empty(t, d.m.ActivationHold(d.now.Add(time.Minute)), "connected")
	assert.NotEmpty(t, d.m.ActivationHold(d.now.Add(time.Hour)), "away an hour: wait until connected")
	assert.Empty(t, d.m.ActivationHold(d.now.Add(rescueAfter)), "away a day: try the newer version")
}
