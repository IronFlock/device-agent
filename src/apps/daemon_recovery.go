package apps

import (
	"context"
	"reagent/safe"
	"time"

	"github.com/rs/zerolog/log"
)

// This file keeps the app-state machinery alive across Docker daemon outages
// (a dockerd restart or upgrade, Docker Desktop stopping at user logout, a
// crashed daemon coming back). The container-event stream is the signal: the
// docker client delivers exactly one error per Events stream and it is up to
// the caller to reopen it, so a dying stream is both the outage detector and
// the point where recovery must be driven. Without this supervision the
// spawner died permanently on the first daemon outage and nothing ever
// reconciled the app states once the daemon returned — app containers (which
// run with RestartPolicy "no") stayed stopped while the agent kept reporting
// them RUNNING until the next WAMP reconnect or agent restart.
var (
	// daemonProbeInterval/daemonProbeTimeout pace awaitDaemonReturn's pings.
	// Deliberately NOT WaitForDaemon: that helper pings at 100ms and logs a
	// debug line per failed attempt — over an hours-long outage (Docker
	// Desktop stopped at user logout) that is ten log lines per second.
	daemonProbeInterval = time.Second * 2
	daemonProbeTimeout  = time.Second * 3
	// spawnerRestartBackoff spaces consecutive spawner restarts so a daemon
	// that answers pings but immediately kills the event stream cannot spin
	// the supervisor loop hot. It escalates across rapid cycles (see
	// nextSpawnerDelay) so a crash-looping dockerd cannot re-drive every app
	// — and reset every crashloop backoff — each ~30s forever, and resets
	// once a stream stays up for spawnerStableReset. Vars, not consts, so
	// tests can shorten them.
	spawnerRestartBackoff    = time.Second * 5
	spawnerRestartBackoffMax = time.Minute * 5
	spawnerStableReset       = time.Minute * 5
)

// nextSpawnerDelay returns the pause before the next spawner restart. A
// stream that stayed up for spawnerStableReset proves the daemon was genuinely
// healthy, so the delay resets to the base; anything shorter is a rapid
// outage cycle and doubles the delay up to the cap.
func nextSpawnerDelay(prev time.Duration, streamUptime time.Duration) time.Duration {
	if streamUptime >= spawnerStableReset {
		return spawnerRestartBackoff
	}
	next := prev * 2
	if next > spawnerRestartBackoffMax {
		next = spawnerRestartBackoffMax
	}
	return next
}

// SetDaemonTransitionFunc wires the immediate device-status re-publish the
// supervisor pokes on daemon-availability transitions. Called once by the
// agent after the WAMP session exists.
func (so *StateObserver) SetDaemonTransitionFunc(fn func()) {
	so.daemonTransitionMu.Lock()
	so.daemonTransitionFn = fn
	so.daemonTransitionMu.Unlock()
}

// fireDaemonTransition pokes the wired status re-publish, if any. The status
// payload probes daemon health itself, so firing on a false alarm (a stream
// hiccup while the daemon is fine) still publishes the truth.
func (so *StateObserver) fireDaemonTransition() {
	so.daemonTransitionMu.Lock()
	fn := so.daemonTransitionFn
	so.daemonTransitionMu.Unlock()

	if fn != nil {
		fn()
	}
}

// superviseObserverSpawner owns the container-event spawner for the lifetime
// of the agent: it (re)starts the spawner, waits for the stream to die, waits
// out the daemon outage that killed it, and reconciles everything the outage
// broke once the daemon is back. Started exactly once, by ObserveAppStates.
func (so *StateObserver) superviseObserverSpawner() {
	safe.Go(func() {
		recovering := false
		delay := spawnerRestartBackoff
		for {
			// (Re)open the event stream FIRST, reconcile after: containers
			// started by the reconcile (EnsureLocalRequestedStates) emit
			// start events that must be seen so their observers are created.
			spawnerErrC := so.initObserverSpawner()
			streamStart := time.Now()

			if recovering {
				so.reconcileAfterDaemonRecovery()
			}

			err := <-spawnerErrC
			log.Warn().Err(err).Msg("container event stream lost; waiting for the Docker daemon to come back...")
			// Push the (probably now degraded) daemon health to the UI right
			// away rather than up to a heartbeat later.
			so.fireDaemonTransition()

			so.awaitDaemonReturn()
			recovering = true
			// And clear the badge as soon as the daemon answers again.
			so.fireDaemonTransition()

			delay = nextSpawnerDelay(delay, time.Since(streamStart))
			time.Sleep(delay)
		}
	})
}

// awaitDaemonReturn blocks until the Docker daemon answers pings again,
// probing at daemonProbeInterval with a "still waiting" line roughly every
// 30s (never per attempt — see daemonProbeInterval).
func (so *StateObserver) awaitDaemonReturn() {
	attempts := 0
	for {
		probeCtx, cancelProbe := context.WithTimeout(context.Background(), daemonProbeTimeout)
		_, err := so.Container.Ping(probeCtx)
		cancelProbe()
		if err == nil {
			return
		}

		attempts++
		if attempts%15 == 0 {
			log.Info().Msgf("still waiting for the Docker daemon to come back (%d probes)...", attempts)
		}
		time.Sleep(daemonProbeInterval)
	}
}

// reconcileAfterDaemonRecovery brings the device back in line after a daemon
// outage. A daemon stop kills every app container (RestartPolicy is "no", so
// nothing restarts them), tears down the per-app observers of that era, and
// severs the app log streams; events that fired between daemon return and the
// stream reopening are gone as well. So: correct the recorded states against
// what actually survived, recreate observers, then re-drive every app to its
// requested state. Each step logs and continues — one failing step must not
// cost the rest, and the next WAMP reconnect reconciles remotely anyway.
func (so *StateObserver) reconcileAfterDaemonRecovery() {
	log.Info().Msg("Docker daemon is available again; reconciling app states...")

	// Compose support may have been probed (and latched negative) while the
	// daemon/CLI was unavailable — same rationale as the late-daemon startup
	// path in NewAgent. A positive latch stays valid across daemon restarts.
	if compose := so.Container.Compose(); compose != nil && !compose.Supported() {
		compose.RefreshSupport()
	}

	if err := so.CorrectAppStates(true); err != nil {
		log.Error().Stack().Err(err).Msg("daemon recovery: failed to correct app states")
	}

	if err := so.ObserveAppStates(); err != nil {
		log.Error().Stack().Err(err).Msg("daemon recovery: failed to re-init app state observers")
	}

	if so.AppManager != nil {
		if err := so.AppManager.CleanupOrphanedContainers(); err != nil {
			log.Error().Stack().Err(err).Msg("daemon recovery: failed to clean up orphaned containers")
		}

		if err := so.AppManager.EnsureLocalRequestedStates(); err != nil {
			log.Error().Stack().Err(err).Msg("daemon recovery: failed to ensure requested app states")
		}
	}

	// App log streams died with the daemon; reattach the ones that still have
	// remote subscribers.
	if so.LogManager != nil {
		if err := so.LogManager.ReviveDeadLogs(); err != nil {
			log.Error().Stack().Err(err).Msg("daemon recovery: failed to revive app log streams")
		}
	}
}
