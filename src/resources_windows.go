//go:build windows

package main

import (
	"github.com/rs/zerolog/log"
	"golang.org/x/sys/windows"
)

// applyResourceProtection keeps the agent responsive when apps load the
// machine: above normal priority, not high, which could starve Windows' own
// services. Child processes start at normal priority unless they ask for
// more (frpc does, tunnel/priority_windows.go); the terminal shell stays
// normal.
func applyResourceProtection() {
	err := windows.SetPriorityClass(windows.CurrentProcess(), windows.ABOVE_NORMAL_PRIORITY_CLASS)
	if err != nil {
		log.Warn().Err(err).Msg("could not raise the agent's priority class")
	}
}
