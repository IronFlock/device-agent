package winmigrate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reagent/winservice"
	"strings"
	"sync"
	"time"
)

// ErrNotEligible wraps why a device is not moved automatically.
var ErrNotEligible = errors.New("not eligible for the layout migration")

// ErrCancelled: the migrate service was asked to stop between two
// operations. The journal persists; the next start resumes.
var ErrCancelled = errors.New("the migration was stopped; it resumes at the next start of the migrate service")

// ErrServiceMarkedForDelete is what System.CreateOrUpdateService returns
// (wrapped) while a service of that name still waits for its deletion: the
// SCM deletes a service only once it stopped.
var ErrServiceMarkedForDelete = errors.New("the service is marked for deletion")

type step struct {
	name string
	do   func(e Env, j *Journal) error
	undo func(e Env, j *Journal) error
}

var steps = []step{
	{"prepare", prepare, unprepare},
	{"stop-legacy", stopLegacy, restoreLegacyStart},
	{"stop-apps", stopApps, restoreApps},
	{"rename-files", renameFiles, unrenameFiles},
	{"move-folder", moveFolder, unmoveFolder},
	{"register-service", registerService, unregisterService},
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

func stepIndex(name string) int {
	for i, s := range steps {
		if s.name == name {
			return i
		}
	}
	return -1
}

func (e Env) withDefaults() Env {
	if e.HealthTimeout == 0 {
		e.HealthTimeout = defaultHealthLimit
	}
	if e.StableFor == 0 {
		e.StableFor = defaultStableFor
	}
	return e
}

// Run continues the migration from its journal: the migrate service's
// `device-agent migrate-layout`. It returns once the migration is done, or
// rolled back (with the error), or was stopped (ErrCancelled). A done
// journal makes it rerun the idempotent cleanup tail; a failed one disables
// the migrate service.
func Run(e Env) error {
	e = e.withDefaults()
	// One engine per machine: the migrate service and a foreground run (or
	// two migrate service starts) must never work one journal at once.
	release, err := e.Sys.LockMigration()
	if err != nil {
		return fmt.Errorf("another migration process is running: %w", err)
	}
	defer release()
	j, err := loadJournal(e.Sys.ReadFile, e.Layout)
	if errors.Is(err, ErrJournalUnreadable) {
		e.logf("migrate: %v; making sure an agent service runs", err)
		e.ensureAnyAgent()
		return err
	}
	if err != nil {
		return err
	}
	if j == nil {
		return nil
	}
	// A final journal: the idempotent tails run again (a power cut may have
	// interrupted them), including ensure-agent when its outcome was never
	// recorded. The new service's executable, lost since (quarantined), is
	// restored from the migrator first: the repair task may still run the
	// universal script, whose migrate branch repairs no agent.
	if j.Done {
		e.restoreMissingExe(j, filepath.Join(j.New.AgentDir, winservice.ExeName))
		e.cleanupTail(j)
		if !j.ensured() {
			e.ensureAgent(j, winservice.ServiceName)
		}
		return nil
	}
	if j.Failed() {
		if j.EnsuredService != winservice.LegacyServiceName {
			// The rollback did not end with the legacy service running (the
			// folder could not be moved back, nothing could be started): its
			// undos are idempotent and run again at every start of the migrate
			// service (a boot clears a holder) until the legacy agent runs.
			// A new agent that connected meanwhile adopted the journal (done).
			e.logf("migrate: the rolled-back migration did not end with %s running; undoing again", winservice.LegacyServiceName)
			e.rollback(j, j.FailedStep)
			if saveErr := e.saveJournal(j); saveErr != nil {
				e.logf("migrate: saving the journal: %v", saveErr)
			}
			e.ensureAgent(j, winservice.LegacyServiceName)
		}
		if j.EnsuredService == winservice.LegacyServiceName {
			e.disableMigrateService()
		}
		return nil
	}
	if j.RestoredFromBackup {
		// The step after the last recorded one may have run partially; the
		// rollback covers it.
		failed := min(j.Step, len(steps)-1)
		return e.fail(j, failed, errors.New("the migration journal was unreadable and restored from its backup"))
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

	for i := j.Step; i < len(steps); i++ {
		s := steps[i]
		if e.cancelled() {
			e.logf("migrate: stopped before %s", s.name)
			return ErrCancelled
		}
		e.logf("migrate: %s", s.name)
		if e.Fault == s.name {
			err = fmt.Errorf("fault injected at %s", s.name)
		} else {
			err = s.do(e, j)
		}
		if errors.Is(err, ErrCancelled) {
			e.logf("migrate: stopped during %s", s.name)
			return ErrCancelled
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
				e.Sys.Sleep(time.Hour)
			}
		}
	}
	e.ensureAgent(j, winservice.ServiceName)
	e.logf("migrate: done; the agent runs as %s from %s", winservice.ServiceName, j.New.AgentDir)
	return nil
}

// fail rolls the migration back from step failed and records it. The
// journal says "rolling back" before the first undo, so a crash in the
// middle continues the rollback, never the migration. Afterwards an agent
// service runs again (ensure-agent) and the migrate service is disabled
// until the next attempt stages it again.
func (e Env) fail(j *Journal, failed int, err error) error {
	if j.Done {
		// Done is final (cleanup sets it first): nothing after it is undone.
		// The migrate service exits with the journal's state; the tail runs
		// again at its next start.
		e.logf("migrate: %v after done; nothing is undone", err)
		return err
	}
	e.logf("migrate: %v; rolling back", err)
	j.RollingBack, j.FailedStep, j.Error = true, failed, err.Error()
	if saveErr := e.saveJournal(j); saveErr != nil {
		e.logf("migrate: saving the journal: %v", saveErr)
	}
	e.rollback(j, failed)
	j.RollingBack = false
	j.FailedAt = e.Sys.Now().UTC().Format(time.RFC3339)
	j.Attempts++
	if saveErr := e.saveJournal(j); saveErr != nil {
		e.logf("migrate: saving the journal: %v", saveErr)
	}
	e.ensureAgent(j, winservice.LegacyServiceName)
	if j.EnsuredService == winservice.LegacyServiceName {
		// Otherwise the migrate service stays enabled: at the next boot it
		// undoes again (Run) until the legacy agent runs.
		e.disableMigrateService()
	}
	return err
}

// rollback undoes the steps before failed (the failed one included: a step
// may have done part of its work) in reverse. It keeps going past errors:
// every undo that works brings the device closer to the layout it ran in
// before.
func (e Env) rollback(j *Journal, failed int) {
	for i := failed; i >= 0; i-- {
		if steps[i].undo == nil {
			continue
		}
		e.logf("migrate: undoing %s", steps[i].name)
		if err := steps[i].undo(e, j); err != nil {
			e.logf("migrate: undoing %s: %v", steps[i].name, err)
		}
	}
}

func (e Env) disableMigrateService() {
	if err := e.Sys.SetStartType(winservice.MigrateServiceName, winservice.StartDisabled, false); err != nil && !errors.Is(err, os.ErrNotExist) {
		e.logf("migrate: disabling the migrate service: %v", err)
	}
}

// ensureStableFor is how long ensure-agent watches a started service.
const ensureStableFor = 60 * time.Second

// ensureAgent, the end of both done and rollback, makes sure an agent service
// runs: the preferred one, else the other one when its executable and folder
// are complete. The outcome is recorded in the journal.
func (e Env) ensureAgent(j *Journal, preferred string) {
	other := winservice.LegacyServiceName
	if preferred == winservice.LegacyServiceName {
		other = winservice.ServiceName
	}
	candidates := []string{preferred, other}
	if j.Done {
		// After done only the new service: re-creating the legacy one from
		// the journal would run a second agent (in place: from the same
		// folder, on a fresh database). A failed ensure is retried by the
		// migrate service's recovery (exitCodeFor).
		candidates = candidates[:1]
	}
	var reasons []string
	for _, name := range candidates {
		ok, reason := e.ensureService(j, name)
		if ok {
			e.logf("migrate: %s is running", name)
			e.recordEnsured(j, name, "")
			return
		}
		reasons = append(reasons, name+": "+reason)
	}
	e.recordEnsured(j, "", strings.Join(reasons, "; "))
	e.logf("migrate: no agent service could be started: %s", j.EnsureError)
}

// recordEnsured writes ensure-agent's outcome. The journal is reloaded
// first: the agent that ensure-agent started as ironflock-device-agent may
// have adopted it meanwhile (Adopt), and that must not be overwritten with
// the state this process still holds in memory.
func (e Env) recordEnsured(j *Journal, name string, reason string) {
	if fresh, err := loadJournal(e.Sys.ReadFile, e.Layout); err == nil && fresh != nil {
		*j = *fresh
	}
	j.EnsuredService, j.EnsureError = name, reason
	if saveErr := e.saveJournal(j); saveErr != nil {
		e.logf("migrate: saving the journal: %v", saveErr)
	}
}

// serviceSpecFor is the service as this journal knows it: the legacy one as
// the plan found it, the new one as it runs once started (automatic,
// delayed).
func (e Env) serviceSpecFor(j *Journal, name string) (winservice.Spec, bool) {
	if !j.Planned {
		return winservice.Spec{}, false
	}
	if name == winservice.LegacyServiceName {
		start := legacyStartType(j)
		display := winservice.DisplayName
		if _, err := e.Sys.Service(winservice.ServiceName); err == nil {
			display = winservice.LegacyDisplayName // the new service holds the name
		}
		return winservice.Spec{
			Name:             name,
			DisplayName:      display,
			Description:      winservice.Description,
			Exe:              j.Legacy.Exe,
			Args:             j.Legacy.Args,
			StartType:        start,
			Delayed:          j.Legacy.Delayed,
			RestartDelays:    []int{5, 30, 120},
			ResetAfter:       86400,
			NonCrashRecovery: true,
			Env:              j.Legacy.Env,
		}, true
	}
	return winservice.AgentSpec(j.New.AgentDir, j.New.AppsDir, extraArgs(j.Legacy.Args), withoutHooks(j.Legacy.Env)), true
}

// legacyStartType is the reagent service's start type as the plan found it;
// a disabled one (someone disabled it while it ran) starts automatically
// again, since the device must come back.
func legacyStartType(j *Journal) winservice.StartType {
	switch j.Legacy.StartType {
	case winservice.StartManual.String():
		return winservice.StartManual
	default:
		return winservice.StartAutomatic
	}
}

// ensureService starts name and watches it for a minute. A service the
// service control manager has is started as it is registered, whatever the
// journal knows (a rollback before prepare planned anything still has the
// legacy service to start); one that is gone is re-created from the journal
// when its files are complete.
func (e Env) ensureService(j *Journal, name string) (bool, string) {
	spec, known := e.serviceSpecFor(j, name)
	cfg, err := e.Sys.Service(name)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if !known {
			return false, "no such service, and the journal does not know it"
		}
		cfg = ServiceConfig{Exe: spec.Exe, Args: spec.Args, StartType: spec.StartType, Delayed: spec.Delayed}
		e.restoreMissingExe(j, cfg.Exe)
		if reason := e.serviceFilesMissing(cfg); reason != "" {
			return false, reason
		}
		e.logf("migrate: re-creating service %s", name)
		if name == winservice.ServiceName {
			if err := e.takeDisplayName(); err != nil {
				return false, err.Error()
			}
			if !j.CreatedService {
				// Recorded before the service exists (as register-service
				// does): a cut in between must not leave a service this
				// journal does not own, which Check would refuse for good.
				j.CreatedService = true
				if saveErr := e.saveJournal(j); saveErr != nil {
					e.logf("migrate: saving the journal: %v", saveErr)
				}
			}
		}
		if err := e.Sys.CreateOrUpdateService(spec); err != nil {
			return false, err.Error()
		}
	case err != nil:
		return false, err.Error()
	default:
		e.restoreMissingExe(j, cfg.Exe)
		if reason := e.serviceFilesMissing(cfg); reason != "" {
			return false, reason
		}
	}
	// It must be able to start now and at the next boot: the start type the
	// journal recorded, else automatic; never disabled.
	start, delayed := winservice.StartAutomatic, cfg.Delayed
	if known {
		start, delayed = spec.StartType, spec.Delayed
	}
	if start == winservice.StartDisabled {
		start = winservice.StartAutomatic
	}
	if err := e.Sys.SetStartType(name, start, delayed); err != nil {
		return false, err.Error()
	}
	if err := e.Sys.StartService(name); err != nil {
		return false, err.Error()
	}
	pid := 0
	deadline := e.Sys.Now().Add(ensureStableFor)
	for {
		status, err := e.Sys.ServiceStatus(name)
		if err != nil {
			return false, err.Error()
		}
		if pid == 0 && status.Running() && status.PID > 0 {
			pid = status.PID
		}
		if pid != 0 && (!status.Running() || status.PID != pid) {
			return false, fmt.Sprintf("stopped within %s of its start", ensureStableFor)
		}
		if !e.Sys.Now().Before(deadline) || e.cancelled() {
			break
		}
		e.Sys.Sleep(5 * time.Second)
	}
	if pid == 0 {
		return false, fmt.Sprintf("not running %s after its start", ensureStableFor)
	}
	return true, ""
}

// restoreMissingExe copies the migrator (a verified copy of the version that
// migrates) over a service executable that went missing during the attempt
// (quarantined, lost with a torn disk): prepare dropped the legacy prev and
// -v files and unprepare removes device-agent-prev.exe, so without this
// nothing at the next boot could restore it either.
func (e Env) restoreMissingExe(j *Journal, exe string) {
	if exe == "" || e.isFile(exe) || !e.isFile(e.Layout.MigratorExe()) {
		return
	}
	if !e.agentFolder(filepath.Dir(exe)) {
		// Not the agent's folder (the other folder of a move): an executable
		// dropped there would make it a folder in use.
		return
	}
	if version, err := e.Sys.ExecVersion(e.Layout.MigratorExe()); err != nil || version != j.Version {
		return
	}
	if err := e.Sys.CopyFile(e.Layout.MigratorExe(), exe, 0o755); err != nil {
		e.logf("migrate: restoring the missing %s from the migrator: %v", exe, err)
		return
	}
	e.logf("migrate: %s was missing; restored from the migrator", exe)
}

// serviceFilesMissing names the executable or configuration a service lacks
// to start, "" when it has both.
func (e Env) serviceFilesMissing(cfg ServiceConfig) string {
	if !e.isFile(cfg.Exe) {
		return "no " + cfg.Exe
	}
	config := flagValue(cfg.Args, "-config", filepath.Join(filepath.Dir(cfg.Exe), winservice.ConfigName))
	if !e.isFile(config) {
		return "no " + config
	}
	return ""
}

// ensureAnyAgent: without a readable journal, start whichever agent service
// has its files (the new one first, the legacy one otherwise). The legacy
// service's files may have been renamed in their folder (rename-files is the
// one step whose outcome leaves the service without an executable while its
// folder is in place): they are renamed back first.
func (e Env) ensureAnyAgent() {
	for _, name := range []string{winservice.ServiceName, winservice.LegacyServiceName} {
		cfg, err := e.Sys.Service(name)
		if err != nil {
			continue
		}
		if name == winservice.LegacyServiceName && !e.isFile(cfg.Exe) {
			e.renameLegacyFilesBack(filepath.Dir(cfg.Exe))
		}
		if !e.isFile(cfg.Exe) {
			continue
		}
		if err := e.Sys.SetStartType(name, winservice.StartAutomatic, true); err != nil {
			e.logf("migrate: %s: %v", name, err)
			continue
		}
		if err := e.Sys.StartService(name); err != nil {
			e.logf("migrate: starting %s: %v", name, err)
			continue
		}
		e.logf("migrate: started %s", name)
		return
	}
}

// renameLegacyFilesBack gives the files of the legacy folder their legacy
// names back (device-agent.* -> reagent.*) when rename-files renamed them and
// no journal says so any more; nothing is overwritten.
func (e Env) renameLegacyFilesBack(dir string) {
	if !e.isFile(filepath.Join(dir, winservice.ExeName)) {
		return
	}
	renames, err := e.fileRenames(dir, "device-agent", "reagent")
	if err != nil {
		return
	}
	j := &Journal{Version: e.Version}
	for _, r := range renames {
		if err := e.renameNoClobber(j, r[0], r[1]); err != nil {
			e.logf("migrate: renaming %s back: %v", r[0], err)
		}
	}
}

// cleanupTail is the idempotent end of a done migration: what only the
// legacy layout used goes, the containers get their restart policies back,
// and the repair task points at the new folder's script. It runs again at
// every start of the migrate service until the agent removes the service,
// and at every start of the agent until it reports true: everything that
// must not be left undone has worked (the containers' policies, the repair
// task), so the leftovers that would rerun it may go.
func (e Env) cleanupTail(j *Journal) bool {
	settled := true
	if _, err := e.Sys.Service(winservice.LegacyServiceName); err == nil {
		_ = e.Sys.SetStartType(winservice.LegacyServiceName, winservice.StartDisabled, false)
		_ = e.Sys.StopService(winservice.LegacyServiceName)
		if err := e.Sys.DeleteService(winservice.LegacyServiceName); err != nil && !errors.Is(err, os.ErrNotExist) {
			e.logf("migrate: deleting the %s service: %v", winservice.LegacyServiceName, err)
		}
	}
	if err := e.Sys.RemoveEventSource(winservice.LegacyServiceName); err != nil && !errors.Is(err, os.ErrNotExist) {
		e.logf("migrate: removing the %s event source: %v", winservice.LegacyServiceName, err)
	}
	if j.Strategy == StrategyMove {
		// In place, the exclusion names the folder the agent still runs in.
		if err := e.Sys.RemoveDefenderExclusion(filepath.Join(j.Legacy.AgentDir, "frpc.exe")); err != nil {
			e.logf("migrate: removing the Defender exclusion of the old folder: %v", err)
		}
	}
	if j.Strategy == StrategyMove && j.Moved && !e.settleOldFolderContainers(j) {
		settled = false
	}
	e.restoreRestartPolicies(j)
	for _, c := range j.Containers {
		if !c.Restored {
			e.logf("migrate: the restart policy of %s is not restored yet; tried again at the next start", c.Name)
			settled = false
			break
		}
	}
	repair := filepath.Join(j.New.AgentDir, winservice.RepairName)
	if err := e.Sys.WriteFile(repair, []byte(winservice.RepairCmd(j.New.AgentDir)), 0o644); err != nil {
		e.logf("migrate: writing %s: %v", repair, err)
		settled = false
	} else if err := e.Sys.CreateTask(winservice.RepairTaskName, repair); err != nil {
		e.logf("migrate: pointing the repair task at %s: %v", repair, err)
		settled = false
	}
	// A legacy folder that came back after the move (Docker recreated it
	// under a container's bind mount at a boot in between) is moved aside,
	// never deleted: the agent recreates its containers from the new folder.
	if j.Strategy == StrategyMove && j.Legacy.AgentDir != "" && !samePath(j.Legacy.AgentDir, j.New.AgentDir) && e.isDir(j.Legacy.AgentDir) {
		stray := fmt.Sprintf("%s.stray-%s", j.Legacy.AgentDir, e.Sys.Now().UTC().Format("20060102-150405"))
		if err := e.Sys.Rename(j.Legacy.AgentDir, stray); err != nil {
			e.logf("migrate: %s came back after the move; could not move it aside: %v", j.Legacy.AgentDir, err)
		} else {
			e.logf("migrate: %s came back after the move; moved aside to %s", j.Legacy.AgentDir, stray)
		}
	}
	if err := e.saveJournal(j); err != nil {
		e.logf("migrate: saving the journal: %v", err)
	}
	return settled
}

// settleOldFolderContainers: after the folder moved, a container that still
// binds the old folder is removed and recreated by the agent (the recreate
// marker; compose by its config hash) with its own restart policy. Given a
// restart policy, Docker would start it against a recreated old folder at
// its next daemon start ("always" starts even a stopped container): a
// recorded one counts as restored without its policy, one stop-apps could
// not see (Docker was unreachable then) loses its policy now. It reports
// false while Docker cannot be asked.
func (e Env) settleOldFolderContainers(j *Journal) bool {
	containers, err := e.Sys.Containers()
	if err != nil {
		e.logf("migrate: Docker is not reachable (%v); the old folder's containers are checked at the next start", err)
		return false
	}
	settled := true
	for _, live := range containers {
		if !bindsUnder(live, j.Legacy.AgentDir) {
			continue
		}
		if live.RestartPolicy != "no" && live.RestartPolicy != "" {
			if err := e.Sys.SetRestartPolicy(live.ID, "no"); err != nil && !errors.Is(err, os.ErrNotExist) {
				e.logf("migrate: taking the restart policy of %s: %v", live.Name, err)
				settled = false
				continue
			}
		}
		recorded := false
		for i, c := range j.Containers {
			if c.ID == live.ID {
				recorded = true
				if !c.Restored {
					e.logf("migrate: %s still binds %s; left to the agent, which recreates it", c.Name, j.Legacy.AgentDir)
					j.Containers[i].Restored = true
				}
			}
		}
		if !recorded {
			e.logf("migrate: %s still binds %s (Docker was not reachable when the apps were stopped); left to the agent, which recreates it", live.Name, j.Legacy.AgentDir)
			j.Containers = append(j.Containers, ContainerRecord{ID: live.ID, Name: live.Name, RestartPolicy: live.RestartPolicy, Dir: j.Legacy.AgentDir, Restored: true})
		}
	}
	return settled
}

// restoreRestartPolicies gives the containers stop-apps changed their
// policies back. A container that is gone was recreated by the agent with
// its own policy and counts as restored.
func (e Env) restoreRestartPolicies(j *Journal) {
	for i, c := range j.Containers {
		if c.Restored {
			continue
		}
		err := e.Sys.SetRestartPolicy(c.ID, c.RestartPolicy)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			e.logf("migrate: restoring the restart policy of %s: %v", c.Name, err)
			continue
		}
		j.Containers[i].Restored = true
	}
}

var leftoversMu sync.Mutex

// RemoveLeftovers removes what a done migration leaves behind — the migrate
// service, %ProgramData%\IronFlock\.migrate and a reagent service the cleanup
// tail could not delete — which the migrator cannot remove itself (its own
// running executable). The agent service calls it at every start and once
// Docker is reachable, with the name of the service it runs as; only the
// agent running as ironflock-device-agent removes anything. The cleanup tail
// runs once more first: at a boot after a power cut in the middle of it, the
// migrate service that would rerun it is the very thing removed here. The
// leftovers stay until the tail has settled (restart policies restored,
// the repair task pointed at the new script): until then the migrate service
// reruns it at every boot, and the agent at every start.
func RemoveLeftovers(e Env, running string) {
	if running != winservice.ServiceName {
		return
	}
	// One at a time within the agent: its start and its Docker-ready hook
	// may both call this.
	leftoversMu.Lock()
	defer leftoversMu.Unlock()
	j, err := loadJournal(e.Sys.ReadFile, e.Layout)
	if err != nil || j == nil || !j.Done {
		return
	}
	_, migrateServiceErr := e.Sys.Service(winservice.MigrateServiceName)
	_, migrateDirErr := e.Sys.Stat(e.Layout.MigrateDir())
	if migrateServiceErr != nil && migrateDirErr != nil {
		return // nothing left
	}
	if j.EnsuredService == "" {
		// The agent running as the new service is the outcome ensure-agent
		// records; the migrate service that would record it (a boot's
		// repair task started this agent before it) goes below.
		j.EnsuredService = winservice.ServiceName
	}
	if !e.cleanupTail(j) {
		e.logf("migrate: the cleanup is not complete; the %s service and %s stay until it is", winservice.MigrateServiceName, e.Layout.MigrateDir())
		return
	}
	for _, name := range []string{winservice.MigrateServiceName, winservice.LegacyServiceName} {
		if _, err := e.Sys.Service(name); err != nil {
			continue
		}
		_ = e.Sys.SetStartType(name, winservice.StartDisabled, false)
		_ = e.Sys.StopService(name)
		if err := e.Sys.DeleteService(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			e.logf("migrate: deleting the %s service: %v", name, err)
		} else {
			e.logf("migrate: removed the %s service", name)
		}
		_ = e.Sys.RemoveEventSource(name)
	}
	if _, err := e.Sys.Stat(e.Layout.MigrateDir()); err == nil {
		if err := e.Sys.RemoveAll(e.Layout.MigrateDir()); err != nil {
			e.logf("migrate: removing %s (again at the next start): %v", e.Layout.MigrateDir(), err)
		}
	}
}

// Adopt is called by the agent running as ironflock-device-agent once it is
// connected: a rollback that ensure-agent could only resolve by starting the
// new service (the legacy folder could not be moved back) leaves a failed
// journal although the device now runs the new layout, connected. The
// journal becomes done (the state it was in is recorded) and the cleanup
// tail runs. A migration in progress — the new agent connects during the
// health step, and a rollback stops it before anything else — is left to the
// migrate service. It reports false while the migrate service still runs (it
// is in its ensure-agent watch, or undoing again): two processes must not
// work one journal, so the agent asks again later.
func Adopt(e Env) bool {
	j, err := loadJournal(e.Sys.ReadFile, e.Layout)
	if err != nil || j == nil || j.Done || !j.Failed() {
		return true
	}
	if status, err := e.Sys.ServiceStatus(winservice.MigrateServiceName); err == nil && !status.Stopped() {
		e.logf("migrate: the %s service is still %s; the rolled-back migration is adopted once it stopped", winservice.MigrateServiceName, status.State)
		return false
	}
	e.logf("migrate: the rolled-back migration is adopted: the agent connected as %s", winservice.ServiceName)
	failedAt := "the start"
	if j.FailedStep >= 0 && j.FailedStep < len(steps) {
		failedAt = steps[j.FailedStep].name
	}
	j.Adopted = fmt.Sprintf("rolled back at %s (%s); %s started by ensure-agent and connected", failedAt, j.Error, winservice.ServiceName)
	j.Done = true
	j.FailedAt = ""
	if err := e.saveJournal(j); err != nil {
		e.logf("migrate: saving the journal: %v", err)
		return true
	}
	e.cleanupTail(j)
	return true
}

// MigrateServiceIdle reports an error while the migrate service runs: a
// previous attempt finishing its rollback (its ensure-agent watch lasts a
// minute), or a migration in progress. A second engine must never work the
// same journal, and a new attempt must not be staged under a running one.
func MigrateServiceIdle(e Env) error {
	status, err := e.Sys.ServiceStatus(winservice.MigrateServiceName)
	if err != nil || status.Stopped() {
		return nil
	}
	return fmt.Errorf("the %s service is still %s (a previous attempt finishing its rollback, or a migration in progress); try again in a minute", winservice.MigrateServiceName, status.State)
}

// ForegroundCheck readies this console process to run the engine and says
// whether it may: the files nobody trusted go first (RemoveForeign), because
// Run reads the journal next and a console run (`migrate-layout` with no
// verb) may come before any Stage hardened the root; then it refuses while
// the migrate service runs, and from an executable inside the legacy agent
// folder, whose running image would block the folder's rename (the process
// itself is never terminated), so that every attempt would end in place.
func ForegroundCheck(e Env, selfExe string) error {
	if err := RemoveForeign(e); err != nil {
		return err
	}
	if err := MigrateServiceIdle(e); err != nil {
		return err
	}
	cfg, err := e.Sys.Service(winservice.LegacyServiceName)
	if err != nil {
		return nil
	}
	if dir := legacyPaths(cfg).AgentDir; underDir(selfExe, dir) {
		return fmt.Errorf("%s runs from the agent folder %s, which the move renames: run it from a copy outside that folder, or without -foreground", selfExe, dir)
	}
	return nil
}

// RemoveForeign removes the migration's files and the new folder when SYSTEM
// or Administrators do not own them: a local user could have planted a
// migrator to run as SYSTEM, or a journal to steer (or block) the move.
// %ProgramData% grants every user create rights until the root is hardened
// (Stage), so this runs before anything reads the journal.
//
// The migration's own files (.migrate, the journal, its backup) are never
// reached through a link: a symbolic link, a junction (a user needs no
// privilege for one; Go reports it as irregular) or any other reparse point
// there goes whoever owns it or its target, removed itself and never what
// it points to. The new folder is judged by its owner only, a link's own
// (winservice.OwnedByAdministrators): an administrator may have linked it to
// another drive, and the move renames a linked Reagent to it.
func RemoveForeign(e Env) error {
	for _, path := range []string{e.Layout.MigrateDir(), e.Layout.Journal(), e.Layout.JournalBak(), e.Layout.NewDir()} {
		info, err := e.Sys.Stat(path)
		if err != nil {
			continue
		}
		if link := info.Mode().Type()&^os.ModeDir != 0; link && path != e.Layout.NewDir() {
			e.logf("migrate: removing %s: a link, and the migration's files are never reached through one", path)
			if err := e.Sys.Remove(path); err != nil {
				return fmt.Errorf("removing the link %s: %w", path, err)
			}
			continue
		}
		if path == e.Layout.NewDir() && e.agentFolderInUse(path) {
			// The live agent folder (moved there, or installed there): its owner
			// is whoever's token made the legacy folder, on Windows 10/11 an
			// elevated administrator's own account rather than the
			// Administrators group, and removing it would take the device's
			// configuration and its apps' data with it. One planted there with
			// a configuration only makes the move keep its folder (in-place).
			continue
		}
		owned, err := e.Sys.OwnedByAdministrators(path)
		if err != nil {
			return fmt.Errorf("reading the owner of %s: %w", path, err)
		}
		if !owned {
			e.logf("migrate: removing %s: not owned by SYSTEM or Administrators", path)
			if err := e.Sys.RemoveAll(path); err != nil {
				return fmt.Errorf("removing %s: %w", path, err)
			}
		}
	}
	return nil
}

// Check reports whether this device is a legacy install to move: elevated,
// with a registered reagent service whose executable, folder and
// configuration are in place, no agent update under way, and no
// ironflock-device-agent service that this journal did not create. An empty
// DeviceAgent folder is removed. That the calling process is the reagent
// service is the automatic trigger's condition (watchLayoutMigration), not
// Check's: `migrate-layout start` runs from a console.
func Check(e Env) error {
	if !e.Sys.Elevated() {
		return fmt.Errorf("%w: not elevated", ErrNotEligible)
	}
	cfg, err := e.Sys.Service(winservice.LegacyServiceName)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: no %s service", ErrNotEligible, winservice.LegacyServiceName)
	}
	if err != nil {
		return fmt.Errorf("%w: reading the %s service: %v", ErrNotEligible, winservice.LegacyServiceName, err)
	}
	if !e.isFile(cfg.Exe) {
		return fmt.Errorf("%w: the %s service's executable %s is missing", ErrNotEligible, winservice.LegacyServiceName, cfg.Exe)
	}
	legacy := legacyPaths(cfg)
	if !e.isDir(legacy.AgentDir) {
		return fmt.Errorf("%w: no agent folder %s", ErrNotEligible, legacy.AgentDir)
	}
	if !e.isFile(legacy.Config) {
		return fmt.Errorf("%w: no %s", ErrNotEligible, legacy.Config)
	}
	if phase := e.updatePhase(legacy.AgentDir); phase != "" {
		return fmt.Errorf("%w: an agent update is %s", ErrNotEligible, phase)
	}
	j, _ := loadJournal(e.Sys.ReadFile, e.Layout)
	if _, err := e.Sys.Service(winservice.ServiceName); err == nil && (j == nil || !j.CreatedService) {
		return fmt.Errorf("%w: a %s service exists already (installed by hand?)", ErrNotEligible, winservice.ServiceName)
	}
	if e.isDir(e.Layout.NewDir()) {
		entries, err := e.Sys.ReadDir(e.Layout.NewDir())
		if err == nil && len(entries) == 0 {
			e.logf("migrate: removing the empty %s", e.Layout.NewDir())
			if err := e.Sys.RemoveAll(e.Layout.NewDir()); err != nil {
				return fmt.Errorf("removing the empty %s: %w", e.Layout.NewDir(), err)
			}
		}
	}
	return nil
}

// updatePhase is the phase of a self-update under way in agentDir
// (update-state.json: "swapping" or "probation"), "" when none.
func (e Env) updatePhase(agentDir string) string {
	data, err := e.Sys.ReadFile(filepath.Join(agentDir, "update-state.json"))
	if err != nil {
		return ""
	}
	phase := jsonField(data, "phase")
	switch phase {
	case "swapping", "probation":
		return "in " + phase
	}
	return ""
}

// Stage prepares the move without starting it: %ProgramData%\IronFlock
// hardened, foreign files removed, the migrator (a verified copy of the
// running executable) in .migrate, the migrate service registered with the
// legacy service's environment, and the journal written last: a migration
// exists only once it can resume.
func Stage(e Env, selfExe string) error {
	// Never under a running migrator (the previous attempt's rollback ends
	// with a minute's watch after its journal says failed): it would record
	// its outcome into this attempt's journal and disable the service.
	if err := MigrateServiceIdle(e); err != nil {
		return err
	}
	// A journal nobody trusted goes before it is read.
	if err := RemoveForeign(e); err != nil {
		return err
	}
	next := &Journal{Version: e.Version, StartedAt: e.Sys.Now().UTC().Format(time.RFC3339)}
	if previous, err := loadJournal(e.Sys.ReadFile, e.Layout); err == nil && previous != nil && previous.Failed() {
		next.Attempts = previous.Attempts
		next.PreferInPlace = previous.PreferInPlace
		// A service the previous attempt created and could not delete is
		// this attempt's to take over and, in a rollback, to delete.
		next.CreatedService = previous.CreatedService
		// Restart policies the previous rollback could not give back (Docker
		// unreachable) are still owed: recorded here, stop-apps keeps the
		// original policy and the end of this attempt restores it.
		for _, c := range previous.Containers {
			if !c.Restored {
				next.Containers = append(next.Containers, c)
			}
		}
	}
	if err := e.stageMigrator(selfExe, true); err != nil {
		return err
	}
	return e.saveJournal(next)
}

// Restage puts the migrator of a migration in progress back from the running
// executable: lost (quarantined) before the repair task ran the universal
// script, which restores it from any agent binary, the migrate service could
// not start and nothing else would ever stage it again. The journal is kept.
func Restage(e Env, selfExe string) error {
	j, err := loadJournal(e.Sys.ReadFile, e.Layout)
	if err != nil {
		return err
	}
	if j == nil || !j.InProgressState() {
		return errors.New("no migration in progress")
	}
	if j.Version != e.Version {
		return fmt.Errorf("the migration in progress is version %s, this agent is %s", j.Version, e.Version)
	}
	return e.stageMigrator(selfExe, false)
}

// stageMigrator does everything Stage does but the journal; fresh drops a
// previous attempt's backups (which this attempt's rollback would restore),
// a restage keeps the attempt's own.
func (e Env) stageMigrator(selfExe string, fresh bool) error {
	if err := e.Sys.MkdirAll(e.Layout.Root, 0o755); err != nil {
		return err
	}
	if err := e.Sys.Harden(e.Layout.Root); err != nil {
		return fmt.Errorf("hardening %s: %w", e.Layout.Root, err)
	}
	// What the agent did not make is not trusted (again, now that nobody
	// can plant anything more).
	if err := RemoveForeign(e); err != nil {
		return err
	}
	if err := e.Sys.MkdirAll(e.Layout.MigrateDir(), 0o755); err != nil {
		return err
	}
	if fresh {
		// A previous attempt's backups would be restored by this attempt's
		// rollback.
		for _, backup := range []string{e.Layout.RepairBackup(), e.Layout.TaskBackup()} {
			if err := e.Sys.Remove(backup); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	if err := e.Sys.Remove(e.Layout.MigratorExe()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("replacing the migrator: %w", err)
	}
	if err := e.Sys.CopyFile(selfExe, e.Layout.MigratorExe(), 0o755); err != nil {
		return fmt.Errorf("copying the migration binary: %w", err)
	}
	version, err := e.Sys.ExecVersion(e.Layout.MigratorExe())
	if err != nil {
		return fmt.Errorf("the migrator copy does not run: %w", err)
	}
	if version != e.Version {
		return fmt.Errorf("the migrator copy reports version %q, expected %q", version, e.Version)
	}
	// Its SHA-256, for the universal repair script: at boot it checks a
	// migrator (and the agent binaries it would restore one from) by hash,
	// because cmd.exe blocks for good on starting a torn executable.
	data, err := e.Sys.ReadFile(e.Layout.MigratorExe())
	if err != nil {
		return fmt.Errorf("reading the migrator copy: %w", err)
	}
	sum := sha256.Sum256(data)
	if err := e.Sys.WriteFile(e.Layout.MigratorHash(), []byte(hex.EncodeToString(sum[:])), 0o644); err != nil {
		return fmt.Errorf("recording the migrator's hash: %w", err)
	}
	legacy, err := e.Sys.Service(winservice.LegacyServiceName)
	if err != nil {
		return fmt.Errorf("reading the %s service: %w", winservice.LegacyServiceName, err)
	}
	// The test hooks come from this process's environment or, when unset
	// there, from the reagent service's own Environment (where a test sets
	// them); they travel only with the migrate service, never into
	// ironflock-device-agent's Environment.
	env := withoutHooks(legacy.Env)
	if fault := firstNonEmpty(e.Fault, envValue(legacy.Env, FaultEnv)); fault != "" {
		env = append(env, FaultEnv+"="+fault)
	}
	if pause := firstNonEmpty(e.Pause, envValue(legacy.Env, PauseEnv)); pause != "" {
		env = append(env, PauseEnv+"="+pause)
	}
	if env == nil {
		env = []string{}
	}
	if err := e.Sys.InstallEventSource(winservice.MigrateServiceName); err != nil {
		e.logf("migrate: registering the migrate service's event source: %v", err)
	}
	return e.Sys.CreateOrUpdateService(winservice.MigrateSpec(e.Layout.MigratorExe(), env))
}

// Begin hands the migration to the migrate service and starts it. The caller
// (the legacy agent) keeps running until the migration stops its service.
func Begin(e Env, selfExe string) error {
	if err := Stage(e, selfExe); err != nil {
		return err
	}
	if err := e.Sys.StartService(winservice.MigrateServiceName); err != nil {
		// Nothing moved yet; recorded as failed, so it is retried tomorrow
		// rather than at the next boot.
		if j, _ := loadJournal(e.Sys.ReadFile, e.Layout); j != nil {
			j.FailedAt, j.Error = e.Sys.Now().UTC().Format(time.RFC3339), "starting the migration: "+err.Error()
			j.Attempts++
			_ = e.saveJournal(j)
		}
		e.disableMigrateService()
		return err
	}
	return nil
}

// agentFolderInUse reports whether dir is an agent's folder: it holds the
// agent's configuration, or a registered agent service runs from it.
func (e Env) agentFolderInUse(dir string) bool {
	if e.agentFolder(dir) {
		return true
	}
	for _, name := range []string{winservice.ServiceName, winservice.LegacyServiceName} {
		if cfg, err := e.Sys.Service(name); err == nil && samePath(legacyPaths(cfg).AgentDir, dir) {
			return true
		}
	}
	return false
}

// --- small helpers --------------------------------------------------------

func (e Env) isDir(path string) bool {
	info, err := e.Sys.Stat(path)
	return err == nil && info.IsDir()
}

func (e Env) isFile(path string) bool {
	info, err := e.Sys.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func (e Env) exists(path string) bool {
	_, err := e.Sys.Stat(path)
	return err == nil
}

// flagValue finds -name's value in a command line (both "-name value" and
// "-name=value"); def when absent.
func flagValue(args []string, name string, def string) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == name || arg == "-"+name {
			if i+1 < len(args) {
				return args[i+1]
			}
			return def
		}
		for _, prefix := range []string{name + "=", "-" + name + "="} {
			if strings.HasPrefix(arg, prefix) {
				return strings.TrimPrefix(arg, prefix)
			}
		}
	}
	return def
}

// pathFlags are the flags the new service sets itself; the rest of a legacy
// ImagePath (-debug, -env, …) is carried over verbatim.
var pathFlags = map[string]bool{"-config": true, "-agentDir": true, "-appsDir": true, "-dbFileName": true, "-logFile": true}

func extraArgs(args []string) []string {
	var extra []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, _, hasValue := strings.Cut(arg, "=")
		if pathFlags[name] || pathFlags["-"+strings.TrimLeft(name, "-")] {
			if !hasValue {
				i++ // the value
			}
			continue
		}
		extra = append(extra, arg)
	}
	return extra
}

// envValue is the value of name in an environment list, "" when absent.
func envValue(env []string, name string) string {
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, name+"="); ok {
			return value
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// withoutHooks drops the migration's test hooks from an environment.
func withoutHooks(env []string) []string {
	if env == nil {
		return nil
	}
	out := make([]string, 0, len(env))
	for _, entry := range env {
		if strings.HasPrefix(entry, FaultEnv+"=") || strings.HasPrefix(entry, PauseEnv+"=") {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// jsonField reads one top-level string field of a JSON document, "" when
// absent or unreadable.
func jsonField(data []byte, field string) string {
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return ""
	}
	value, _ := doc[field].(string)
	return value
}
