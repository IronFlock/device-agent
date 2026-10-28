package store

import (
	"context"
	"encoding/json"
	"fmt"
	"reagent/common"
	"reagent/errdefs"
	"reagent/messenger"
	"reagent/messenger/topics"
	"reagent/persistence"
	"strings"

	"github.com/rs/zerolog/log"
	"golang.org/x/sync/semaphore"
)

type AppStore struct {
	database  persistence.Database
	Messenger messenger.Messenger
	apps      []*common.App
}

func NewAppStore(database persistence.Database, messenger messenger.Messenger) AppStore {
	return AppStore{
		database:  database,
		Messenger: messenger,
		apps:      make([]*common.App, 0),
	}
}

func (am *AppStore) SetMessenger(messenger messenger.Messenger) {
	am.Messenger = messenger
}

func (am *AppStore) GetAllApps() ([]*common.App, error) {
	return am.database.GetAppStates()
}

func (am *AppStore) GetRequestedStates() ([]common.TransitionPayload, error) {
	return am.database.GetRequestedStates()
}

func (am *AppStore) GetRequestedState(appKey uint64, stage common.Stage) (common.TransitionPayload, error) {
	return am.database.GetRequestedState(appKey, stage)
}

func (am *AppStore) GetApp(appKey uint64, stage common.Stage) (*common.App, error) {
	for i := range am.apps {
		state := am.apps[i]
		if state.AppKey == appKey && state.Stage == stage {
			return state, nil
		}
	}

	// app was not found in memory
	app, err := am.database.GetAppState(appKey, stage)
	if err != nil {
		return nil, err
	}

	// app was also not found in database
	if app == nil {
		return nil, nil
	}

	app.TransitionLock = semaphore.NewWeighted(1)
	am.apps = append(am.apps, app)

	return app, nil
}

// CheckAppContainer refuses a container name that does not belong to an app
// installed on this device.
//
// Procedures that act on a container — its logs, a shell in it — take the name
// from their caller, and Docker answers for any container on the host. On an
// appliance that host also runs the platform itself — its database, router,
// registry — which no privilege on the device may reach. Any other name is
// answered "not found", so the answer does not tell a caller which other
// containers exist either.
func (am *AppStore) CheckAppContainer(containerName string) error {
	notFound := errdefs.ContainerNotFound(fmt.Errorf("no app container %q on this device", containerName))

	stage, appKey, _, err := common.ParseContainerName(containerName)
	if err != nil || stage == "" {
		return notFound
	}

	app, err := am.GetApp(appKey, stage)
	if err != nil {
		return err
	}
	if app == nil {
		return notFound
	}

	if containerName == common.BuildContainerName(stage, appKey, app.AppName) {
		return nil
	}

	// A DEV app's release build logs under pub_, which parses to DEV.
	if stage == common.DEV && containerName == common.BuildContainerName("pub", appKey, app.AppName) {
		return nil
	}

	return notFound
}

func (am *AppStore) AddApp(payload common.TransitionPayload) (*common.App, error) {
	app := &common.App{
		AppKey:              payload.AppKey,
		AppName:             payload.AppName,
		CurrentState:        payload.CurrentState,
		DeviceToAppKey:      payload.DeviceToAppKey,
		ReleaseKey:          payload.ReleaseKey,
		RequestorAccountKey: payload.RequestorAccountKey,
		RequestedState:      payload.RequestedState,
		Stage:               payload.Stage,
		Version:             payload.PresentVersion,
		RequestUpdate:       payload.RequestUpdate,
		TransitionLock:      semaphore.NewWeighted(1),
	}

	if payload.CurrentState == "" {
		app.StateLock.Lock()
		app.CurrentState = common.REMOVED
		app.StateLock.Unlock()
	}

	am.apps = append(am.apps, app)

	// Insert the newly created app state data into the database
	app.StateLock.Lock()
	curAppState := app.CurrentState
	app.StateLock.Unlock()

	_, err := am.database.UpsertAppState(app, curAppState)
	if err != nil {
		log.Error().Stack().Err(err)
	}

	if err != nil {
		return nil, err
	}

	return app, nil
}

func (am *AppStore) UpdateLocalRequestedState(payload common.TransitionPayload) error {
	return am.database.UpsertRequestedStateChange(payload)
}

func (am *AppStore) GetRegistryToken(callerID uint64) (string, error) {
	ctx := context.Background()
	args := []interface{}{common.Dict{"callerID": callerID}}
	resp, err := am.Messenger.Call(ctx, topics.GetRegistryToken, args, nil, nil, nil)
	if err != nil {
		return "", err
	}

	if resp.Arguments == nil {
		return "", nil
	}

	registryTokenArg := resp.Arguments[0]
	registryToken, ok := registryTokenArg.(string)

	if !ok {
		return "", fmt.Errorf("invalid registry_token payload")
	}

	return registryToken, nil
}

// GetAppCredKey fetches this device's per-app-credential HMAC key from the
// backend, minting it there on first call. The RPC takes NO arguments — the
// backend resolves the device from the session's own authid — so a device can
// only ever obtain its own key.
//
// The key stays in agent memory and is never written into a container: it is
// the material from which every app's APP_AUTH_SECRET is derived, so a
// container that held it could mint its neighbours' credentials.
func (am *AppStore) GetAppCredKey() (string, error) {
	ctx := context.Background()
	resp, err := am.Messenger.Call(ctx, topics.GetAppCredKey, []interface{}{}, nil, nil, nil)
	if err != nil {
		return "", err
	}

	if len(resp.Arguments) == 0 {
		return "", fmt.Errorf("empty app_cred_key payload")
	}

	key, ok := resp.Arguments[0].(string)
	if !ok || key == "" {
		return "", fmt.Errorf("invalid app_cred_key payload")
	}

	return key, nil
}

// UpdateLocalAppState updates the local app database
func (am *AppStore) UpdateLocalAppState(app *common.App, stateToSet common.AppState) error {
	timestamp, err := am.database.UpsertAppState(app, stateToSet)
	if err != nil {
		log.Error().Err(err).Msg("Failed to upsert app state")
	}

	app.StateLock.Lock()
	app.LastUpdated = timestamp
	app.StateLock.Unlock()

	if err != nil {
		return err
	}

	return nil
}

// UpdateRemoteAppState update sthe remote app database
func (am *AppStore) UpdateRemoteAppState(ctx context.Context, app *common.App, stateToSet common.AppState) error {
	config := am.Messenger.GetConfig()

	payload := []interface{}{common.Dict{
		"app_key":               app.AppKey,
		"device_key":            config.ReswarmConfig.DeviceKey,
		"swarm_key":             config.ReswarmConfig.SwarmKey,
		"serial_number":         config.ReswarmConfig.SerialNumber,
		"stage":                 app.Stage,
		"state":                 stateToSet,
		"device_to_app_key":     app.DeviceToAppKey,
		"requestor_account_key": app.RequestorAccountKey,
		"release_key":           app.ReleaseKey,
		"request_update":        app.RequestUpdate,
		"release_build":         app.ReleaseBuild,
		"updateStatus":          app.UpdateStatus,
	}}

	_, err := am.Messenger.Call(ctx, topics.SetActualAppOnDeviceState, payload, nil, nil, nil)
	if err != nil {
		return err
	}

	app.StateLock.Lock()
	// we successfully let the backend know we updated, we can now set this to false
	if app.UpdateStatus == common.PENDING_REMOTE_CONFIRMATION {
		app.UpdateStatus = common.COMPLETED
	}

	app.StateLock.Unlock()

	return nil
}

func (am *AppStore) UpdateRequestedStatesWithRemote() error {
	appStateChanges, err := am.FetchRequestedAppStates()
	if err != nil {
		return err
	}

	err = am.database.BulkUpsertRequestedStateChanges(appStateChanges)
	if err != nil {
		return err
	}

	return nil
}

func (am *AppStore) FetchRequestedAppStates() ([]common.TransitionPayload, error) {
	ctx := context.Background()
	config := am.Messenger.GetConfig()
	args := []interface{}{common.Dict{"device_key": config.ReswarmConfig.DeviceKey}}

	log.Debug().Msgf("Fetching requested states for device with key: %d", config.ReswarmConfig.DeviceKey)

	result, err := am.Messenger.Call(ctx, topics.GetRequestedAppStates, args, nil, nil, nil)
	if err != nil {
		return []common.TransitionPayload{}, err
	}
	byteArr, err := json.Marshal(result.Arguments[0])
	if err != nil {
		return []common.TransitionPayload{}, err
	}

	deviceSyncStateResponse := make([]common.DeviceSyncResponse, 0)
	_ = json.Unmarshal(byteArr, &deviceSyncStateResponse)

	appPayloads := make([]common.TransitionPayload, 0)
	for _, deviceSyncState := range deviceSyncStateResponse {

		payload := common.BuildTransitionPayload(deviceSyncState.AppKey, deviceSyncState.AppName,
			uint64(deviceSyncState.RequestorAccountKey),
			common.Stage(deviceSyncState.Stage),
			common.AppState(deviceSyncState.CurrentState),
			common.AppState(deviceSyncState.TargetState),
			uint64(deviceSyncState.ReleaseKey),
			uint64(deviceSyncState.NewReleaseKey),
			config,
		)

		payload.DeviceOwnerAccountKey = uint64(deviceSyncState.DeviceOwnerAccountKey)
		payload.RequestUpdate = deviceSyncState.RequestUpdate
		payload.PresentVersion = deviceSyncState.PresentVersion
		payload.DockerCompose = deviceSyncState.DockerCompose
		payload.NewDockerCompose = deviceSyncState.NewDockerCompose
		payload.Version = deviceSyncState.PresentVersion
		payload.NewestVersion = deviceSyncState.NewestVersion
		payload.EnvironmentVariables = deviceSyncState.Environment
		payload.EnvironmentTemplate = deviceSyncState.EnvironmentTemplate
		payload.Ports = deviceSyncState.Ports
		payload.AppCredEpoch = deviceSyncState.AppCredEpoch
		payload.DockerCredentials = common.NormalizeDockerCredentialKeys(deviceSyncState.DockerCredentials)

		appPayloads = append(appPayloads, payload)
	}

	return appPayloads, nil
}

func (am *AppStore) DeleteAppState(appKey uint64, stage common.Stage) error {
	return am.database.DeleteAppState(appKey, stage)
}

func (am *AppStore) DeleteRequestedState(appKey uint64, stage common.Stage) error {
	return am.database.DeleteRequestedState(appKey, stage)
}

// GetAppByContainerName finds an app by its container name.
// Container names follow the format: {stage}_{appKey}_{appName}
// This is primarily used for DEV apps during file transfers.
func (am *AppStore) GetAppByContainerName(containerName string) *common.App {
	for _, app := range am.apps {
		// Build expected container name for this app
		expectedContainerName := fmt.Sprintf("%s_%d_%s", string(app.Stage), app.AppKey, app.AppName)
		if strings.EqualFold(expectedContainerName, containerName) {
			return app
		}
	}
	return nil
}
