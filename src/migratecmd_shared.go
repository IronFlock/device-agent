//go:build linux || windows

package main

import (
	"reagent/config"
	"time"
)

// What the Linux (migratecmd_linux.go) and the Windows
// (migratecmd_windows.go) layout migrations share of their trigger.
const (
	// migrationFirstCheck: the new version first proves itself in the legacy
	// layout (the legacy manager's probation) before it moves.
	migrationFirstCheck = 5 * time.Minute
	// migrationSwitch holds the fleet-wide switches next to the update
	// manifest: {"enabled": bool, "windows": bool}. Linux reads "enabled"
	// (absent means on), Windows "windows" (absent means off). It can only
	// hold devices back, so it is not signed.
	migrationSwitch = "device-agent/migration.json"
)

// updateBase is where the agent updates from: the .flock's update_url (an
// appliance mirror), else -remoteUpdateURL. The switch lives next to the
// manifest there.
func updateBase(cliArgs *config.CommandLineArguments) string {
	if flock, err := config.LoadReswarmConfig(cliArgs.ConfigFileLocation); err == nil && flock.UpdateURL != "" {
		return flock.UpdateURL
	}
	return cliArgs.RemoteUpdateURL
}
