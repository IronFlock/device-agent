//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reagent/config"
	"reagent/lifecycle"
	"reagent/migrate/winmigrate"
	"reagent/release"
	"reagent/safe"
	"reagent/selfupdate"
	"reagent/winservice"
	"time"

	"github.com/rs/zerolog/log"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

// serviceName is the agent's service since 1.4.0; a device installed before
// runs the service reagent until the layout migration (package winmigrate)
// moves it. svc.Run takes the name for SERVICE_WIN32_OWN_PROCESS without
// checking it, so one constant serves both.
const serviceName = winservice.ServiceName

// runningAsService reports whether the SCM started this process.
func runningAsService() bool {
	isService, err := svc.IsWindowsService()
	if err != nil {
		log.Error().Err(err).Msg("failed to detect windows service context")
		return false
	}
	return isService
}

// runningServiceName is the name of the service this process runs as,
// ironflock-device-agent or reagent, found by matching the SCM's process id
// of each against our own; "" for a console run. A 1.4.0 that a legacy
// service updated itself to runs as reagent until it is migrated: its event
// log source, the connected file and the migration gate all follow this.
func runningServiceName() string {
	m, err := mgr.Connect()
	if err != nil {
		return ""
	}
	defer func() { _ = m.Disconnect() }()
	for _, name := range []string{winservice.ServiceName, winservice.LegacyServiceName} {
		service, err := m.OpenService(name)
		if err != nil {
			continue
		}
		status, err := service.Query()
		_ = service.Close()
		if err == nil && status.ProcessId == uint32(os.Getpid()) { //nolint:gosec // G115: a pid fits
			return name
		}
	}
	return ""
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
		_ = elog.Info(1, "the device agent is exiting for a supervised restart: "+reason)
	}
	log.Info().Msgf("exiting for a supervised restart: %s", reason)
	os.Exit(1)
}

// connectedFile is what the layout migration's health check reads: the
// agent service writes "<pid> <version> <service>" once it is connected to
// IronFlock (winmigrate). It is removed at the start of every service run,
// so a file an earlier process wrote never passes as this one's; the
// migrator never removes it.
func connectedFile(agentDir string) string {
	return filepath.Join(agentDir, winservice.ConnectedName)
}

func writeConnectedFile(agentDir string, service string) {
	content := fmt.Sprintf("%d %s %s\n", os.Getpid(), release.GetVersion(), service)
	err := os.WriteFile(connectedFile(agentDir), []byte(content), 0o644) //nolint:gosec // G306: pid file, no secrets
	if err != nil {
		log.Warn().Err(err).Msgf("could not write %s", connectedFile(agentDir))
	}
}

func (s *agentService) Execute(args []string, requests <-chan svc.ChangeRequest, changes chan<- svc.Status) (ssec bool, errno uint32) {
	// ownName stays "" when the service control manager could not say which
	// service this is: the migration helpers below then do nothing, and the
	// connected file is not written (a wrong name could pass as the new
	// service's). Only the event log falls back to the current name.
	ownName := runningServiceName()
	logName := ownName
	if logName == "" {
		logName = serviceName
	}
	elog, err := eventlog.Open(logName)
	if err == nil {
		defer func() { _ = elog.Close() }()
	} else {
		elog = nil
	}

	changes <- svc.Status{State: svc.StartPending, CheckPoint: 1, WaitHint: 30_000}

	if err := os.Remove(connectedFile(s.cliArgs.AgentDir)); err != nil && !os.IsNotExist(err) {
		log.Warn().Err(err).Msg("could not remove the connected file of an earlier run")
	}

	migrationEnv := winmigrate.SystemEnv(release.GetVersion(), func(format string, args ...any) {
		log.Info().Msgf(format, args...)
	})

	// Update-probation bookkeeping must run before the agent: it may decide
	// to roll back to the previous binary and restart.
	updateManager := selfupdate.New(s.cliArgs.AgentDir, selfupdate.NamesFor(s.cliArgs.BinaryName))
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
		_ = elog.Info(1, "device agent service started (v"+release.GetVersion()+")")
	}

	// A finished layout migration leaves its migrate service and
	// %ProgramData%\IronFlock\.migrate behind: the migrator cannot delete its
	// own executable. The agent removes them at its start — once it reported
	// running: the tail may take minutes (Defender, Docker, schtasks), and
	// the migrator's ensure-agent watch waits for RUNNING.
	safe.Go(func() { winmigrate.RemoveLeftovers(migrationEnv, ownName) })

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
		OnConnected: func() {
			if ownName == "" {
				log.Warn().Msg("not writing the connected file: the service this process runs as is unknown")
				return
			}
			writeConnectedFile(s.cliArgs.AgentDir, ownName)
			// A rollback that ensure-agent could only resolve by starting
			// this service: now that it is connected, the move counts as
			// done (winmigrate.Adopt) — once the migrate service, which may
			// still be watching this service or undoing again, has stopped.
			if ownName == winservice.ServiceName {
				safe.Go(func() {
					for !winmigrate.Adopt(migrationEnv) {
						time.Sleep(30 * time.Second)
					}
				})
			}
		},
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
				_ = elog.Error(1, "the device agent failed to start: "+err.Error())
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
					_ = elog.Info(1, "device agent service stopping")
				}
				shutdownAgent()
				// svc.Run reports SERVICE_STOPPED (exit code 0) after
				// Execute returns — a clean stop, no recovery restart.
				return false, 0
			}
		}
	}
}
