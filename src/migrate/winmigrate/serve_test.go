package winmigrate

import (
	"errors"
	"fmt"
	"reagent/winservice"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeClock makes After fire at once for the named duration and never for
// the others.
func fireOnly(d time.Duration) func(time.Duration) <-chan time.Time {
	return func(wait time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		if wait == d {
			ch <- time.Time{}
		}
		return ch
	}
}

type exitRecorder struct{ codes []int }

func (r *exitRecorder) exit(code int) { r.codes = append(r.codes, code) }

// A migration that reaches its end (done here) exits with 0; the service
// control manager does not start the service again.
func TestServeExitsCleanlyAfterAResult(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	exits := &exitRecorder{}
	code := Serve(e, ServeOptions{Exit: exits.exit, After: fireOnly(0)})
	assert.Equal(t, uint32(0), code)
	assert.Empty(t, exits.codes)
	assert.True(t, d.journal().Done)
}

// A rolled-back migration is final too.
func TestServeExitsCleanlyAfterARollback(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	e.Fault = "health"
	code := Serve(e, ServeOptions{Exit: (&exitRecorder{}).exit, After: fireOnly(0)})
	assert.Equal(t, uint32(0), code)
	assert.True(t, d.journal().Failed())
}

// The watchdog: a migration that hangs (the test hook's pause, a stuck
// command) makes the process exit with 1 without reporting stopped, so the
// service's recovery starts it again and the run is counted (maxRuns).
func TestServeWatchdogExitsForARestart(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	release := make(chan struct{})
	defer close(release)
	exits := &exitRecorder{}
	code := Serve(e, ServeOptions{
		Run:   func(Env) error { <-release; return nil },
		Exit:  exits.exit,
		After: fireOnly(DefaultWatchdog),
	})
	assert.Equal(t, uint32(1), code)
	assert.Equal(t, []int{1}, exits.codes)
}

// A stop request (sc stop) sets the cancel flag; the engine returns between
// two operations and the process exits cleanly, the journal in progress for
// the next start.
func TestServeStopCancelsAndExitsCleanly(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	stop := make(chan struct{})
	close(stop)
	exits := &exitRecorder{}
	code := Serve(e, ServeOptions{
		Stop: stop,
		Run: func(e Env) error {
			for !e.cancelled() {
				time.Sleep(time.Millisecond)
			}
			return ErrCancelled
		},
		Exit:  exits.exit,
		After: fireOnly(0),
	})
	assert.Equal(t, uint32(0), code)
	assert.Empty(t, exits.codes)
	assert.True(t, d.journal().InProgressState())
}

// A stop request that lands while no agent service runs (after stop-legacy,
// before start-agent: an operator's sc stop, a management tool): the
// process asks for a restart, so the recovery action resumes the migration
// in 30 s and a headless device does not wait for its next boot.
func TestServeStopWithoutARunningAgentAsksForARestart(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	stop := make(chan struct{})
	var once sync.Once
	e.Logf = func(format string, args ...any) {
		if fmt.Sprintf(format, args...) == "migrate: stop-apps" { // stop-legacy is done
			once.Do(func() { close(stop) })
		}
	}
	code := Serve(e, ServeOptions{Stop: stop, Exit: (&exitRecorder{}).exit, After: fireOnly(0)})
	j := d.journal()
	require.True(t, j.InProgressState())
	require.Equal(t, "", d.connectedService(), "no agent runs after stop-legacy")
	require.False(t, d.serviceRunning(winservice.LegacyServiceName))
	assert.Equal(t, uint32(1), code)

	// the recovery action starts the migrate service again; it resumes
	d.migratorExits()
	require.NoError(t, d.sys.startService(winservice.MigrateServiceName))
	require.NoError(t, d.run(e))
	assert.True(t, d.journal().Done)
	assert.Equal(t, winservice.ServiceName, d.connectedService())
}

// An operation that does not finish within the stop's grace period: the
// process exits anyway (like a power cut, which the journal survives).
func TestServeStopGraceExits(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	stop := make(chan struct{})
	close(stop)
	release := make(chan struct{})
	defer close(release)
	exits := &exitRecorder{}
	code := Serve(e, ServeOptions{
		Stop:  stop,
		Run:   func(Env) error { <-release; return nil },
		Exit:  exits.exit,
		After: fireOnly(DefaultStopGrace),
	})
	assert.Equal(t, uint32(0), code)
	assert.Equal(t, []int{0}, exits.codes)
}

// A run that returns while the journal is still in progress (nothing the
// engine does today, a safety net) asks for a restart; after a result it
// does not.
func TestServeAsksForARestartWhileInProgress(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	stub := func(Env) error { return errors.New("gave up") }
	code := Serve(e, ServeOptions{Run: stub, Exit: (&exitRecorder{}).exit, After: fireOnly(0)})
	assert.Equal(t, uint32(1), code, "in progress")

	require.NoError(t, d.run(e))
	require.True(t, d.journal().Done)
	code = Serve(e, ServeOptions{Run: stub, Exit: (&exitRecorder{}).exit, After: fireOnly(0)})
	assert.Equal(t, uint32(0), code, "done")
}
