//go:build !windows

package config

// runningAsService is a Windows question (the layout of a console run differs
// from the service's); off Windows the layout comes from the filesystem.
func runningAsService() bool {
	return false
}
