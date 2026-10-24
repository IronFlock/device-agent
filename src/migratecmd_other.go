//go:build !linux && !windows

package main

import (
	"fmt"
	"os"
	"reagent/config"
)

// runMigrateCommand handles `device-agent migrate-layout`; Linux and Windows only.
func runMigrateCommand(args []string) int {
	fmt.Fprintln(os.Stderr, "the 'migrate-layout' subcommand is only supported on Linux and Windows")
	return 1
}

// watchLayoutMigration moves legacy Linux and Windows installs; nothing to do here.
func watchLayoutMigration(cliArgs *config.CommandLineArguments, connected func() bool) {}
