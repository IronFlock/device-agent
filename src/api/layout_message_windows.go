//go:build windows

package api

// layoutMigrationMessage tells the user in Studio what the update button
// started on a Windows device installed before agent 1.4.0 (package
// migrate/winmigrate).
const layoutMigrationMessage = "moving the agent to its new service name (ironflock-device-agent); the agent and its apps restart once"
