package winmigrate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reagent/winservice"
	"strconv"
	"strings"
	"time"

	"github.com/Masterminds/semver"
)

// legacyPaths are the folders and files the legacy service's ImagePath names.
type legacyPathSet struct {
	AgentDir, AppsDir, Config string
}

func legacyPaths(cfg ServiceConfig) legacyPathSet {
	agentDir := trimSeparators(flagValue(cfg.Args, "-agentDir", filepath.Dir(cfg.Exe)))
	return legacyPathSet{
		AgentDir: agentDir,
		AppsDir:  trimSeparators(flagValue(cfg.Args, "-appsDir", filepath.Join(agentDir, "apps"))),
		Config:   flagValue(cfg.Args, "-config", filepath.Join(agentDir, winservice.ConfigName)),
	}
}

// trimSeparators drops a folder's trailing separators: the installer keeps
// `-agentDir C:\…\Reagent\` verbatim in the ImagePath, and the plan's suffix
// arithmetic (…\DeviceAgent + \apps) and every comparison need the bare
// folder. A drive root keeps its separator.
func trimSeparators(path string) string {
	trimmed := strings.TrimRight(path, `\/`)
	if trimmed == "" || strings.HasSuffix(trimmed, ":") {
		return path
	}
	return trimmed
}

// newAgentSpec is the ironflock-device-agent service as register-service
// makes it: the new folder's paths, the legacy ImagePath's other flags, the
// legacy environment without the test hooks; demand start until start-agent.
func newAgentSpec(j *Journal) winservice.Spec {
	spec := winservice.AgentSpec(j.New.AgentDir, j.New.AppsDir, extraArgs(j.Legacy.Args), withoutHooks(j.Legacy.Env))
	spec.StartType = winservice.StartManual
	spec.Delayed = false
	return spec
}

// --- prepare ------------------------------------------------------------------

// minRenamedVersion: a downloaded reagent-v<v>.exe from before 1.4.0 would
// take the names from the OS and refuse every update once renamed; such
// downloads are dropped, newer ones renamed with the rest.
var minRenamedVersion = semver.MustParse("1.4.0")

func prepare(e Env, j *Journal) error {
	if !j.Planned {
		cfg, err := e.Sys.Service(winservice.LegacyServiceName)
		if err != nil {
			return fmt.Errorf("reading the %s service: %w", winservice.LegacyServiceName, err)
		}
		status, _ := e.Sys.ServiceStatus(winservice.LegacyServiceName)
		paths := legacyPaths(cfg)
		// Check refused an update under way, and the agent activates none
		// while the journal exists; a swap that began just before the journal
		// was written may still be renaming the executables.
		if phase := e.updatePhase(paths.AgentDir); phase != "" {
			return fmt.Errorf("an agent update is %s in %s", phase, paths.AgentDir)
		}
		j.Legacy = LegacyService{
			AgentDir:  paths.AgentDir,
			AppsDir:   paths.AppsDir,
			Config:    paths.Config,
			Exe:       cfg.Exe,
			Args:      cfg.Args,
			Env:       cfg.Env,
			StartType: cfg.StartType.String(),
			Delayed:   cfg.Delayed,
			PID:       status.PID,
		}
		j.Strategy, j.StrategyReason = chooseStrategy(e, j)
		j.New = NewService{AgentDir: j.Legacy.AgentDir, AppsDir: j.Legacy.AppsDir}
		if j.Strategy == StrategyMove {
			j.New.AgentDir = e.Layout.NewDir()
			if underDir(j.Legacy.AppsDir, j.Legacy.AgentDir) {
				j.New.AppsDir = e.Layout.NewDir() + j.Legacy.AppsDir[len(j.Legacy.AgentDir):]
			}
		}
		j.Planned = true
		if err := e.saveJournal(j); err != nil {
			return err
		}
		e.logf("migrate: strategy %s (%s): %s -> %s", j.Strategy, j.StrategyReason, j.Legacy.AgentDir, j.New.AgentDir)
	}
	legacyDir := j.Legacy.AgentDir

	// The repair task runs the universal script during the move; what it
	// ran before is kept for the rollback.
	legacyRepair := filepath.Join(legacyDir, winservice.RepairName)
	if !j.RepairBackedUp {
		if e.isFile(legacyRepair) {
			if err := e.Sys.CopyFile(legacyRepair, e.Layout.RepairBackup(), 0o644); err != nil {
				return err
			}
		}
		j.RepairBackedUp = true
		if err := e.saveJournal(j); err != nil {
			return err
		}
	}
	if !j.TaskBackedUp {
		xml, err := e.Sys.ExportTask(winservice.RepairTaskName)
		if err == nil {
			if err := e.Sys.WriteFile(e.Layout.TaskBackup(), xml, 0o644); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			e.logf("migrate: exporting the repair task: %v", err)
		}
		j.TaskBackedUp = true
		if err := e.saveJournal(j); err != nil {
			return err
		}
	}
	script := winservice.UniversalRepairCmd(e.Layout.Root, legacyDir, j.New.AgentDir)
	if err := e.Sys.WriteFile(e.Layout.RepairCmd(), []byte(script), 0o644); err != nil {
		return err
	}
	if err := e.Sys.CreateTask(winservice.RepairTaskName, e.Layout.RepairCmd()); err != nil {
		return fmt.Errorf("pointing the repair task at %s: %w", e.Layout.RepairCmd(), err)
	}

	// The rollback target of the new service is this very version: a 1.3.x
	// reagent-prev.exe renamed into the new folder would run under the new
	// service and refuse every update.
	prev := filepath.Join(legacyDir, "device-agent-prev.exe")
	if err := e.Sys.Remove(prev); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := e.Sys.CopyFile(e.Layout.MigratorExe(), prev, 0o755); err != nil {
		return fmt.Errorf("writing %s: %w", prev, err)
	}

	// Legacy update files that would block updates once renamed.
	entries, err := e.Sys.ReadDir(legacyDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !droppedLegacyFile(entry.Name()) {
			continue
		}
		if err := e.Sys.Remove(filepath.Join(legacyDir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	downloads := filepath.Join(legacyDir, "downloads")
	if entries, err := e.Sys.ReadDir(downloads); err == nil {
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasPrefix(strings.ToLower(entry.Name()), "reagent-") {
				continue
			}
			if err := e.Sys.Remove(filepath.Join(downloads, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

// droppedLegacyFile: reagent-prev.exe, reagent-failed-v<v>.exe, and
// reagent-v<v>.exe before 1.4.0.
func droppedLegacyFile(name string) bool {
	lower := strings.ToLower(name)
	if lower == "reagent-prev.exe" || strings.HasPrefix(lower, "reagent-failed-v") && strings.HasSuffix(lower, ".exe") {
		return true
	}
	if version, ok := downloadVersion(lower, "reagent"); ok {
		parsed, err := semver.NewVersion(version)
		return err != nil || parsed.LessThan(minRenamedVersion)
	}
	return false
}

// downloadVersion returns v of "<prefix>-v<v>.exe".
func downloadVersion(lower string, prefix string) (string, bool) {
	if !strings.HasPrefix(lower, prefix+"-v") || !strings.HasSuffix(lower, ".exe") {
		return "", false
	}
	return strings.TrimSuffix(strings.TrimPrefix(lower, prefix+"-v"), ".exe"), true
}

// chooseStrategy: the folder moves unless it is not the default one (a
// custom -agentDir), the new folder is already in use, another volume would
// make the rename a copy, an earlier attempt could not move it, or Docker
// Desktop's Hyper-V backend would not share the new folder with containers.
func chooseStrategy(e Env, j *Journal) (string, string) {
	if j.PreferInPlace {
		return StrategyInPlace, "an earlier attempt could not move the folder"
	}
	if !samePath(j.Legacy.AgentDir, e.Layout.LegacyDir()) {
		return StrategyInPlace, "the agent folder is " + j.Legacy.AgentDir + ", not " + e.Layout.LegacyDir()
	}
	if e.isDir(e.Layout.NewDir()) {
		if entries, err := e.Sys.ReadDir(e.Layout.NewDir()); err != nil || len(entries) > 0 {
			return StrategyInPlace, e.Layout.NewDir() + " exists and is not empty"
		}
	}
	same, err := e.Sys.SameVolume(j.Legacy.AgentDir, e.Layout.Root)
	if err != nil {
		return StrategyInPlace, "the volumes are unreadable: " + err.Error()
	}
	if !same {
		return StrategyInPlace, j.Legacy.AgentDir + " is on another volume than " + e.Layout.Root
	}
	if hyperV, shared := e.Sys.DockerDesktopSharing(); hyperV {
		covered := false
		for _, dir := range shared {
			if underDir(e.Layout.NewDir(), dir) {
				covered = true
			}
		}
		if !covered {
			return StrategyInPlace, "Docker Desktop's file sharing does not include " + e.Layout.NewDir()
		}
	}
	return StrategyMove, "the default folder"
}

func unprepare(e Env, j *Journal) error {
	legacyDir := j.Legacy.AgentDir
	if legacyDir == "" || !e.isDir(legacyDir) {
		// The folder is not back at its place (a failed move-back): its
		// files stay as they are for the new service.
		return nil
	}
	var errs []error
	legacyScript := filepath.Join(legacyDir, winservice.RepairName)
	restored := false
	if e.isFile(e.Layout.TaskBackup()) {
		xml, err := e.Sys.ReadFile(e.Layout.TaskBackup())
		if err == nil {
			err = e.Sys.ImportTask(winservice.RepairTaskName, xml)
		}
		if err != nil {
			e.logf("migrate: restoring the repair task from its definition: %v; registering it anew", err)
		} else {
			restored = true
		}
	}
	if !restored && (e.isFile(legacyScript) || e.isFile(e.Layout.RepairBackup())) {
		if err := e.Sys.CreateTask(winservice.RepairTaskName, legacyScript); err != nil {
			errs = append(errs, fmt.Errorf("restoring the repair task: %w", err))
		}
	}
	if e.isFile(e.Layout.RepairBackup()) {
		if err := e.Sys.CopyFile(e.Layout.RepairBackup(), filepath.Join(legacyDir, winservice.RepairName), 0o644); err != nil {
			errs = append(errs, fmt.Errorf("restoring %s: %w", winservice.RepairName, err))
		}
	}
	prev := filepath.Join(legacyDir, "device-agent-prev.exe")
	legacyExe := filepath.Join(legacyDir, winservice.LegacyExeName)
	legacyPrev := filepath.Join(legacyDir, "reagent-prev.exe")
	switch {
	case !e.isFile(legacyExe) && e.isFile(prev):
		// The executable was lost during the attempt (quarantined): the
		// rollback target, a copy of this very version, runs under the
		// legacy name as well. Never removed while it is the only one.
		if err := e.Sys.Rename(prev, legacyExe); err != nil {
			errs = append(errs, fmt.Errorf("restoring %s from %s: %w", legacyExe, prev, err))
		} else {
			e.logf("migrate: %s was missing; restored from the move's rollback target", legacyExe)
		}
	case e.isFile(prev) && !e.isFile(legacyPrev):
		// prepare dropped the legacy spare binaries: the rollback target (this
		// version) becomes the spare the legacy repair script and a self-update
		// rollback look for, so a later loss of reagent.exe is still repaired.
		if err := e.Sys.Rename(prev, legacyPrev); err != nil {
			errs = append(errs, fmt.Errorf("keeping %s as %s: %w", prev, legacyPrev, err))
		}
	default:
		if err := e.Sys.Remove(prev); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// --- stop-legacy ----------------------------------------------------------------

// stopWait is how long a stopped service's process may take to exit before
// it is terminated; killWait how long the termination may take.
const (
	stopWait = 30 * time.Second
	killWait = 2 * time.Minute
)

func stopLegacy(e Env, j *Journal) error {
	// Disabled first: the SCM's recovery actions would otherwise restart it
	// after the termination below.
	if err := e.Sys.SetStartType(winservice.LegacyServiceName, winservice.StartDisabled, false); err != nil {
		return err
	}
	if err := e.Sys.StopService(winservice.LegacyServiceName); err != nil {
		// A stop that hangs or is refused (the service still starting) does
		// not keep the move: the process is terminated below, like any one
		// that outlives its stop; disabled, recovery cannot bring it back.
		e.logf("migrate: stopping %s: %v; terminating its processes", winservice.LegacyServiceName, err)
	}
	// The SCM reports STOPPED before the process is gone, and frpc may
	// outlive the agent: wait, then terminate what is left under the folder.
	return e.clearProcessesUnder(j.Legacy.AgentDir)
}

// clearProcessesUnder waits stopWait for the processes running from dir to
// exit, terminates the rest and waits for them to be gone.
func (e Env) clearProcessesUnder(dir string) error {
	deadline := e.Sys.Now().Add(stopWait)
	for {
		processes, err := e.Sys.ProcessesUnder(dir)
		if err != nil {
			return err
		}
		if len(processes) == 0 {
			return nil
		}
		if !e.Sys.Now().Before(deadline) {
			break
		}
		if e.cancelled() {
			return ErrCancelled
		}
		e.Sys.Sleep(time.Second)
	}
	processes, err := e.Sys.ProcessesUnder(dir)
	if err != nil {
		return err
	}
	for _, p := range processes {
		e.logf("migrate: terminating %s (pid %d)", p.Exe, p.PID)
		if err := e.Sys.KillProcess(p.PID); err != nil {
			e.logf("migrate: terminating pid %d: %v", p.PID, err)
		}
	}
	deadline = e.Sys.Now().Add(killWait)
	for {
		processes, err := e.Sys.ProcessesUnder(dir)
		if err != nil {
			return err
		}
		if len(processes) == 0 {
			return nil
		}
		if !e.Sys.Now().Before(deadline) {
			names := make([]string, 0, len(processes))
			for _, p := range processes {
				names = append(names, fmt.Sprintf("%s (pid %d)", filepath.Base(p.Exe), p.PID))
			}
			return fmt.Errorf("processes still run from %s: %s", dir, strings.Join(names, ", "))
		}
		e.Sys.Sleep(time.Second)
	}
}

func restoreLegacyStart(e Env, j *Journal) error {
	if _, err := e.Sys.Service(winservice.LegacyServiceName); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	e.restoreLegacyDisplayName()
	return e.Sys.SetStartType(winservice.LegacyServiceName, legacyStartType(j), j.Legacy.Delayed)
}

// takeDisplayName lets the legacy service give "IronFlock Device Agent" up
// before a service of the new name is created, so services.msc never lists
// the name twice. Cosmetic (Windows accepts a duplicate display name, seen on
// the canary device): a failure is logged and never fails the migration.
func (e Env) takeDisplayName() error {
	if _, err := e.Sys.Service(winservice.LegacyServiceName); err != nil {
		return nil
	}
	if err := e.Sys.SetDisplayName(winservice.LegacyServiceName, winservice.LegacyDisplayName); err != nil {
		e.logf("migrate: renaming the %s service (cosmetic): %v", winservice.LegacyServiceName, err)
	}
	return nil
}

// restoreLegacyDisplayName gives the legacy service its display name back
// once no service of the new name holds it.
func (e Env) restoreLegacyDisplayName() {
	if _, err := e.Sys.Service(winservice.ServiceName); err == nil {
		return
	}
	if _, err := e.Sys.Service(winservice.LegacyServiceName); err != nil {
		return
	}
	if err := e.Sys.SetDisplayName(winservice.LegacyServiceName, winservice.DisplayName); err != nil {
		e.logf("migrate: restoring the display name of %s: %v", winservice.LegacyServiceName, err)
	}
}

// --- stop-apps ------------------------------------------------------------------

// stopApps records and takes away the restart policy of every container
// bound into the folder, and stops it: Docker would otherwise start it at
// boot during the move and recreate the old folder under its bind mounts.
// An unreachable Docker is recorded and does not stop the move: a stray
// folder is handled by the rollback.
func stopApps(e Env, j *Journal) error {
	return e.stopContainersUnder(j, j.Legacy.AgentDir)
}

func (e Env) stopContainersUnder(j *Journal, dir string) error {
	containers, err := e.Sys.Containers()
	if err != nil {
		e.logf("migrate: Docker is not reachable (%v); the containers keep their restart policies", err)
		j.DockerUnavailable = true
		return e.saveJournal(j)
	}
	for _, c := range containers {
		if !bindsUnder(c, dir) {
			continue
		}
		if !j.recorded(c.ID) {
			j.Containers = append(j.Containers, ContainerRecord{ID: c.ID, Name: c.Name, RestartPolicy: c.RestartPolicy, Dir: dir})
			if err := e.saveJournal(j); err != nil {
				return err
			}
		}
		if c.RestartPolicy != "no" && c.RestartPolicy != "" {
			if err := e.Sys.SetRestartPolicy(c.ID, "no"); err != nil {
				e.logf("migrate: taking the restart policy of %s: %v", c.Name, err)
				j.DockerUnavailable = true
				continue
			}
		}
		if c.Running {
			if err := e.Sys.StopContainer(c.ID); err != nil {
				e.logf("migrate: stopping %s: %v", c.Name, err)
				j.DockerUnavailable = true
			}
		}
	}
	return e.saveJournal(j)
}

func (j *Journal) recorded(id string) bool {
	for _, c := range j.Containers {
		if c.ID == id {
			return true
		}
	}
	return false
}

func bindsUnder(c Container, dir string) bool {
	for _, bind := range c.Binds {
		if underDir(bind, dir) {
			return true
		}
	}
	return false
}

func restoreApps(e Env, j *Journal) error {
	if j.Strategy == StrategyMove {
		// The containers the new agent made bind the new folder, which the
		// rollback moves back: with a restart policy Docker would start them
		// at its next start and recreate the new folder under their mounts.
		// The legacy agent recreates them (the marker; compose by its config
		// hash) with their own policies.
		for i, c := range j.Containers {
			if !c.Restored && c.Dir != "" && samePath(c.Dir, j.New.AgentDir) && !samePath(c.Dir, j.Legacy.AgentDir) {
				e.logf("migrate: %s binds %s, which goes away; left to the agent, which recreates it", c.Name, c.Dir)
				j.Containers[i].Restored = true
			}
		}
	}
	e.restoreRestartPolicies(j)
	return e.saveJournal(j)
}

// --- rename-files ---------------------------------------------------------------

// renamedSuffixes are what follows the binary name in the files that change
// their name: the database, the logs (lumberjack backups are
// <name>-<timestamp>.log), the executable and downloaded versions.
func renamedSuffix(lower string, prefix string) (string, bool) {
	if !strings.HasPrefix(lower, prefix) {
		return "", false
	}
	rest := lower[len(prefix):]
	switch rest {
	case ".db", ".db-wal", ".db-shm", ".log", ".exe":
		return rest, true
	}
	if strings.HasPrefix(rest, "-") && strings.HasSuffix(rest, ".log") {
		return rest, true
	}
	if strings.HasPrefix(rest, "-v") && strings.HasSuffix(rest, ".exe") {
		return rest, true
	}
	if strings.HasPrefix(rest, "-failed-v") && strings.HasSuffix(rest, ".exe") {
		return rest, true
	}
	return "", false
}

// fileRenames lists the renames of dir's files from one binary name to the
// other, in a stable order.
func (e Env) fileRenames(dir string, from, to string) ([][2]string, error) {
	entries, err := e.Sys.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var renames [][2]string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		lower := strings.ToLower(entry.Name())
		rest, ok := renamedSuffix(lower, from)
		if !ok || strings.HasPrefix(rest, "-prev.") {
			continue
		}
		// The suffix in its own case (a lumberjack timestamp has a T).
		rest = entry.Name()[len(from):]
		renames = append(renames, [2]string{filepath.Join(dir, entry.Name()), filepath.Join(dir, to+rest)})
	}
	return renames, nil
}

func renameFiles(e Env, j *Journal) error {
	renames, err := e.fileRenames(j.Legacy.AgentDir, "reagent", "device-agent")
	if err != nil {
		return err
	}
	for _, r := range renames {
		if err := e.renameNoClobber(j, r[0], r[1]); err != nil {
			return err
		}
	}
	return nil
}

func unrenameFiles(e Env, j *Journal) error {
	if !e.isDir(j.Legacy.AgentDir) {
		// The folder is not back at its place (a failed move-back): the new
		// service keeps its names.
		return nil
	}
	renames, err := e.fileRenames(j.Legacy.AgentDir, "device-agent", "reagent")
	if err != nil {
		return err
	}
	// Every file that can go back goes back: one refused rename (a scan
	// holding the executable) must not keep the database or the log from
	// their names.
	var errs []error
	for _, r := range renames {
		if err := e.renameNoClobber(j, r[0], r[1]); err != nil {
			e.logf("migrate: renaming %s back: %v", r[0], err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// renameNoClobber renames from to to without ever overwriting. An existing
// destination is moved aside (<to>.stale-<time>); for executables the one
// reporting the journal's version keeps the destination name. A rename
// refused for a while (an antivirus scanning the file) is retried.
func (e Env) renameNoClobber(j *Journal, from, to string) error {
	if !e.exists(from) {
		return nil
	}
	if e.exists(to) {
		if strings.HasSuffix(strings.ToLower(to), ".exe") {
			if version, err := e.Sys.ExecVersion(to); err == nil && version == j.Version {
				if fromVersion, err := e.Sys.ExecVersion(from); err != nil || fromVersion != j.Version {
					e.logf("migrate: %s is v%s already; %s moved aside", to, version, from)
					return e.moveAside(from)
				}
			}
		}
		e.logf("migrate: %s exists; moved aside", to)
		if err := e.moveAside(to); err != nil {
			return err
		}
	}
	return e.renameWithBackoff(from, to)
}

func (e Env) moveAside(path string) error {
	stamp := e.Sys.Now().UTC().Format("20060102-150405")
	for n := 0; n < 100; n++ {
		aside := fmt.Sprintf("%s.stale-%s", path, stamp)
		if n > 0 {
			aside += "-" + strconv.Itoa(n)
		}
		if e.exists(aside) {
			continue
		}
		return e.Sys.Rename(path, aside)
	}
	return fmt.Errorf("no free name to move %s aside", path)
}

// --- move-folder ----------------------------------------------------------------

// renameRetry: a folder rename blocked by an open handle (Explorer, an
// antivirus scan, a container that came back) is retried with backoff for
// about two minutes before the holders are named.
var renameRetryDelays = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second, 30 * time.Second, 60 * time.Second}

func (e Env) renameWithBackoff(from, to string) error {
	var err error
	for attempt := 0; ; attempt++ {
		err = e.Sys.Rename(from, to)
		if err == nil || errors.Is(err, os.ErrExist) || errors.Is(err, os.ErrNotExist) {
			return err
		}
		if attempt >= len(renameRetryDelays) {
			break
		}
		e.logf("migrate: renaming %s: %v; retrying in %s", from, err, renameRetryDelays[attempt])
		e.Sys.Sleep(renameRetryDelays[attempt])
		if e.cancelled() {
			return ErrCancelled
		}
	}
	if holders := e.Sys.HoldersOf(from); len(holders) > 0 {
		return fmt.Errorf("renaming %s: %w (held open by %s)", from, err, strings.Join(holders, ", "))
	}
	return fmt.Errorf("renaming %s: %w", from, err)
}

func moveFolder(e Env, j *Journal) error {
	if j.Strategy != StrategyMove {
		return nil
	}
	legacy, newDir := j.Legacy.AgentDir, j.New.AgentDir
	marker := filepath.Join(newDir, winservice.RecreateMarkerName)
	switch {
	case e.isDir(legacy) && e.isDir(newDir):
		entries, err := e.Sys.ReadDir(newDir)
		if err != nil {
			return err
		}
		if len(entries) > 0 {
			if e.agentFolder(newDir) && !e.agentFolder(legacy) {
				// Moved already; the old folder came back meanwhile (Docker
				// recreated it under a container's bind mount at a boot in
				// between): aside, never deleted.
				if err := e.moveStrayAside(legacy); err != nil {
					return err
				}
				return e.markMoved(j, marker)
			}
			j.PreferInPlace = true
			_ = e.saveJournal(j)
			return fmt.Errorf("both %s and %s exist", legacy, newDir)
		}
		if err := e.Sys.RemoveAll(newDir); err != nil {
			return err
		}
	case !e.isDir(legacy) && e.isDir(newDir):
		// Moved already (a resume after the rename).
		return e.markMoved(j, marker)
	case !e.isDir(legacy):
		return fmt.Errorf("the agent folder %s is gone", legacy)
	}
	if j.DockerUnavailable {
		// Docker was not reachable when the apps were stopped; one that came
		// up since (Docker Desktop at a logon) runs the containers again,
		// with files open under the folder.
		if err := e.stopContainersUnder(j, legacy); err != nil {
			return err
		}
	}
	// Something may have started from the folder meanwhile.
	if err := e.clearProcessesUnder(legacy); err != nil {
		return err
	}
	if err := e.renameWithBackoff(legacy, newDir); err != nil {
		if !errors.Is(err, ErrCancelled) {
			j.PreferInPlace = true
			_ = e.saveJournal(j)
		}
		return err
	}
	return e.markMoved(j, marker)
}

// markMoved records the rename and leaves the recreate marker in the new folder.
func (e Env) markMoved(j *Journal, marker string) error {
	if !j.Moved {
		j.Moved = true
		if err := e.saveJournal(j); err != nil {
			return err
		}
	}
	return e.writeMarker(marker)
}

// agentFolder reports whether dir holds the agent's configuration: the folder
// that moves always does (Check requires it); one Docker made under a bind
// mount, or someone else made, does not.
func (e Env) agentFolder(dir string) bool {
	return e.isFile(filepath.Join(dir, winservice.ConfigName))
}

// moveStrayAside renames a folder that is not the agent's out of the way,
// never deleting it.
func (e Env) moveStrayAside(dir string) error {
	stray := fmt.Sprintf("%s.stray-%s", dir, e.Sys.Now().UTC().Format("20060102-150405"))
	e.logf("migrate: %s came back during the move; renaming it to %s", dir, stray)
	if err := e.Sys.Rename(dir, stray); err != nil {
		return fmt.Errorf("moving the stray %s aside: %w", dir, err)
	}
	return nil
}

// writeMarker leaves the recreate marker at path: an empty file, or, when an
// agent consumed one there already (it then holds the volumes of the
// containers it removed and has not recreated yet), that one with its
// listing reset, so the containers made since, which name the other folder,
// are recreated as well. Never an empty file over a consumed one.
func (e Env) writeMarker(path string) error {
	data, err := e.Sys.ReadFile(path)
	if err != nil || len(bytes.TrimSpace(data)) == 0 {
		return e.Sys.WriteFile(path, nil, 0o644)
	}
	var doc map[string]any
	if json.Unmarshal(data, &doc) == nil {
		doc["listed"] = false
		if kept, err := json.Marshal(doc); err == nil {
			data = kept
		}
	}
	return e.Sys.WriteFile(path, data, 0o644)
}

func unmoveFolder(e Env, j *Journal) error {
	if j.Strategy != StrategyMove {
		return nil
	}
	legacy, newDir := j.Legacy.AgentDir, j.New.AgentDir
	switch {
	case e.isDir(newDir) && e.isDir(legacy) && e.agentFolder(legacy):
		// The agent's files are at the old name (the folder never left, or
		// is back already); what sits at the new name is not theirs and is
		// left as it is.
		e.logf("migrate: %s holds the agent's files; %s is left alone", legacy, newDir)
		if !j.Moved {
			return nil
		}
	case e.isDir(newDir):
		if e.isDir(legacy) {
			// Docker recreated the old folder under a container's bind
			// mount (or someone made one): aside, never deleted.
			if err := e.moveStrayAside(legacy); err != nil {
				return err
			}
		}
		if err := e.clearProcessesUnder(newDir); err != nil {
			e.logf("migrate: %v", err)
		}
		if err := e.renameWithBackoff(newDir, legacy); err != nil {
			return fmt.Errorf("moving the folder back: %w", err)
		}
	case !j.Moved || !e.isDir(legacy):
		return nil
	}
	// The legacy agent recreates its containers: their bind mounts name the
	// new folder now.
	if err := e.writeMarker(filepath.Join(legacy, winservice.RecreateMarkerName)); err != nil {
		return err
	}
	// Moved back: a rerun of the rollback (the migrate service's every start
	// until the legacy agent runs) leaves the marker, which the legacy agent
	// may have consumed by then, alone. Saved with the journal by the caller.
	j.Moved = false
	return nil
}

// --- register-service -----------------------------------------------------------

// markedForDeleteWait: a service of the new name that still waits for its
// deletion (the SCM deletes a stopped service with a delay) is retried.
const markedForDeleteWait = 2 * time.Minute

func registerService(e Env, j *Journal) error {
	if !j.CreatedService {
		// Recorded before the service exists: a crash in between must not
		// leave a service this journal does not own (Check).
		j.CreatedService = true
		if err := e.saveJournal(j); err != nil {
			return err
		}
	}
	if err := e.takeDisplayName(); err != nil {
		return err
	}
	spec := newAgentSpec(j)
	deadline := e.Sys.Now().Add(markedForDeleteWait)
	for {
		err := e.Sys.CreateOrUpdateService(spec)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrServiceMarkedForDelete) || !e.Sys.Now().Before(deadline) {
			return err
		}
		e.logf("migrate: %v; retrying", err)
		e.Sys.Sleep(5 * time.Second)
	}
	if err := e.Sys.InstallEventSource(winservice.ServiceName); err != nil {
		e.logf("migrate: registering the event source: %v", err)
	}
	if err := e.Sys.AddDefenderExclusion(filepath.Join(j.New.AgentDir, "frpc.exe")); err != nil {
		e.logf("migrate: adding the Defender exclusion for frpc: %v", err)
	}
	return nil
}

func unregisterService(e Env, j *Journal) error {
	if _, err := e.Sys.Service(winservice.ServiceName); errors.Is(err, os.ErrNotExist) {
		e.restoreLegacyDisplayName()
		j.CreatedService = false
		return e.saveJournal(j)
	} else if err != nil {
		return err
	}
	if !j.CreatedService {
		return nil
	}
	// Disabled first: recovery must not restart it while it goes.
	if err := e.Sys.SetStartType(winservice.ServiceName, winservice.StartDisabled, false); err != nil {
		return err
	}
	if err := e.Sys.StopService(winservice.ServiceName); err != nil {
		e.logf("migrate: stopping %s: %v", winservice.ServiceName, err)
	}
	if err := e.clearProcessesUnder(j.New.AgentDir); err != nil {
		e.logf("migrate: %v", err)
	}
	if err := e.Sys.DeleteService(winservice.ServiceName); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := e.Sys.RemoveEventSource(winservice.ServiceName); err != nil && !errors.Is(err, os.ErrNotExist) {
		e.logf("migrate: removing the event source: %v", err)
	}
	if j.Strategy == StrategyMove {
		if err := e.Sys.RemoveDefenderExclusion(filepath.Join(j.New.AgentDir, "frpc.exe")); err != nil {
			e.logf("migrate: removing the Defender exclusion: %v", err)
		}
	}
	e.restoreLegacyDisplayName()
	j.CreatedService = false
	return e.saveJournal(j)
}

// --- start-agent ----------------------------------------------------------------

func startAgent(e Env, j *Journal) error {
	if err := e.Sys.SetStartType(winservice.ServiceName, winservice.StartAutomatic, true); err != nil {
		return err
	}
	return e.Sys.StartService(winservice.ServiceName)
}

// stopAgent, the undo: disabled first so recovery cannot restart it
// mid-rollback, then stopped and terminated, then its apps are stopped
// before the folder moves back under them.
func stopAgent(e Env, j *Journal) error {
	if _, err := e.Sys.Service(winservice.ServiceName); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := e.Sys.SetStartType(winservice.ServiceName, winservice.StartDisabled, false); err != nil {
		return err
	}
	if err := e.Sys.StopService(winservice.ServiceName); err != nil {
		e.logf("migrate: stopping %s: %v", winservice.ServiceName, err)
	}
	if err := e.clearProcessesUnder(j.New.AgentDir); err != nil {
		e.logf("migrate: %v", err)
	}
	return e.stopContainersUnder(j, j.New.AgentDir)
}

// --- health ---------------------------------------------------------------------

type connectedRecord struct {
	pid     int
	version string
	service string
}

func (e Env) readConnected(agentDir string) connectedRecord {
	data, err := e.Sys.ReadFile(filepath.Join(agentDir, winservice.ConnectedName))
	if err != nil {
		return connectedRecord{}
	}
	fields := strings.Fields(string(data))
	if len(fields) != 3 {
		return connectedRecord{}
	}
	pid, _ := strconv.Atoi(fields[0])
	return connectedRecord{pid: pid, version: fields[1], service: fields[2]}
}

// waitHealthy: the agent the new service started reports that it is
// connected to IronFlock as the service's process, at the version that
// migrates, under the new service name, and is still that process a minute
// later. A stale file (an earlier process, a reused pid) cannot pass: all
// three fields must match the SCM's running process.
func waitHealthy(e Env, j *Journal) error {
	// A no-op when it runs; after a power cut the boot may not have got to it.
	if err := e.Sys.StartService(winservice.ServiceName); err != nil {
		return err
	}
	deadline := e.Sys.Now().Add(e.HealthTimeout)
	var pid int
	for {
		if e.cancelled() {
			return ErrCancelled
		}
		record := e.readConnected(j.New.AgentDir)
		status, err := e.Sys.ServiceStatus(winservice.ServiceName)
		if err == nil && status.Running() && status.PID > 0 &&
			record.pid == status.PID && record.version == j.Version && record.service == winservice.ServiceName {
			pid = status.PID
			break
		}
		if !e.Sys.Now().Before(deadline) {
			return fmt.Errorf("the agent did not connect to IronFlock as %s within %s", winservice.ServiceName, e.HealthTimeout)
		}
		e.Sys.Sleep(2 * time.Second)
	}
	e.logf("migrate: %s connected as pid %d; watching it for %s", winservice.ServiceName, pid, e.StableFor)
	e.Sys.Sleep(e.StableFor)
	status, err := e.Sys.ServiceStatus(winservice.ServiceName)
	if err != nil {
		return err
	}
	if !status.Running() || status.PID != pid {
		return fmt.Errorf("the agent restarted within its first %s (pid %d -> %d)", e.StableFor, pid, status.PID)
	}
	return nil
}

// --- cleanup --------------------------------------------------------------------

// cleanup removes what only the legacy layout used. Done first: from here on
// there is no way back, and nothing may fail the migration any more.
func cleanup(e Env, j *Journal) error {
	j.Done = true
	if err := e.saveJournal(j); err != nil {
		// A proven-healthy migration is not rolled back over a journal write
		// (a scanner holding the file): the tail saves again, and a cut
		// before any save lands reruns cleanup from the journal's step.
		e.logf("migrate: recording done: %v", err)
	}
	e.cleanupTail(j)
	return nil
}

// --- path helpers ---------------------------------------------------------------

// restartPolicyString is a container's restart policy as the journal records
// it: Docker's name, with the retry count of on-failure ("on-failure:3"),
// which Docker reports apart from the name.
func restartPolicyString(name string, maximumRetryCount int) string {
	if name == "" {
		return "no"
	}
	if name == "on-failure" && maximumRetryCount > 0 {
		return fmt.Sprintf("on-failure:%d", maximumRetryCount)
	}
	return name
}

// parseRestartPolicy is the inverse: the name and the retry count to give
// Docker back.
func parseRestartPolicy(policy string) (name string, maximumRetryCount int) {
	if policy == "" {
		return "no", 0
	}
	name, count, ok := strings.Cut(policy, ":")
	if !ok || name != "on-failure" {
		return policy, 0
	}
	n, err := strconv.Atoi(count)
	if err != nil || n < 0 {
		return name, 0
	}
	return name, n
}

// hostPathOf maps a path as Docker reports a bind source to a comparable
// host path: lower case, forward slashes; a WSL engine's /mnt/c/…, Docker
// Desktop's /run/desktop/mnt/host/c/… (WSL 2 backend) and /host_mnt/c/…
// (Hyper-V backend) become c:/….
func hostPathOf(path string) string {
	s := strings.ToLower(strings.ReplaceAll(path, `\`, "/"))
	for _, prefix := range []string{"/run/desktop/mnt/host/", "/host_mnt/", "/mnt/"} {
		rest, ok := strings.CutPrefix(s, prefix)
		if !ok || rest == "" || rest[0] < 'a' || rest[0] > 'z' {
			continue
		}
		if len(rest) == 1 || rest[1] == '/' {
			s = rest[:1] + ":" + rest[1:]
			break
		}
	}
	return strings.TrimSuffix(s, "/")
}

// underDir reports whether path is dir or below it, both as Docker or the
// host may spell them.
func underDir(path, dir string) bool {
	p, d := hostPathOf(path), hostPathOf(dir)
	return p == d || strings.HasPrefix(p, d+"/")
}

func samePath(a, b string) bool {
	return hostPathOf(a) == hostPathOf(b)
}
