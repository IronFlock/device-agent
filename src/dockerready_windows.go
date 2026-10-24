//go:build windows

package main

import (
	"reagent/migrate/winmigrate"
	"reagent/release"

	"github.com/rs/zerolog/log"
)

// afterDockerReady runs once the Docker daemon is reachable (at the agent's
// start, or at a logon long after it when Docker Desktop starts then): a done
// layout migration gives the containers their restart policies back only
// through Docker, and removes its leftovers only once it has.
func afterDockerReady() {
	env := winmigrate.SystemEnv(release.GetVersion(), func(format string, args ...any) {
		log.Info().Msgf(format, args...)
	})
	winmigrate.RemoveLeftovers(env, runningServiceName())
}
