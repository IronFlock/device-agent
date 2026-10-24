//go:build windows || linux

package system

import (
	"reagent/lifecycle"
	"reagent/migratestate"
	"reagent/safe"
	"reagent/selfupdate"
	"time"

	"github.com/rs/zerolog/log"
)

// migrationInProgress reports whether the layout migration is under way (a
// test replaces it); activationDelay is how long the update_agent response
// gets to flush before the restart.
var (
	migrationInProgress = migratestate.InProgress
	activationDelay     = 10 * time.Second
	activateUpdate      = (*selfupdate.Manager).Activate
)

// maybeActivateAgentUpdate swaps a freshly downloaded agent version into the
// active binary path and requests a supervised restart. Only active under a
// supervisor (the Windows service, ironflock-device-agent.service on Linux):
// console runs (development, FlockFlasher test devices) and legacy Linux
// installs, where reagent-manager.sh activates updates, only download.
func (system *System) maybeActivateAgentUpdate(result UpdateResult) {
	if !result.DidAgentUpdate && !result.DidUpdate {
		return
	}

	if !lifecycle.Supervised() {
		log.Info().Msgf("agent v%s downloaded but not activated here: not running under the agent's service", result.LatestVersion)
		return
	}

	// The layout migration's health check waits for THIS version to stay up
	// for a minute; activating an update meanwhile would roll the move back.
	// The next update (the agent's next start, or one a user starts)
	// activates it.
	if migrationInProgress() {
		log.Info().Msgf("agent v%s downloaded but not activated: the layout migration is still checking this version", result.LatestVersion)
		return
	}

	manager := selfupdate.New(system.config.CommandLineArguments.AgentDir, selfupdate.NamesFor(system.config.CommandLineArguments.BinaryName))
	if reason := manager.ActivationHold(time.Now()); reason != "" {
		log.Info().Msgf("agent v%s downloaded but not activated yet: %s", result.LatestVersion, reason)
		return
	}
	if manager.IsVersionBlocked(result.LatestVersion) {
		log.Warn().Msgf("agent v%s previously failed on this device, not activating it again", result.LatestVersion)
		return
	}

	// Taken now, not read by the goroutine: a test swaps them between runs.
	delay, inProgress, activate := activationDelay, migrationInProgress, activateUpdate
	safe.Go(func() {
		// Give the update_agent WAMP response and the progress publishes time
		// to flush before the process restarts (the Linux manager has a
		// comparable <=15s activation lag).
		time.Sleep(delay)

		// Checked again at the moment of the swap: the layout migration may
		// have started meanwhile (its first step renames these executables).
		if inProgress() {
			log.Info().Msgf("agent v%s downloaded but not activated: the layout migration started meanwhile", result.LatestVersion)
			return
		}

		err := activate(manager, result.LatestVersion, result.CurrentVersion)
		if err != nil {
			log.Error().Err(err).Msgf("failed to activate agent update v%s", result.LatestVersion)
			return
		}

		err = lifecycle.RequestRestart("agent update v" + result.LatestVersion)
		if err != nil {
			log.Error().Err(err).Msg("activated the agent update but failed to request a restart")
		}
	})
}
