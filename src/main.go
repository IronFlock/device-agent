package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"reagent/benchmark"

	// "reagent/common"
	"reagent/config"
	"reagent/filesystem"
	"reagent/lifecycle"
	"reagent/logging"
	"reagent/release"
	"reagent/safe"
	"reagent/system"

	"runtime"
	"runtime/debug"
	"strconv"
	"syscall"
	"time"

	_ "net/http/pprof" //nolint:gosec // G108: profiling is opt-in (-profiling flag) for local debugging

	"github.com/rs/zerolog/log"
)

func main() {
	defer func() {
		err := recover()

		if err != nil {
			log.Fatal().Msgf("Panic: %+v \n Stack Trace: %s", err, debug.Stack())
		}
	}()

	// `reagent service install|uninstall|start|stop|status` manages the
	// Windows service. It must be dispatched before GetCliArguments: the
	// global flag package stops parsing at the first positional argument.
	if len(os.Args) > 1 && os.Args[1] == "service" {
		os.Exit(runServiceCommand(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "migrate-layout" {
		os.Exit(runMigrateCommand(os.Args[2:]))
	}

	cliArgs, err := config.GetCliArguments()
	if err != nil {
		log.Fatal().Stack().Err(err).Msg("Failed to get CLI args")
	}

	// if release.BuildArch == "" && cliArgs.Environment != string(common.LOCAL) {
	// 	fmt.Println("The 'reagent/release.BuildArch' build flag was not included during the build of this version.")
	// 	os.Exit(1)
	// }

	if cliArgs.ConfigFileLocation == "" {
		if cliArgs.Version {
			fmt.Println(release.GetVersion())
			os.Exit(0)
		}

		if cliArgs.Arch {
			fmt.Println(release.BuildArch)
			os.Exit(0)
		}

		fmt.Println("'-config' argument is required. -help for usage")
		os.Exit(0)
	}

	// Refuse a second agent on the machine (a service + console double-run
	// makes the two agents delete each other's containers) and, on Windows,
	// reap child processes on any exit.
	err = acquireSingleInstanceLock()
	if err != nil {
		log.Fatal().Err(err).Msg("failed to acquire single-instance lock")
	}
	setupProcessJobObject()

	if runningAsService() {
		runService(cliArgs)
		return
	}

	agent, err := runAgent(cliArgs)
	if err != nil {
		// Also on stderr: the journal is where an operator looks first, and
		// the log file only shows up once they know to look for it.
		fmt.Fprintln(os.Stderr, "failed to start agent:", err)
		log.Fatal().Stack().Err(err).Msg("failed to start agent")
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	<-sigChan

	agent.Shutdown(time.Second * 10)
}

// runAgent brings up the full agent (directories, config, docker, WAMP,
// OnConnect) and returns once the agent is operational. Shared by console mode
// and the Windows service.
func runAgent(cliArgs *config.CommandLineArguments) (*Agent, error) {
	if cliArgs.Profiling {
		port := cliArgs.ProfilingPort
		if cliArgs.ProfilingPort == 0 {
			port = 80
		}

		server := newProfilingServer(port)
		safe.Go(func() {
			err := server.ListenAndServe()
			if err != nil {
				log.Error().Stack().Err(err).Msgf("Failed to init web server")
			}
		})

	}

	logging.SetupLogger(cliArgs)

	benchmark.PreConnectInit = time.Now()
	benchmark.OnConnectInit = time.Now()
	benchmark.SocketConnectionInitFromLaunch = time.Now()
	benchmark.GreenInit = time.Now()

	startupLogChannel := make(chan string)
	safe.Go(func() {
		for logMsg := range startupLogChannel {
			if !cliArgs.PrettyLogging {
				fmt.Println(logMsg)
			} else {
				log.Info().Msg(logMsg)
			}
		}
	})

	startupLogChannel <- fmt.Sprintf("Starting... Reagent initialization sequence (OOS: %s, ARCH: %s)", runtime.GOOS, runtime.GOARCH)

	err := filesystem.InitDirectories(cliArgs)
	if err != nil {
		return nil, fmt.Errorf("failed to init reagent directories: %w", err)
	}

	reswarmConfig, err := config.LoadReswarmConfig(cliArgs.ConfigFileLocation)
	if err != nil {
		return nil, fmt.Errorf("failed to load .flock config file: %w", err)
	}
	applyTestFault(reswarmConfig)

	generalConfig := config.New(cliArgs, reswarmConfig)
	generalConfig.StartupLogChannel = startupLogChannel

	// The agent updates itself only here, at its start, and when a user asks
	// for it (the update_agent call) — never on its own while it runs, which
	// would interrupt the device's service. The check runs before NewAgent,
	// which waits for the connection to IronFlock: a version that cannot
	// connect still receives the release that fixes it when it starts again
	// (a rollback, a crash, a reboot).
	checkForUpdateAtStart(&generalConfig)

	agent := NewAgent(&generalConfig)
	lifecycle.MarkReady()
	watchLayoutMigration(cliArgs, agent.Messenger.Connected)

	startupLogChannel <- "Running onConnect handler"

	err = agent.OnConnect(false)
	if err != nil {
		return nil, fmt.Errorf("failed to run the onConnect handler: %w", err)
	}

	benchmark.TimeTillOnConnectAfterConnection = time.Since(benchmark.OnConnectInitAfterConnection)
	benchmark.TimeTillOnConnect = time.Since(benchmark.OnConnectInit)

	benchmark.LogBenchmarkResultsWhenFinished(&generalConfig)

	return agent, nil
}

// newProfilingServer returns the pprof server (net/http/pprof on the default
// mux). It listens on the loopback interface only, so it is reachable on the
// device itself or through an SSH tunnel, never from the network. The write
// timeout leaves room for the default 30 s CPU profile: pprof refuses a profile
// longer than the server's WriteTimeout.
func newProfilingServer(port uint) *http.Server {
	return &http.Server{
		Addr:              net.JoinHostPort("127.0.0.1", strconv.FormatUint(uint64(port), 10)),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
}

func checkForUpdateAtStart(cfg *config.Config) {
	updates := system.New(cfg, nil)
	safe.Go(func() {
		if _, err := updates.UpdateSystem(nil, cfg.CommandLineArguments.ShouldUpdateAgent); err != nil {
			log.Error().Err(err).Msg("update check failed")
		}
	})
}

// applyTestFault breaks a test build on purpose (release.TestFault).
func applyTestFault(flock *config.ReswarmConfig) {
	switch release.TestFault {
	case "":
	case "crash":
		log.Error().Msg("test build: crashing at start (release.TestFault=crash)")
		os.Exit(1)
	case "offline":
		log.Error().Msg("test build: never reaching IronFlock (release.TestFault=offline)")
		flock.DeviceEndpointURL = "ws://127.0.0.1:9/ws-test-fault"
	}
}
