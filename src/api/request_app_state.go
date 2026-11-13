package api

import (
	"context"
	"fmt"
	"reagent/common"
	"reagent/config"
	"reagent/errdefs"
	"reagent/messenger"
	"reagent/safe"
	"strconv"

	"github.com/rs/zerolog/log"
)

func (ex *External) requestAppStateHandler(ctx context.Context, response messenger.Result) (*messenger.InvokeResult, error) {
	payload, err := responseToTransitionPayload(ex.Config, response)
	if err != nil {
		return nil, err
	}

	// Before the transition, which builds the app's environment from it.
	ex.learnInstanceKey(response.ArgumentsKw["instance_key"])

	err = ex.AppManager.CreateOrUpdateApp(payload)
	if err != nil {
		return nil, err
	}

	safe.Go(func() {
		err = ex.AppManager.RequestAppState(payload)
		if err != nil {
			log.Error().Err(err).Msgf("failed to request app state")
			return
		}
	})

	return &messenger.InvokeResult{}, nil
}

// learnInstanceKey takes the instance key of an app-state push into the
// .flock. An appliance's backend names the key in every push and a cloud
// backend in none, so the .flock follows the last push: the apps' INSTANCE_KEY
// comes from there (config.Platform), also for an app the agent starts from
// its stored state before it is connected. A value the agent cannot read
// changes nothing and must not block the app sync. A failed save costs only
// the persistence.
func (ex *External) learnInstanceKey(value interface{}) {
	instanceKey, ok := instanceKeyOf(value)
	if !ok {
		log.Warn().Msgf("app-state push with an instance_key that cannot be read (%T); keeping the one in the .flock", value)
		return
	}

	changed := ex.Config.Learn(func(cfg *config.ReswarmConfig) bool {
		if cfg.InstanceKey == instanceKey {
			return false
		}
		cfg.InstanceKey = instanceKey
		return true
	})
	if !changed {
		return
	}

	if err := ex.Config.Persist(); err != nil {
		log.Warn().Err(err).Msg("failed to save the instance key to the .flock")
	}
}

// instanceKeyOf reads instance_key as an app-state push carries it. Absent is
// 0: the device belongs to no instance. ok is false for a value that cannot be
// read.
func instanceKeyOf(value interface{}) (instanceKey uint64, ok bool) {
	if value == nil {
		return 0, true
	}
	if text, isString := value.(string); isString {
		parsed, err := strconv.ParseUint(text, 10, 64)
		return parsed, err == nil
	}
	return common.ToUint64(value)
}

// responseToTransitionPayload parses a Messenger response to a generic common.TransitionPayload struct.
// Values that were not provided will be nil.
func responseToTransitionPayload(config *config.Config, result messenger.Result) (common.TransitionPayload, error) {
	kwargs := result.ArgumentsKw
	// details := result.Details

	appKeyKw := kwargs["app_key"]
	appNameKw := kwargs["app_name"]
	stageKw := kwargs["stage"]
	requestedStateKw := kwargs["target_state"]
	manuallyRequestedStateKw := kwargs["manually_requested_state"]
	releaseKeyKw := kwargs["release_key"]
	newReleaseKeyKw := kwargs["new_release_key"]
	currentStateKw := kwargs["current_state"]
	currentStateKw2 := kwargs["state"]
	requestorAccountKeyKw := kwargs["requestor_account_key"]
	requestorAccountKeyKw2 := kwargs["account_id"]
	deviceOwnerAccountKeyKw := kwargs["device_owner_account_key"]
	environmentKw := kwargs["environment"]
	portsKw := kwargs["ports"]
	// dtaKeyKw := kwargs["device_to_app_key"]
	versionKw := kwargs["version"]
	presentVersionKw := kwargs["present_version"]
	newestVersionKw := kwargs["newest_version"]
	requestUpdateKw := kwargs["request_update"]
	cancelTransitionKw := kwargs["cancel_transition"]
	environmentTemplateKw := kwargs["environment_template"]
	dockerComposeKw := kwargs["docker_compose"]
	newDockerComposeKw := kwargs["new_docker_compose"]
	privilegedKw := kwargs["privileged"]
	newPrivilegedKw := kwargs["new_privileged"]
	dockerCredentialsKw := kwargs["docker_credentials"]
	appCredEpochKw := kwargs["app_cred_epoch"]

	var appKey uint64
	var releaseKey uint64
	var newReleaseKey uint64
	var requestorAccountKey uint64
	var requestorAccountKey2 uint64
	var deviceOwnerAccountKey uint64
	var appCredEpoch uint64
	var appName string
	var stage string
	var requestedState string
	var manuallyRequestedState string
	var currentState string
	var currentState2 string
	var version string
	var presentVersion string
	var newestVersion string
	var requestUpdate bool
	var cancelTransition bool
	var ok bool
	var ports []interface{}
	var environmentTemplate map[string]interface{}
	var environment map[string]interface{}
	var dockerCompose map[string]interface{}
	var newDockerCompose map[string]interface{}
	var privileged *bool
	var newPrivileged *bool
	var dockerCredentials map[string]common.DockerCredential

	// TODO: can be simplified with parser function, but unneccessary
	if appKeyKw != nil {
		appKey, ok = appKeyKw.(uint64)
		if !ok {
			return common.TransitionPayload{}, fmt.Errorf("%w app_key", errdefs.ErrFailedToParse)
		}
		if appKey == 0 {
			return common.TransitionPayload{}, fmt.Errorf("%w app_key", errdefs.ErrMissingFromPayload)
		}
	}

	if appNameKw != nil {
		appName, ok = appNameKw.(string)
		if !ok {
			return common.TransitionPayload{}, fmt.Errorf("%w appName", errdefs.ErrFailedToParse)
		}
		if appName == "" {
			return common.TransitionPayload{}, fmt.Errorf("%w appName", errdefs.ErrFailedToParse)
		}
	}

	if stageKw != nil {
		stage, ok = stageKw.(string)
		if !ok {
			return common.TransitionPayload{}, fmt.Errorf("%w stage", errdefs.ErrFailedToParse)
		}
		if stage == "" {
			return common.TransitionPayload{}, fmt.Errorf("%w stage", errdefs.ErrMissingFromPayload)
		}
	}

	if requestedStateKw != nil {
		requestedState, ok = requestedStateKw.(string)
		if !ok {
			return common.TransitionPayload{}, fmt.Errorf("%w requestedState", errdefs.ErrFailedToParse)
		}
	}

	if manuallyRequestedStateKw != nil {
		manuallyRequestedState, ok = manuallyRequestedStateKw.(string)
		if !ok {
			return common.TransitionPayload{}, fmt.Errorf("%w manuallyRequestedState", errdefs.ErrFailedToParse)
		}
	}

	if currentStateKw != nil {
		currentState, ok = currentStateKw.(string)
		if !ok {
			return common.TransitionPayload{}, fmt.Errorf("%w currentState", errdefs.ErrFailedToParse)
		}
	}

	if currentStateKw2 != nil {
		currentState2, ok = currentStateKw2.(string)
		if !ok {
			return common.TransitionPayload{}, fmt.Errorf("%w currentState2", errdefs.ErrFailedToParse)
		}
	}

	if requestorAccountKeyKw != nil {
		requestorAccountKey, ok = requestorAccountKeyKw.(uint64)
		if !ok {
			requestorAccountKeyString, ok := requestorAccountKeyKw.(string)
			if !ok {
				return common.TransitionPayload{}, fmt.Errorf("%w requestorAccountKey", errdefs.ErrFailedToParse)
			}

			value, err := strconv.Atoi(requestorAccountKeyString)
			if err != nil {
				return common.TransitionPayload{}, err
			}
			requestorAccountKey = uint64(value)
		}
	}

	if requestorAccountKeyKw2 != nil {
		requestorAccountKey2, ok = requestorAccountKeyKw2.(uint64)
		if !ok {
			requestorAccountKeyString2, ok := requestorAccountKeyKw2.(string)
			if !ok {
				return common.TransitionPayload{}, fmt.Errorf("%w requestorAccountKey2", errdefs.ErrFailedToParse)
			}

			value, err := strconv.Atoi(requestorAccountKeyString2)
			if err != nil {
				return common.TransitionPayload{}, err
			}
			requestorAccountKey2 = uint64(value)
		}
	}

	// device_owner_account_key comes from device.f_read_sync_status on the cloud
	// side and lets RequestAppState mint the registry token under the device
	// owner instead of the requestor — required when the requestor is not the
	// app owner (determine_registry_access in auth.ts requires app.account_key
	// to match the JWT caller).
	if deviceOwnerAccountKeyKw != nil {
		deviceOwnerAccountKey, ok = deviceOwnerAccountKeyKw.(uint64)
		if !ok {
			deviceOwnerAccountKeyString, ok := deviceOwnerAccountKeyKw.(string)
			if !ok {
				return common.TransitionPayload{}, fmt.Errorf("%w deviceOwnerAccountKey", errdefs.ErrFailedToParse)
			}

			value, err := strconv.Atoi(deviceOwnerAccountKeyString)
			if err != nil {
				return common.TransitionPayload{}, err
			}
			deviceOwnerAccountKey = uint64(value)
		}
	}

	// app_cred_epoch is the generation of this app's per-app WAMP credential.
	// Absent (older backend) or unparseable means epoch 1 — never fail the
	// sync over it, or a backend that predates the column would stop apps.
	// Coerced rather than type-asserted: which numeric type a value arrives as
	// depends on the negotiated serializer.
	if appCredEpochKw != nil {
		if parsed, parsedOk := common.ToUint64(appCredEpochKw); parsedOk {
			appCredEpoch = parsed
		}
	}

	if releaseKeyKw != nil {
		releaseKey, ok = releaseKeyKw.(uint64)
		if !ok {
			releaseKeyString, ok := releaseKeyKw.(string)
			if !ok {
				return common.TransitionPayload{}, fmt.Errorf("%w releaseKeyString", errdefs.ErrFailedToParse)
			}

			// due to a bug the release key can be stored as string...
			parsedReleaseKey, err := strconv.ParseUint(releaseKeyString, 10, 64)
			if err != nil {
				return common.TransitionPayload{}, err
			}

			releaseKey = parsedReleaseKey
		}
	}

	if newReleaseKeyKw != nil {
		newReleaseKey, ok = newReleaseKeyKw.(uint64)
		if !ok {
			return common.TransitionPayload{}, fmt.Errorf("%w newReleaseKey", errdefs.ErrFailedToParse)
		}
	}

	if versionKw != nil {
		version, ok = versionKw.(string)
		if !ok {
			return common.TransitionPayload{}, fmt.Errorf("%w version", errdefs.ErrFailedToParse)
		}
	}

	if newestVersionKw != nil {
		newestVersion, ok = newestVersionKw.(string)
		if !ok {
			return common.TransitionPayload{}, fmt.Errorf("%w newestVersion", errdefs.ErrFailedToParse)
		}
	}

	if presentVersionKw != nil {
		presentVersion, ok = presentVersionKw.(string)
		if !ok {
			return common.TransitionPayload{}, fmt.Errorf("%w presentVersion", errdefs.ErrFailedToParse)
		}
	}

	if requestUpdateKw != nil {
		requestUpdate, ok = requestUpdateKw.(bool)
		if !ok {
			return common.TransitionPayload{}, fmt.Errorf("%w requestUpdate", errdefs.ErrFailedToParse)
		}
	}

	if cancelTransitionKw != nil {
		cancelTransition, ok = cancelTransitionKw.(bool)
		if !ok {
			return common.TransitionPayload{}, fmt.Errorf("%w cancelTransition", errdefs.ErrFailedToParse)
		}
	}

	if environmentKw != nil {
		environment, ok = environmentKw.(map[string]interface{})
		if !ok {
			return common.TransitionPayload{}, fmt.Errorf("%w environment", errdefs.ErrFailedToParse)
		}
	}

	if environmentTemplateKw != nil {
		environmentTemplate, ok = environmentTemplateKw.(map[string]interface{})
		if !ok {
			return common.TransitionPayload{}, fmt.Errorf("%w environment template", errdefs.ErrFailedToParse)
		}
	}

	if portsKw != nil {
		ports, ok = portsKw.([]interface{})
		if !ok {
			return common.TransitionPayload{}, fmt.Errorf("%w ports", errdefs.ErrFailedToParse)
		}
	}

	if dockerComposeKw != nil {
		dockerCompose, ok = dockerComposeKw.(map[string]interface{})
		if !ok {
			return common.TransitionPayload{}, fmt.Errorf("%w docker compose", errdefs.ErrFailedToParse)
		}
	}

	if newDockerComposeKw != nil {
		newDockerCompose, ok = newDockerComposeKw.(map[string]interface{})
		if !ok {
			return common.TransitionPayload{}, fmt.Errorf("%w new docker compose", errdefs.ErrFailedToParse)
		}
	}

	// privileged / new_privileged: absent or null (a backend that predates the
	// grant) stays nil, which keeps the app's privileges as they always were.
	if privilegedKw != nil {
		value, ok := privilegedKw.(bool)
		if !ok {
			return common.TransitionPayload{}, fmt.Errorf("%w privileged", errdefs.ErrFailedToParse)
		}
		privileged = &value
	}

	if newPrivilegedKw != nil {
		value, ok := newPrivilegedKw.(bool)
		if !ok {
			return common.TransitionPayload{}, fmt.Errorf("%w new privileged", errdefs.ErrFailedToParse)
		}
		newPrivileged = &value
	}

	if dockerCredentialsKw != nil {
		// Output map to store the parsed credentials
		dockerCredentials = make(map[string]common.DockerCredential)

		dockerCredentialsMap, ok := dockerCredentialsKw.(map[string]interface{})
		if !ok {
			return common.TransitionPayload{}, fmt.Errorf("%w docker credentials", errdefs.ErrFailedToParse)
		}

		// Iterate over the input map
		for key, value := range dockerCredentialsMap {
			// Assert the value to map[string]interface{}
			valMap, ok := value.(map[string]interface{})
			if !ok {
				return common.TransitionPayload{}, fmt.Errorf("%w docker credentials", errdefs.ErrFailedToParse)
			}

			// Extract username and password
			username, usernameOK := valMap["username"].(string)
			password, passwordOK := valMap["password"].(string)

			// Check if the username and password exist and are strings
			if !usernameOK || !passwordOK {
				return common.TransitionPayload{}, fmt.Errorf("%w docker credentials", errdefs.ErrFailedToParse)
			}

			// Key by the canonical host form so lookups by image-ref host
			// match regardless of how the user spelled the registry.
			host := common.NormalizeRegistryHost(key)
			if host == "" {
				continue
			}

			// Assign the credentials to the output map
			dockerCredentials[host] = common.DockerCredential{
				Username: username,
				Password: password,
			}
		}
	}

	// callerAuthIDString := details["caller_authid"]

	// callerAuthID, err := strconv.Atoi(callerAuthIDString.(string))

	// callerAuthID, ok := callerAuthIDString.(string)
	// if !ok {
	// 	return common.TransitionPayload{}, fmt.Errorf("Failed to parse callerAuthid")
	// }

	// happens where there is no app yet, for example, when you press stop on first build
	if requestedState == "" && manuallyRequestedState != "" {
		requestedState = manuallyRequestedState
	}

	if currentState == "" && currentState2 != "" {
		currentState = currentState2
	}

	if requestorAccountKey == 0 && requestorAccountKey2 != 0 {
		requestorAccountKey = requestorAccountKey2
	}

	payload := common.BuildTransitionPayload(appKey, appName, requestorAccountKey,
		common.Stage(stage), common.AppState(currentState),
		common.AppState(requestedState), releaseKey, newReleaseKey, config,
	)

	payload.DeviceOwnerAccountKey = deviceOwnerAccountKey
	payload.AppCredEpoch = appCredEpoch
	payload.RequestUpdate = requestUpdate

	// Version used to publish a release
	payload.Version = version

	// Newest version that is available of app
	payload.NewestVersion = newestVersion

	// Version that is currently on the device
	payload.PresentVersion = presentVersion

	payload.EnvironmentVariables = environment
	payload.EnvironmentTemplate = environmentTemplate

	payload.Ports = ports
	payload.DockerCompose = dockerCompose
	payload.NewDockerCompose = newDockerCompose
	payload.Privileged = privileged
	payload.NewPrivileged = newPrivileged

	payload.CancelTransition = cancelTransition

	payload.DockerCredentials = dockerCredentials

	// registryToken is added before we transition state and is not part of the response payload
	return payload, nil
}
