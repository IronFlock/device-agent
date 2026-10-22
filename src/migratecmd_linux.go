//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"reagent/config"
	"reagent/filesystem"
	"reagent/layout"
	"reagent/lifecycle"
	"reagent/migrate"
	"reagent/release"
	"reagent/safe"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// runMigrateCommand handles `device-agent migrate-layout`:
//
//	(no verb)  continue the migration from its journal: the ExecStart of the
//	           ironflock-device-agent-migrate unit
//	start      check the device and start the migration in its unit;
//	           -foreground runs it in this process instead (the appliance
//	           installer, while the stack is stopped)
//	status     print the journal
func runMigrateCommand(args []string) int {
	stderrLog := func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }
	env := migrate.SystemEnv(release.GetVersion(), stderrLog)
	var err error
	switch {
	case len(args) == 0:
		err = migrate.Run(env)
	case args[0] == "start":
		err = migrateStart(env, args[1:])
	case args[0] == "status":
		var j *migrate.Journal
		if j, err = migrate.LoadJournal(""); err == nil {
			if j == nil {
				fmt.Println("no migration")
				return 0
			}
			out, _ := json.MarshalIndent(j, "", "  ")
			fmt.Println(string(out))
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: device-agent migrate-layout [start [-foreground] | status]")
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func migrateStart(env migrate.Env, args []string) error {
	flags := flag.NewFlagSet("migrate-layout start", flag.ContinueOnError)
	foreground := flags.Bool("foreground", false, "run the migration in this process and wait for it")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return errors.New("migrate-layout must run as root")
	}
	if j, err := migrate.LoadJournal(""); err != nil {
		return err
	} else if j != nil && !j.Done && !j.Failed() {
		return errors.New("a migration is in progress; see `device-agent migrate-layout status`")
	}
	if err := migrate.Check(env); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if !*foreground {
		return migrate.Begin(env, self)
	}
	if err := migrate.Stage(env, self); err != nil {
		return err
	}
	return migrate.Run(env)
}

const (
	// migrationFirstCheck: the new version first proves itself in the legacy
	// layout (the legacy manager's probation) before it moves.
	migrationFirstCheck = 5 * time.Minute
	// migrationSwitch (absent means enabled) stops new migrations
	// fleet-wide: {"enabled": false}. It can only hold devices back, so it
	// is not signed.
	migrationSwitch = "device-agent/migration.json"
)

// watchLayoutMigration moves a legacy install to the current layout. The move
// restarts Docker and every app once, so — like an update — it starts only
// at the agent's start (once, 5 minutes after the agent connected: the
// release that brings it arrived at a start or by a user's update) and when
// a user asks for it (Studio's update button, `migrate-layout start`), never
// on its own while the agent runs. A rolled-back attempt is tried again at a
// later start, a day after the failure at the earliest.
func watchLayoutMigration(cliArgs *config.CommandLineArguments, connected func() bool) {
	if cliArgs.Layout == layout.Current || runningAsService() || os.Geteuid() != 0 {
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
	env := migrate.SystemEnv(release.GetVersion(), func(format string, args ...any) {
		log.Info().Msgf(format, args...)
	})
	// Staged, but the legacy agent died before it started the unit (or the
	// unit is not running for another reason): start it; a running one is
	// left alone.
	if migrate.InProgress("") {
		return true, env.Systemctl("start", "--no-block", migrate.MigrateUnitName)
	}
	due, err := migrate.Due(env)
	if err != nil {
		return false, err
	}
	if !due && !(manual && migrate.Failed("")) {
		return false, nil
	}
	// Only from a connected agent: the move is done once the new agent
	// connects, and a device that cannot connect now would only be rolled
	// back (and restart Docker and its apps for nothing).
	if !connected() {
		log.Info().Msg("layout migration waits until the agent is connected to IronFlock")
		return false, nil
	}
	if err := migrate.Check(env); err != nil {
		if errors.Is(err, migrate.ErrNotEligible) {
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
	log.Info().Msgf("moving the agent to %s", layout.AgentDir)
	return true, migrate.Begin(env, self)
}

// updateBase is where the agent updates from: the .flock's update_url (an
// appliance mirror), else -remoteUpdateURL. The switch lives next to the
// manifest there.
func updateBase(cliArgs *config.CommandLineArguments) string {
	if flock, err := config.LoadReswarmConfig(cliArgs.ConfigFileLocation); err == nil && flock.UpdateURL != "" {
		return flock.UpdateURL
	}
	return cliArgs.RemoteUpdateURL
}

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
		return true, nil
	default:
		return false, fmt.Errorf("reading %s: HTTP %d", url, status)
	}
	var sw struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(body).Decode(&sw); err != nil {
		return false, fmt.Errorf("reading %s: %w", url, err)
	}
	if sw.Enabled != nil && !*sw.Enabled {
		log.Info().Msgf("layout migration switched off by %s", url)
		return false, nil
	}
	return true, nil
}
