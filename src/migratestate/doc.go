// Package migratestate tells the running agent about the layout migration of
// its OS — the Linux one in package migrate, the Windows one in package
// migrate/winmigrate — for the heartbeat stats (layout_error) and the
// self-update activation interlock, without either caller choosing a
// platform's journal path itself.
package migratestate
