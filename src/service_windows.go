//go:build windows

package main

import (
	"context"
	"os"
	"reagent/config"
	"reagent/lifecycle"
	"reagent/release"
	"reagent/safe"
	"reagent/selfupdate"
	"time"

	"github.com/rs/zerolog/log"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
)

const serviceName = "reagent"

// runningAsService reports whether the SCM started this process.
func runningAsService() bool {
	isService, err := svc.IsWindowsService()
	if err != nil {
		log.Error().Err(err).Msg("failed to detect windows service context")
		return false
	}
	return isService
}

// runService hosts the agent under the service control manager.
func runService(cliArgs *config.CommandLineArguments) {
	err := svc.Run(serviceName, &agentService{cliArgs: cliArgs})
	if err != nil {
		log.Error().Err(err).Msg("service run failed")
		os.Exit(1)
	}
}

type agentService struct {
	cliArgs *config.CommandLineArguments
}

// exitForRestart terminates the process without reporting SERVICE_STOPPED.
// The SCM treats that as a crash, so the configured failure actions restart
// the service under ANY recovery configuration — this is the deliberate
// restart mechanism used for update activation, remote restarts, and fatal
// init errors. (Returning a non-zero code from Execute would report STOPPED
// and only restart when SetRecoveryActionsOnNonCrashFailures is on.)
func exitForRestart(elog *eventlog.Log, reason string) {
	if elog != nil {
		_ = elog.Info(1, "reagent is exiting for a supervised restart: "+reason)
	}
	log.Info().Msgf("exiting for a supervised restart: %s", reason)
	os.Exit(1)
}

func (s *agentService) Execute(args []string, requests <-chan svc.ChangeRequest, changes chan<- svc.Status) (ssec bool, errno uint32) {
	elog, err := eventlog.Open(serviceName)
	if err == nil {
		defer func() { _ = elog.Close() }()
	} else {
		elog = nil
	}

	changes <- svc.Status{State: svc.StartPending, CheckPoint: 1, WaitHint: 30_000}

	// Update-probation bookkeeping must run before the agent: it may decide
	// to roll back to the previous binary and restart.
	updateManager := selfupdate.New(s.cliArgs.AgentDir)
	startResult := updateManager.OnServiceStart(release.GetVersion())
	if startResult.RollbackPerformed {
		exitForRestart(elog, "rolled back a failed agent update")
	}

	restartRequests := lifecycle.Subscribe()

	agentChan := make(chan *Agent, 1)
	errChan := make(chan error, 1)
	safe.Go(func() {
		agent, err := runAgent(s.cliArgs)
		if err != nil {
			errChan <- err
			return
		}
		agentChan <- agent
	})

	// Report RUNNING immediately: agent init waits for WAMP and (possibly
	// much later, at user login) Docker Desktop — the SCM must not time out
	// on either.
	changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	if elog != nil {
		_ = elog.Info(1, "reagent service started (v"+release.GetVersion()+")")
	}

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
		Connected: lifecycle.Connected,
	})

	var agent *Agent
	shutdownAgent := func() {
		changes <- svc.Status{State: svc.StopPending, WaitHint: 20_000}
		if agent != nil {
			agent.Shutdown(15 * time.Second)
		}
	}

	for {
		select {
		case reason := <-rollback:
			shutdownAgent()
			exitForRestart(elog, reason)

		case startedAgent := <-agentChan:
			agent = startedAgent

		case err := <-errChan:
			log.Error().Err(err).Msg("agent failed to start")
			if elog != nil {
				_ = elog.Error(1, "reagent failed to start: "+err.Error())
			}
			// Crash-exit so SCM restarts us (throttled by the recovery
			// delays); Docker/WAMP outages at boot resolve themselves this
			// way, real breakage stays visible in the event log.
			exitForRestart(elog, "agent startup failed")

		case reason := <-restartRequests:
			shutdownAgent()
			exitForRestart(elog, reason)

		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				changes <- request.CurrentStatus
			case svc.Stop, svc.Shutdown:
				if elog != nil {
					_ = elog.Info(1, "reagent service stopping")
				}
				shutdownAgent()
				// svc.Run reports SERVICE_STOPPED (exit code 0) after
				// Execute returns — a clean stop, no recovery restart.
				return false, 0
			}
		}
	}
}
