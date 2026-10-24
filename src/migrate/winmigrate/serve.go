package winmigrate

import (
	"errors"
	"os"
	"reagent/winservice"
	"sync/atomic"
	"time"
)

// Defaults of ServeOptions.
const (
	// DefaultWatchdog: the migrate service exits (for the service control
	// manager's recovery to start it again, counted as a run) when the
	// migration has not reached a result by then.
	DefaultWatchdog = 45 * time.Minute
	// DefaultStopGrace: how long a stop request waits for the operation in
	// flight before the process exits anyway.
	DefaultStopGrace = 3 * time.Minute
)

// ServeOptions drive Serve; the zero value is the migrate service's.
type ServeOptions struct {
	// Stop is closed when the service control manager asks the service to
	// stop (or shut down).
	Stop      <-chan struct{}
	Watchdog  time.Duration
	StopGrace time.Duration
	// Run is the migration (Run); Exit ends the process (os.Exit); After is
	// time.After. Tests replace them.
	Run   func(Env) error
	Exit  func(code int)
	After func(time.Duration) <-chan time.Time
}

func (o ServeOptions) withDefaults() ServeOptions {
	if o.Watchdog == 0 {
		o.Watchdog = DefaultWatchdog
	}
	if o.StopGrace == 0 {
		o.StopGrace = DefaultStopGrace
	}
	if o.Run == nil {
		o.Run = Run
	}
	if o.Exit == nil {
		o.Exit = os.Exit
	}
	if o.After == nil {
		o.After = time.After
	}
	return o
}

// Serve is the migrate service's life: the migration runs in the background
// while this watches the clock and the stop request. It returns the
// service's exit code: 0 when the migration reached a final state (done,
// rolled back, nothing to do, both journals unreadable) or was stopped, 1
// when the journal is still in progress, so that the service control
// manager's recovery starts the service again (counted as a run, see
// maxRuns). The watchdog exits the process the same way when the migration
// hangs; a stop request sets the cancel flag the steps check between
// operations and waits StopGrace for the operation in flight.
func Serve(e Env, opts ServeOptions) uint32 {
	opts = opts.withDefaults()
	var stopped atomic.Bool
	e.Cancelled = stopped.Load

	done := make(chan error, 1)
	go func() { done <- opts.Run(e) }()

	select {
	case err := <-done:
		return exitCodeFor(e, err)
	case <-opts.After(opts.Watchdog):
		e.logf("migrate: no result after %s; exiting for the service's recovery to start it again", opts.Watchdog)
		opts.Exit(1)
		return 1
	case <-opts.Stop:
		stopped.Store(true)
		e.logf("migrate: stop requested; finishing the operation in flight")
		select {
		case err := <-done:
			return exitCodeFor(e, err)
		case <-opts.After(opts.StopGrace):
			e.logf("migrate: the operation in flight did not finish within %s; exiting (the journal resumes at the next start)", opts.StopGrace)
			opts.Exit(0)
			return 0
		}
	}
}

// exitCodeFor: a stopped or final migration ends the process cleanly; one
// still in progress asks for a restart, and so does a final one that left
// no agent service running (ensure-agent could start nothing: a transient
// error of the service control manager, a held folder), so that the
// service's recovery action tries again in 30 s rather than at the next boot.
// A final migration whose last resort is the new service running is left
// alone: it adopts the journal once it connects. A stop request (sc stop, a
// management tool) that lands while no agent service runs (between
// stop-legacy and start-agent) asks for a restart too: a headless device
// must not wait for its next boot.
func exitCodeFor(e Env, err error) uint32 {
	if errors.Is(err, ErrCancelled) {
		if e.agentServiceRunning() {
			return 0
		}
		e.logf("migrate: stopped while no agent service runs; requesting a restart")
		return 1
	}
	j, loadErr := loadJournal(e.Sys.ReadFile, e.Layout)
	if loadErr != nil || j == nil {
		return 0
	}
	switch {
	case j.InProgressState():
		e.logf("migrate: the migration is still in progress (%v); requesting a restart", err)
		return 1
	case !j.ensured():
		e.logf("migrate: no agent service is running (%s); requesting a restart", j.EnsureError)
		return 1
	}
	return 0
}

// agentServiceRunning reports whether one of the agent services runs.
func (e Env) agentServiceRunning() bool {
	for _, name := range []string{winservice.ServiceName, winservice.LegacyServiceName} {
		if status, err := e.Sys.ServiceStatus(name); err == nil && status.Running() {
			return true
		}
	}
	return false
}
