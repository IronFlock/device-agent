//go:build linux

package main

import (
	"context"
	"os"
	"os/exec"
	"reagent/linuxservice"
	"reagent/safe"
	"time"

	"github.com/rs/zerolog/log"
)

// oomScoreAdj keeps the OOM killer away from the agent without exempting it:
// an agent that leaks memory is still killed last, and systemd restarts it.
const oomScoreAdj = "-900"

// daemonReloadTimeout bounds the reload: systemctl itself waits up to ten
// minutes on a busy systemd.
const daemonReloadTimeout = time.Minute

// applyResourceProtection keeps the agent reachable when apps use up the
// device's CPU or memory (docs/AGENT-RESOURCE-PROTECTION.md). Under one of
// its units it lowers its own OOM score at once and writes the slice and
// drop-in that move the unit into ironflock.slice at its next start. Nothing
// here is fatal, and nothing here holds up the start: a device without these
// settings is the device of today.
func applyResourceProtection() {
	unit := linuxservice.ReadCurrentUnit()
	if !linuxservice.IsAgentUnit(unit) {
		return
	}

	if err := os.WriteFile("/proc/self/oom_score_adj", []byte(oomScoreAdj), 0o600); err != nil {
		log.Warn().Err(err).Msg("could not lower the agent's OOM score")
	}

	// The files take effect only at the unit's next start, so the start goes
	// on to the connection while they are written and systemd reloads.
	safe.Go(func() { writeResourceSettings(unit) })
}

func writeResourceSettings(unit string) {
	changed, err := linuxservice.WriteResourceFiles("", unit, !linuxservice.ReadMemoryRecursiveProt())
	if err != nil {
		log.Warn().Err(err).Msg("could not write the agent's resource settings")
		return
	}
	if !changed {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), daemonReloadTimeout)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "systemctl", "daemon-reload").CombinedOutput(); err != nil {
		log.Warn().Err(err).Str("output", string(out)).Msg("resource settings written, but systemd did not reload them; they apply after the next reload")
		return
	}
	log.Info().Msgf("resource settings of %s written; they apply at its next start", unit)
}
