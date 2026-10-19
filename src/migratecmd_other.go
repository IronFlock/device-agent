//go:build !linux

package main

import (
	"fmt"
	"os"
	"reagent/config"
)

// runMigrateCommand handles `device-agent migrate-layout`; Linux only.
func runMigrateCommand(args []string) int {
	fmt.Fprintln(os.Stderr, "the 'migrate-layout' subcommand is only supported on Linux")
	return 1
}

// watchLayoutMigration moves legacy Linux installs; nothing to do here.
func watchLayoutMigration(cliArgs *config.CommandLineArguments, connected func() bool) {}
