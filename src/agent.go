package main

import (
	"context"
	"errors"
	"os"
	"reagent/api"
	"reagent/apps"
	"reagent/benchmark"
	"reagent/common"
	"reagent/config"
	"reagent/container"
	"reagent/diskguard"
	"reagent/filesystem"
	"reagent/lifecycle"
	"reagent/logging"
	"reagent/messenger"
	"reagent/messenger/topics"
	"reagent/network"
	"reagent/persistence"
	"reagent/privilege"
	"reagent/release"
	"reagent/safe"
	"reagent/store"
	"reagent/system"
	"reagent/terminal"
	"reagent/trust"
	"reagent/tunnel"
	"runtime"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

type Agent struct {
	Container       container.Container
	Messenger       messenger.Messenger
	LogMessenger    messenger.Messenger
	Database        persistence.Database
	Network         network.Network
	System          *system.System
	Config          *config.Config
	External        *api.External
	LogManager      *logging.LogManager
	TerminalManager *terminal.TerminalManager
	TunnelManager   tunnel.TunnelManager
	Filesystem      *filesystem.Filesystem
	AppManager      *apps.AppManager
	StateObserver   *apps.StateObserver
	StateMachine    *apps.StateMachine

	// daemonReady is closed once the Docker daemon has been reachable and the
	// local app state was reconciled against it. On hosts where Docker starts
	// late (Docker Desktop starts at user login), the agent keeps running and
	// container features come up when the daemon does.
	daemonReady chan struct{}
}

// Shutdown releases the agent's external resources (WAMP session, database)
// with a bounded wait. App containers keep running — their state is
// reconciled on the next agent start.
func (agent *Agent) Shutdown(timeout time.Duration) {
	done := make(chan struct{})
	safe.Go(func() {
		if agent.Messenger != nil {
			agent.Messenger.Close()
		}
		if agent.Database != nil {
			err := agent.Database.Close()
			if err != nil {
				log.Error().Err(err).Msg("failed to close app state database")
			}
		}
		close(done)
	})

	select {
	case <-done:
		log.Info().Msg("agent shutdown complete")
	case <-time.After(timeout):
		log.Warn().Msg("agent shutdown timed out")
	}
}

func (agent *Agent) OnConnect(reconnect bool) error {
	var wg sync.WaitGroup

	// Refresh the cached device/project identity before anything is written
	// back. The device may have been moved to another project while we were
	// away, and every device-scoped backend write is keyed on swarm_key: the
	// backend resolves the device row by (device_key, swarm_key) and rejects a
	// stale pairing outright. Doing this after the first status update — as it
	// used to be — made the refresh unreachable for exactly the devices that
	// needed it, since that update is the first thing to fail.
	log.Info().Msg("Updating Device Meta Data ...")
	swarmChanged, err := agent.System.UpdateDeviceMetadata()
	if err != nil {
		log.Error().Err(err).Msg("update device metadata")
	}

	// A new swarm_key is also a new WAMP identity — authid is
	// "<swarm_key>-<device_key>", negotiated once per connection. Carry on over
	// this session and every call would present the new swarm_key under the old
	// authid, which the backend rejects just as hard as the stale pairing.
	// Reconnect instead; OnConnect runs again from the top on the new session.
	if swarmChanged {
		log.Info().Msgf("Device was moved to project %s (%d), reconnecting with the new identity ...",
			agent.Config.ReswarmConfig.SwarmName, agent.Config.ReswarmConfig.SwarmKey)
		agent.Messenger.Reconnect()
		return nil
	}

	// Per-app credential key: the material from which each app container's
	// APP_AUTH_SECRET is derived, so the platform can authorize the connecting
	// APP rather than only its device. Fetched here (once per session, cheap,
	// minted server-side on first call) and kept in agent memory only.
	// Never fatal: on failure apps keep the legacy device-wide credential,
	// which still authenticates them on their OWN realm.
	if appCredKey, credErr := agent.AppManager.AppStore.GetAppCredKey(); credErr != nil {
		log.Error().Err(credErr).Msg("could not fetch app credential key; apps fall back to the device credential")
	} else {
		agent.AppManager.StateMachine.SetAppCredKey(appCredKey)
	}

	log.Info().Msg("Updating Remote Device Status ...")
	// Never fatal. A rejected status write is recoverable (the next heartbeat
	// retries, and a reconnect re-runs the metadata refresh above), but exiting
	// here takes the agent down before it can reach any of that — which is what
	// left transferred devices in a permanent restart loop.
	err = agent.Messenger.UpdateRemoteDeviceStatus(messenger.CONFIGURING)
	if err != nil {
		log.Error().Stack().Err(err).Msg("failed to update remote device status")
	}

	// Register all endpoints early so terminal/system commands are available
	// even if Docker is not running - allows remote debugging
	log.Debug().Msg("Registering all endpoints...")
	err = agent.External.RegisterAll()
	if err != nil {
		log.Error().Stack().Err(err).Msg("failed to register all external endpoints")
	}

	// Re-establish per-terminal control registrations (term_write/term_resize)
	// for any open device terminals after a reconnect — the router dropped them
	// on the disconnect and the WampSession does not replay dynamic regs, so an
	// open terminal would otherwise lose input/resize until the agent restarts.
	if reconnect {
		terminal.ReregisterControlTopics(agent.Messenger)
	}

	// frpc is delivered on every supported platform: embedded on Linux/macOS,
	// downloaded (separate signed binary) on Windows. If it can't be acquired
	// or won't connect, SuperviseStart settles the device into an
	// unavailable-but-alive state — the agent and app starts are unaffected.
	if !reconnect {
		err = agent.System.DownloadFrpIfNotExists()
		if err != nil {
			log.Error().Stack().Err(err).Msg("failed to acquire frp tunnel client")
		}

		log.Info().Msg("Starting TunnelManager ...")
		safe.Go(func() {
			agent.TunnelManager.SuperviseStart()
		})
	}

	log.Info().Msg("Publishing agent metadata to backend ...")
	err = agent.updateRemoteDevice()
	if err != nil {
		log.Error().Stack().Err(err).Msg("failed to publish agent metadata to backend")
	}

	wg.Add(1)
	safe.Go(func() {
		defer wg.Done()

		err = agent.LogManager.ReviveDeadLogs()
		if err != nil {
			log.Error().Stack().Err(err).Msg("failed to revive dead logs")
		}

		err := agent.LogManager.SetupEndpoints()
		if err != nil {
			log.Error().Stack().Err(err).Msg("failed to setup endpoints")
		}
	})

	// Wait for Docker daemon before container operations. daemonReady also
	// guarantees the local app state was reconciled (NewAgent), so the remote
	// sync below starts from a consistent local view.
	log.Info().Msg("Waiting for Docker Daemon to be available...")
	<-agent.daemonReady
	log.Info().Msg("Docker Daemon is available")

	// Step 1: Fetch requested app states from backend
	log.Info().Msg("Fetching requested app states from backend...")
	remotePayloads, err := agent.AppManager.AppStore.FetchRequestedAppStates()
	if err != nil {
		log.Error().Stack().Err(err).Msg("failed to fetch requested app states from backend")
	} else {
		log.Info().Interface("remotePayloads", remotePayloads).Msg("Backend returned requested app states")

		// Step 2: Clean up orphaned apps from database using fetched payloads
		log.Info().Msg("Cleaning up orphaned apps from database...")
		err = agent.AppManager.CleanupOrphanedAppsFromDatabase(remotePayloads)
		if err != nil {
			log.Error().Stack().Err(err).Msg("failed to cleanup orphaned apps from database")
		}

		// Step 3: Update local database with remote payloads
		log.Info().Msg("Syncing local database with backend state...")
		err = agent.AppManager.UpdateLocalRequestedAppStatesWithRemote(remotePayloads)
		if err != nil {
			log.Error().Stack().Err(err).Msg("failed to sync app states with remote")
		} else {
			agent.AppManager.MarkDatabaseSynced()
		}

		// Step 4: Clean up orphaned containers not in database
		log.Info().Msg("Cleaning up orphaned containers...")
		err = agent.AppManager.CleanupOrphanedContainers()
		if err != nil {
			log.Error().Stack().Err(err).Msg("failed to cleanup orphaned containers")
		}
	}

	log.Info().Msg("Correcting local app states ...")
	err = agent.StateObserver.CorrectAppStates(true)
	if err != nil {
		log.Error().Stack().Err(err).Msg("failed to CorrectLocalAndUpdateRemoteAppStates")
	}

	log.Info().Msg("Starting app state observer ...")
	err = agent.StateObserver.ObserveAppStates()
	if err != nil {
		log.Error().Stack().Err(err).Msg("failed to init app state observers")
	}

	log.Info().Msg("Ensuring app states ...")
	err = agent.AppManager.EnsureRemoteRequestedStates()
	if err != nil {
		log.Error().Stack().Err(err).Msg("failed to EvaluateRequestedStates")
	}

	log.Debug().Msg("Waiting for startup setup to finish...")
	wg.Wait()

	log.Debug().Msg("Startup setup finished!")
	err = agent.Messenger.UpdateRemoteDeviceStatus(messenger.CONNECTED)
	if err != nil {
		log.Error().Stack().Err(err).Msg("failed to update remote device status")
	}

	benchmark.TimeTillGreen = time.Since(benchmark.GreenInit)

	return err
}

func (agent *Agent) updateRemoteDevice() error {
	config := agent.Config
	ctx := context.Background()

	sysInfo := release.GetSystemInfo()
	payload := common.Dict{
		"swarm_key":     config.ReswarmConfig.SwarmKey,
		"device_key":    config.ReswarmConfig.DeviceKey,
		"architecture":  sysInfo.Arch + sysInfo.Variant,
		"os":            sysInfo.DetailedOS,
		"arch":          sysInfo.Arch,
		"variant":       sysInfo.Variant,
		"agent_version": release.GetVersion(),
	}

	_, err := agent.Messenger.Call(ctx, topics.UpdateDeviceArchitecture, []interface{}{payload}, nil, nil, nil)
	if err != nil {
		return err
	}

	return nil
}

// ensureAppCABundle refreshes the CA bundle app containers verify TLS against.
//
// It runs before any app starts so that the first container of the boot
// already sees a current bundle. Failure is never fatal: apps then run without
// the mount, exactly as they did before the bundle existed, and the reason is
// logged rather than blocking startup on a device whose network is not up yet.
func ensureAppCABundle(generalConfig *config.Config) {
	appsDir := generalConfig.CommandLineArguments.AppsDirectory

	if !trust.Enabled(generalConfig.ReswarmConfig.ApplianceDomain) {
		// A device detached from its appliance domain must not keep mounting
		// a trust set nobody refreshes.
		if err := trust.Remove(appsDir); err != nil {
			log.Debug().Err(err).Msg("failed to remove the stale app CA bundle")
		}
		return
	}

	path, err := trust.Build(appsDir, generalConfig.ReswarmConfig.DeviceEndpointURL)
	if err != nil {
		log.Error().Err(err).Msg("failed to build the app CA bundle; apps will fall back to their image's own trust store")
		return
	}
	log.Debug().Str("path", path).Msg("app CA bundle written")
}

func NewAgent(generalConfig *config.Config) (agent *Agent) {
	cliArgs := generalConfig.CommandLineArguments

	ensureAppCABundle(generalConfig)

	// A database file this start creates is empty: see
	// AppManager.MarkDatabaseUnsynced.
	_, statErr := os.Stat(cliArgs.DatabaseFileName)
	freshDatabase := errors.Is(statErr, os.ErrNotExist)

	database, err := persistence.NewSQLiteDb(generalConfig)
	if err != nil {
		log.Fatal().Stack().Err(err).Msg("failed to create SQLite db instance")
	}

	err = database.Init()
	if err != nil {
		log.Fatal().Stack().Err(err).Msg("failed to initalize SQLite database")
	}

	// setup the agent struct with a dummy/offline implementation of the messenger
	dummyMessenger := messenger.NewOffline(generalConfig)
	container, err := container.NewDocker(generalConfig)
	if err != nil {
		log.Fatal().Stack().Err(err).Msg("failed to setup docker")
	}

	filesystem := filesystem.New()
	tunnelManager, err := tunnel.NewFrpTunnelManager(dummyMessenger, generalConfig)
	if err != nil {
		log.Fatal().Stack().Err(err).Msg("failed to init tunnel manager")
	}

	appStore := store.NewAppStore(database, dummyMessenger)
	logManager := logging.NewLogManager(container, dummyMessenger, database, appStore)
	stateObserver := apps.NewObserver(container, &appStore, &logManager)
	stateMachine := apps.NewStateMachine(container, &logManager, &stateObserver, &filesystem)
	appManager := apps.NewAppManager(&stateMachine, &appStore, &stateObserver, tunnelManager)
	if freshDatabase {
		appManager.MarkDatabaseUnsynced()
	}
	terminalManager := terminal.NewTerminalManager(dummyMessenger, container)

	var networkInstance network.Network
	if runtime.GOOS == "linux" && cliArgs.UseNetworkManager {
		networkInstance, err = network.NewNMWNetwork()
		if err != nil {
			log.Fatal().Stack().Err(err).Msg("failed to setup network")
		}

		// Keep the device reachable: make NetworkManager retry activation indefinitely
		// instead of giving up after 4 tries, so a transient DHCP/router outage doesn't
		// leave a headless device permanently offline until it is rebooted.
		if err := networkInstance.SetInfiniteAutoconnectRetries(); err != nil {
			log.Error().Stack().Err(err).Msg("failed to set infinite autoconnect-retries")
		}
	} else {
		// TODO: write implementations for other environments. (issue: https://github.com/IronFlock/device-agent/issues/41)
		networkInstance = network.NewDummyNetwork()
	}

	// Disk-full guard: keep the device online and remotely reachable by capping
	// the unbounded log sinks now and, as the disk runs low, reclaiming space
	// safely and entering a disk-emergency state (see diskguard.IsEmergency) that
	// stops non-platform containers and is reported to the cloud, while the state
	// machine fails new RUNNING/BUILDING/DOWNLOADING transitions.
	var diskGuard *diskguard.Guard
	if runtime.GOOS == "linux" {
		diskguard.EnsurePreventionConfig()
		// Watch the daemon's actual data-root: on FlockOS it lives on the apps
		// partition (e.g. /apps/docker), not the /var/lib/docker default.
		var dataRoot string
		infoCtx, cancelInfo := context.WithTimeout(context.Background(), 10*time.Second)
		if root, err := container.DataRootDir(infoCtx); err != nil {
			log.Warn().Err(err).Msg("diskguard: could not resolve docker data-root, using default")
		} else {
			dataRoot = root
		}
		cancelInfo()
		diskGuard = diskguard.New(container, diskguard.Config{
			DataRoot:       dataRoot,
			AppsComposeDir: cliArgs.AppsComposeDir,
			AppsBuildDir:   cliArgs.AppsBuildDir,
			// Volumes of every locally known app (stopped included) are
			// protected from the volume pruning; an error keeps the guard away
			// from compose volumes entirely.
			InstalledAppNames: func() ([]string, error) {
				payloads, err := appStore.GetRequestedStates()
				if err != nil {
					return nil, err
				}
				names := make([]string, 0, len(payloads))
				for _, p := range payloads {
					names = append(names, p.AppName)
				}
				return names, nil
			},
			// Superseded app images are reclaimed only when this registry is
			// local to the device (an appliance's on-box registry), where a
			// re-pull is a loopback copy. On cloud devices this is a WAN host
			// and the guard leaves tagged app images alone.
			AppImageRegistry: generalConfig.ReswarmConfig.DockerRegistryURL,
			// Every image reference the device still needs: present AND newest
			// version per stage, so a reclaim that races an in-flight update
			// cannot delete the version being pulled or the one still running.
			WantedAppImages: func() (map[string]bool, error) {
				payloads, err := appStore.GetRequestedStates()
				if err != nil {
					return nil, err
				}
				wanted := make(map[string]bool, len(payloads)*4)
				for _, p := range payloads {
					for _, repo := range []string{p.RegistryImageName.Prod, p.RegistryImageName.Dev} {
						if repo == "" {
							continue
						}
						for _, version := range []string{p.PresentVersion, p.NewestVersion, p.Version} {
							if version != "" {
								wanted[repo+":"+version] = true
							}
						}
					}
				}
				return wanted, nil
			},
			// On recovery, reinstate the apps' requested states (which were
			// stopped/blocked during the emergency), and reconcile every app's
			// tunnels: the agent's own tunnel re-syncs stood down meanwhile
			// (see AppManager.retryPortSync).
			OnRecover: func() {
				if err := appManager.EnsureLocalRequestedStates(); err != nil {
					log.Error().Stack().Err(err).Msg("diskguard recovery: failed to reinstate app states")
				}
				appManager.SyncAllPortStates()
			},
		})
		// Synchronously evaluate disk BEFORE EnsureLocalRequestedStates below, so
		// if the device boots disk-critical the emergency flag (and the app-start
		// gate) is active before any container is started — and any containers
		// Docker auto-restarted are stopped — instead of racing the Run loop.
		diskGuard.CheckNow()
		safe.Go(func() { diskGuard.Run(context.Background()) })
	}

	// Reconcile local app state against the Docker daemon. fatal=true keeps
	// the historical behavior (die and let the supervisor restart us); the
	// late-daemon path must not kill an otherwise healthy agent, so it logs
	// errors instead.
	initLocalAppStates := func(fatal bool) {
		fail := func(err error, msg string) {
			if fatal {
				log.Fatal().Stack().Err(err).Msg(msg)
			} else {
				log.Error().Stack().Err(err).Msg(msg)
			}
		}

		// After the agent folder moved (the Windows layout migration), the
		// app containers are recreated from the new folder once, before any
		// of them starts (apps/recreate_marker.go).
		if err := appManager.RecreateContainersAfterMove(cliArgs.AgentDir); err != nil {
			log.Error().Stack().Err(err).Msg("could not recreate the app containers after the folder move; trying again at the next start")
		}
		// What the migration still owes Docker (restart policies) can be
		// settled now that it is reachable.
		afterDockerReady()

		err := stateObserver.CorrectAppStates(false)
		if err != nil {
			fail(err, "failed to correct local states")
		}

		err = stateObserver.ObserveAppStates()
		if err != nil {
			fail(err, "failed to init app state observers")
		}

		// Clean up orphaned containers not in database (offline check)
		err = appManager.CleanupOrphanedContainers()
		if err != nil {
			log.Error().Stack().Err(err).Msg("failed to cleanup orphaned containers")
		}

		// setup the containers on start
		err = appManager.EnsureLocalRequestedStates()
		if err != nil {
			fail(err, "failed to ensure local app states")
		}
	}

	daemonReady := make(chan struct{})
	err = container.WaitForDaemon(time.Second * 5)
	if err == nil {
		// The fast path (always taken on Linux, where Docker is up before the
		// agent): behavior identical to before this gate existed.
		initLocalAppStates(true)
		close(daemonReady)
	} else {
		// Docker Desktop starts at user login, possibly long after this
		// service started at boot. Keep the agent alive so WAMP endpoints
		// register for remote debugging; container features come up when the
		// daemon does.
		generalConfig.StartupLogChannel <- "Docker daemon is not reachable yet, continuing startup; container features become available once Docker is up"
		safe.Go(func() {
			for {
				err := container.WaitForDaemon(time.Second * 30)
				if err == nil {
					break
				}
				log.Info().Msg("still waiting for the Docker daemon...")
			}

			// compose support was probed (and latched negative) while the
			// daemon was down
			container.Compose().RefreshSupport()
			initLocalAppStates(false)
			close(daemonReady)
		})
	}

	// try to establish the main session
	mainSocketConfig := messenger.SocketConfig{
		SetupTestament:    true,
		ResponseTimeout:   time.Millisecond * time.Duration(cliArgs.ResponseTimeout),
		PingPongTimeout:   time.Millisecond * time.Duration(cliArgs.PingPongTimeout),
		ConnectionTimeout: time.Millisecond * time.Duration(cliArgs.ConnectionEstablishTimeout),
	}

	benchmark.TimeTillPreConnectInit = time.Since(benchmark.PreConnectInit)

	generalConfig.StartupLogChannel <- "Attempting to establish a socket connection..."

	benchmark.OnConnectInitAfterConnection = time.Now()
	benchmark.SocketConnectionInit = time.Now()

	mainSession, err := messenger.NewWampSession(generalConfig, &mainSocketConfig, container, nil)
	if err != nil {
		log.Fatal().Stack().Err(err).Msg("failed to setup wamp connection")
	}
	lifecycle.SetConnectivity(mainSession.Connected)

	benchmark.TimeTillSocketConnection = time.Since(benchmark.SocketConnectionInit)
	benchmark.TimeTillSocketConnectionFromLaunch = time.Since(benchmark.SocketConnectionInitFromLaunch)

	generalConfig.StartupLogChannel <- "Connected!"

	systemAPI := system.New(generalConfig, mainSession)

	// established a connection, replace the dummy messenger
	appStore.SetMessenger(mainSession)
	terminalManager.SetMessenger(mainSession)
	logManager.SetMessenger(mainSession)
	tunnelManager.SetMessenger(mainSession)
	// The appliance's appstore registry keeps its blobs on this same disk, and
	// removed apps' blobs otherwise wait on the registry's debounced sweep.
	// Give the diskguard a direct lever: an immediate registry garbage
	// collection over the local router. Appliance HOST only — on a cloud
	// device (or a device merely connected to an appliance) the same URI would
	// reach a registry on someone else's disk. The docker/dev appliance
	// variant carries no appliance_domain, hence the board-model fallback.
	if diskGuard != nil &&
		(generalConfig.ReswarmConfig.ApplianceDomain != "" ||
			generalConfig.ReswarmConfig.Board.Model == "appliance") {
		diskGuard.SetRegistryGC(func(ctx context.Context) {
			result, err := mainSession.Call(ctx, topics.RunRegistryGC, nil, nil, nil, nil)
			if err != nil {
				log.Warn().Err(err).Msg("diskguard: registry GC call failed")
				return
			}
			if len(result.Arguments) > 0 {
				if res, ok := result.Arguments[0].(map[string]interface{}); ok {
					log.Info().Fields(res).Msg("diskguard: registry GC completed")
					return
				}
			}
			log.Info().Msg("diskguard: registry GC completed")
		})
	}
	// Let the tunnel manager re-fetch frpc if it is found missing at runtime
	// (e.g. antivirus quarantined it) instead of crash-looping on a gone file.
	tunnelManager.SetReacquireFrpc(systemAPI.DownloadFrpIfNotExists)
	// Reconcile every app's tunnels whenever the device becomes able to
	// tunnel: tunnels skipped while it could not, and adds that failed while
	// frpc was starting, come up the moment it can tunnel again. Nothing else
	// would ask for them.
	tunnelManager.SetOnAvailable(appManager.SyncAllPortStates)
	// Report per-device tunnel capability on the heartbeat, so the UI reflects
	// it live without a dedicated get_agent_metadata call.
	mainSession.SetTunnelCapableFunc(tunnelManager.TunnelCapable)
	// Report live Docker daemon health on the heartbeat the same way, so the
	// UI can badge a device whose Docker is down. Probed at send time with a
	// short timeout: a hung daemon reads as unavailable, not only a stopped
	// one.
	mainSession.SetDockerAvailableFunc(func() bool {
		pingCtx, cancelPing := context.WithTimeout(context.Background(), time.Second*3)
		defer cancelPing()
		_, pingErr := container.Ping(pingCtx)
		return pingErr == nil
	})
	// On daemon-availability transitions (event stream lost / daemon back) the
	// recovery supervisor pokes an immediate status push so the badge flips
	// right away instead of on the next heartbeat.
	stateObserver.SetDaemonTransitionFunc(func() {
		safe.Go(func() {
			if err := mainSession.UpdateRemoteDeviceStatus(messenger.CONNECTED); err != nil {
				log.Debug().Err(err).Msg("failed to push device status on a docker daemon transition")
			}
		})
	})
	privilege := privilege.NewPrivilege(mainSession, generalConfig)

	external := api.External{
		Container:       container,
		Messenger:       mainSession,
		LogMessenger:    mainSession,
		Database:        database,
		Network:         networkInstance,
		Privilege:       &privilege,
		Filesystem:      &filesystem,
		TunnelManager:   tunnelManager,
		System:          &systemAPI,
		AppManager:      appManager,
		TerminalManager: &terminalManager,
		LogManager:      &logManager,
		Config:          generalConfig,
	}

	agent = &Agent{
		Config:          generalConfig,
		System:          &systemAPI,
		External:        &external,
		LogManager:      &logManager,
		Network:         networkInstance,
		TerminalManager: &terminalManager,
		TunnelManager:   tunnelManager,
		AppManager:      appManager,
		StateObserver:   &stateObserver,
		StateMachine:    &stateMachine,
		Filesystem:      &filesystem,
		Container:       container,
		Messenger:       mainSession,
		LogMessenger:    mainSession,
		Database:        database,
		daemonReady:     daemonReady,
	}

	// Set up the onConnect callback - messenger will call this after each reconnection
	mainSession.SetOnConnect(func(reconnect bool) {
		if reconnect {
			err := agent.OnConnect(true)
			if err != nil {
				// Not fatal: the error is the closing CONNECTED status update,
				// which fails when the connection dropped again mid-handler (a
				// rolling router restart). The next reconnect runs this
				// handler again and the heartbeat reports CONNECTED every
				// interval; exiting only turned a router restart into an agent
				// restart, which reagent-manager reverts to the previous
				// binary when it lands within a minute of an agent update.
				log.Error().Stack().Err(err).Msg("failed to run on connect handler after reconnection")
			}
		}
	})

	return agent
}
