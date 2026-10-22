package apps

import (
	"context"
	"errors"
	"fmt"
	"reagent/common"
	"reagent/container"
	"reagent/errdefs"
)

// pullComposeImages downloads the given services' images (all services when
// none are given) via `docker compose pull`, streaming the CLI output to the
// app's PROD log topic. ctx must be the context registered as the app's
// cancelable compose transition (registerComposeTransitionCancel): a cancel
// kills the CLI process, and the kill surfaces as a canceled stream so
// RequestAppState unwinds the transition as canceled rather than marking the
// app FAILED. Callers must register BEFORE publishing DOWNLOADING — the cloud
// may answer that state with a cancel at any moment, and a cancel arriving
// before registration would be lost while the pull keeps running.
func (sm *StateMachine) pullComposeImages(ctx context.Context, payload common.TransitionPayload, dockerComposePath string, services []string) error {
	compose := sm.Container.Compose()

	pullOutput, pullCmd, err := compose.PullContext(ctx, dockerComposePath, services...)
	if err != nil {
		return composeTransitionErr(ctx, err)
	}

	_, err = sm.LogManager.StreamLogsChannel(pullOutput, payload.ContainerName.Prod)
	if err != nil {
		return composeTransitionErr(ctx, err)
	}

	err = pullCmd.Wait()
	if err != nil {
		return composeTransitionErr(ctx, err)
	}

	return nil
}

func (sm *StateMachine) pullComposeApp(payload common.TransitionPayload, app *common.App) error {
	topicForLogStream := payload.ContainerName.Prod

	err := sm.LogManager.ClearLogHistory(topicForLogStream)
	if err != nil {
		return err
	}

	// Fail a definition this device can never run before pulling anything.
	err = sm.validateComposeForDevice(payload.DockerCompose, topicForLogStream)
	if err != nil {
		return err
	}

	// Register the cancelable pull context before publishing DOWNLOADING; see
	// pullComposeImages.
	ctx, cancel := context.WithCancel(context.Background())
	sm.registerComposeTransitionCancel(payload.Stage, payload.AppKey, cancel)
	defer func() {
		sm.clearComposeTransitionCancel(payload.Stage, payload.AppKey)
		cancel()
	}()

	err = sm.setState(app, common.DOWNLOADING)
	if err != nil {
		return err
	}

	compose := sm.Container.Compose()

	// TODO: make sure that folder exists so that compose can be started, make a different folder for PROD apps
	dockerComposePath, err := sm.SetupComposeFiles(payload, app, false)
	if err != nil {
		return err
	}

	if !compose.Supported() {
		message := "Docker Compose is not supported for this device"
		writeErr := sm.LogManager.Write(topicForLogStream, message)
		if writeErr != nil {
			return writeErr
		}
		return errdefs.DockerComposeNotSupported(errors.New("docker compose is not supported"))
	}

	err = sm.teardownComposeProject(payload, app, dockerComposePath)
	if err != nil {
		return err
	}

	err = sm.HandleRegistryLoginsWithDefault(payload)
	if err != nil {
		writeErr := sm.LogManager.Write(topicForLogStream, err.Error())
		if writeErr != nil {
			return writeErr
		}
		return err
	}

	sm.warnUncredentialedComposeRegistries(payload, payload.DockerCompose, topicForLogStream)

	err = sm.pullComposeImages(ctx, payload, dockerComposePath, nil)
	if err != nil {
		return err
	}

	buildMessage := "Compose Image Installed successfully"
	err = sm.LogManager.Write(topicForLogStream, buildMessage)
	if err != nil {
		return err
	}

	return sm.setState(app, common.PRESENT)
}

func (sm *StateMachine) pullApp(payload common.TransitionPayload, app *common.App) error {
	if payload.DockerCompose != nil {
		return sm.pullComposeApp(payload, app)
	}

	if payload.Stage == common.DEV {
		// cannot pull dev apps from registry
		return errors.New("cannot pull dev apps")
	}

	err := sm.LogManager.ClearRemote(payload.ContainerName.Prod)
	if err != nil {
		return err
	}

	initMessage := fmt.Sprintf("Initialising download for the app: %s...", payload.AppName)
	err = sm.LogManager.Write(payload.ContainerName.Prod, initMessage)
	if err != nil {
		return err
	}

	err = sm.LogManager.ClearLogHistory(payload.ContainerName.Prod)
	if err != nil {
		return err
	}

	// Need to authenticate to private registry to determine proper privileges to pull the app
	err = sm.HandleRegistryLoginsWithDefault(payload)
	if err != nil {
		writeErr := sm.LogManager.Write(payload.ContainerName.Prod, err.Error())
		if writeErr != nil {
			return writeErr
		}
		return err
	}

	fullImageNameWithVersion := fmt.Sprintf("%s:%s", payload.RegistryImageName.Prod, payload.NewestVersion)
	pullOptions := container.PullOptions{
		AuthConfig: sm.authConfigForImage(payload, fullImageNameWithVersion, payload.ContainerName.Prod),
		PullID:     common.BuildDockerPullID(payload.AppKey, payload.AppName),
	}

	reader, err := sm.Container.Pull(context.Background(), fullImageNameWithVersion, pullOptions)
	if err != nil {
		errorMessage := fmt.Sprintf("Error occured while trying to pull the image: %s", err.Error())
		_ = sm.LogManager.Write(payload.ContainerName.Prod, errorMessage)
		return err
	}

	err = sm.setState(app, common.DOWNLOADING)
	if err != nil {
		return err
	}

	streamErr := sm.LogManager.StreamBlocking(payload.ContainerName.Prod, common.PULL, reader)
	if streamErr != nil {
		if errdefs.IsDockerStreamCanceled(streamErr) {
			pullMessage := "The app download was canceled"
			writeErr := sm.LogManager.Write(payload.ContainerName.Prod, pullMessage)
			if writeErr != nil {
				return writeErr
			}
			// this error will not cause a failed state and is handled upstream
			return streamErr
		}

		return streamErr
	}

	pullMessage := fmt.Sprintf("Succesfully installed the app: %s (Version: %s)", payload.AppName, payload.NewestVersion)
	writeErr := sm.LogManager.Write(payload.ContainerName.Prod, pullMessage)
	if writeErr != nil {
		return writeErr
	}

	err = sm.setState(app, common.PRESENT)
	if err != nil {
		return err
	}

	return nil
}
