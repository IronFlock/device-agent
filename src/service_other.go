//go:build !windows && !linux

package main

import (
	"fmt"
	"os"
	"reagent/config"
)

// runningAsService reports whether a service manager supervises the process.
// Always false here (macOS): the agent runs as a plain console process.
func runningAsService() bool {
	return false
}

// runService hosts the agent under a service manager; unreachable here.
func runService(cliArgs *config.CommandLineArguments) {
	panic("runService is only available on Windows and Linux")
}

// runServiceCommand handles `device-agent service <verb>`; Windows and Linux only.
func runServiceCommand(args []string) int {
	fmt.Fprintln(os.Stderr, "the 'service' subcommand is only supported on Windows and Linux")
	return 1
}
