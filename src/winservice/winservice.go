// Package winservice holds what the agent's Windows service is called, the
// files it lives with and the repair scripts it starts with, and
// (scm_windows.go) writes the service. `device-agent service install` and the
// layout migration (package migrate/winmigrate) both go through it, so a
// fresh install and a migrated device run the same service. It is the
// Windows counterpart of package linuxservice.
//
// Names before and since agent 1.4.0:
//
//	service / event source   reagent                        ironflock-device-agent
//	folder                   %ProgramData%\IronFlock\Reagent  %ProgramData%\IronFlock\DeviceAgent
//	executable               reagent.exe                    device-agent.exe
//	database / log           reagent.db, reagent.log        device-agent.db, device-agent.log
//
// A device installed before 1.4.0 is moved to the new names by the layout
// migration, under the temporary service ironflock-device-agent-migrate.
package winservice

import (
	"fmt"
	"path/filepath"
	"strings"
)

const (
	ServiceName        = "ironflock-device-agent"
	LegacyServiceName  = "reagent"
	MigrateServiceName = "ironflock-device-agent-migrate"

	DisplayName        = "IronFlock Device Agent"
	MigrateDisplayName = "IronFlock Device Agent Migration"
	// LegacyDisplayName is the reagent service's display name while the
	// ironflock-device-agent service exists next to it, so services.msc never
	// shows two "IronFlock Device Agent" (Windows itself accepts the duplicate).
	LegacyDisplayName  = "IronFlock Device Agent (before 1.4.0)"
	Description        = "Connects this device to the IronFlock platform, runs apps via Docker, and keeps the agent up to date."
	MigrateDescription = "Moves the IronFlock device agent to its current service name and folder. Removed by the agent once the move is done."

	// RepairTaskName is the ONSTART scheduled task that runs the repair script.
	RepairTaskName = "IronFlock Agent Repair"

	ExeName       = "device-agent.exe"
	LegacyExeName = "reagent.exe"
	ConfigName    = "device.flock"
	RepairName    = "repair.cmd"

	// Folders below %ProgramData%\IronFlock.
	Folder        = "DeviceAgent"
	LegacyFolder  = "Reagent"
	MigrateFolder = ".migrate"
	// JournalName is the migration's journal, next to the folders.
	JournalName = ".device-agent-migration.json"

	// ConnectedName is the file the agent service writes once it is
	// connected to IronFlock ("<pid> <version> <service>"), read by the
	// migration's health check.
	ConnectedName = "connected"
	// RecreateMarkerName is left in the agent dir by a folder move: the agent
	// recreates its app containers once (their bind mounts name the old
	// folder).
	RecreateMarkerName = "recreate-containers"
)

// Root is %ProgramData%\IronFlock for programData.
func Root(programData string) string { return filepath.Join(programData, "IronFlock") }

// winPath joins Windows path elements for script text with a backslash,
// whatever the host OS of the generator (the tests run on any OS; the
// scripts only ever run on Windows).
func winPath(dir string, name string) string {
	return strings.TrimRight(dir, `\`) + `\` + name
}

// RepairCmd is the boot-time repair script of an installed agent, registered
// as the ONSTART scheduled task RepairTaskName. It closes the one gap
// in-process code cannot: an interrupted self-update swap that left the
// service ImagePath vacant (SCM recovery actions do not fire for a service
// that fails to START). Prefers the known-good previous binary, falls back to
// the newest downloaded one.
func RepairCmd(agentDir string) string {
	return strings.Join([]string{
		"@echo off",
		"rem IronFlock agent repair (registered as scheduled task '" + RepairTaskName + "')",
		"rem Restores the service binary if an interrupted self-update left it missing.",
		"set \"AGENTDIR=" + agentDir + "\"",
		"if exist \"%AGENTDIR%\\device-agent.exe\" goto start",
		"if exist \"%AGENTDIR%\\device-agent-prev.exe\" (",
		"  ren \"%AGENTDIR%\\device-agent-prev.exe\" \"device-agent.exe\"",
		"  goto start",
		")",
		"for /f \"delims=\" %%F in ('dir /b /o-d \"%AGENTDIR%\\device-agent-v*.exe\" 2^>nul') do (",
		"  ren \"%AGENTDIR%\\%%F\" \"device-agent.exe\"",
		"  goto start",
		")",
		":start",
		"sc start " + ServiceName,
		"",
	}, "\r\n")
}

// UniversalRepairCmd is the repair script the ONSTART task runs during a
// layout migration, written to <root>\.migrate\repair.cmd at the first step.
// It is valid at every instant of the move:
//
//   - while the journal exists, it makes sure the migrator's executable is
//     there and runs (copying any agent binary of this move it finds: never
//     a legacy spare, which is from before 1.4.0, nor a failed download) and
//     starts the migrate service, which resumes the move or its rollback;
//   - otherwise, for each agent service whose registry Start is not Disabled,
//     it restores that service's binary from its prev / newest -v copy and
//     starts it.
//
// root is %ProgramData%\IronFlock; legacyDir and newDir the agent folders of
// this device (a custom -agentDir keeps its folder, see winmigrate).
func UniversalRepairCmd(root, legacyDir, newDir string) string {
	return strings.Join([]string{
		"@echo off",
		"rem IronFlock agent repair during the move to the " + ServiceName + " service",
		"rem (registered as scheduled task '" + RepairTaskName + "'). While the move's journal",
		"rem exists it makes sure the migrator can run and starts it; otherwise it restores",
		"rem the binary of whichever agent service is enabled and starts that service.",
		"set \"ROOT=" + root + "\"",
		"set \"MIGRATE=" + winPath(root, MigrateFolder) + "\"",
		"set \"NEWDIR=" + newDir + "\"",
		"set \"OLDDIR=" + legacyDir + "\"",
		"if not exist \"%ROOT%\\" + JournalName + "\" goto agents",
		"rem a migrator whose SHA-256 is not the one recorded when it was staged (a copy torn by a",
		"rem power cut) is replaced. Nothing is run to check it: cmd.exe blocks for good on starting",
		"rem a truncated or empty executable, and nobody answers at boot.",
		"if not exist \"%MIGRATE%\\" + ExeName + "\" goto restore",
		"call :verified \"%MIGRATE%\\" + ExeName + "\"",
		"if defined OK goto migrate",
		"del /f /q \"%MIGRATE%\\" + ExeName + "\" >nul 2>&1",
		":restore",
		"if not exist \"%MIGRATE%\" mkdir \"%MIGRATE%\"",
		"rem the first agent binary of this move with the recorded hash, else the first one there;",
		"rem copied through a temporary name: a cut inside the copy must not leave a torn migrator",
		"set \"FALLBACK=\"",
		"for %%F in (\"%NEWDIR%\\" + ExeName + "\" \"%OLDDIR%\\" + LegacyExeName + "\" \"%OLDDIR%\\" + ExeName + "\" \"%NEWDIR%\\device-agent-prev.exe\" \"%OLDDIR%\\device-agent-prev.exe\") do (",
		"  if exist \"%%~F\" (",
		"    if not defined FALLBACK set \"FALLBACK=%%~F\"",
		"    call :verified \"%%~F\"",
		"    if defined OK (",
		"      call :install \"%%~F\"",
		"      goto migrate",
		"    )",
		"  )",
		")",
		"for /f \"delims=\" %%F in ('dir /b /o-d \"%NEWDIR%\\device-agent-v*.exe\" 2^>nul') do (",
		"  if not defined FALLBACK set \"FALLBACK=%NEWDIR%\\%%F\"",
		"  call :verified \"%NEWDIR%\\%%F\"",
		"  if defined OK (",
		"    call :install \"%NEWDIR%\\%%F\"",
		"    goto migrate",
		"  )",
		")",
		"for /f \"delims=\" %%F in ('dir /b /o-d \"%OLDDIR%\\reagent-v*.exe\" 2^>nul') do (",
		"  if not defined FALLBACK set \"FALLBACK=%OLDDIR%\\%%F\"",
		"  call :verified \"%OLDDIR%\\%%F\"",
		"  if defined OK (",
		"    call :install \"%OLDDIR%\\%%F\"",
		"    goto migrate",
		"  )",
		")",
		"if defined FALLBACK call :install \"%FALLBACK%\"",
		":migrate",
		"rem a rolled-back move disabled the migrate service, a done one removes it: then the agents.",
		"rem Disabled while the journal is neither done nor rolled back, it lost its configuration to",
		"rem a power cut (the registry is written back lazily; the journal is flushed): enabled again.",
		"call :startvalue " + MigrateServiceName,
		"if not defined START goto agents",
		"if /i not \"%START%\"==\"0x4\" goto startmigrate",
		"findstr /l /c:\"\\\"done\\\"\" \"%ROOT%\\" + JournalName + "\" >nul 2>&1 && goto agents",
		"findstr /l /c:\"\\\"failed_at\\\"\" \"%ROOT%\\" + JournalName + "\" >nul 2>&1 && goto agents",
		"sc config " + MigrateServiceName + " start= auto",
		":startmigrate",
		"sc start " + MigrateServiceName,
		"exit /b 0",
		":agents",
		"call :repair " + ServiceName + " \"%NEWDIR%\" device-agent",
		"call :repair " + LegacyServiceName + " \"%OLDDIR%\" reagent",
		"exit /b 0",
		":verified",
		"rem OK is set when %1 has the SHA-256 recorded next to the migrator (or none was recorded)",
		"set \"OK=\"",
		"if not exist \"%MIGRATE%\\" + ExeName + ".sha256\" (",
		"  set \"OK=1\"",
		"  exit /b 0",
		")",
		"set \"WANT=\"",
		"set /p WANT=<\"%MIGRATE%\\" + ExeName + ".sha256\"",
		"set \"HAVE=\"",
		"for /f \"skip=1 delims=\" %%H in ('certutil -hashfile \"%~1\" SHA256 2^>nul') do if not defined HAVE set \"HAVE=%%H\"",
		"if not defined HAVE exit /b 0",
		"set \"HAVE=%HAVE: =%\"",
		"if /i \"%HAVE%\"==\"%WANT%\" set \"OK=1\"",
		"exit /b 0",
		":install",
		"copy /y \"%~1\" \"%MIGRATE%\\" + ExeName + ".tmp\" >nul && move /y \"%MIGRATE%\\" + ExeName + ".tmp\" \"%MIGRATE%\\" + ExeName + "\" >nul",
		"exit /b 0",
		":startvalue",
		"set \"START=\"",
		"for /f \"tokens=3\" %%S in ('reg query \"HKLM\\SYSTEM\\CurrentControlSet\\Services\\%~1\" /v Start 2^>nul') do set \"START=%%S\"",
		"exit /b 0",
		":repair",
		"set \"SVC=%~1\"",
		"set \"DIR=%~2\"",
		"set \"NAME=%~3\"",
		"call :startvalue %SVC%",
		"if not defined START exit /b 0",
		"if /i \"%START%\"==\"0x4\" exit /b 0",
		"if exist \"%DIR%\\%NAME%.exe\" goto startsvc",
		"if exist \"%DIR%\\%NAME%-prev.exe\" (",
		"  ren \"%DIR%\\%NAME%-prev.exe\" \"%NAME%.exe\"",
		"  goto startsvc",
		")",
		"for /f \"delims=\" %%F in ('dir /b /o-d \"%DIR%\\%NAME%-v*.exe\" 2^>nul') do (",
		"  ren \"%DIR%\\%%F\" \"%NAME%.exe\"",
		"  goto startsvc",
		")",
		"rem the move's own rollback target (a copy of the agent that moves) serves either name",
		"if exist \"%DIR%\\device-agent-prev.exe\" (",
		"  copy /y \"%DIR%\\device-agent-prev.exe\" \"%DIR%\\%NAME%.exe\" >nul",
		"  goto startsvc",
		")",
		":startsvc",
		"sc start %SVC%",
		"exit /b 0",
		"",
	}, "\r\n")
}

// StartType of a service.
type StartType int

const (
	StartAutomatic StartType = iota + 1
	StartManual
	StartDisabled
)

func (s StartType) String() string {
	switch s {
	case StartAutomatic:
		return "auto"
	case StartManual:
		return "demand"
	case StartDisabled:
		return "disabled"
	}
	return fmt.Sprintf("start-type-%d", int(s))
}

// Spec is a service as the agent wants it; CreateOrUpdateService (Windows)
// opens or creates it and applies every field.
type Spec struct {
	Name        string
	DisplayName string
	Description string
	// Exe and Args make the ImagePath.
	Exe  string
	Args []string
	// StartType and Delayed (delayed automatic start).
	StartType StartType
	Delayed   bool
	// Restart delays of the recovery actions (the last one repeats forever);
	// ResetAfter is the failure-count reset period.
	RestartDelays []int // seconds
	ResetAfter    int   // seconds
	// NonCrashRecovery also fires the recovery actions when the process
	// reports STOPPED with a non-zero exit code.
	NonCrashRecovery bool
	// Env is the service's Environment registry value (REG_MULTI_SZ). nil
	// leaves the value as it is; an empty, non-nil slice removes it.
	Env []string
}

// AgentSpec is the agent service as `service install` and the migration
// register it: exe <agentDir>\device-agent.exe with every path explicit, so
// no path ever falls back to os.UserHomeDir (meaningless under LocalSystem).
// extra are flags carried over from a legacy ImagePath (-debug, -env, …).
func AgentSpec(agentDir, appsDir string, extra []string, env []string) Spec {
	args := []string{
		"-config", filepath.Join(agentDir, ConfigName),
		"-agentDir", agentDir,
		"-appsDir", appsDir,
		"-dbFileName", filepath.Join(agentDir, "device-agent.db"),
		"-logFile", filepath.Join(agentDir, "device-agent.log"),
	}
	args = append(args, extra...)
	return Spec{
		Name:        ServiceName,
		DisplayName: DisplayName,
		Description: Description,
		Exe:         filepath.Join(agentDir, ExeName),
		Args:        args,
		StartType:   StartAutomatic,
		Delayed:     true,
		// Restart on every failure, never give up: the last action repeats
		// when the list is exhausted, so the 120s delay throttles a crash
		// loop while a deliberate exit-for-restart (updates, remote restart)
		// recovers in 5s.
		RestartDelays:    []int{5, 30, 120},
		ResetAfter:       86400,
		NonCrashRecovery: true,
		Env:              env,
	}
}

// MigrateSpec is the temporary migrate service: automatic start so a power
// cut resumes the move, restarted every 30 s whatever happened to it, with the
// legacy service's environment (a WSL engine's DOCKER_HOST, a proxy) plus the
// migration's test hooks.
func MigrateSpec(exe string, env []string) Spec {
	return Spec{
		Name:             MigrateServiceName,
		DisplayName:      MigrateDisplayName,
		Description:      MigrateDescription,
		Exe:              exe,
		Args:             []string{"migrate-layout"},
		StartType:        StartAutomatic,
		RestartDelays:    []int{30},
		ResetAfter:       86400,
		NonCrashRecovery: true,
		Env:              env,
	}
}
