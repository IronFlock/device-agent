//go:build linux

package migratestate

import "reagent/migrate"

// InProgress reports whether the Linux layout migration (package migrate)
// has started and is neither done nor rolled back.
func InProgress() bool { return migrate.InProgress("") }

// LastError is the error of a failed Linux layout migration.
func LastError() string { return migrate.LastError("") }
