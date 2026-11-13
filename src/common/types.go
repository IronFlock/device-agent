package common

import (
	"errors"
	"reagent/config"
	"sync"

	"github.com/rs/zerolog/log"
	"golang.org/x/sync/semaphore"
)

type Dict map[string]interface{}
type Timestamp string

// Result is the value received from a Messenger request (Call, Register callback, Subscription callback)
type Result struct {
	Arguments    []interface{}
	ArgumentsKw  Dict
	Details      Dict
	Registration uint64 // Values will be filled based on Result type
	Request      uint64
	Subscription uint64
	Publication  uint64
}

// InvokeResult is the value sent whenever a procedure is invoked
type InvokeResult struct {
	Arguments   []interface{}
	ArgumentsKw Dict
	Err         string
}

type App struct {
	AppKey              uint64
	DeviceToAppKey      uint64
	RequestorAccountKey uint64
	ReleaseKey          uint64
	AppName             string
	RequestedState      AppState
	CurrentState        AppState
	UpdateStatus        UpdateStatus
	Stage               Stage
	RequestUpdate       bool
	ReleaseBuild        bool
	Version             string
	LastUpdated         Timestamp
	TransitionLock      *semaphore.Weighted
	StateLock           sync.Mutex
}

func (app *App) SecureTransition() bool {
	if app.TransitionLock == nil {
		log.Error().Err(errors.New("no semaphore initialized"))
		return false
	}
	return !app.TransitionLock.TryAcquire(1)
}

func (app *App) UnlockTransition() {
	app.TransitionLock.Release(1)
}

func (app *App) IsCancelable() bool {
	app.StateLock.Lock()
	currAppState := app.CurrentState
	app.StateLock.Unlock()

	return IsCancelableState(currAppState)
}

type StageBasedResult struct {
	Dev  string
	Prod string
}

type DockerCredential struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type PortForwardRule struct {
	Main                  bool   `json:"main"`
	RuleName              string `json:"name"`
	Active                bool   `json:"active"`
	Public                bool   `json:"public"`
	Port                  uint64 `json:"port"`
	Protocol              string `json:"protocol"`
	LocalIP               string `json:"local_ip"`
	RemotePortEnvironment string `json:"remote_port_environment"`
	RemotePort            uint64 `json:"remote_port"`
	// HostPort is the host port the agent published the declared Port on.
	// Assigned by the agent, persisted upstream in t_device_to_app.ports and
	// shown in the UI; it comes back on sync so a restarted agent can reuse
	// the same port even after the container was removed.
	HostPort uint64 `json:"host_port,omitempty"`
	// CloudRemotePort is the internet-facing port of a tcp/udp tunnel of an
	// instance device: the cloud-side port the instance's tunnel forwarder
	// allocated (distinct from RemotePort, which is instance-local there).
	// Patched fresh into every sync by the instance backend — a value that
	// round-trips through t_device_to_app.ports is just a stale cached hint.
	// Injected into app containers as {RemotePortEnvironment}_CLOUD.
	CloudRemotePort uint64 `json:"cloud_remote_port,omitempty"`
	// ReservedHostPort / ReservedRemotePort are user-owned desired state: the
	// device host port / tunnel remote port the user pinned in the UI. The
	// agent carries them and honors them exactly — or fails loudly — but never
	// modifies them (the backend SQL discards agent-written reserved_* on the
	// device write-back path anyway).
	ReservedHostPort   uint64 `json:"reserved_host_port,omitempty"`
	ReservedRemotePort uint64 `json:"reserved_remote_port,omitempty"`
	// ReservationError is the agent's async failure surface for a reservation
	// it could not honor; persisted upstream and shown in the UI, cleared on
	// the next successful reconcile.
	ReservationError string `json:"reservation_error,omitempty"`
}

// TransitionPayload provides the data used by the StateMachine to transition between states.
type TransitionPayload struct {
	RequestedState        AppState
	CurrentState          AppState
	Stage                 Stage
	RequestorAccountKey   uint64
	DeviceOwnerAccountKey uint64
	DeviceToAppKey        uint64
	AppKey                uint64
	// AppCredEpoch is the generation of this app's per-app WAMP credential
	// (cross-app data access). The agent derives APP_AUTH_SECRET for exactly
	// this epoch and the backend verifies against it; bumping it server-side
	// rotates the credential. 0 == absent == epoch 1, so an un-migrated
	// backend needs no special handling.
	AppCredEpoch         uint64
	CallerAuthID         int
	AppName              string
	ImageName            StageBasedResult
	PresentImageName     string
	RegistryImageName    StageBasedResult
	ContainerName        StageBasedResult
	DockerCredentials    map[string]DockerCredential
	EnvironmentVariables map[string]any
	EnvironmentTemplate  map[string]any
	DockerCompose        map[string]any
	NewDockerCompose     map[string]any
	// Privileged / NewPrivileged say whether the project granted privileged
	// access (host devices, capabilities, host binds) to the release behind
	// DockerCompose / NewDockerCompose (and PresentVersion / NewestVersion).
	// nil: the backend predates the grant, the app runs as it always did.
	Privileged           *bool
	NewPrivileged        *bool
	Ports                []any
	PublishContainerName string
	RegisteryToken       string
	NewestVersion        string
	PresentVersion       string
	ReleaseKey           uint64
	NewReleaseKey        uint64
	Version              string
	RequestUpdate        bool
	Retrying             bool
	CancelTransition     bool
}

func BuildTransitionPayload(appKey uint64, appName string, requestorAccountKey uint64,
	stage Stage, currentState AppState, requestedState AppState, releaseKey uint64, newReleaseKey uint64,
	config *config.Config,
) TransitionPayload {

	payload := TransitionPayload{
		Stage:               stage,
		RequestedState:      requestedState,
		AppName:             appName,
		AppKey:              appKey,
		CurrentState:        currentState,
		ReleaseKey:          releaseKey,
		NewReleaseKey:       newReleaseKey,
		RequestorAccountKey: requestorAccountKey,
	}

	payload.initContainerData(appKey, appName, config)

	return payload
}

type DeviceSyncResponse struct {
	DeviceKey              int                    `json:"device_key"`
	SwarmKey               int                    `json:"swarm_key"`
	AccountKey             int                    `json:"account_key"`
	DeviceOwnerAccountKey  int                    `json:"device_owner_account_key"`
	SerialNumber           string                 `json:"serial_number"`
	AppName                string                 `json:"app_name"`
	Name                   string                 `json:"name"` // device name
	Status                 string                 `json:"status"`
	Architecture           string                 `json:"architecture"`
	Address                string                 `json:"address"`
	ReleaseKey             int                    `json:"release_key"`
	NewReleaseKey          int                    `json:"new_release_key"`
	DeviceToAppKey         int                    `json:"device_to_app_key"`
	Environment            map[string]interface{} `json:"environment"`
	EnvironmentTemplate    map[string]interface{} `json:"environment_template"`
	DockerCompose          map[string]interface{} `json:"docker_compose"`
	NewDockerCompose       map[string]interface{} `json:"new_docker_compose"`
	Privileged             *bool                  `json:"privileged"`
	NewPrivileged          *bool                  `json:"new_privileged"`
	Ports                  []interface{}          `json:"ports"`
	CurrentState           string                 `json:"current_state"`
	Stage                  string                 `json:"stage"`
	ContainerName          string                 `json:"container_name"`
	RequestUpdate          bool                   `json:"request_update"`
	TargetState            string                 `json:"target_state"`
	ManuallyRequestedState string                 `json:"manually_requested_state"`
	InheritFromGroup       interface{}            `json:"inherit_from_group"`
	RequestorAccountKey    int                    `json:"requestor_account_key"`
	PresentVersion         string                 `json:"present_version"`
	PresentImageName       string                 `json:"present_image_name"`
	NewImageName           string                 `json:"new_image_name"`
	NewestVersion          string                 `json:"newest_version"`
	Description            string                 `json:"description"`
	AppKey                 uint64                 `json:"app_key"`
	// Generation of this app's per-app WAMP credential; absent (0) = epoch 1.
	AppCredEpoch uint64 `json:"app_cred_epoch"`
	// Third-party registry credentials for the app's external images, keyed by
	// registry host. The backend only emits them for DEV-stage rows (the
	// developer's own devices) — PROD releases are re-hosted into the store
	// registry at publish time and never need them.
	DockerCredentials map[string]DockerCredential `json:"docker_credentials"`
}
