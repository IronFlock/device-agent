package winmigrate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

const (
	retryAfterFailure = 24 * time.Hour
	// maxRuns: the migration process may stop (crash, kill, power cut, the
	// watchdog) this often within one attempt before the attempt is rolled
	// back.
	maxRuns = 5
	// defaultHealthLimit: the new agent must connect to IronFlock this soon.
	defaultHealthLimit = 10 * time.Minute
	defaultStableFor   = time.Minute
)

const (
	StrategyMove    = "move"
	StrategyInPlace = "in-place"
)

// LegacyService is what the plan found of the reagent service.
type LegacyService struct {
	AgentDir  string   `json:"agent_dir"`
	AppsDir   string   `json:"apps_dir"`
	Config    string   `json:"config"`
	Exe       string   `json:"exe"`
	Args      []string `json:"args"`
	Env       []string `json:"env,omitempty"`
	StartType string   `json:"start_type"`
	Delayed   bool     `json:"delayed,omitempty"`
	PID       int      `json:"pid,omitempty"`
}

// NewService is where the agent runs afterwards.
type NewService struct {
	AgentDir string `json:"agent_dir"`
	AppsDir  string `json:"apps_dir"`
}

// ContainerRecord is a container the migration stopped, with the restart
// policy to give back.
type ContainerRecord struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	RestartPolicy string `json:"restart_policy"`
	// Dir is the agent folder the container was bound into when it was
	// recorded: the legacy one (stop-apps) or the new one (the new agent's
	// containers, recorded by the rollback before the folder moves back).
	Dir      string `json:"dir,omitempty"`
	Restored bool   `json:"restored,omitempty"`
}

// Journal records the migration's progress. Its semantics are the Linux
// journal's (package migrate): in progress = !Done && FailedAt == "";
// rolling back = RollingBack with FailedStep; failed = FailedAt != "".
type Journal struct {
	Version   string `json:"version"`
	Step      int    `json:"step"` // steps completed
	Done      bool   `json:"done,omitempty"`
	FailedAt  string `json:"failed_at,omitempty"`
	Error     string `json:"error,omitempty"`
	StartedAt string `json:"started_at"`
	UpdatedAt string `json:"updated_at"`

	Planned        bool   `json:"planned,omitempty"`
	Strategy       string `json:"strategy,omitempty"`
	StrategyReason string `json:"strategy_reason,omitempty"`
	// PreferInPlace: an earlier attempt could not move the folder; this one
	// keeps it in place.
	PreferInPlace bool `json:"prefer_in_place,omitempty"`

	Legacy LegacyService `json:"legacy"`
	New    NewService    `json:"new"`

	Containers        []ContainerRecord `json:"containers,omitempty"`
	DockerUnavailable bool              `json:"docker_unavailable,omitempty"`
	// RepairBackedUp / TaskBackedUp: prepare saved the legacy repair script
	// and task definition to .migrate.
	RepairBackedUp bool `json:"repair_backed_up,omitempty"`
	TaskBackedUp   bool `json:"task_backed_up,omitempty"`
	// CreatedService: this journal created ironflock-device-agent.
	CreatedService bool `json:"created_service,omitempty"`
	// EnsuredService is the service ensure-agent left running ("" when it
	// could not), EnsureError why not.
	EnsuredService string `json:"ensured_service,omitempty"`
	EnsureError    string `json:"ensure_error,omitempty"`
	// Adopted: the agent running as ironflock-device-agent after a rollback
	// that ensure-agent resolved by starting it took the journal over
	// (the state it found).
	Adopted string `json:"adopted,omitempty"`

	// RollingBack: the steps up to FailedStep are being undone; a resume
	// continues the rollback, never the migration.
	RollingBack bool `json:"rolling_back,omitempty"`
	FailedStep  int  `json:"failed_step,omitempty"`
	// Runs counts the migration processes of this attempt (maxRuns).
	Runs int `json:"runs,omitempty"`
	// Attempts counts the attempts that rolled back.
	Attempts int `json:"attempts,omitempty"`
	// Moved: move-folder renamed the folder (the undo moves it back).
	Moved bool `json:"moved,omitempty"`
	// RestoredFromBackup: the journal was unreadable and its .bak was used;
	// one more step may have partially run.
	RestoredFromBackup bool `json:"restored_from_backup,omitempty"`
}

// Failed reports whether the migration failed and was rolled back.
func (j *Journal) Failed() bool { return j.FailedAt != "" }

// InProgressState reports whether the migration started and is neither done
// nor rolled back.
func (j *Journal) InProgressState() bool { return !j.Done && !j.Failed() }

// ensured reports whether ensure-agent ended with an agent service running.
// A recorded error is not an outcome: the next start of the migrate service
// (its recovery action, or the boot) tries again.
func (j *Journal) ensured() bool { return j.EnsuredService != "" }

// ErrJournalUnreadable: both the journal and its backup exist but neither
// can be read; the migration can only make sure an agent runs.
var ErrJournalUnreadable = errors.New("the migration journal and its backup are unreadable")

type reader func(path string) ([]byte, error)

// LoadJournal reads the journal of layout l from the filesystem; nil when
// there is none. An unreadable journal falls back to its backup (the journal
// is then marked RestoredFromBackup). ErrJournalUnreadable when both are
// unreadable.
func LoadJournal(l Layout) (*Journal, error) {
	return loadJournal(os.ReadFile, l)
}

func loadJournal(read reader, l Layout) (*Journal, error) {
	j, err := readJournal(read, l.Journal())
	if err == nil {
		return j, nil
	}
	primaryMissing := errors.Is(err, os.ErrNotExist)
	backup, backupErr := readJournal(read, l.JournalBak())
	if backupErr == nil {
		if backup == nil {
			if primaryMissing {
				return nil, nil
			}
			return nil, ErrJournalUnreadable
		}
		backup.RestoredFromBackup = true
		return backup, nil
	}
	if primaryMissing && errors.Is(backupErr, os.ErrNotExist) {
		return nil, nil
	}
	return nil, ErrJournalUnreadable
}

func readJournal(read reader, path string) (*Journal, error) {
	data, err := read(path)
	if err != nil {
		return nil, err
	}
	var j Journal
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("unreadable migration journal %s: %w", path, err)
	}
	if j.Version == "" {
		return nil, fmt.Errorf("unreadable migration journal %s: no version", path)
	}
	return &j, nil
}

// saveJournal writes the journal; the previous file is kept as .bak first,
// so one of the two is always readable (the write itself is atomic). A
// previous file that cannot be read (this run was restored from the backup)
// is not copied over the backup: a cut between that copy and the write would
// leave both unreadable.
func (e Env) saveJournal(j *Journal) error {
	j.UpdatedAt = e.Sys.Now().UTC().Format(time.RFC3339)
	j.RestoredFromBackup = false
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	if err := e.Sys.MkdirAll(e.Layout.Root, 0o755); err != nil {
		return err
	}
	if _, err := readJournal(e.Sys.ReadFile, e.Layout.Journal()); err == nil {
		if err := e.Sys.CopyFile(e.Layout.Journal(), e.Layout.JournalBak(), 0o644); err != nil {
			return fmt.Errorf("keeping the journal backup: %w", err)
		}
	}
	return e.Sys.WriteFile(e.Layout.Journal(), data, 0o644)
}

// Failed reports whether the last migration of layout l rolled back.
func Failed(l Layout) bool {
	j, err := LoadJournal(l)
	return err == nil && j != nil && j.Failed()
}

// InProgress reports whether a migration of layout l has started and is
// neither done nor rolled back.
func InProgress(l Layout) bool {
	j, err := LoadJournal(l)
	return err == nil && j != nil && j.InProgressState()
}

// Due reports whether a migration should be started now: none started yet,
// or the last attempt failed more than a day ago (without end: every device
// moves eventually).
func Due(e Env) (bool, error) {
	j, err := loadJournal(e.Sys.ReadFile, e.Layout)
	if err != nil {
		return false, err
	}
	if j == nil {
		return true, nil
	}
	if j.Done || !j.Failed() {
		// done, or in progress (the migrate service resumes it)
		return false, nil
	}
	failedAt, err := time.Parse(time.RFC3339, j.FailedAt)
	elapsed := e.Sys.Now().Sub(failedAt)
	// A clock behind the stamp (no RTC, a stale saved clock) must not hold
	// the retry off: early is harmless, never is not.
	return err != nil || elapsed >= retryAfterFailure || elapsed < 0, nil
}

// LastError is the error of a failed migration, for the heartbeat stats.
func LastError(l Layout) string {
	j, err := LoadJournal(l)
	if err != nil {
		return err.Error()
	}
	if j == nil || !j.Failed() {
		return ""
	}
	return fmt.Sprintf("%s (attempt %d; retried at the agent's next start a day later, or by an update from Studio)", j.Error, j.Attempts)
}
