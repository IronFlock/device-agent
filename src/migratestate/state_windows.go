//go:build windows

package migratestate

import "reagent/migrate/winmigrate"

// InProgress reports whether the Windows layout migration (package
// winmigrate) has started and is neither done nor rolled back.
func InProgress() bool { return winmigrate.InProgress(winmigrate.DefaultLayout()) }

// LastError is the error of a failed Windows layout migration.
func LastError() string { return winmigrate.LastError(winmigrate.DefaultLayout()) }
