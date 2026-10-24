// Package winmigrate moves a Windows install from before agent 1.4.0 — the
// service reagent running %ProgramData%\IronFlock\Reagent\reagent.exe — to the
// current names: the service ironflock-device-agent running
// %ProgramData%\IronFlock\DeviceAgent\device-agent.exe (see package winservice
// for every name). It is the Windows counterpart of package migrate, which
// does the same for Linux; the two share their journal semantics but no code,
// because the Linux engine joins a test root to absolute Unix paths and
// hard-codes systemd units.
//
// The move starts only while the legacy agent is connected to IronFlock, and
// it is done only once the new agent is: a device must never lose its way
// back to IronFlock over it. A failure undoes the completed steps in reverse
// (the failed one included) and makes sure an agent service runs again
// (ensure-agent); the attempt is retried at a later start of the agent, a day
// after the failure at the earliest, without end, or when a user asks.
//
// The legacy agent (reagent, running the first 1.4.0 as reagent.exe) stages
// the move (Begin): it hardens %ProgramData%\IronFlock, copies itself to
// .migrate\device-agent.exe, registers the temporary service
// ironflock-device-agent-migrate (automatic start, restarted every 30 s, so a
// power cut or a crash resumes the move) and starts it. The move is a service
// of its own because the agent's kill-on-close job object would take a child
// process down with the stopped legacy service.
//
// The steps, each idempotent, with the journal saved after each one:
//
//	prepare           read the legacy service, choose the strategy (move the
//	                  folder, or in place), point the repair task at the
//	                  universal script, write device-agent-prev.exe, drop the
//	                  legacy update files that would block updates once renamed
//	stop-legacy       disable and stop reagent; kill what SCM left behind
//	stop-apps         restart policy "no" and stop for every container bound
//	                  into the folder (restored at the end)
//	rename-files      reagent.db/.log/.exe -> device-agent…, never overwriting
//	move-folder       Reagent -> DeviceAgent (strategy move), with the
//	                  recreate-containers marker for the agent
//	register-service  ironflock-device-agent, demand start, every setting; the
//	                  legacy service gives its display name up meanwhile
//	start-agent       delayed-auto, start
//	health            the new service's PID connected to IronFlock at this
//	                  version, then stable for a minute
//	cleanup           Done first; then the reagent service, event source and
//	                  Defender exclusion go, the restart policies come back,
//	                  the repair task points at DeviceAgent\repair.cmd
//	ensure-agent      (after done and after a rollback) an agent service runs
//
// Everything outside plain Go — files (so a test can cut the power after any
// operation), processes, the service control manager, the scheduled task,
// Defender, Docker — goes through the System interface; system_windows.go is
// the real one, the tests model all of it.
package winmigrate

import (
	"os"
	"path/filepath"
	"reagent/winservice"
	"time"
)

// Layout names the migration's own files below Root, which is
// %ProgramData%\IronFlock (tests: a temporary directory).
type Layout struct {
	Root string
}

// DefaultLayout is the layout of this machine (%ProgramData%\IronFlock).
func DefaultLayout() Layout {
	programData := os.Getenv("ProgramData")
	if programData == "" {
		programData = `C:\ProgramData`
	}
	return Layout{Root: winservice.Root(programData)}
}

func (l Layout) Journal() string     { return filepath.Join(l.Root, winservice.JournalName) }
func (l Layout) JournalBak() string  { return l.Journal() + ".bak" }
func (l Layout) MigrateDir() string  { return filepath.Join(l.Root, winservice.MigrateFolder) }
func (l Layout) MigratorExe() string { return filepath.Join(l.MigrateDir(), winservice.ExeName) }

// MigratorHash holds the migrator's SHA-256 (hex): the universal repair
// script checks a migrator against it without running anything.
func (l Layout) MigratorHash() string { return l.MigratorExe() + ".sha256" }
func (l Layout) RepairCmd() string    { return filepath.Join(l.MigrateDir(), winservice.RepairName) }
func (l Layout) RepairBackup() string { return l.RepairCmd() + ".bak" }
func (l Layout) TaskBackup() string   { return filepath.Join(l.MigrateDir(), "task.bak.xml") }
func (l Layout) LogFile() string      { return filepath.Join(l.MigrateDir(), "migrate.log") }

// LegacyDir and NewDir are the default folders; a custom -agentDir keeps its
// folder (strategy in-place).
func (l Layout) LegacyDir() string { return filepath.Join(l.Root, winservice.LegacyFolder) }
func (l Layout) NewDir() string    { return filepath.Join(l.Root, winservice.Folder) }

// ServiceConfig is a service as the service control manager has it.
type ServiceConfig struct {
	// Exe and Args are the ImagePath, decomposed (windows.DecomposeCommandLine).
	Exe         string
	Args        []string
	StartType   winservice.StartType
	Delayed     bool
	Env         []string
	Description string
}

// ServiceStatus is a service's current state.
type ServiceStatus struct {
	// State: "running", "stopped", "start-pending", "stop-pending" or
	// another SCM state. The SCM reports "stopped" before the process is gone.
	State string
	PID   int
}

func (s ServiceStatus) Running() bool { return s.State == StateRunning }
func (s ServiceStatus) Stopped() bool { return s.State == StateStopped }

const (
	StateRunning      = "running"
	StateStopped      = "stopped"
	StateStartPending = "start-pending"
	StateStopPending  = "stop-pending"
)

// Process is a running process and the executable it runs.
type Process struct {
	PID int
	Exe string
}

// Container is an app container as Docker lists it.
type Container struct {
	ID   string
	Name string
	// RestartPolicy: "no", "always", "unless-stopped", "on-failure".
	RestartPolicy string
	// Binds are the host sources of its bind mounts, as the engine reports
	// them (C:\…, /mnt/c/…, /run/desktop/mnt/host/c/…).
	Binds   []string
	Running bool
}

// System is everything the migration touches outside the Go runtime. Every
// file operation goes through it too, so a test can cut the power after any
// one of them.
type System interface {
	// --- files ---

	// Stat does not follow links (os.Lstat).
	Stat(path string) (os.FileInfo, error)
	ReadFile(path string) ([]byte, error)
	// WriteFile replaces the file atomically (a temporary file, then a
	// replacing rename).
	WriteFile(path string, data []byte, mode os.FileMode) error
	// Rename never overwrites: an existing destination is an error
	// (os.ErrExist). It moves files and folders.
	Rename(from, to string) error
	Remove(path string) error
	RemoveAll(path string) error
	MkdirAll(path string, mode os.FileMode) error
	ReadDir(path string) ([]os.DirEntry, error)
	// CopyFile copies from to to, replacing to.
	CopyFile(from, to string, mode os.FileMode) error
	// SameVolume reports whether two paths are on one volume (a rename across
	// volumes copies).
	SameVolume(a, b string) (bool, error)
	// HoldersOf names the processes that hold path or something below it
	// open (the Restart Manager); best effort, for the error message.
	HoldersOf(path string) []string

	// --- processes ---

	// ExecVersion runs `<exe> -version` with a timeout.
	ExecVersion(exe string) (string, error)
	// ProcessesUnder lists the processes whose executable is below dir.
	ProcessesUnder(dir string) ([]Process, error)
	ProcessAlive(pid int) bool
	// KillProcess terminates pid and waits until it is gone.
	KillProcess(pid int) error

	// --- the service control manager ---

	// Service reads a service; os.ErrNotExist when there is none.
	Service(name string) (ServiceConfig, error)
	// ServiceStatus; os.ErrNotExist when there is none.
	ServiceStatus(name string) (ServiceStatus, error)
	// CreateOrUpdateService opens or creates the service and applies every
	// setting (winservice.CreateOrUpdateService).
	CreateOrUpdateService(spec winservice.Spec) error
	SetStartType(name string, start winservice.StartType, delayed bool) error
	// SetDisplayName; os.ErrNotExist when there is none. The legacy service
	// gives "IronFlock Device Agent" up while the new one exists, so
	// services.msc never lists the name twice (cosmetic: Windows accepts it).
	SetDisplayName(name, display string) error
	// StartService; a service that already runs is success.
	StartService(name string) error
	// StopService asks the service to stop and waits for the SCM to report
	// stopped; a service that is not running is success. The process may
	// still be exiting (ProcessesUnder, KillProcess).
	StopService(name string) error
	// DeleteService; a service marked for deletion while it still runs is
	// success (it goes when it stops). os.ErrNotExist when there is none.
	DeleteService(name string) error
	InstallEventSource(name string) error
	RemoveEventSource(name string) error

	// --- the repair scheduled task ---

	// ExportTask returns the task's XML definition (schtasks /Query /XML);
	// os.ErrNotExist when there is none.
	ExportTask(name string) ([]byte, error)
	// CreateTask registers (or replaces) the ONSTART task running script as
	// SYSTEM.
	CreateTask(name, script string) error
	// ImportTask replaces the task from an exported definition.
	ImportTask(name string, xml []byte) error

	// --- Windows Defender ---

	AddDefenderExclusion(path string) error
	RemoveDefenderExclusion(path string) error

	// --- Docker ---

	// Containers lists every container (running or not).
	Containers() ([]Container, error)
	SetRestartPolicy(id, policy string) error
	StopContainer(id string) error

	// --- the machine ---

	// Harden restricts %ProgramData%\IronFlock (winservice.HardenRoot).
	Harden(root string) error
	OwnedByAdministrators(path string) (bool, error)
	// DockerDesktopSharing reports whether Docker Desktop runs the Hyper-V
	// backend (no WSL engine) and the folders its file sharing allows.
	DockerDesktopSharing() (hyperV bool, shared []string)
	Elevated() bool
	Now() time.Time
	Sleep(time.Duration)
	// LockMigration takes the machine-wide lock of the migration engine (a
	// named mutex) and returns how to release it; an error while another
	// process holds it. One journal, one engine: the migrate service and a
	// `migrate-layout start -foreground` must never run side by side.
	LockMigration() (release func(), err error)
}

// Env is one migration's configuration.
type Env struct {
	Layout Layout
	Sys    System
	// Version is the agent version doing the migration.
	Version string
	// Fault and Pause are test hooks (IRONFLOCK_MIGRATE_FAULT/_PAUSE): fail
	// at, or stop after, the named step.
	Fault, Pause string
	// HealthTimeout: the new agent must connect this soon; StableFor: and
	// stay up this long.
	HealthTimeout time.Duration
	StableFor     time.Duration
	Logf          func(format string, args ...any)
	// Cancelled reports whether the service was asked to stop; checked
	// between operations. The journal persists; the next start resumes.
	Cancelled func() bool
}

func (e Env) logf(format string, args ...any) {
	if e.Logf != nil {
		e.Logf(format, args...)
	}
}

func (e Env) cancelled() bool {
	return e.Cancelled != nil && e.Cancelled()
}

// The test hooks' environment variables, shared with the Linux migration.
const (
	FaultEnv = "IRONFLOCK_MIGRATE_FAULT"
	PauseEnv = "IRONFLOCK_MIGRATE_PAUSE"
)
