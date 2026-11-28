package apps

import (
	"context"
	"errors"
	"fmt"
	"reagent/common"
	"reagent/config"
	"reagent/container"
	"reagent/errdefs"
	"time"
)

func (sm *StateMachine) publishApp(payload common.TransitionPayload, app *common.App) error {
	if payload.DockerCompose != nil {
		return sm.publishComposeApp(payload, app)
	}

	// nothing of an earlier build is reported for this one
	setPushedDigests(app, nil)

	err := sm.LogManager.Write(payload.PublishContainerName, fmt.Sprintf("Initializing publish process for %s...", app.AppName))
	if err != nil {
		return err
	}

	err = sm.buildDevApp(payload, app, true)
	if err != nil {
		return err
	}

	err = sm.LogManager.Write(payload.PublishContainerName, "App build has finished, Starting to publish...")
	if err != nil {
		return err
	}

	// Pushed under the per-architecture name of the platform the build is
	// for (releaseBuildRepository): the backend expects exactly that name,
	// and the registry's token allows no other. The platform's names
	// (PresentImageName, NewImageName) are for pulling and never used here.
	prodImage := fmt.Sprintf("%s:%s", releaseBuildRepository(sm.reswarmConfig(), payload), payload.Version)
	tagContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	err = sm.Container.Tag(tagContext, payload.RegistryImageName.Dev, prodImage)
	if err != nil {
		return err
	}

	err = sm.setState(app, common.PUBLISHING)
	if err != nil {
		return err
	}

	err = sm.HandleRegistryLoginsWithDefault(payload)
	if err != nil {
		writeErr := sm.LogManager.Write(payload.PublishContainerName, err.Error())
		if writeErr != nil {
			return writeErr
		}
		return err
	}

	pushOptions := container.PushOptions{
		AuthConfig: container.AuthConfig{
			Username: payload.RegisteryToken,
			Password: sm.Container.GetConfig().ReswarmConfig.Secret,
		},
		PushID: common.BuildDockerPushID(payload.AppKey, payload.AppName),
	}

	reader, err := sm.Container.Push(context.Background(), prodImage, pushOptions)
	if err != nil {
		return err
	}
	reader, pushed := capturePushDigest(reader)

	streamErr := sm.LogManager.StreamBlocking(payload.PublishContainerName, common.PUSH, reader)
	if streamErr != nil {
		if errdefs.IsDockerStreamCanceled(streamErr) {
			pushMessage := "The app release was canceled"
			writeErr := sm.LogManager.Write(payload.PublishContainerName, pushMessage)
			if writeErr != nil {
				return writeErr
			}
			// this error will not cause a failed state and is handled upstream
			return streamErr
		}

		return streamErr
	}

	if digest, ok := pushedDigestOf(prodImage, pushed); ok {
		setPushedDigests(app, map[string]string{prodImage: digest})
	}

	err = sm.setState(app, common.PUBLISHED)
	if err != nil {
		return err
	}

	err = sm.LogManager.ClearLogHistory(payload.PublishContainerName)
	if err != nil {
		return err
	}

	return nil
}

func (sm *StateMachine) publishComposeApp(payload common.TransitionPayload, app *common.App) error {
	// nothing of an earlier build is reported for this one
	setPushedDigests(app, nil)

	err := sm.LogManager.Write(payload.PublishContainerName, fmt.Sprintf("Initializing publish process for %s...", app.AppName))
	if err != nil {
		return err
	}

	err = sm.buildDevApp(payload, app, true)
	if err != nil {
		return err
	}

	err = sm.LogManager.Write(payload.PublishContainerName, "App build has finished, Starting to publish...")
	if err != nil {
		return err
	}

	err = sm.setState(app, common.PUBLISHING)
	if err != nil {
		return err
	}

	dockerComposePath, err := sm.SetupComposeFiles(payload, app, false)
	if err != nil {
		return err
	}

	compose := sm.Container.Compose()

	err = sm.HandleRegistryLoginsWithDefault(payload)
	if err != nil {
		writeErr := sm.LogManager.Write(payload.PublishContainerName, err.Error())
		if writeErr != nil {
			return writeErr
		}
		return err
	}

	pushedDigests, err := sm.rehostSourceImages(payload)
	if err != nil {
		if errdefs.IsDockerStreamCanceled(err) {
			// like a canceled push below: no failed state, handled upstream
			return err
		}
		writeErr := sm.LogManager.Write(payload.PublishContainerName, err.Error())
		if writeErr != nil {
			return writeErr
		}
		return err
	}

	pushOutput, pushCmd, err := compose.Push(dockerComposePath)
	if err != nil {
		return err
	}

	_, err = sm.LogManager.StreamLogsChannel(pushOutput, payload.PublishContainerName)
	if err != nil {
		return err
	}

	err = pushCmd.Wait()
	if err != nil {
		return err
	}

	// The images compose pushed, under the names of the compose file (as it
	// pushed them), with the digests their repositories now carry.
	for _, ref := range composePushedImages(dockerComposePath) {
		if digest, ok := sm.repoDigestOf(ref); ok {
			pushedDigests[ref] = digest
		}
	}
	setPushedDigests(app, pushedDigests)

	err = sm.setState(app, common.PUBLISHED)
	if err != nil {
		return err
	}

	err = sm.LogManager.ClearLogHistory(payload.PublishContainerName)
	if err != nil {
		return err
	}

	return nil
}

// rehostSourceImages makes a release self-contained: f_create_release rewrites
// every service's image to a store-registry name and keeps the authored
// external ref of image-only services in the x-source-image extension field.
// `docker compose push` skips services without a build section, so those
// images are pulled from their source registry here (with the app's
// docker_credentials — carried only by build/publish payloads), retagged to
// the store name and pushed explicitly. After this, installs never touch the
// external registry or need the app's credentials. Releases created before
// the rewrite carry no x-source-image fields, making this a no-op for them.
// It answers the digest each push answered, where its stream named one
// (pushed_digests of the PUBLISHED report).
func (sm *StateMachine) rehostSourceImages(payload common.TransitionPayload) (map[string]string, error) {
	pushedDigests := map[string]string{}
	services, ok := (payload.DockerCompose["services"]).(map[string]interface{})
	if !ok {
		return pushedDigests, errors.New("failed to infer services")
	}

	config := sm.Container.GetConfig()

	for serviceName, serviceInterface := range services {
		service, ok := (serviceInterface).(map[string]interface{})
		if !ok {
			return pushedDigests, errors.New("failed to infer service")
		}

		sourceImage, _ := service["x-source-image"].(string)
		if sourceImage == "" {
			continue
		}

		targetImage, _ := service["image"].(string)
		if targetImage == "" {
			return pushedDigests, fmt.Errorf("service %s declares x-source-image but no image", serviceName)
		}

		err := sm.LogManager.Write(payload.PublishContainerName, fmt.Sprintf("Re-hosting image %s of service %s as %s...", sourceImage, serviceName, targetImage))
		if err != nil {
			return pushedDigests, err
		}

		sourceHost := registryHostOfImage(sourceImage)
		authConfig, credentialFound := credentialForRegistryHost(payload.DockerCredentials, sourceHost)
		if !credentialFound {
			err = sm.LogManager.Write(payload.PublishContainerName, registryCredentialMissMessage(sourceHost))
			if err != nil {
				return pushedDigests, err
			}
		}

		pullOptions := container.PullOptions{
			AuthConfig: authConfig,
			PullID:     common.BuildDockerPullID(payload.AppKey, payload.AppName),
		}

		pullReader, err := sm.Container.Pull(context.Background(), sourceImage, pullOptions)
		if err != nil {
			if isRegistryAuthError(err) {
				if !credentialFound {
					return pushedDigests, fmt.Errorf("registry %s rejected the pull of %s: %w — the registry requires credentials and none are stored for it; add them for host %q under App Settings → Docker Registry Credentials, then publish again", sourceHost, sourceImage, err, common.NormalizeRegistryHost(sourceHost))
				}
				return pushedDigests, fmt.Errorf("registry %s rejected the stored credentials while pulling %s: %w — check the username/password under App Settings → Docker Registry Credentials, then publish again", sourceHost, sourceImage, err)
			}
			return pushedDigests, err
		}

		streamErr := sm.LogManager.StreamBlocking(payload.PublishContainerName, common.PULL, pullReader)
		if streamErr != nil {
			return pushedDigests, streamErr
		}

		tagContext, cancelTag := context.WithTimeout(context.Background(), time.Second*30)
		err = sm.Container.Tag(tagContext, sourceImage, targetImage)
		cancelTag()
		if err != nil {
			return pushedDigests, err
		}

		pushOptions := container.PushOptions{
			AuthConfig: container.AuthConfig{
				Username: payload.RegisteryToken,
				Password: config.ReswarmConfig.Secret,
			},
			PushID: common.BuildDockerPushID(payload.AppKey, payload.AppName),
		}

		pushReader, err := sm.Container.Push(context.Background(), targetImage, pushOptions)
		if err != nil {
			return pushedDigests, err
		}
		pushReader, pushed := capturePushDigest(pushReader)

		streamErr = sm.LogManager.StreamBlocking(payload.PublishContainerName, common.PUSH, pushReader)
		if streamErr != nil {
			return pushedDigests, streamErr
		}
		if digest, ok := pushedDigestOf(targetImage, pushed); ok {
			pushedDigests[targetImage] = digest
		}
	}

	return pushedDigests, nil
}

// releaseBuildRepository is the repository a release build's single image is
// pushed under: prod_<architecture>_<app key>_<name> in the device's
// registry and main repository, for the architecture the build is for. That
// is the device's architecture as the platform stores it
// (TransitionPayload.Architecture, the sync answer's architecture: what
// dockerfileArch builds for, and what the platform registers the build as),
// where the platform names one that apps are built for. It is the
// architecture the agent was built for on every device whose apps are
// matched by it, so the name is the agent's own (RegistryImageName.Prod),
// as it always was. It differs where the platform matches the device's apps
// by its Docker engine: an arm64 agent next to a 32-bit engine builds armv7
// images, and pushed under the agent's own name they would replace the
// arm64 image of the same version under the tag arm64 devices pull, while
// armv7 devices would find none under theirs. From an older backend, a
// stored state or for a value such as ARM (no architecture named), and
// without a configuration to form the name with: the agent's own name.
// (A Compose build pushes the names its Compose file carries, which the
// platform wrote for the same architecture.)
func releaseBuildRepository(rc *config.ReswarmConfig, payload common.TransitionPayload) string {
	switch payload.Architecture {
	case "amd64", "arm64", "armv7", "armv6", "armv5":
		if rc != nil {
			return common.BuildRegistryImageName(rc.DockerRegistryURL, rc.DockerMainRepository,
				common.BuildImageName(common.PROD, payload.Architecture, payload.AppKey, payload.AppName))
		}
	}
	return payload.RegistryImageName.Prod
}
