//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reagent/config"
	"reagent/filesystem"
	"reagent/layout"
	"reagent/lifecycle"
	"reagent/migrate/winmigrate"
	"reagent/release"
	"reagent/safe"
	"reagent/winservice"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
)

// runMigrateCommand handles `device-agent migrate-layout` on Windows:
//
//	(no verb)  started by the service control manager: the migrate service
//	           ironflock-device-agent-migrate, which continues the migration
//	           from its journal; from a console: the same, in the foreground
//	start      check the device and start the migration in the migrate
//	           service (a migration in progress is started again);
//	           -foreground runs it in this process instead. It ignores the
//	           fleet switch: this is how a device is moved by hand.
//	status     print the journal
func runMigrateCommand(args []string) int {
	if len(args) == 0 {
		if isService, err := svc.IsWindowsService(); err == nil && isService {
			return runMigrateService()
		}
	}
	stderrLog := func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }
	env := winmigrate.SystemEnv(release.GetVersion(), stderrLog)
	var err error
	switch {
	case len(args) == 0:
		if err = requireElevation(); err == nil {
			err = runMigrationHere(env)
		}
	case args[0] == "start":
		err = migrateStart(env, args[1:])
	case args[0] == "status":
		var j *winmigrate.Journal
		if j, err = winmigrate.LoadJournal(env.Layout); err == nil {
			if j == nil {
				fmt.Println("no migration")
				return 0
			}
			out, _ := json.MarshalIndent(j, "", "  ")
			fmt.Println(string(out))
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: device-agent.exe migrate-layout [start [-foreground] | status]")
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func migrateStart(env winmigrate.Env, args []string) error {
	flags := flag.NewFlagSet("migrate-layout start", flag.ContinueOnError)
	foreground := flags.Bool("foreground", false, "run the migration in this process and wait for it")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := requireElevation(); err != nil {
		return err
	}
	// Files nobody trusted go before the journal is read.
	if err := winmigrate.RemoveForeign(env); err != nil {
		return err
	}
	j, err := winmigrate.LoadJournal(env.Layout)
	if err != nil {
		return err
	}
	if j != nil && j.InProgressState() {
		// Staged or interrupted: the migrate service resumes it. A running
		// one is left alone.
		if err := startMigrateService(env); err != nil {
			return fmt.Errorf("the migration is in progress but its service could not be started: %w", err)
		}
		fmt.Printf("the migration is in progress; service %s started (see `device-agent.exe migrate-layout status`)\n", winservice.MigrateServiceName)
		return nil
	}
	if err := winmigrate.Check(env); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if !*foreground {
		if err := winmigrate.Begin(env, self); err != nil {
			return err
		}
		fmt.Printf("started the move to the %s service in service %s; follow it with `device-agent.exe migrate-layout status` and %s\n",
			winservice.ServiceName, winservice.MigrateServiceName, env.Layout.LogFile())
		return nil
	}
	if err := winmigrate.ForegroundCheck(env, self); err != nil {
		return err
	}
	if err := winmigrate.Stage(env, self); err != nil {
		return err
	}
	return winmigrate.Run(env)
}

// runMigrationHere runs the engine in this console process (`migrate-layout`
// with no verb, as on Linux): only after the files nobody trusted are gone
// (ForegroundCheck runs RemoveForeign: the root may not be hardened yet, and
// Run reads the journal), never next to the migrate service, and never from
// inside the folder the move renames.
func runMigrationHere(env winmigrate.Env) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := winmigrate.ForegroundCheck(env, self); err != nil {
		return err
	}
	return winmigrate.Run(env)
}

// runMigrateService hosts the migration under the service control manager.
func runMigrateService() int {
	if err := svc.Run(winservice.MigrateServiceName, &migrateService{}); err != nil {
		return 1
	}
	return 0
}

// migrateService is the service handler; env and serve are what Execute
// runs (the real machine and winmigrate.Serve unless a test sets them).
type migrateService struct {
	env   *winmigrate.Env
	serve func(winmigrate.Env, winmigrate.ServeOptions) uint32
}

// Execute runs the migration (winmigrate.Serve: the watchdog, the stop
// request, the exit code) and reports the service's states. Its log goes to
// .migrate\migrate.log, never inside the folder being moved, and to the
// event log.
func (s *migrateService) Execute(args []string, requests <-chan svc.ChangeRequest, changes chan<- svc.Status) (ssec bool, errno uint32) {
	changes <- svc.Status{State: svc.StartPending, WaitHint: 10_000}
	var env winmigrate.Env
	if s.env != nil {
		env = *s.env
	} else {
		env = winmigrate.SystemEnv(release.GetVersion(), migrateLogger())
	}
	serve := s.serve
	if serve == nil {
		serve = winmigrate.Serve
	}

	stop := make(chan struct{})
	var stopOnce sync.Once
	result := make(chan uint32, 1)
	safe.Go(func() { result <- serve(env, winmigrate.ServeOptions{Stop: stop}) })

	changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case code := <-result:
			changes <- svc.Status{State: svc.StopPending}
			// A non-zero code is a service-specific exit code: the service
			// control manager counts it as a failure and runs the recovery
			// actions (SetRecoveryActionsOnNonCrashFailures), starting the
			// service again.
			return code != 0, code
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				changes <- request.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending, WaitHint: uint32(winmigrate.DefaultStopGrace / time.Millisecond)}
				stopOnce.Do(func() { close(stop) })
			}
		}
	}
}

// migrateLogger writes to .migrate\migrate.log (appending; best effort) and
// the migrate service's event log source.
func migrateLogger() func(format string, args ...any) {
	var file *os.File
	path := winmigrate.DefaultLayout().LogFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
		file, _ = os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644) //nolint:gosec // G302/G304: the migration's own log
	}
	elog, _ := eventlog.Open(winservice.MigrateServiceName)
	var mu sync.Mutex
	return func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		mu.Lock()
		defer mu.Unlock()
		if file != nil {
			_, _ = fmt.Fprintf(file, "%s %s\n", time.Now().UTC().Format(time.RFC3339), line)
		}
		if elog != nil {
			_ = elog.Info(1, line)
		}
	}
}

// watchLayoutMigration moves a Windows service install from before 1.4.0
// (the service reagent) to the current names. The move stops the agent and
// its apps once, so — like an update — it starts only at the agent's start
// (once, 5 minutes after the agent connected) and when a user asks for it
// (Studio's update button, `migrate-layout start`), never on its own while
// the agent runs. A rolled-back attempt is tried again at a later start, a
// day after the failure at the earliest.
//
// The gate: this process runs the legacy layout as the service reagent (the
// service control manager's process of that service is this one), elevated.
// A console reagent.exe (FlockFlasher's test device) has no service to
// replace and never moves.
func watchLayoutMigration(cliArgs *config.CommandLineArguments, connected func() bool) {
	if cliArgs.Layout != layout.Legacy || runningServiceName() != winservice.LegacyServiceName || !windows.GetCurrentProcessToken().IsElevated() {
		return
	}
	lifecycle.SetLayoutMigration(func() (bool, error) { return tryLayoutMigration(cliArgs, connected, true) })
	safe.Go(func() {
		time.Sleep(migrationFirstCheck)
		for !connected() {
			time.Sleep(30 * time.Second)
		}
		if _, err := tryLayoutMigration(cliArgs, connected, false); err != nil {
			log.Warn().Err(err).Msg("layout migration not started")
		}
	})
}

// tryLayoutMigration starts the move when it is due (manual: a user asked,
// so the day's wait after a failure does not apply). It reports whether it
// started one.
func tryLayoutMigration(cliArgs *config.CommandLineArguments, connected func() bool, manual bool) (bool, error) {
	env := winmigrate.SystemEnv(release.GetVersion(), func(format string, args ...any) {
		log.Info().Msgf(format, args...)
	})
	// A journal (or migrator) a local user planted before the root was
	// hardened goes before anything reads it: it could block the move and,
	// with it, every update, for good.
	if err := winmigrate.RemoveForeign(env); err != nil {
		return false, err
	}
	// Staged, but the migrate service is not running (the legacy agent died
	// before it started it, or it was stopped): start it; a running one is
	// left alone.
	if winmigrate.InProgress(env.Layout) {
		return true, startMigrateService(env)
	}
	due, err := winmigrate.Due(env)
	if err != nil {
		return false, err
	}
	if !due && !(manual && winmigrate.Failed(env.Layout)) {
		return false, nil
	}
	// Only from a connected agent: the move is done once the new agent
	// connects, and a device that cannot connect now would only be rolled
	// back (and restart its apps for nothing).
	if !connected() {
		log.Info().Msg("layout migration waits until the agent is connected to IronFlock")
		return false, nil
	}
	if err := winmigrate.Check(env); err != nil {
		if errors.Is(err, winmigrate.ErrNotEligible) {
			log.Debug().Err(err).Msg("layout migration")
			return false, nil
		}
		return false, err
	}
	enabled, err := migrationEnabled(updateBase(cliArgs))
	if err != nil || !enabled {
		return false, err
	}
	self, err := os.Executable()
	if err != nil {
		return false, err
	}
	log.Info().Msgf("moving the agent to the %s service", winservice.ServiceName)
	return true, winmigrate.Begin(env, self)
}

// startMigrateService starts the migrate service of a migration in progress.
// When it cannot start because its executable went missing (an antivirus
// took it before the repair task knew the migration), the migrator is staged
// again from this executable, the same version, and started once more.
func startMigrateService(env winmigrate.Env) error {
	err := env.Sys.StartService(winservice.MigrateServiceName)
	if err == nil {
		return nil
	}
	self, selfErr := os.Executable()
	if selfErr != nil {
		return err
	}
	log.Warn().Err(err).Msg("the migrate service could not be started; staging the migrator again")
	if restageErr := winmigrate.Restage(env, self); restageErr != nil {
		return fmt.Errorf("%w; staging it again: %v", err, restageErr)
	}
	return env.Sys.StartService(winservice.MigrateServiceName)
}

// migrationEnabled reads the fleet switch for Windows: only {"windows": true}
// starts the move; an absent key, false or no file (404) keeps the fleet
// where it is. Any other answer (403 is what GCS gives for a missing object
// read by name, another status, a network error) is an error: nothing starts
// on a guess.
func migrationEnabled(base string) (bool, error) {
	url := strings.TrimSuffix(base, "/") + "/" + migrationSwitch
	body, status, err := filesystem.GetRemoteFileWithStatus(url)
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", url, err)
	}
	defer func() { _ = body.Close() }()
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		log.Info().Msgf("layout migration for Windows not switched on: no %s", url)
		return false, nil
	default:
		return false, fmt.Errorf("reading %s: HTTP %d", url, status)
	}
	var sw struct {
		Windows *bool `json:"windows"`
	}
	if err := json.NewDecoder(body).Decode(&sw); err != nil {
		return false, fmt.Errorf("reading %s: %w", url, err)
	}
	if sw.Windows == nil || !*sw.Windows {
		log.Info().Msgf("layout migration for Windows not switched on by %s", url)
		return false, nil
	}
	return true, nil
}
