package apps

import (
	"context"
	"fmt"
	"reagent/common"
	"reagent/container"
	"reagent/diskguard"
	"reagent/errdefs"
	"reagent/filesystem"
	"reagent/logging"
	"reagent/safe"
	"sync"

	"github.com/rs/zerolog/log"
)

type TransitionFunc func(TransitionPayload common.TransitionPayload, app *common.App) error

type StateMachine struct {
	StateObserver *StateObserver
	Filesystem    *filesystem.Filesystem
	Container     container.Container
	LogManager    *logging.LogManager
	appStates     []*common.App

	// composeTransitionCancels holds the cancel func of each in-flight cancelable
	// compose transition (an update, or the pull phase of an install), keyed by
	// app. A canceling request (cancelUpdate on a PRESENT request while UPDATING,
	// cancelPull on a teardown request while DOWNLOADING) calls it to kill the
	// `docker compose` CLI so a hung pull/down unwinds and releases the
	// transition lock, instead of blocking on cmd.Wait() forever.
	composeTransitionCancels     map[string]context.CancelFunc
	composeTransitionCancelMutex sync.Mutex

	// Per-device HMAC key for deriving each app's APP_AUTH_SECRET (cross-app
	// data access). Fetched from the backend on connect and kept in memory
	// ONLY — writing it into a container would let that app mint its
	// neighbours' credentials. Empty until the first successful fetch, which
	// makes appCredential() return empty strings; callers then leave existing
	// credential files untouched rather than writing blanks, so a backend
	// outage cannot brick running apps.
	appCredKey      string
	appCredKeyMutex sync.RWMutex

	// recreateMu serialises the writers of the recreate marker
	// (recreate_marker.go): the apps start in parallel.
	recreateMu sync.Mutex
}

// SetAppCredKey stores the per-device app-credential key. Safe to call on
// every reconnect; a fetch failure must pass "" and is ignored so a transient
// backend outage does not clear a working key.
func (sm *StateMachine) SetAppCredKey(key string) {
	if key == "" {
		return
	}
	sm.appCredKeyMutex.Lock()
	defer sm.appCredKeyMutex.Unlock()
	sm.appCredKey = key
}

func (sm *StateMachine) AppCredKey() string {
	sm.appCredKeyMutex.RLock()
	defer sm.appCredKeyMutex.RUnlock()
	return sm.appCredKey
}

func NewStateMachine(container container.Container, logManager *logging.LogManager, observer *StateObserver, filesystem *filesystem.Filesystem) StateMachine {
	appStates := make([]*common.App, 0)
	return StateMachine{
		StateObserver:            observer,
		Container:                container,
		LogManager:               logManager,
		Filesystem:               filesystem,
		appStates:                appStates,
		composeTransitionCancels: make(map[string]context.CancelFunc),
	}
}

func composeTransitionKey(stage common.Stage, appKey uint64) string {
	return fmt.Sprintf("%s_%d", stage, appKey)
}

// registerComposeTransitionCancel records the cancel func of an in-flight
// cancelable compose transition so cancelUpdate/cancelPull can reach it.
func (sm *StateMachine) registerComposeTransitionCancel(stage common.Stage, appKey uint64, cancel context.CancelFunc) {
	key := composeTransitionKey(stage, appKey)
	sm.composeTransitionCancelMutex.Lock()
	sm.composeTransitionCancels[key] = cancel
	sm.composeTransitionCancelMutex.Unlock()
}

// clearComposeTransitionCancel drops the cancel func once the transition has
// unwound.
func (sm *StateMachine) clearComposeTransitionCancel(stage common.Stage, appKey uint64) {
	key := composeTransitionKey(stage, appKey)
	sm.composeTransitionCancelMutex.Lock()
	delete(sm.composeTransitionCancels, key)
	sm.composeTransitionCancelMutex.Unlock()
}

// cancelComposeTransition cancels the context of an in-flight compose transition
// for the app, killing the docker compose CLI. Returns true if one was in flight.
func (sm *StateMachine) cancelComposeTransition(stage common.Stage, appKey uint64) bool {
	key := composeTransitionKey(stage, appKey)
	sm.composeTransitionCancelMutex.Lock()
	cancel := sm.composeTransitionCancels[key]
	sm.composeTransitionCancelMutex.Unlock()
	if cancel != nil {
		cancel()
		return true
	}
	return false
}

func (sm *StateMachine) noActionTransitionFunc(TransitionPayload common.TransitionPayload, app *common.App) error {
	return errdefs.NoActionTransition()
}

func (sm *StateMachine) getTransitionFunc(prevState common.AppState, nextState common.AppState) TransitionFunc {
	var stateTransitionMap = map[common.AppState]map[common.AppState]TransitionFunc{
		common.REMOVED: {
			common.PRESENT:     sm.removedToPresent,
			common.RUNNING:     sm.removedToRunning,
			common.BUILT:       sm.buildApp,
			common.PUBLISHED:   sm.publishApp,
			common.UNINSTALLED: sm.uninstallApp,
			common.REMOVED:     sm.noActionTransitionFunc,
		},
		common.UNINSTALLED: {
			common.PRESENT:     sm.pullApp,
			common.RUNNING:     sm.runApp,
			common.BUILT:       sm.buildApp,
			common.PUBLISHED:   sm.publishApp,
			common.UNINSTALLED: sm.noActionTransitionFunc,
		},
		// UNINSTALLED mirrors REMOVED for the in-flight rows: the cancel
		// handlers settle at the REQUESTED teardown (settleCanceledTransition
		// runs the full uninstall for UNINSTALLED). Without these rows an
		// UNINSTALLED request during a build/publish resolved to no transition
		// at all and was silently dropped.
		common.PUBLISHING: {
			common.REMOVED:     sm.cancelPush,
			common.UNINSTALLED: sm.cancelPush,
		},
		common.BUILDING: {
			common.REMOVED:     sm.cancelBuild,
			common.UNINSTALLED: sm.cancelBuild,
		},
		common.STOPPED: {
			common.REMOVED:     sm.removeApp,
			common.UNINSTALLED: sm.uninstallApp,
			common.RUNNING:     sm.runApp,
			common.BUILT:       sm.buildApp,
			common.PUBLISHED:   sm.publishApp,
			common.STOPPED:     sm.noActionTransitionFunc,
		},
		common.PRESENT: {
			common.REMOVED:     sm.removeApp,
			common.UNINSTALLED: sm.uninstallApp,
			common.RUNNING:     sm.runApp,
			common.BUILT:       sm.buildApp,
			common.PUBLISHED:   sm.publishApp,
			common.PRESENT:     sm.noActionTransitionFunc,
		},
		common.FAILED: {
			common.REMOVED:     sm.removeApp,
			common.UNINSTALLED: sm.uninstallApp,
			common.PRESENT:     sm.recoverFailToPresentHandler,
			common.RUNNING:     sm.recoverFailToRunningHandler,
			common.BUILT:       sm.buildApp,
			common.PUBLISHED:   sm.publishApp,
		},
		common.BUILT: {
			common.REMOVED:     sm.removeApp,
			common.UNINSTALLED: sm.uninstallApp,
			common.PRESENT:     sm.noActionTransitionFunc,
			common.RUNNING:     sm.runApp,
			common.BUILT:       sm.buildApp,
			common.PUBLISHED:   sm.publishApp,
		},
		common.TRANSFERED: {
			common.REMOVED:     sm.removeApp,
			common.UNINSTALLED: sm.uninstallApp,
			common.PRESENT:     sm.pullApp,
		},
		common.TRANSFERING: {
			common.REMOVED:     sm.cancelTransfer,
			common.UNINSTALLED: sm.cancelTransfer,
			common.PRESENT:     sm.cancelTransfer,
		},
		common.PUBLISHED: {
			common.REMOVED:     sm.removeApp,
			common.UNINSTALLED: sm.uninstallApp,
			common.RUNNING:     sm.runApp,
			common.PRESENT:     sm.noActionTransitionFunc,
			common.BUILT:       sm.buildApp,
			common.PUBLISHED:   sm.publishApp,
		},
		common.RUNNING: {
			common.RUNNING:     sm.noActionTransitionFunc,
			common.PRESENT:     sm.stopApp,
			common.BUILT:       sm.stopApp,
			common.PUBLISHED:   sm.removeAndPublishApp,
			common.REMOVED:     sm.removeApp,
			common.UNINSTALLED: sm.uninstallApp,
		},
		common.DOWNLOADING: {
			common.PRESENT:     sm.pullApp,
			common.REMOVED:     sm.cancelPull,
			common.UNINSTALLED: sm.cancelPull,
		},
		common.STARTING: {
			common.PRESENT:     sm.stopApp,
			common.REMOVED:     sm.removeApp,
			common.UNINSTALLED: sm.uninstallApp,
			common.RUNNING:     sm.runApp,
		},
		common.STOPPING: {
			common.PRESENT:     sm.stopApp,
			common.REMOVED:     sm.removeApp,
			common.UNINSTALLED: sm.uninstallApp,
			common.RUNNING:     sm.runApp,
		},
		common.UPDATING: {
			common.PRESENT:     sm.cancelUpdate,
			common.REMOVED:     sm.cancelUpdateAndRemove,
			common.UNINSTALLED: sm.cancelUpdateAndUninstall,
			common.RUNNING:     nil,
		},
		common.DELETING: {
			common.PRESENT:     nil,
			common.REMOVED:     sm.removeApp,
			common.UNINSTALLED: sm.uninstallApp,
			common.RUNNING:     nil,
		},
	}

	return stateTransitionMap[prevState][nextState]
}

func (sm *StateMachine) setState(app *common.App, state common.AppState) error {
	err := sm.StateObserver.Notify(app, state)
	if err != nil {
		return err
	}
	return nil
}

func (sm *StateMachine) executeTransition(app *common.App, payload common.TransitionPayload, transitionFunc TransitionFunc) chan error {
	errChannel := make(chan error, 1)

	safe.Go(func() {
		log.Info().Msgf("Executing transition from %s to %s for %s (%s)...", app.CurrentState, payload.RequestedState, app.AppName, app.Stage)
		err := transitionFunc(payload, app)

		// send potential error to errChannel
		// if error = nil, the transition has completed successfully
		errChannel <- err

		// we are done sending, should close the channel
		close(errChannel)
	})

	return errChannel
}

func (sm *StateMachine) CancelTransition(app *common.App, payload common.TransitionPayload) chan error {
	app.StateLock.Lock()
	curAppState := app.CurrentState
	app.StateLock.Unlock()

	transitionFunc := sm.getTransitionFunc(curAppState, payload.RequestedState)
	if transitionFunc == nil {
		log.Debug().Msgf("It appears the cancel transition function does not exist. In %s to %s for %s (%s)", curAppState, payload.RequestedState, payload.AppName, payload.Stage)
		return nil
	}

	return sm.executeTransition(app, payload, transitionFunc)
}

func (sm *StateMachine) InitTransition(app *common.App, payload common.TransitionPayload) chan error {
	// While the device is in a disk-emergency, refuse transitions that would
	// pull, build, or start an app and consume more disk. Fail fast: the caller
	// marks the app FAILED and reports it to the cloud.
	if diskguard.IsEmergency() && common.IsDiskGrowingState(payload.RequestedState) {
		log.Warn().Msgf("disk-emergency: refusing transition to %s for %s (%s)", payload.RequestedState, payload.AppName, payload.Stage)
		errChannel := make(chan error, 1)
		errChannel <- fmt.Errorf("disk emergency: device is critically low on storage; refusing to transition %s (%s) to %s", payload.AppName, payload.Stage, payload.RequestedState)
		close(errChannel)
		return errChannel
	}

	app.StateLock.Lock()
	curAppState := app.CurrentState
	app.StateLock.Unlock()

	var transitionFunc TransitionFunc
	if payload.RequestUpdate && payload.NewestVersion != app.Version && payload.RequestedState != common.UNINSTALLED {
		transitionFunc = sm.getUpdateTransition(payload, app)
	} else {
		transitionFunc = sm.getTransitionFunc(curAppState, payload.RequestedState)
	}

	if transitionFunc == nil {
		log.Debug().Msgf("Not yet implemented transition from %s to %s", curAppState, payload.RequestedState)
		return nil
	}

	return sm.executeTransition(app, payload, transitionFunc)
}

func (sm *StateMachine) HandleRegistryLoginsWithDefault(payload common.TransitionPayload) error {
	config := sm.Container.GetConfig()

	// Log in with canonical host keys (`docker login` stores auth under the
	// bare host anyway) into a fresh map, so the store entry can never
	// clobber a user credential inside the payload's own map.
	credentials := common.NormalizeDockerCredentialKeys(payload.DockerCredentials)
	if credentials == nil {
		credentials = make(map[string]common.DockerCredential, 1)
	}

	storeCredential := common.DockerCredential{
		Username: payload.RegisteryToken,
		Password: config.ReswarmConfig.Secret,
	}
	credentials[common.NormalizeRegistryHost(config.ReswarmConfig.DockerRegistryURL)] = storeCredential

	// An appliance in domain mode serves ONE registry under TWO names, and the
	// appliance's own colocated agent is configured with the loopback one
	// (localhost:15001) while the app-store sync stamps registry.<appliance_domain>
	// into every image ref it rewrites (see applianceRegistryHost). `docker
	// compose pull` looks credentials up by the image's own host, so with a login
	// under the loopback name only, a synced app's pull ran anonymously and the
	// daemon reported a bare "Authentication required" for every image
	// (tls-sf015 ironflock-instance, 2026-09-10). Log in under the alias as well —
	// but only when this app's compose actually references it: on an appliance
	// whose cert the host's docker does not trust (local-CA --tls) that login
	// would fail, and it must not take down transitions that never need it.
	if alias := applianceRegistryHost(config.ReswarmConfig); alias != "" {
		if _, exists := credentials[alias]; !exists &&
			(composeReferencesRegistryHost(payload.DockerCompose, alias) ||
				composeReferencesRegistryHost(payload.NewDockerCompose, alias)) {
			credentials[alias] = storeCredential
		}
	}

	return sm.Container.HandleRegistryLogins(credentials)
}
