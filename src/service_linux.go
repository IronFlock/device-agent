//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"reagent/config"
	"reagent/lifecycle"
	"reagent/release"
	"reagent/safe"
	"reagent/selfupdate"
	"syscall"
	"time"

	"github.com/rs/zerolog/log"
)

// supervisorEnv is set by ironflock-device-agent.service. Under it the agent
// activates its own updates and restarts by exiting. A legacy reagent.service
// sets nothing, so a legacy install keeps the download-only behavior and
// reagent-manager.sh activates updates there.
const supervisorEnv = "IRONFLOCK_AGENT_SUPERVISOR"

// exitCodeRestart is the unit's SuccessExitStatus: a deliberate exit, which
// Restart=always turns into a restart without logging a failure.
const exitCodeRestart = 75

// readyFile and connectedFile tell `device-agent migrate-layout` that the
// agent under the new unit finished its local start, and that it is
// connected to IronFlock ("<pid> <version>"). /run/ironflock-device-agent is
// the unit's RuntimeDirectory, recreated empty at every start.
const (
	readyFile     = "/run/ironflock-device-agent/ready"
	connectedFile = "/run/ironflock-device-agent/connected"
)

func runningAsService() bool {
	return os.Getenv(supervisorEnv) == "systemd"
}

func exitForRestart(reason string) {
	log.Info().Msgf("exiting for a supervised restart: %s", reason)
	os.Exit(exitCodeRestart)
}

// runService hosts the agent under systemd, mirroring the Windows service
// loop (service_windows.go): update probation first, then the agent, then
// restart requests and signals.
func runService(cliArgs *config.CommandLineArguments) {
	updateManager := selfupdate.New(cliArgs.AgentDir, selfupdate.NamesFor(cliArgs.BinaryName))
	if updateManager.OnServiceStart(release.GetVersion()).RollbackPerformed {
		exitForRestart("rolled back a failed agent update")
	}

	restartRequests := lifecycle.Subscribe()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, os.Interrupt)

	agentChan := make(chan *Agent, 1)
	errChan := make(chan error, 1)
	safe.Go(func() {
		agent, err := runAgent(cliArgs)
		if err != nil {
			errChan <- err
			return
		}
		agentChan <- agent
	})

	// Probation and the judgement of a rolled-back version hang on the
	// connection to IronFlock (selfupdate.Watch).
	ready := lifecycle.Ready()
	rollback := updateManager.Watch(context.Background(), selfupdate.WatchConfig{
		Version: release.GetVersion(),
		Ready: func() bool {
			select {
			case <-ready:
				return true
			default:
				return false
			}
		},
		Connected:   lifecycle.Connected,
		OnConnected: func() { writeRunFile(connectedFile) },
	})
	safe.Go(func() {
		<-ready
		writeRunFile(readyFile)
	})

	var agent *Agent
	shutdown := func() {
		if agent != nil {
			agent.Shutdown(15 * time.Second)
		}
	}

	for {
		select {
		case reason := <-rollback:
			shutdown()
			exitForRestart(reason)

		case startedAgent := <-agentChan:
			agent = startedAgent

		case err := <-errChan:
			// Also on stderr: the journal is where an operator looks first.
			fmt.Fprintln(os.Stderr, "failed to start agent:", err)
			log.Error().Err(err).Msg("agent failed to start")
			exitForRestart("agent startup failed")

		case reason := <-restartRequests:
			shutdown()
			exitForRestart(reason)

		case sig := <-signals:
			log.Info().Msgf("received %s, shutting down", sig)
			shutdown()
			os.Exit(0)
		}
	}
}

// writeRunFile records the running agent's PID and version. Failure only
// costs migrate-layout its health signal (it then rolls back), so it is
// logged, never fatal.
func writeRunFile(path string) {
	err := os.MkdirAll(filepath.Dir(path), 0o755)
	if err == nil {
		err = os.WriteFile(path, []byte(fmt.Sprintf("%d %s\n", os.Getpid(), release.GetVersion())), 0o644) //nolint:gosec // G306: pid file, no secrets
	}
	if err != nil {
		log.Warn().Err(err).Msgf("could not write %s", path)
	}
}
