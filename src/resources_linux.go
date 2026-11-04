//go:build linux

package main

import (
	"os"
	"reagent/linuxservice"

	"github.com/rs/zerolog/log"
)

// oomScoreAdj keeps the OOM killer away from the agent without exempting it:
// an agent that leaks memory is still killed last, and systemd restarts it.
const oomScoreAdj = "-900"

// applyResourceProtection keeps the agent reachable when apps use up the
// device's CPU or memory (docs/AGENT-RESOURCE-PROTECTION.md). Under one of
// its units it lowers its own OOM score at once and writes the slice and
// drop-in that move the unit into ironflock.slice at its next start. Nothing
// here is fatal: a device without these settings is the device of today.
func applyResourceProtection() {
	unit := linuxservice.ReadCurrentUnit()
	if !linuxservice.IsAgentUnit(unit) {
		return
	}

	if err := os.WriteFile("/proc/self/oom_score_adj", []byte(oomScoreAdj), 0o644); err != nil {
		log.Warn().Err(err).Msg("could not lower the agent's OOM score")
	}

	changed, err := linuxservice.WriteResourceFiles("", unit, !linuxservice.ReadMemoryRecursiveProt())
	if err != nil {
		log.Warn().Err(err).Msg("could not write the agent's resource settings")
		return
	}
	if !changed {
		return
	}
	if err := systemctl("daemon-reload"); err != nil {
		log.Warn().Err(err).Msg("resource settings written, but systemd did not reload them")
		return
	}
	log.Info().Msgf("resource settings of %s written; they apply at its next start", unit)
}
