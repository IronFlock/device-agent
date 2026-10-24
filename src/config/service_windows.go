//go:build windows

package config

import "golang.org/x/sys/windows/svc"

// runningAsService reports whether the service control manager started this
// process: a legacy reagent.exe without one (a console run) can never move to
// the current layout by itself (layout.LegacyConsole).
func runningAsService() bool {
	isService, err := svc.IsWindowsService()
	return err == nil && isService
}
