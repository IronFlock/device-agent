// Package migrate moves a legacy install — /opt/reagent, run by
// reagent.service (and on most, the reagent-manager.sh supervisor) — into the
// current layout: /opt/ironflock/device-agent, run by
// ironflock-device-agent.service. It runs on the device, in place, on every
// kind of install (see plan.go). It starts only while the legacy agent is
// connected to IronFlock, and it is done only once the new agent is: a
// device must never lose its way back to IronFlock over it. A rollback
// is retried at a later start of the agent (a day after the failure at the
// earliest), without end, or when a user asks.
//
// The agent starts it (Begin) from inside the legacy reagent.service, but the
// work runs in a oneshot unit of its own, ironflock-device-agent-migrate:
// stopping reagent.service would otherwise kill the migration, and the unit
// resumes it at boot after a power cut. Every step is idempotent and recorded
// in a journal; a failure undoes the completed steps in reverse and restarts
// the legacy agent.
//
// The order keeps exactly one working layout at every instant:
//
//	prepare           the plan (plan.go); new unit (not enabled), repair.sh,
//	                  enrolled, the new binary names, all INSIDE /opt/reagent
//	block-docker      docker.service may not start at boot while switching:
//	                  it would create an empty data-root at the old path
//	stop-legacy       stop, disable and mask reagent-manager + reagent (a real
//	                  unit file is moved aside first)
//	stop-docker       only for a move with Docker's data-root below /opt/reagent
//	rename-database   reagent.db -> device-agent.db
//	move-folder       move: one rename to /opt/ironflock/device-agent, then the
//	                  compat link /opt/reagent -> there; link: the link
//	                  /opt/ironflock/device-agent -> /opt/reagent
//	docker-data-root  move: daemon.json data-root follows
//	config-file       the .flock link to /boot becomes a 0600 copy (not on
//	                  OS images, whose boot scripts relink it)
//	logs              /var/log/reagent.log -> /var/log/ironflock/device-agent.log
//	unblock-docker
//	start-agent       enable and start the new unit
//	health            the unit's main PID connected to IronFlock, then stable a minute
//	cleanup           legacy files, the /boot secret copies (not on OS images),
//	                  the migration unit
package migrate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reagent/layout"
	"reagent/linuxservice"
	"strconv"
	"strings"
	"time"
)

const (
	JournalPath       = "/opt/ironflock/.device-agent-migration.json"
	FaultEnv          = "IRONFLOCK_MIGRATE_FAULT"
	PauseEnv          = "IRONFLOCK_MIGRATE_PAUSE"
	migrateDir        = "/opt/ironflock/.migrate"
	migrateBinary     = migrateDir + "/device-agent"
	MigrateUnitName   = "ironflock-device-agent-migrate.service"
	switchingMarker   = "/opt/ironflock/.device-agent-migration-switching"
	dockerBlockDropIn = "/etc/systemd/system/docker.service.d/ironflock-migration.conf"
	connectedFile     = "/run/ironflock-device-agent/connected"
	legacyAgentUnit   = "reagent.service"
	legacyManagerUnit = "reagent-manager.service"
	legacyDropInDir   = "/etc/systemd/system/reagent.service.d"
	legacyServicesDir = layout.LegacyAgentDir + "/reswarmify/services"
	daemonJSON        = "/etc/docker/daemon.json"
	daemonNvidiaJSON  = "/etc/docker/daemon-nvidia.json"
	legacyLog         = "/var/log/reagent.log"
	rootKeyCopy       = "/root/key.pem" // the device key, extracted by the legacy installer
	rootPubCopy       = "/root/id.pub"
	legacyFrpcLog     = "/var/log/frpc.log"
	bootDeviceINI     = "/boot/device.ini"
	legacyConfigLink  = layout.LegacyAgentDir + "/device-config.flock"
	retryAfterFailure = 24 * time.Hour
	// maxRuns: the migration process may stop (crash, kill, power cut) this
	// often within one attempt before the attempt is rolled back.
	maxRuns = 5
	// defaultHealthLimit: the new agent must connect to IronFlock this soon.
	defaultHealthLimit = 10 * time.Minute
	defaultStableFor   = time.Minute
)

// migrateUnitPath is the migration's own oneshot unit.
var migrateUnitPath = filepath.Join("/etc/systemd/system", MigrateUnitName)

// Env is everything the migration touches outside the files below Root, so
// tests run it against a temporary directory with fakes.
type Env struct {
	// Root prefixes every path ("" for the real filesystem).
	Root string
	// Systemctl runs systemctl with args.
	Systemctl func(args ...string) error
	// MainPID returns the main PID of a running unit (0 when not running).
	MainPID func(unit string) (int, error)
	// UnitLoaded reports whether systemd knows the unit (stopping one it
	// does not know fails). nil: every unit is.
	UnitLoaded func(unit string) bool
	// MountPoints lists the mount points.
	MountPoints func() ([]string, error)
	// SameFilesystem reports whether two paths are on one filesystem.
	SameFilesystem func(a, b string) (bool, error)
	// Unmount detaches a mount point (lazily); nil: nothing to unmount.
	Unmount func(path string) error
	Now     func() time.Time
	Sleep   func(time.Duration)
	// Version is the agent version doing the migration.
	Version string
	// Fault and Pause are test hooks (IRONFLOCK_MIGRATE_FAULT/_PAUSE): fail
	// at, or stop after, the named step.
	Fault, Pause  string
	HealthTimeout time.Duration
	StableFor     time.Duration
	Logf          func(format string, args ...any)
}

func (e Env) p(path string) string {
	if e.Root == "" || e.Root == "/" {
		return path
	}
	return filepath.Join(e.Root, path)
}

// unroot strips Root from a path below it.
func (e Env) unroot(path string) string {
	if e.Root == "" || e.Root == "/" {
		return path
	}
	return "/" + strings.TrimPrefix(strings.TrimPrefix(path, e.Root), "/")
}

func (e Env) loaded(unit string) bool {
	return e.UnitLoaded == nil || e.UnitLoaded(unit)
}

func (e Env) logf(format string, args ...any) {
	if e.Logf != nil {
		e.Logf(format, args...)
	}
}

// Journal records the migration's progress.
type Journal struct {
	Version   string `json:"version"`
	Step      int    `json:"step"` // steps completed
	Done      bool   `json:"done,omitempty"`
	FailedAt  string `json:"failed_at,omitempty"`
	Error     string `json:"error,omitempty"`
	StartedAt string `json:"started_at"`
	UpdatedAt string `json:"updated_at"`
	// DockerStopped: step stop-docker stopped Docker (its data-root was
	// below /opt/reagent), so it has to be started again.
	DockerStopped bool `json:"docker_stopped,omitempty"`
	// ConfigTarget is the file the legacy device-config.flock pointed at.
	ConfigTarget string `json:"config_target,omitempty"`
	// ConfigLinkCreated: prepare created /opt/reagent/device-config.flock
	// (installs from before 2024-09 used device-config.reswarm).
	ConfigLinkCreated bool `json:"config_link_created,omitempty"`
	// ConfigWasFile: the config was a file, not a link; it becomes 0600 and
	// a rollback puts ConfigMode back.
	ConfigWasFile bool        `json:"config_was_file,omitempty"`
	ConfigMode    os.FileMode `json:"config_mode,omitempty"`
	// The plan (plan.go).
	Planned        bool         `json:"planned,omitempty"`
	Strategy       string       `json:"strategy,omitempty"`
	StrategyReason string       `json:"strategy_reason,omitempty"`
	Image          bool         `json:"image,omitempty"`
	Units          []legacyUnit `json:"units,omitempty"`
	LegacyConfig   string       `json:"legacy_config,omitempty"`
	LegacyLog      string       `json:"legacy_log,omitempty"`
	AppsDir        string       `json:"apps_dir,omitempty"`
	ExtraArgs      []string     `json:"extra_args,omitempty"`
	// RollingBack: the steps up to FailedStep are being undone; a resume
	// continues the rollback, never the migration.
	RollingBack bool `json:"rolling_back,omitempty"`
	FailedStep  int  `json:"failed_step,omitempty"`
	// Runs counts the migration processes of this attempt (maxRuns).
	Runs int `json:"runs,omitempty"`
	// Attempts counts the attempts that rolled back.
	Attempts int `json:"attempts,omitempty"`
	// PreferLink: an earlier attempt could not free /opt/reagent of mounts;
	// this one leaves it in place (StrategyLink).
	PreferLink bool `json:"prefer_link,omitempty"`
}

// Failed reports whether the migration failed and was rolled back.
func (j *Journal) Failed() bool { return j.FailedAt != "" }

// Failed reports whether the last migration below root rolled back.
func Failed(root string) bool {
	j, err := LoadJournal(root)
	return err == nil && j != nil && j.Failed()
}

// InProgress reports whether a migration below root has started and is
// neither done nor rolled back.
func InProgress(root string) bool {
	j, err := LoadJournal(root)
	return err == nil && j != nil && !j.Done && !j.Failed()
}

// LoadJournal reads the journal below root; nil when there is none.
func LoadJournal(root string) (*Journal, error) {
	data, err := os.ReadFile(Env{Root: root}.p(JournalPath))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var j Journal
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("unreadable migration journal: %w", err)
	}
	return &j, nil
}

func (e Env) saveJournal(j *Journal) error {
	j.UpdatedAt = e.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(e.p(JournalPath)), 0o755); err != nil {
		return err
	}
	return linuxservice.WriteFileAtomic(e.p(JournalPath), data, 0o644)
}

// ErrNotEligible wraps why a device is not moved automatically.
var ErrNotEligible = errors.New("not eligible for the layout migration")

// Check reports whether this device is a legacy install to move: an
// /opt/reagent folder (or mount) run by a reagent.service, and no
// /opt/ironflock/device-agent yet. Every such install moves (plan.go says
// how).
func Check(e Env) error {
	if info, err := os.Stat(e.p(layout.LegacyAgentDir)); err != nil || !info.IsDir() {
		return fmt.Errorf("%w: no %s", ErrNotEligible, layout.LegacyAgentDir)
	}
	if exists(e.p(layout.AgentDir)) {
		return fmt.Errorf("%w: %s exists already", ErrNotEligible, layout.AgentDir)
	}
	if info, err := os.Lstat(e.p(layout.LegacyAgentDir)); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s is a link (moved already?)", ErrNotEligible, layout.LegacyAgentDir)
	}
	if _, ok := findLegacyUnit(e, legacyAgentUnit); !ok {
		return fmt.Errorf("%w: no %s runs the agent", ErrNotEligible, legacyAgentUnit)
	}
	return nil
}

// Due reports whether a migration should be started now: none started yet,
// or the last attempt failed more than a day ago (without end: every device
// moves eventually).
func Due(e Env) (bool, error) {
	j, err := LoadJournal(e.Root)
	if err != nil {
		return false, err
	}
	if j == nil {
		return true, nil
	}
	if j.Done || !j.Failed() {
		// done, or in progress (the migration unit resumes it)
		return false, nil
	}
	failedAt, err := time.Parse(time.RFC3339, j.FailedAt)
	elapsed := e.Now().Sub(failedAt)
	// A clock behind the stamp (no RTC, a stale saved clock) must not hold
	// the retry off: early is harmless, never is not.
	return err != nil || elapsed >= retryAfterFailure || elapsed < 0, nil
}

// LastError is the error of a failed migration, for the heartbeat stats.
func LastError(root string) string {
	j, err := LoadJournal(root)
	if err != nil {
		return err.Error()
	}
	if j == nil || !j.Failed() {
		return ""
	}
	return fmt.Sprintf("%s (attempt %d; retried at the agent's next start a day later, or by an update from Studio)", j.Error, j.Attempts)
}

// Begin hands the migration to its own unit and starts it without waiting.
// The caller (the legacy agent) keeps running until the migration stops
// reagent.service.
func Begin(e Env, selfExe string) error {
	if err := Stage(e, selfExe); err != nil {
		return err
	}
	if err := e.Systemctl("start", "--no-block", MigrateUnitName); err != nil {
		// Nothing moved yet; recorded as failed, so it is retried tomorrow
		// rather than at the next boot.
		if j, _ := LoadJournal(e.Root); j != nil {
			j.FailedAt, j.Error = e.Now().UTC().Format(time.RFC3339), "starting the migration: "+err.Error()
			j.Attempts++
			_ = e.saveJournal(j)
		}
		e.disableMigrationUnit()
		return err
	}
	return nil
}

// Stage copies the running binary (selfExe) outside /opt/reagent, writes and
// enables the migration unit (it resumes the migration at boot) and starts
// the journal. Run then does the migration, in the unit or in the caller.
func Stage(e Env, selfExe string) error {
	attempts, preferLink := 0, false
	if previous, err := LoadJournal(e.Root); err == nil && previous != nil && previous.Failed() {
		attempts = previous.Attempts
		preferLink = previous.PreferLink || (previous.FailedStep == stepIndex("stop-docker") && previous.Strategy == StrategyMove)
	}
	if err := os.MkdirAll(e.p(migrateDir), 0o700); err != nil {
		return err
	}
	// A previous attempt's daemon.json backups would be restored by this
	// attempt's rollback.
	backups, _ := filepath.Glob(filepath.Join(e.p(migrateDir), "*.bak"))
	for _, backup := range backups {
		if err := os.Remove(backup); err != nil {
			return err
		}
	}
	if err := copyFile(selfExe, e.p(migrateBinary), 0o755); err != nil {
		return fmt.Errorf("copying the migration binary: %w", err)
	}
	if err := linuxservice.WriteFileAtomic(e.p(migrateUnitPath), []byte(migrateUnit(e)), 0o644); err != nil {
		return err
	}
	// The unit's condition is the journal, so it is enabled first and the
	// journal written last: a migration exists only once it can resume.
	if err := e.Systemctl("daemon-reload"); err != nil {
		return err
	}
	if err := e.Systemctl("enable", MigrateUnitName); err != nil {
		return err
	}
	return e.saveJournal(&Journal{Version: e.Version, StartedAt: e.Now().UTC().Format(time.RFC3339), Attempts: attempts, PreferLink: preferLink})
}

// migrateUnit runs `migrate-layout` until the migration is done or rolled
// back, and again at boot while the journal says it is in progress.
func migrateUnit(e Env) string {
	lines := []string{
		"[Unit]",
		"Description=Move the IronFlock device agent to " + layout.AgentDir,
		"ConditionPathExists=" + JournalPath,
		"After=local-fs.target",
		"StartLimitIntervalSec=0",
		"",
		"[Service]",
		// A process that dies (crash, OOM kill, the runtime limit) is
		// started again: between stop-legacy and start-agent no agent runs,
		// and a remote device may never see the reboot that would resume it.
		// A done or rolled-back journal makes the next run a no-op.
		"Type=simple",
		"ExecStart=" + migrateBinary + " migrate-layout",
		"Restart=on-failure",
		"RestartSec=30",
		"RuntimeMaxSec=45min",
	}
	// The test hooks travel from the agent's environment into the unit.
	if e.Fault != "" {
		lines = append(lines, "Environment="+FaultEnv+"="+e.Fault)
	}
	if e.Pause != "" {
		lines = append(lines, "Environment="+PauseEnv+"="+e.Pause)
	}
	lines = append(lines, "", "[Install]", "WantedBy=multi-user.target", "")
	return strings.Join(lines, "\n")
}

type step struct {
	name string
	do   func(e Env, j *Journal) error
	undo func(e Env, j *Journal) error
}

var steps = []step{
	{"prepare", prepare, unprepare},
	{"block-docker", blockDocker, unblockDockerFiles},
	{"stop-legacy", stopLegacy, restoreLegacyUnits},
	{"stop-docker", stopDocker, nil},
	{"rename-database", renameDatabase, unrenameDatabase},
	{"move-folder", moveFolder, unmoveFolder},
	{"docker-data-root", moveDockerDataRoot, restoreDockerDataRoot},
	{"config-file", copyConfig, relinkConfig},
	{"logs", moveLogs, unmoveLogs},
	{"unblock-docker", unblockDocker, restopDocker},
	{"start-agent", startAgent, stopAgent},
	{"health", waitHealthy, nil},
	{"cleanup", cleanup, nil},
}

// StepNames lists the steps in order (for the test hooks).
func StepNames() []string {
	names := make([]string, len(steps))
	for i, s := range steps {
		names[i] = s.name
	}
	return names
}

// Run continues the migration from its journal: the migration unit's
// `device-agent migrate-layout`. It returns once the migration is done, or
// rolled back (with the error).
func Run(e Env) error {
	j, err := LoadJournal(e.Root)
	if err != nil {
		return err
	}
	if j == nil {
		return nil
	}
	if j.Done || j.Failed() {
		e.disableMigrationUnit()
		return nil
	}
	if e.HealthTimeout == 0 {
		e.HealthTimeout = defaultHealthLimit
	}
	if e.StableFor == 0 {
		e.StableFor = defaultStableFor
	}
	if j.RollingBack {
		e.logf("migrate: continuing the rollback from %s", steps[j.FailedStep].name)
		return e.fail(j, j.FailedStep, errors.New(j.Error))
	}
	j.Runs++
	if j.Runs > maxRuns {
		failed := min(j.Step, len(steps)-1)
		return e.fail(j, failed, fmt.Errorf("%s: the migration process stopped %d times", steps[failed].name, maxRuns))
	}
	if err := e.saveJournal(j); err != nil {
		return e.fail(j, min(j.Step, len(steps)-1), fmt.Errorf("saving the journal: %w", err))
	}

	// The plan first: a rollback needs it, from the first step on.
	if err := plan(e, j); err != nil {
		return e.fail(j, 0, fmt.Errorf("plan: %w", err))
	}

	for i := j.Step; i < len(steps); i++ {
		s := steps[i]
		e.logf("migrate: %s", s.name)
		if e.Fault == s.name {
			err = fmt.Errorf("fault injected at %s", s.name)
		} else {
			err = s.do(e, j)
		}
		if err == nil {
			j.Step = i + 1
			if saveErr := e.saveJournal(j); saveErr != nil {
				err = fmt.Errorf("saving the journal: %w", saveErr)
			}
		}
		if err != nil {
			return e.fail(j, i, fmt.Errorf("%s: %w", s.name, err))
		}
		if e.Pause == s.name {
			e.logf("migrate: paused after %s (test hook)", s.name)
			for {
				e.Sleep(time.Hour)
			}
		}
	}
	j.Done = true
	if err := e.saveJournal(j); err != nil {
		return err
	}
	e.disableMigrationUnit()
	e.logf("migrate: done; the agent runs from %s", layout.AgentDir)
	return nil
}

// fail rolls the migration back from step failed and records it. The
// journal says "rolling back" before the first undo, so a crash in the
// middle continues the rollback, never the migration.
func (e Env) fail(j *Journal, failed int, err error) error {
	e.logf("migrate: %v; rolling back", err)
	j.RollingBack, j.FailedStep, j.Error = true, failed, err.Error()
	if saveErr := e.saveJournal(j); saveErr != nil {
		e.logf("migrate: saving the journal: %v", saveErr)
	}
	e.rollback(j, failed)
	j.RollingBack = false
	j.FailedAt = e.Now().UTC().Format(time.RFC3339)
	j.Attempts++
	if saveErr := e.saveJournal(j); saveErr != nil {
		e.logf("migrate: saving the journal: %v", saveErr)
	}
	e.disableMigrationUnit()
	return err
}

// rollback undoes the steps before failed (the failed one included: a step
// may have done part of its work) in reverse, then starts Docker and the
// legacy supervisor again. It keeps going past errors: every undo that
// works brings the device closer to the legacy layout it ran in before.
func (e Env) rollback(j *Journal, failed int) {
	for i := failed; i >= 0; i-- {
		if steps[i].undo == nil {
			continue
		}
		if err := steps[i].undo(e, j); err != nil {
			e.logf("migrate: undoing %s: %v", steps[i].name, err)
		}
	}
	// Also when this attempt did not stop Docker: a boot during the switch
	// kept it from starting (the block is gone again by now).
	if failed >= 1 {
		if err := e.startDocker(); err != nil {
			e.logf("migrate: starting Docker: %v", err)
		}
	}
	// The manager starts the agent; without one, the agent itself.
	legacy := legacyAgentUnit
	for _, u := range j.Units {
		if u.Name == legacyManagerUnit && e.loaded(u.Name) {
			legacy = legacyManagerUnit
		}
	}
	if err := e.Systemctl("start", legacy); err != nil {
		e.logf("migrate: starting %s: %v", legacy, err)
	}
}

func (e Env) startDocker() error {
	if e.loaded("docker.socket") {
		if err := e.Systemctl("start", "docker.socket"); err != nil {
			return err
		}
	}
	return e.Systemctl("start", "docker.service")
}

func (e Env) disableMigrationUnit() {
	_ = e.Systemctl("disable", MigrateUnitName)
}

// --- steps -------------------------------------------------------------------

var newPaths = linuxservice.DefaultPaths()

var legacyConfigFile = layout.LegacyAgentDir + "/device-config.flock"

// in names a file of the new layout inside /opt/reagent, where it is created
// so that the folder rename moves it into place.
func in(name string) string { return filepath.Join(layout.LegacyAgentDir, name) }

func prepare(e Env, j *Journal) error {
	if err := linuxservice.WriteFileAtomic(e.p(in("repair.sh")), []byte(newPaths.RepairScript()), 0o755); err != nil {
		return err
	}
	if err := copyFile(e.p(migrateBinary), e.p(in("device-agent")), 0o755); err != nil {
		return err
	}
	prev := e.p(in("device-agent-prev"))
	_ = os.Remove(prev)
	if err := os.Link(e.p(in("device-agent")), prev); err != nil {
		return err
	}
	if err := linuxservice.WriteFileAtomic(e.p(in("enrolled")), nil, 0o644); err != nil {
		return err
	}

	// The new unit reads device-config.flock; installs from before 2024-09
	// named it device-config.reswarm. Remember what it points at.
	configPath := j.LegacyConfig
	info, err := os.Lstat(e.p(configPath))
	if err != nil {
		return fmt.Errorf("the legacy configuration %s: %w", configPath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := filepath.EvalSymlinks(e.p(configPath))
		if err != nil {
			return fmt.Errorf("the legacy configuration %s: %w", configPath, err)
		}
		j.ConfigTarget = e.unroot(target)
	} else {
		// A file (copied in by hand): there is no link to restore.
		j.ConfigTarget = ""
	}
	if configPath != legacyConfigLink && !exists(e.p(legacyConfigLink)) {
		// Relative: it resolves in the old folder and in the new one.
		if err := os.Symlink(filepath.Base(configPath), e.p(legacyConfigLink)); err != nil {
			return err
		}
		j.ConfigLinkCreated = true
		if err := e.saveJournal(j); err != nil {
			return err
		}
	}

	if err := linuxservice.WriteFileAtomic(e.p(linuxservice.UnitPath), []byte(newUnitPaths(j).Unit()), 0o644); err != nil {
		return err
	}
	// Drop-ins (the proxy environment, AGENT_UPDATE_ALLOW_UNSIGNED, …) belong
	// to the agent, not to the unit name.
	dropIns, _ := filepath.Glob(filepath.Join(e.p(legacyDropInDir), "*.conf"))
	if len(dropIns) > 0 {
		if err := os.MkdirAll(e.p(linuxservice.DropInDir), 0o755); err != nil {
			return err
		}
	}
	for _, dropIn := range dropIns {
		dst := filepath.Join(e.p(linuxservice.DropInDir), filepath.Base(dropIn))
		if exists(dst) {
			continue
		}
		if err := copyFile(dropIn, dst, 0o644); err != nil {
			return err
		}
	}
	// Enabled only by start-agent: until then a boot (after a power cut) must
	// not start it on a half-moved folder; the migration unit resumes instead.
	return e.Systemctl("daemon-reload")
}

func unprepare(e Env, j *Journal) error {
	for _, path := range []string{linuxservice.UnitPath, in("repair.sh"), in("device-agent"), in("device-agent-prev"), in("enrolled")} {
		_ = os.Remove(e.p(path))
	}
	_ = os.RemoveAll(e.p(linuxservice.DropInDir))
	if j.ConfigLinkCreated {
		_ = os.Remove(e.p(legacyConfigLink))
	}
	return e.Systemctl("daemon-reload")
}

func blockDocker(e Env, j *Journal) error {
	if err := linuxservice.WriteFileAtomic(e.p(switchingMarker), nil, 0o644); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(e.p(dockerBlockDropIn)), 0o755); err != nil {
		return err
	}
	dropIn := "[Unit]\n# Written by device-agent migrate-layout while it moves Docker's data-root.\nConditionPathExists=!" + switchingMarker + "\n"
	if err := linuxservice.WriteFileAtomic(e.p(dockerBlockDropIn), []byte(dropIn), 0o644); err != nil {
		return err
	}
	return e.Systemctl("daemon-reload")
}

func unblockDockerFiles(e Env, j *Journal) error {
	_ = os.Remove(e.p(switchingMarker))
	_ = os.Remove(e.p(dockerBlockDropIn))
	return e.Systemctl("daemon-reload")
}

func stopLegacy(e Env, j *Journal) error {
	// The manager first (j.Units has it first): it would roll back or restart
	// the agent.
	var names []string
	for _, u := range j.Units {
		names = append(names, u.Name)
		if !e.loaded(u.Name) {
			continue
		}
		if err := e.Systemctl("stop", u.Name); err != nil {
			return err
		}
	}
	if err := e.Systemctl(append([]string{"disable"}, names...)...); err != nil {
		return err
	}
	// mask cannot replace a unit file: move it out of systemd's sight.
	for i, u := range j.Units {
		if !u.movable() || u.MovedAside {
			continue
		}
		if exists(e.p(u.Path)) {
			if err := os.Rename(e.p(u.Path), e.p(u.Path+movedAsideSuffix)); err != nil {
				return err
			}
		}
		j.Units[i].MovedAside = true
		if err := e.saveJournal(j); err != nil {
			return err
		}
	}
	if err := e.Systemctl("daemon-reload"); err != nil {
		return err
	}
	return e.Systemctl(append([]string{"mask"}, names...)...)
}

func restoreLegacyUnits(e Env, j *Journal) error {
	if len(j.Units) == 0 {
		return nil
	}
	var names, enable []string
	for _, u := range j.Units {
		names = append(names, u.Name)
	}
	_ = e.Systemctl(append([]string{"unmask"}, names...)...)
	for i, u := range j.Units {
		if u.MovedAside {
			if exists(e.p(u.Path + movedAsideSuffix)) {
				if err := os.Rename(e.p(u.Path+movedAsideSuffix), e.p(u.Path)); err != nil {
					return err
				}
			}
			j.Units[i].MovedAside = false
		}
		if u.LinkTarget != "" {
			enable = append(enable, u.LinkTarget) // a linked unit file: enable links it again
		} else {
			enable = append(enable, u.Name)
		}
	}
	if err := e.Systemctl("daemon-reload"); err != nil {
		return err
	}
	return e.Systemctl(append([]string{"enable"}, enable...)...)
}

func stopDocker(e Env, j *Journal) error {
	if j.Strategy == StrategyLink {
		return nil // nothing moves under Docker
	}
	dataRoot, err := dockerDataRoot(e, daemonJSON)
	if err != nil {
		return err
	}
	if !underLegacyDir(dataRoot) {
		return nil
	}
	// Recorded first: a failure half-way still has Docker started again.
	j.DockerStopped = true
	if err := e.saveJournal(j); err != nil {
		return err
	}
	for _, unit := range []string{"docker.socket", "docker.service"} {
		if !e.loaded(unit) {
			continue
		}
		if err := e.Systemctl("stop", unit); err != nil {
			return err
		}
	}
	// Mounts dockerd leaves behind below its data-root (after a crash of it):
	// stale now, detach them.
	if e.Unmount != nil && underLegacyDir(dataRoot) {
		if mounts, err := e.MountPoints(); err == nil {
			for _, mount := range mounts {
				if strings.HasPrefix(mount, dataRoot+"/") {
					if err := e.Unmount(mount); err != nil {
						e.logf("migrate: unmounting %s: %v", mount, err)
					}
				}
			}
		}
	}
	// live-restore keeps containers (and their mounts) past dockerd.
	mount, err := mountBelowLegacyDir(e, "")
	if err != nil {
		return err
	}
	if mount != "" {
		return fmt.Errorf("%s is still mounted after Docker stopped (live-restore?)", mount)
	}
	return nil
}

var databaseSuffixes = []string{"", "-wal", "-shm"}

func renameDatabase(e Env, j *Journal) error {
	for _, suffix := range databaseSuffixes {
		from, to := e.p(in("reagent.db"+suffix)), e.p(in("device-agent.db"+suffix))
		if exists(from) {
			if err := os.Rename(from, to); err != nil {
				return err
			}
		}
	}
	return nil
}

func unrenameDatabase(e Env, j *Journal) error {
	for _, suffix := range databaseSuffixes {
		from, to := e.p(in("device-agent.db"+suffix)), e.p(in("reagent.db"+suffix))
		if exists(from) && !exists(to) {
			if err := os.Rename(from, to); err != nil {
				return err
			}
		}
	}
	return nil
}

func moveFolder(e Env, j *Journal) error {
	legacy, current := e.p(layout.LegacyAgentDir), e.p(layout.AgentDir)
	if err := os.MkdirAll(filepath.Dir(current), 0o755); err != nil {
		return err
	}
	if j.Strategy == StrategyLink {
		if !exists(current) {
			if err := os.Symlink(e.p(layout.LegacyAgentDir), current); err != nil {
				return err
			}
		}
		syncDir(filepath.Dir(current))
		return nil
	}
	if info, err := os.Lstat(legacy); err == nil && info.IsDir() {
		if exists(current) {
			return fmt.Errorf("both %s and %s exist", layout.LegacyAgentDir, layout.AgentDir)
		}
		if err := os.Rename(legacy, current); err != nil {
			return err
		}
	}
	if !exists(legacy) {
		if err := os.Symlink(e.p(layout.AgentDir), legacy); err != nil {
			return err
		}
	}
	syncDir(filepath.Dir(legacy))
	syncDir(filepath.Dir(current))
	return nil
}

func unmoveFolder(e Env, j *Journal) error {
	legacy, current := e.p(layout.LegacyAgentDir), e.p(layout.AgentDir)
	if j.Strategy == StrategyLink {
		if info, err := os.Lstat(current); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return os.Remove(current)
		}
		return nil
	}
	if info, err := os.Lstat(legacy); err == nil && info.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(legacy); err != nil {
			return err
		}
	}
	if info, err := os.Lstat(current); err == nil && info.IsDir() && !exists(legacy) {
		if err := os.Rename(current, legacy); err != nil {
			return err
		}
	}
	syncDir(filepath.Dir(legacy))
	return nil
}

func moveDockerDataRoot(e Env, j *Journal) error {
	if j.Strategy == StrategyLink {
		return nil // /opt/reagent stays where it is
	}
	for _, name := range []string{daemonJSON, daemonNvidiaJSON} {
		path := e.p(name)
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		backup := e.p(filepath.Join(migrateDir, filepath.Base(name)+".bak"))
		if !exists(backup) {
			if err := linuxservice.WriteFileAtomic(backup, data, 0o644); err != nil {
				return err
			}
		}
		var config map[string]any
		if err := json.Unmarshal(data, &config); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		root, _ := config["data-root"].(string)
		if !underLegacyDir(root) {
			continue
		}
		config["data-root"] = layout.AgentDir + strings.TrimPrefix(root, layout.LegacyAgentDir)
		out, err := json.MarshalIndent(config, "", "  ")
		if err != nil {
			return err
		}
		if err := linuxservice.WriteFileAtomic(path, append(out, '\n'), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func restoreDockerDataRoot(e Env, j *Journal) error {
	for _, name := range []string{daemonJSON, daemonNvidiaJSON} {
		backup := e.p(filepath.Join(migrateDir, filepath.Base(name)+".bak"))
		data, err := os.ReadFile(backup)
		if err != nil {
			continue
		}
		if err := linuxservice.WriteFileAtomic(e.p(name), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func copyConfig(e Env, j *Journal) error {
	if j.Image {
		// The image's boot script relinks it from /boot at every boot.
		return nil
	}
	config := e.p(newPaths.Config())
	info, err := os.Lstat(config)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		// Already a file: it holds the device secret, so root only.
		if !j.ConfigWasFile {
			j.ConfigWasFile, j.ConfigMode = true, info.Mode().Perm()
			if err := e.saveJournal(j); err != nil {
				return err
			}
		}
		return os.Chmod(config, 0o600)
	}
	data, err := os.ReadFile(config) // through the link
	if err != nil {
		return err
	}
	return linuxservice.WriteFileAtomic(config, data, 0o600) // the rename replaces the link
}

func relinkConfig(e Env, j *Journal) error {
	if j.ConfigTarget == "" {
		if !j.ConfigWasFile {
			return nil
		}
		for _, path := range []string{e.p(legacyConfigLink), e.p(newPaths.Config())} {
			if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
				return os.Chmod(path, j.ConfigMode.Perm())
			}
		}
		return nil
	}
	// Runs before unmove-folder, after it when a rollback resumes: the file
	// may be in either folder.
	for _, path := range []string{e.p(legacyConfigLink), e.p(newPaths.Config())} {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || e.unroot(path) == j.ConfigTarget {
			continue
		}
		tmp := path + ".link"
		_ = os.Remove(tmp)
		if err := os.Symlink(e.p(j.ConfigTarget), tmp); err != nil {
			return err
		}
		if err := os.Rename(tmp, path); err != nil {
			return err
		}
	}
	return nil
}

func moveLogs(e Env, j *Journal) error {
	if err := os.MkdirAll(e.p(layout.LogDir), 0o755); err != nil {
		return err
	}
	for from, to := range logMoves(e) {
		if exists(e.p(from)) && !exists(e.p(to)) {
			if err := os.Rename(e.p(from), e.p(to)); err != nil {
				return err
			}
		}
	}
	return nil
}

func unmoveLogs(e Env, j *Journal) error {
	for from, to := range logMoves(e) {
		if exists(e.p(to)) && !exists(e.p(from)) {
			if err := os.Rename(e.p(to), e.p(from)); err != nil {
				return err
			}
		}
	}
	return nil
}

// logMoves maps the legacy log files to their new names.
func logMoves(e Env) map[string]string {
	moves := map[string]string{
		legacyLog:     filepath.Join(layout.LogDir, "device-agent.log"),
		legacyFrpcLog: filepath.Join(layout.LogDir, "frpc.log"),
	}
	rotated, _ := filepath.Glob(e.p("/var/log/reagent-*.log"))
	for _, path := range rotated {
		name := strings.TrimPrefix(filepath.Base(path), "reagent-")
		moves[e.unroot(path)] = filepath.Join(layout.LogDir, "device-agent-"+name)
	}
	moved, _ := filepath.Glob(e.p(filepath.Join(layout.LogDir, "device-agent-*.log")))
	for _, path := range moved {
		name := strings.TrimPrefix(filepath.Base(path), "device-agent-")
		moves["/var/log/reagent-"+name] = e.unroot(path)
	}
	return moves
}

func unblockDocker(e Env, j *Journal) error {
	if err := unblockDockerFiles(e, j); err != nil {
		return err
	}
	// Also when this step did not stop Docker: a boot during the switch kept
	// it from starting.
	err := e.startDocker()
	if j.DockerStopped {
		return err
	}
	return nil
}

// restopDocker: the folder is about to move back, Docker must not hold it.
func restopDocker(e Env, j *Journal) error {
	if !j.DockerStopped {
		return nil
	}
	if e.loaded("docker.socket") {
		_ = e.Systemctl("stop", "docker.socket")
	}
	return e.Systemctl("stop", "docker.service")
}

func startAgent(e Env, j *Journal) error {
	// No removal of an old ready file: RuntimeDirectory= empties its folder
	// at every unit start, and an agent the boot started already wrote its.
	if err := e.Systemctl("enable", linuxservice.UnitName); err != nil {
		return err
	}
	return e.Systemctl("start", linuxservice.UnitName)
}

func stopAgent(e Env, j *Journal) error {
	_ = e.Systemctl("stop", linuxservice.UnitName)
	return e.Systemctl("disable", linuxservice.UnitName)
}

// waitHealthy: the agent the new unit started reports that it is connected
// to IronFlock (the connected file) as the unit's main PID, at the version
// that migrates, and is still that process a minute later.
func waitHealthy(e Env, j *Journal) error {
	// A no-op when it runs; after a power cut the boot may not have got to it.
	if err := e.Systemctl("start", linuxservice.UnitName); err != nil {
		return err
	}
	deadline := e.Now().Add(e.HealthTimeout)
	var pid int
	for {
		readyPID, version := readConnected(e)
		mainPID, err := e.MainPID(linuxservice.UnitName)
		if err == nil && mainPID > 0 && readyPID == mainPID && version == j.Version {
			pid = mainPID
			break
		}
		if !e.Now().Before(deadline) {
			return fmt.Errorf("the agent did not connect to IronFlock within %s", e.HealthTimeout)
		}
		e.Sleep(2 * time.Second)
	}
	e.Sleep(e.StableFor)
	mainPID, err := e.MainPID(linuxservice.UnitName)
	if err != nil {
		return err
	}
	if mainPID != pid {
		return fmt.Errorf("the agent restarted within its first %s (pid %d -> %d)", e.StableFor, pid, mainPID)
	}
	return nil
}

func readConnected(e Env) (int, string) {
	data, err := os.ReadFile(e.p(connectedFile))
	if err != nil {
		return 0, ""
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		return 0, ""
	}
	pid, _ := strconv.Atoi(fields[0])
	return pid, fields[1]
}

// cleanup removes what only the legacy layout used. From here on there is no
// way back to it, and nothing may fail the migration any more.
func cleanup(e Env, j *Journal) error {
	dir := e.p(layout.AgentDir)
	// The legacy binaries and their links; on an OS image nothing else (its
	// boot scripts use the rest).
	legacyFiles, _ := filepath.Glob(filepath.Join(dir, "reagent-*"))
	legacyFiles = append(legacyFiles, filepath.Join(dir, "Reagent-active"), filepath.Join(dir, "Reagent-latest"), filepath.Join(dir, "Reagent-previous"))
	if !j.Image {
		for _, name := range []string{"reswarm-mode", "device-config.ini", "device-config.reswarm"} {
			legacyFiles = append(legacyFiles, filepath.Join(dir, name))
		}
	}
	for _, path := range legacyFiles {
		_ = os.Remove(path)
	}
	if !j.Image {
		_ = os.RemoveAll(filepath.Join(dir, "reswarmify"))
		_ = os.RemoveAll(e.p(legacyDropInDir))
		// The units stay masked on an image (its boot script would enable
		// them again); elsewhere they go with their files.
		var names []string
		for _, u := range j.Units {
			names = append(names, u.Name)
			if u.MovedAside {
				_ = os.Remove(e.p(u.Path + movedAsideSuffix))
			}
		}
		_ = e.Systemctl(append([]string{"unmask"}, names...)...)

		// The secrets ironflock-init copied to /boot: the .flock (the
		// device's private key and secret) and device.ini (the secret as a
		// password); the key the legacy installer extracted to /root. Kept
		// while another unit still runs something in /opt/reagent: a legacy
		// boot script may relink the config from /boot (or remove it when
		// /boot has none).
		if users := legacyDirUnits(e, j); len(users) > 0 {
			e.logf("migrate: keeping the /boot copies of the config: %s still use %s", strings.Join(users, ", "), layout.LegacyAgentDir)
		} else {
			if strings.HasPrefix(j.ConfigTarget, "/boot/") {
				_ = os.Remove(e.p(j.ConfigTarget))
			}
			_ = os.Remove(e.p(bootDeviceINI))
		}
		_ = os.Remove(e.p(rootKeyCopy))
		_ = os.Remove(e.p(rootPubCopy))
	}

	// Done before the unit goes: a crash after this must not leave a
	// journal that says "in progress" with no unit to finish it.
	j.Done = true
	if err := e.saveJournal(j); err != nil {
		e.logf("migrate: saving the journal: %v", err)
	}
	_ = e.Systemctl("disable", MigrateUnitName)
	_ = os.Remove(e.p(migrateUnitPath))
	_ = os.RemoveAll(e.p(migrateDir))
	_ = e.Systemctl("daemon-reload")
	return nil
}

// --- helpers -----------------------------------------------------------------

func dockerDataRoot(e Env, name string) (string, error) {
	data, err := os.ReadFile(e.p(name))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	root, _ := config["data-root"].(string)
	return root, nil
}

// mountBelowLegacyDir returns a mount point at or below /opt/reagent that is
// not below skip (Docker's data-root, "" for none).
func mountBelowLegacyDir(e Env, skip string) (string, error) {
	mounts, err := e.MountPoints()
	if err != nil {
		return "", err
	}
	for _, mount := range mounts {
		if !underLegacyDir(mount) {
			continue
		}
		// Strictly below: the data-root itself on a partition of its own
		// stays mounted when Docker stops.
		if skip != "" && underLegacyDir(skip) && strings.HasPrefix(mount, skip+"/") {
			continue
		}
		return mount, nil
	}
	return "", nil
}

func underLegacyDir(path string) bool {
	return path == layout.LegacyAgentDir || strings.HasPrefix(path, layout.LegacyAgentDir+"/")
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func syncDir(path string) {
	if dir, err := os.Open(path); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(tmp, mode)
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

func stepIndex(name string) int {
	for i, s := range steps {
		if s.name == name {
			return i
		}
	}
	return -1
}

// legacyDirUnits lists the enabled units, other than the legacy agent's,
// whose unit file refers to /opt/reagent.
func legacyDirUnits(e Env, j *Journal) []string {
	own := map[string]bool{legacyAgentUnit: true, legacyManagerUnit: true, linuxservice.UnitName: true, MigrateUnitName: true}
	var users []string
	wants, _ := filepath.Glob(e.p(systemUnitDir + "/*.wants/*.service"))
	seen := map[string]bool{}
	for _, link := range wants {
		name := filepath.Base(link)
		if own[name] || seen[name] {
			continue
		}
		seen[name] = true
		data, err := os.ReadFile(link) // through the link
		if err == nil && strings.Contains(string(data), layout.LegacyAgentDir+"/") {
			users = append(users, name)
		}
	}
	return users
}
