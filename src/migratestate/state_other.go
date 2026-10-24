//go:build !linux && !windows

package migratestate

// InProgress: no layout migration exists off Linux and Windows.
func InProgress() bool { return false }

// LastError: no layout migration exists off Linux and Windows.
func LastError() string { return "" }
