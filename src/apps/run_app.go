package apps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reagent/common"
	"reagent/config"
	"reagent/container"
	"reagent/errdefs"
	reagentnetwork "reagent/network"
	"reagent/system"
	"reagent/trust"
	"reagent/tunnel"
	"strings"
	"time"

	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/rs/zerolog/log"
)

func (sm *StateMachine) runApp(payload common.TransitionPayload, app *common.App) error {
	if payload.Stage == common.DEV {
		return sm.runDevApp(payload, app)
	} else if payload.Stage == common.PROD {
		return sm.runProdApp(payload, app)
	}
	return nil
}

func (sm *StateMachine) runProdApp(payload common.TransitionPayload, app *common.App) error {
	if payload.DockerCompose != nil {
		return sm.runProdComposeApp(payload, app)
	}

	err := sm.LogManager.ClearLogHistory(payload.ContainerName.Prod)
	if err != nil {
		return err
	}

	err = sm.LogManager.Write(payload.ContainerName.Prod, fmt.Sprintf("Starting %s...", payload.AppName))
	if err != nil {
		return err
	}

	err = sm.setState(app, common.STARTING)
	if err != nil {
		return err
	}

	containerID, err := sm.startContainer(payload, app)
	if err != nil {
		return err
	}

	pollingRate := time.Second * 1
	waitForRunningContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	runningSignal, errC := sm.Container.WaitForRunning(waitForRunningContext, containerID, pollingRate)

	// block and wait for running, if exited status then return as a failed state
	select {
	case err = <-errC:
		_ = sm.LogManager.Write(payload.ContainerName.Dev, fmt.Sprintf("The app failed to start, reason: %s", err.Error()))
		options := common.Dict{"follow": true, "stdout": true, "stderr": true}
		ioReader, logsErr := sm.Container.Logs(context.Background(), containerID, options)
		if logsErr != nil {
			return logsErr
		}

		_ = sm.LogManager.StreamBlocking(payload.ContainerName.Prod, common.APP, ioReader)
		return err
	case <-runningSignal:
		break
	}

	err = sm.setState(app, common.RUNNING)
	if err != nil {
		return err
	}

	err = sm.LogManager.Write(payload.ContainerName.Prod, fmt.Sprintf("Now running app %s (%s)", payload.AppName, app.Version))
	if err != nil {
		return err
	}

	err = sm.LogManager.Stream(payload.ContainerName.Prod, common.APP, nil)
	if err != nil {
		return err
	}

	return nil
}

// composeUpAttempts bounds the retry when `docker compose up` loses a host
// port to a process outside the agent between the registry's probe and
// docker's bind.
const composeUpAttempts = 3

// composeFallbackStopTimeout mirrors the compose CLI's default grace period
// between SIGTERM and SIGKILL for containers stopped through the Docker API.
const composeFallbackStopTimeout = 10 * time.Second

// teardownComposeProject stops and removes every container of an app's compose
// project (`docker compose stop` + `rm -f`; networks and volumes survive).
//
// It reconciles against the daemon instead of trusting the CLI alone:
//   - a project with no containers is already torn down, so the CLI is not
//     spawned at all;
//   - when the CLI fails although the daemon answers, the containers are
//     stopped and removed through the Docker API. The compose CLI is a
//     separate binary that can break on its own — 2026-09-17, tls-sf002:
//     `docker compose stop` aborted with a Go runtime crash on every run, so
//     every stop of the app failed while its containers kept running, and the
//     user had no way to stop the app from the UI. The transition's goal (no
//     container of the project left) does not depend on which tool reaches it.
//
// The CLI error is still returned when the fallback cannot finish the job.
func (sm *StateMachine) teardownComposeProject(payload common.TransitionPayload, app *common.App, dockerComposePath string) error {
	project := common.BuildComposeContainerName(payload.Stage, app.AppKey, app.AppName)

	listCtx, cancelList := context.WithTimeout(context.Background(), time.Second*30)
	containers, listErr := container.ListComposeProjectContainers(listCtx, sm.Container, project)
	cancelList()
	if listErr == nil && len(containers) == 0 {
		log.Debug().Msgf("compose project %s has no containers; nothing to tear down", project)
		return nil
	}

	compose := sm.Container.Compose()
	composeErr := compose.Stop(dockerComposePath)
	if composeErr == nil {
		composeErr = compose.Remove(dockerComposePath)
	}
	if composeErr == nil {
		return nil
	}

	fallbackErr := sm.removeComposeProjectContainers(project)
	if fallbackErr != nil {
		log.Error().Err(fallbackErr).Msgf("docker compose teardown of %s failed and the Docker API fallback could not finish it", project)
		return composeErr
	}

	log.Warn().Err(composeErr).Msgf("docker compose teardown of %s failed; its containers were stopped and removed through the Docker API instead", project)

	logTopic := payload.ContainerName.Prod
	if payload.Stage == common.DEV {
		logTopic = payload.ContainerName.Dev
	}
	writeErr := sm.LogManager.Write(logTopic, fmt.Sprintf("docker compose could not stop %s (%s); its containers were stopped and removed directly", payload.AppName, composeErr))
	if writeErr != nil {
		log.Debug().Err(writeErr).Msg("failed to write the compose fallback notice to the app log")
	}

	return nil
}

// removeComposeProjectContainers stops and removes the project's containers
// through the Docker API — the end state `docker compose stop` + `rm -f`
// reach. Listed fresh (the CLI may have got partway) and by project label,
// which is how compose itself finds them. A container that disappears
// underneath is already gone, not a failure.
func (sm *StateMachine) removeComposeProjectContainers(project string) error {
	listCtx, cancelList := context.WithTimeout(context.Background(), time.Second*30)
	containers, err := container.ListComposeProjectContainers(listCtx, sm.Container, project)
	cancelList()
	if err != nil {
		return err
	}

	for _, cont := range containers {
		err = sm.removeComposeProjectContainer(cont.ID)
		if err != nil {
			return err
		}
	}

	return nil
}

func (sm *StateMachine) removeComposeProjectContainer(containerID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), composeFallbackStopTimeout+time.Second*30)
	defer cancel()

	err := sm.Container.StopContainerByID(ctx, containerID, composeFallbackStopTimeout)
	if err != nil && !errdefs.IsContainerNotFound(err) {
		return fmt.Errorf("stop container %s: %w", containerID, err)
	}

	err = sm.Container.RemoveContainerByID(ctx, containerID, map[string]interface{}{"force": true})
	if err != nil && !errdefs.IsContainerNotFound(err) {
		return fmt.Errorf("remove container %s: %w", containerID, err)
	}

	return nil
}

// composeUp brings the project up, streaming the CLI output to the app's log
// topic. A bind conflict is retried with freshly assigned host ports, the
// compose equivalent of what startContainer does for single-container apps.
// The managed ports live in the compose file rather than in the launch call,
// so a retry has to regenerate the file — and tear the half-started project
// down first, or SetupComposeFiles recovers the conflicting port straight back
// out of the still-running previous generation.
//
// On a failure that is not retryable the reason is written to the app log:
// compose reports it on stderr and exits non-zero, and nothing else in the
// transition surfaces it to the user.
func (sm *StateMachine) composeUp(payload common.TransitionPayload, app *common.App, dockerComposePath string, logTopic string) error {
	compose := sm.Container.Compose()

	// PROD images always come from the registry; DEV apps are built on the
	// device by the BUILT transition, so their `up` may still build.
	up := compose.Up
	if payload.Stage == common.PROD {
		up = compose.UpNoBuild
	}

	var lastErr error
	for attempt := 0; attempt < composeUpAttempts; attempt++ {
		outputChan, upCmd, err := up(dockerComposePath)
		if err != nil {
			return err
		}

		_, err = sm.LogManager.StreamLogsChannel(outputChan, logTopic)
		if err != nil {
			return err
		}

		lastErr = upCmd.Wait()
		if lastErr == nil {
			return nil
		}

		if !isPortAllocationError(lastErr) || !sm.StateObserver.AppManager.reassignComposePortsAfterBindConflict(payload, lastErr) {
			break
		}

		log.Warn().Str("app", payload.AppName).Msg("Retrying compose up with freshly assigned host ports")

		err = sm.teardownComposeProject(payload, app, dockerComposePath)
		if err != nil {
			return err
		}

		_, err = sm.SetupComposeFiles(payload, app, false)
		if err != nil {
			return err
		}
	}

	_ = sm.LogManager.Write(logTopic, fmt.Sprintf("The app failed to start, reason: %s", lastErr.Error()))

	// Best effort: a failed `up` can leave part of the project behind, and the
	// teardown error must not mask the reason the start failed.
	cleanupErr := sm.teardownComposeProject(payload, app, dockerComposePath)
	if cleanupErr != nil {
		log.Warn().Err(cleanupErr).Str("app", payload.AppName).Msg("Failed to clean up the compose project after a failed start")
	}

	return lastErr
}

func (sm *StateMachine) runDevComposeApp(payload common.TransitionPayload, app *common.App) error {
	err := sm.LogManager.ClearLogHistory(payload.ContainerName.Dev)
	if err != nil {
		return err
	}

	err = sm.validateComposeForDevice(payload.DockerCompose, payload.ContainerName.Dev)
	if err != nil {
		return err
	}

	compose := sm.Container.Compose()

	// Mark STARTING before SetupComposeFiles: that step reads the previous
	// generation's published ports from the Docker API, which can be slow on a
	// contended daemon; leaving the app in PRESENT until then misleads the UI.
	err = sm.setState(app, common.STARTING)
	if err != nil {
		return err
	}

	dockerComposePath, err := sm.SetupComposeFiles(payload, app, false)
	if err != nil {
		return err
	}

	err = sm.HandleRegistryLoginsWithDefault(payload)
	if err != nil {
		writeErr := sm.LogManager.Write(payload.ContainerName.Prod, err.Error())
		if writeErr != nil {
			return writeErr
		}
		return err
	}

	sm.warnUncredentialedComposeRegistries(payload, payload.DockerCompose, payload.ContainerName.Dev)

	err = sm.teardownComposeProject(payload, app, dockerComposePath)
	if err != nil {
		return err
	}

	err = sm.LogManager.Write(payload.ContainerName.Dev, fmt.Sprintf("Starting %s...", payload.AppName))
	if err != nil {
		return err
	}

	err = sm.composeUp(payload, app, dockerComposePath, payload.ContainerName.Dev)
	if err != nil {
		return err
	}

	waitForRunningContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	pollingRate := time.Second * 1
	runningSignal, errC := compose.WaitForRunning(waitForRunningContext, dockerComposePath, pollingRate)

	// block and wait for running, if exited status then return as a failed state
	select {
	case err = <-errC:
		if err != nil {
			// cleanup docker containers
			cleanupErr := sm.teardownComposeProject(payload, app, dockerComposePath)
			if cleanupErr != nil {
				return cleanupErr
			}

			_ = sm.LogManager.Write(payload.ContainerName.Dev, fmt.Sprintf("The app failed to start, reason: %s", err.Error()))
			return err
		}
		break
	case <-runningSignal:
		break
	}

	err = sm.setState(app, common.RUNNING)
	if err != nil {
		return err
	}

	err = sm.LogManager.Write(payload.ContainerName.Dev, fmt.Sprintf("Now running app %s (%s)", payload.AppName, app.Version))
	if err != nil {
		return err
	}

	logsChannel, err := compose.LogStream(dockerComposePath)
	if err != nil {
		return err
	}

	_, err = sm.LogManager.StreamLogsChannel(logsChannel, payload.ContainerName.Dev)
	if err != nil {
		return err
	}

	return nil
}

func (sm *StateMachine) runProdComposeApp(payload common.TransitionPayload, app *common.App) error {
	err := sm.LogManager.ClearLogHistory(payload.ContainerName.Prod)
	if err != nil {
		return err
	}

	compose := sm.Container.Compose()
	if !compose.Supported() {
		message := "Docker Compose is not supported for this device"
		writeErr := sm.LogManager.Write(payload.ContainerName.Prod, message)
		if writeErr != nil {
			return writeErr
		}

		return errdefs.DockerComposeNotSupported(errors.New("docker compose is not supported"))
	}

	// Refuse a definition this device can never run before tearing down or
	// starting anything; the engine's own rejection would come one service at a
	// time, mid-`up`, with the project already half-created.
	err = sm.validateComposeForDevice(payload.DockerCompose, payload.ContainerName.Prod)
	if err != nil {
		return err
	}

	err = sm.HandleRegistryLoginsWithDefault(payload)
	if err != nil {
		writeErr := sm.LogManager.Write(payload.ContainerName.Prod, err.Error())
		if writeErr != nil {
			return writeErr
		}
		return err
	}

	sm.warnUncredentialedComposeRegistries(payload, payload.DockerCompose, payload.ContainerName.Prod)

	// Decide up front whether this is a FRESH INSTALL whose service images
	// still have to be downloaded. `compose up` pulls missing images
	// implicitly, which used to leave the app in STARTING for the whole
	// download — for a fresh install we surface a real DOWNLOADING phase below,
	// explicitly pulling just the missing services (mirroring up's missing-only
	// pull, so an unreachable third-party registry for an already-present
	// shared image cannot fail the install). Restarts of an already-installed
	// app deliberately keep the implicit pull: the cloud answers a cancel
	// during DOWNLOADING with an uninstall — correct when an install is being
	// canceled, but never what the stop button may do to an installed app.
	// If the check itself fails, fall back to the implicit pull so a
	// Docker-API hiccup cannot break a start that would otherwise succeed.
	app.StateLock.Lock()
	freshInstall := app.CurrentState == common.UNINSTALLED || app.CurrentState == common.REMOVED
	app.StateLock.Unlock()

	var missingImageServices []string
	if freshInstall {
		getImagesContext, cancelGetImages := context.WithTimeout(context.Background(), time.Second*30)
		localImages, imagesErr := sm.Container.GetImages(getImagesContext, "")
		cancelGetImages()
		if imagesErr != nil {
			log.Warn().Err(imagesErr).Msgf("failed to list local images for %s; deferring the download to compose up", payload.AppName)
		} else {
			missingImageServices, err = composeServicesWithMissingImages(payload.DockerCompose, localImages)
			if err != nil {
				log.Warn().Err(err).Msgf("failed to check compose images for %s; deferring the download to compose up", payload.AppName)
				missingImageServices = nil
			}
		}
	}
	needsDownload := len(missingImageServices) > 0

	// Register the cancelable pull context before publishing DOWNLOADING; see
	// pullComposeImages.
	var pullCtx context.Context
	if needsDownload {
		var cancelPull context.CancelFunc
		pullCtx, cancelPull = context.WithCancel(context.Background())
		sm.registerComposeTransitionCancel(payload.Stage, payload.AppKey, cancelPull)
		defer func() {
			sm.clearComposeTransitionCancel(payload.Stage, payload.AppKey)
			cancelPull()
		}()
	}

	// Mark STARTING (or DOWNLOADING when images must be pulled) before
	// SetupComposeFiles: on a contended boot that step (a Docker-API read of
	// the previous generation's published ports) can take several seconds, and
	// until the app leaves PRESENT the cloud sees it as not-yet-starting while
	// re-drive pushes just bounce off the held lock.
	initialState := common.STARTING
	if needsDownload {
		initialState = common.DOWNLOADING
	}
	err = sm.setState(app, initialState)
	if err != nil {
		return err
	}

	dockerComposePath, err := sm.SetupComposeFiles(payload, app, false)
	if err != nil {
		return err
	}

	err = sm.teardownComposeProject(payload, app, dockerComposePath)
	if err != nil {
		return err
	}

	if needsDownload {
		err = sm.LogManager.Write(payload.ContainerName.Prod, fmt.Sprintf("Downloading images for %s...", payload.AppName))
		if err != nil {
			return err
		}

		err = sm.pullComposeImages(pullCtx, payload, dockerComposePath, missingImageServices)
		if err != nil {
			return err
		}
	}

	err = sm.LogManager.Write(payload.ContainerName.Prod, fmt.Sprintf("Starting %s...", payload.AppName))
	if err != nil {
		return err
	}

	// Leave DOWNLOADING again once the pull is done. Without a download phase
	// the early STARTING publish above already covers this — don't publish the
	// same state twice.
	if needsDownload {
		err = sm.setState(app, common.STARTING)
		if err != nil {
			return err
		}
	}

	err = sm.composeUp(payload, app, dockerComposePath, payload.ContainerName.Prod)
	if err != nil {
		return err
	}

	pollingRate := time.Second * 1
	waitForRunningContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	runningSignal, errC := compose.WaitForRunning(waitForRunningContext, dockerComposePath, pollingRate)

	// block and wait for running, if exited status then return as a failed state
	select {
	case err = <-errC:
		if err != nil {
			// cleanup docker containers
			cleanupErr := sm.teardownComposeProject(payload, app, dockerComposePath)
			if cleanupErr != nil {
				return cleanupErr
			}

			_ = sm.LogManager.Write(payload.ContainerName.Prod, fmt.Sprintf("The app failed to start, reason: %s", err.Error()))
			return err
		}
		break
	case <-runningSignal:
		break
	}

	err = sm.setState(app, common.RUNNING)
	if err != nil {
		return err
	}

	err = sm.LogManager.Write(payload.ContainerName.Prod, fmt.Sprintf("Now running app %s (%s)", payload.AppName, app.Version))
	if err != nil {
		return err
	}

	logsChannel, err := compose.LogStream(dockerComposePath)
	if err != nil {
		return err
	}

	_, err = sm.LogManager.StreamLogsChannel(logsChannel, payload.ContainerName.Prod)
	if err != nil {
		return err
	}

	return nil
}

func (sm *StateMachine) runDevApp(payload common.TransitionPayload, app *common.App) error {
	if payload.DockerCompose != nil {
		return sm.runDevComposeApp(payload, app)
	}

	getContainerContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	// remove old container first, if it exists
	cont, err := sm.Container.GetContainer(getContainerContext, payload.ContainerName.Dev)
	if err != nil {
		if !errdefs.IsContainerNotFound(err) {
			return err
		}
	} else {
		RemoveContainerByIDContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
		defer cancel()

		removeContainerErr := sm.Container.RemoveContainerByID(RemoveContainerByIDContext, cont.ID, map[string]interface{}{"force": true})
		if removeContainerErr != nil {
			// It's possible we're trying to remove the container when it's already being removed
			// RUNNING -> STOPPED -> RUNNING
			// It's also possible the container does not exist yet, if it's the first time you're building the app
			if !errdefs.IsContainerRemovalAlreadyInProgress(removeContainerErr) && !errdefs.IsContainerNotFound(removeContainerErr) {
				return removeContainerErr
			}
		}

		waitForContainerByIDContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
		defer cancel()

		_, err = sm.Container.WaitForContainerByID(waitForContainerByIDContext, cont.ID, dockercontainer.WaitConditionRemoved)
		if err != nil {
			// expected behaviour, see: https://github.com/docker/docker-py/issues/2270
			// still useful, and will wait if it's still not removed
			if !errdefs.IsContainerNotFound(err) {
				return err
			}
		}

	}

	err = sm.LogManager.ClearLogHistory(payload.ContainerName.Dev)
	if err != nil {
		return err
	}

	err = sm.LogManager.Write(payload.ContainerName.Dev, fmt.Sprintf("Starting %s...", payload.AppName))
	if err != nil {
		return err
	}

	err = sm.setState(app, common.STARTING)
	if err != nil {
		return err
	}

	newContainerID, err := sm.startContainer(payload, app)
	if err != nil {
		return err
	}

	pollingRate := time.Second * 1
	waitForRunningContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	runningSignal, errC := sm.Container.WaitForRunning(waitForRunningContext, newContainerID, pollingRate)

	// block and wait for running, if exited status then return as a failed state
	select {
	case err = <-errC:
		_ = sm.LogManager.Write(payload.ContainerName.Dev, fmt.Sprintf("The app failed to start, reason: %s", err.Error()))
		options := common.Dict{"follow": true, "stdout": true, "stderr": true}
		ioReader, logsErr := sm.Container.Logs(context.Background(), newContainerID, options)
		if logsErr != nil {
			return logsErr
		}

		_ = sm.LogManager.StreamBlocking(payload.ContainerName.Dev, common.APP, ioReader)
		return err
	case <-runningSignal:
		break
	}

	err = sm.setState(app, common.RUNNING)
	if err != nil {
		return err
	}

	err = sm.LogManager.Write(payload.ContainerName.Dev, fmt.Sprintf("Now running app %s", payload.AppName))
	if err != nil {
		return err
	}

	err = sm.LogManager.Stream(payload.ContainerName.Dev, common.APP, nil)
	if err != nil {
		return err
	}

	return nil
}

// reservedEnvNames may not be overridden by an app's own environment settings.
// Docker takes the LAST occurrence of a duplicate, and payload env is appended
// after the defaults — so without this filter an app could set its own
// APP_AUTH_ID and volunteer a foreign app identity to the authenticator.
var reservedEnvNames = map[string]bool{
	"APP_AUTH_ID":     true,
	"APP_AUTH_SECRET": true,
}

func buildProdEnvironmentVariables(defaultEnvironmentVariables []string, payloadEnvironmentVariables map[string]interface{}) []string {
	payloadVars := common.EnvironmentVarsToStringArray(payloadEnvironmentVariables)
	filtered := make([]string, 0, len(payloadVars))
	for _, envVar := range payloadVars {
		name := strings.SplitN(envVar, "=", 2)[0]
		if reservedEnvNames[name] {
			log.Warn().Str("name", name).Msg("ignoring app-supplied override of a reserved environment variable")
			continue
		}
		filtered = append(filtered, envVar)
	}
	return append(defaultEnvironmentVariables, filtered...)
}

const maxEnvVarSize = 20 * 1024 // 20KB - reasonable maximum for environment variables

func writeEnvironmentVariablesToFiles(appSpecificDirectory string, envVars []string) error {
	envDir := appSpecificDirectory + "/env"
	err := ensureEnvDir(envDir)
	if err != nil {
		return err
	}

	for _, envVar := range envVars {
		parts := strings.SplitN(envVar, "=", 2)
		if len(parts) != 2 {
			continue
		}
		varName := parts[0]
		varValue := parts[1]

		err := writeEnvValue(envDir, varName, varValue)
		if err != nil {
			return fmt.Errorf("failed to write env var %s to file: %w", varName, err)
		}
	}

	return nil
}

// filterLargeEnvVars returns only env vars that are below the size threshold
// Large env vars should only be accessed via files in /data/env/
func filterLargeEnvVars(envVars []string) []string {
	filtered := make([]string, 0, len(envVars))
	for _, envVar := range envVars {
		if len(envVar) <= maxEnvVarSize {
			filtered = append(filtered, envVar)
		}
	}
	return filtered
}

// hostDBusDir holds the host's D-Bus system bus socket; a variable so tests
// can point it at a temp dir.
var hostDBusDir = "/run/dbus"

// appDataDirMode lets the app write its /data whatever user its container runs
// as: the agent cannot know that user (an image's USER, a compose service's
// user:, a remapped user namespace), and a root-owned 0755 dir would leave a
// non-root container with a read-only /data. Other host users are kept out by
// the private stage dirs (filesystem.InitDirectories). Only /data itself is
// opened: what the app keeps in it is the app's, with the modes it chose.
const appDataDirMode os.FileMode = 0777

func computeMounts(stage common.Stage, appName string, config *config.Config) ([]mount.Mount, error) {
	appSpecificDirectory := config.CommandLineArguments.AppsDirectory + "/" + strings.ToLower(string(stage)) + "/" + strings.ToLower(appName)

	err := os.MkdirAll(appSpecificDirectory, appDataDirMode)
	if err != nil {
		if !os.IsExist(err) {
			return nil, err
		}
	}
	// MkdirAll applies the umask and leaves an existing dir alone: older
	// agents created /data 0755.
	if err := os.Chmod(appSpecificDirectory, appDataDirMode); err != nil {
		return nil, err
	}

	mounts := []mount.Mount{
		{
			Type:     mount.TypeBind,
			Source:   config.CommandLineArguments.AppsSharedDir,
			Target:   "/shared",
			ReadOnly: false,
		},
		{
			Type:     mount.TypeBind,
			Source:   appSpecificDirectory,
			Target:   "/data",
			ReadOnly: false,
		},
	}

	if _, err := os.Stat("/sys/bus/w1/devices"); !os.IsNotExist(err) {
		mounts = append(mounts, mount.Mount{
			Type:     mount.TypeBind,
			Source:   "/sys/bus/w1/devices",
			Target:   "/sys/bus/w1/devices",
			ReadOnly: false,
		})
	}

	// Trust: the device's own CA set, so an app on a corporate-cert appliance
	// can verify the endpoints it is told to connect to (see reagent/trust).
	if caDir := appCABundleHostDir(config); caDir != "" {
		mounts = append(mounts, mount.Mount{
			Type:     mount.TypeBind,
			Source:   caDir,
			Target:   trust.ContainerDir,
			ReadOnly: true,
		})
	}

	// The host's D-Bus system bus, so apps can drive host services such as
	// BlueZ (bleak, bluetoothctl). Apps run on the bridge network, where the
	// kernel refuses AF_BLUETOOTH sockets outside the initial network
	// namespace: going through the host's bluetoothd is the only Bluetooth
	// path left. The directory, not the socket, is bound so a dbus restart
	// (new socket inode) does not leave the container with a stale file.
	if _, err := os.Stat(hostDBusDir); err == nil {
		mounts = append(mounts, mount.Mount{
			Type:   mount.TypeBind,
			Source: hostDBusDir,
			Target: "/run/dbus",
		})
	}

	// for nvidia
	if _, err := os.Stat("/usr/local/cuda"); !os.IsNotExist(err) {
		mounts = append(mounts, mount.Mount{
			Type:     mount.TypeBind,
			Source:   "/usr/local/cuda",
			Target:   "/usr/local/cuda",
			ReadOnly: false,
		})
	}

	return mounts, nil
}

// appCredKey is the per-device HMAC key from which this app's per-app WAMP
// credential is derived; "" (not fetched yet) simply omits APP_AUTH_* and the
// SDK falls back to the legacy device-wide credential.
//
// NOTE: DEVICE_SECRET is deliberately NOT injected. It is the device's realm1
// credential, and realm1's swarm_device role is allow-all — so every app
// container could act as its own device (read any device's app configuration,
// publish as the device). Nothing in the platform or either SDK ever read it
// from a container; apps that did must migrate to APP_AUTH_*.
func buildDefaultEnvironmentVariables(config *config.Config, payload common.TransitionPayload, environment common.Stage, app *common.App, appCredKey string) []string {
	environmentVariables := []string{
		fmt.Sprintf("DEVICE_SERIAL_NUMBER=%s", config.ReswarmConfig.SerialNumber),
		fmt.Sprintf("ENV=%s", environment),
		fmt.Sprintf("DEVICE_KEY=%d", config.ReswarmConfig.DeviceKey),
		fmt.Sprintf("SWARM_KEY=%d", config.ReswarmConfig.SwarmKey),
		fmt.Sprintf("APP_KEY=%d", app.AppKey),
		fmt.Sprintf("APP_NAME=%s", app.AppName),
		fmt.Sprintf("DEVICE_NAME=%s", config.ReswarmConfig.Name),
		// Rewritten for the container's vantage point: a loopback endpoint
		// (local dev) is unreachable from a bridge network.
		fmt.Sprintf("DEVICE_ENDPOINT_URL=%s", appEndpointURL(config.ReswarmConfig.DeviceEndpointURL)),
		// The domain direct device tunnels are published under. Lets SDKs
		// (getRemoteAccessUrlForPort) compose a port's public URL without
		// replicating server-side logic.
		fmt.Sprintf("TUNNEL_DOMAIN=%s", tunnelDomainForApps(config)),
	}

	// Computed at container start: an IP change is reflected on the next app
	// restart. LAN deployments are expected to use static IPs or DHCP
	// reservations.
	lanIP := reagentnetwork.GetPrimaryLANIP()
	if lanIP != "" {
		environmentVariables = append(environmentVariables, fmt.Sprintf("DEVICE_LAN_IP=%s", lanIP))
	}

	// Point the common runtimes at the CA bundle mounted above. Without it an
	// app on a corporate-cert appliance cannot complete a TLS handshake to
	// wss://ws.<appliance domain> and reconnects forever.
	environmentVariables = append(environmentVariables, caBundleEnvironmentVariables(config)...)

	// Hand the device's own corporate proxy down to the app. Without it an app
	// on a proxied site has no outbound route at all, and every app author has
	// to rediscover the site's proxy and wire it up per app. These are
	// defaults an app can override — see proxyEnvironmentVariables.
	environmentVariables = append(environmentVariables, proxyEnvironmentVariables(config, lanIP)...)

	// Per-app WAMP identity: lets the platform authorize THIS app rather than
	// only its device, which is what makes the app-data-access consent switch
	// binding for co-located apps. Omitted (not blanked) when the key is not
	// available, so the SDK's legacy fallback keeps the app connected.
	if authID, secret := appCredential(config, appCredKey, payload); authID != "" && secret != "" {
		environmentVariables = append(environmentVariables,
			fmt.Sprintf("APP_AUTH_ID=%s", authID),
			fmt.Sprintf("APP_AUTH_SECRET=%s", secret),
		)
	}

	if payload.InstanceKey > 0 {
		// Instance devices: apps compose the cloud-forwarded route
		// https://i<INSTANCE_KEY>-<deviceKey>-<appName>-<port>.<cloud edge>.
		environmentVariables = append(environmentVariables, fmt.Sprintf("INSTANCE_KEY=%d", payload.InstanceKey))
	}

	if config.ReswarmConfig.ReswarmBaseURL != "" {
		deviceURL := fmt.Sprintf("%s/%s/swarms/%s/settings/devices/%s", config.ReswarmConfig.ReswarmBaseURL, config.ReswarmConfig.SwarmOwnerName, config.ReswarmConfig.SwarmName, config.ReswarmConfig.Name)
		environmentVariables = append(environmentVariables, fmt.Sprintf("RESWARM_URL=%s", config.ReswarmConfig.ReswarmBaseURL))
		environmentVariables = append(environmentVariables, fmt.Sprintf("DEVICE_URL=%s", deviceURL))
	}

	return environmentVariables
}

// remotePortEnvVars builds the env vars announcing the public side of an
// app's tunneled ports. Every port rule gets the canonical
// REMOTE_PORT_FOR_<port> name (with a _CLOUD companion for the
// internet-facing cloud port an instance backend patched into the sync
// payload); a rule's custom remote_port_environment name is emitted alongside
// for backward compatibility.
//
// A development run gets none: its only tunnels are http(s) ones, which have
// no remote port, and the PROD install's tunnel of the same port is not its
// own.
func (sm *StateMachine) remotePortEnvVars(config *config.Config, appName string, stage common.Stage, portRules []common.PortForwardRule) []string {
	if stage == common.DEV {
		return nil
	}
	var envs []string
	for _, portRule := range portRules {
		names := []string{fmt.Sprintf("REMOTE_PORT_FOR_%d", portRule.Port)}
		if portRule.RemotePortEnvironment != "" {
			names = append(names, portRule.RemotePortEnvironment)
		}

		subdomain := tunnel.CreateSubdomain(tunnel.Protocol(portRule.Protocol), uint64(config.ReswarmConfig.DeviceKey), appName, portRule.Port, stage)
		tunnelID := tunnel.CreateTunnelID(subdomain, portRule.Protocol)
		activeTunnel := sm.StateObserver.AppManager.tunnelManager.Get(tunnelID)

		for _, name := range names {
			if activeTunnel != nil {
				envs = append(envs, fmt.Sprintf("%s=%d", name, activeTunnel.Config.RemotePort))
			}

			// Payload-borne — no local tunnel object needed.
			if portRule.CloudRemotePort > 0 {
				envs = append(envs, fmt.Sprintf("%s_CLOUD=%d", name, portRule.CloudRemotePort))
			}
		}
	}
	return envs
}

func (sm *StateMachine) computeContainerConfigs(payload common.TransitionPayload, app *common.App) (*dockercontainer.Config, *dockercontainer.HostConfig, error) {
	config := sm.Container.GetConfig()
	systemDefaultVariables := buildDefaultEnvironmentVariables(config, payload, app.Stage, app, sm.AppCredKey())
	environmentVariables := buildProdEnvironmentVariables(systemDefaultVariables, payload.EnvironmentVariables)
	environmentTemplateDefaults := common.EnvironmentTemplateToStringArray(payload.EnvironmentTemplate)

	// Get app-specific directory for writing env vars
	appSpecificDirectory := config.CommandLineArguments.AppsDirectory + "/" + strings.ToLower(string(app.Stage)) + "/" + strings.ToLower(app.AppName)

	portRules, err := tunnel.InterfaceToPortForwardRule(payload.Ports)
	if err != nil {
		return nil, nil, err
	}

	containerName := payload.ContainerName.Prod
	if app.Stage == common.DEV {
		containerName = payload.ContainerName.Dev
	}

	// Apps run on the default bridge network with their declared ports
	// published on agent-managed host ports. Publishing (instead of the
	// former NetworkMode "host") is what keeps concurrent apps from fighting
	// over the same host port: every container may listen on its declared
	// port privately, and the agent picks a collision-free host port.
	// Computed before the environment variables so the resulting host ports
	// can be announced to the app as DEVICE_PORT_FOR_<port>.
	exposedPorts, portBindings, err := sm.computePortBindings(payload, portRules, containerName)
	if err != nil {
		return nil, nil, err
	}
	devicePortEnvs := devicePortEnvsFromBindings(portBindings)

	var containerConfig dockercontainer.Config

	if app.Stage == common.DEV {
		// Write all environment variables to files
		allEnvVars := append(systemDefaultVariables, environmentTemplateDefaults...)
		allEnvVars = append(allEnvVars, devicePortEnvs...)
		err := writeEnvironmentVariablesToFiles(appSpecificDirectory, allEnvVars)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to write environment variables to files: %w", err)
		}

		// Only pass small env vars to container, large ones are available in /data/env/
		containerEnvVars := filterLargeEnvVars(allEnvVars)

		containerConfig = dockercontainer.Config{
			Image:        payload.RegistryImageName.Dev,
			Env:          containerEnvVars,
			Labels:       map[string]string{"real": "True"},
			Volumes:      map[string]struct{}{},
			AttachStdin:  true,
			AttachStdout: true,
			AttachStderr: true,
			OpenStdin:    true,
			Tty:          true,
		}
	} else {

		fullImageNameWithTag := fmt.Sprintf("%s:%s", payload.RegistryImageName.Prod, app.Version)
		getImageContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
		defer cancel()

		_, err := sm.Container.GetImage(getImageContext, payload.RegistryImageName.Prod, payload.PresentVersion)
		if err != nil {
			if errdefs.IsImageNotFound(err) {
				log.Error().Msgf("Image %s:%s was not found, pulling......", payload.RegistryImageName.Prod, payload.PresentVersion)
				pullErr := sm.pullApp(payload, app)
				if pullErr != nil {
					return nil, nil, pullErr
				}
			}
		}

		var missingDefaultEnvs []string
		for _, templateEnvString := range environmentTemplateDefaults {
			envStringSplit := strings.Split(templateEnvString, "=")
			environmentName := envStringSplit[0]

			found := false
			for _, envVariableString := range environmentVariables {
				if strings.Contains(envVariableString, environmentName) {
					found = true
				}
			}

			if !found {
				missingDefaultEnvs = append(missingDefaultEnvs, templateEnvString)
			}
		}

		remotePortEnvs := sm.remotePortEnvVars(config, app.AppName, app.Stage, portRules)

		// Write all environment variables to files
		allEnvVars := append(environmentVariables, append(remotePortEnvs, missingDefaultEnvs...)...)
		allEnvVars = append(allEnvVars, devicePortEnvs...)
		err = writeEnvironmentVariablesToFiles(appSpecificDirectory, allEnvVars)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to write environment variables to files: %w", err)
		}

		// Only pass small env vars to container, large ones are available in /data/env/
		containerEnvVars := filterLargeEnvVars(allEnvVars)

		containerConfig = dockercontainer.Config{
			Image:  fullImageNameWithTag,
			Env:    containerEnvVars,
			Labels: map[string]string{"real": "True"},
			Tty:    true,
		}
	}

	mounts, err := computeMounts(app.Stage, app.AppName, config)
	if err != nil {
		return nil, nil, err
	}

	containerConfig.ExposedPorts = exposedPorts

	hostConfig := dockercontainer.HostConfig{
		// CapDrop: []string{"NET_ADMIN"},
		RestartPolicy: dockercontainer.RestartPolicy{
			Name: "no",
		},
		Mounts: mounts,
		Resources: dockercontainer.Resources{
			Devices: []dockercontainer.DeviceMapping{
				{
					PathOnHost:      "/dev",
					PathInContainer: "/dev",
				},
			},
		},
		Privileged:   true,
		NetworkMode:  "bridge",
		PortBindings: portBindings,
		ExtraHosts:   []string{hostGatewayEntry},
		CapAdd:       []string{"ALL"},
	}

	if system.HasNvidiaGPU() {
		log.Debug().Msgf("Detected a NVIDIA GPU, will request NVIDIA Device capabilities...")
		hostConfig.Runtime = "nvidia"
		// hostConfig.DeviceRequests = []dockercontainer.DeviceRequest{
		// 	{
		// 		Driver: "nvidia",
		// 		Count:  -1,
		// 		Capabilities: [][]string{
		// 			{
		// 				"compute", "compat32", "graphics", "utility", "video", "display",
		// 			},
		// 		},
		// 	},
		// }
	}

	return &containerConfig, &hostConfig, nil
}

func (sm *StateMachine) createContainer(payload common.TransitionPayload, app *common.App, cConfig *dockercontainer.Config, hConfig *dockercontainer.HostConfig) (string, error) {
	var containerID string
	var containerName string

	if app.Stage == common.PROD {
		containerName = payload.ContainerName.Prod
	} else {
		containerName = payload.ContainerName.Dev
	}

	getContainerContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	cont, err := sm.Container.GetContainer(getContainerContext, containerName)
	if err == nil && sm.containerNetworkConfigOutdated(cont, hConfig, containerName) {
		// Network mode and port bindings are immutable on an existing
		// container: recreate to apply them (migrates pre-managed-port
		// containers off host networking, and picks up reassigned ports).
		log.Info().Str("container", containerName).Msg("Recreating container to apply updated network/port configuration")

		removeContainerContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
		defer cancel()
		err = sm.Container.RemoveContainerByID(removeContainerContext, cont.ID, map[string]interface{}{"force": true})
		if err != nil && !errdefs.IsContainerNotFound(err) {
			return "", err
		}

		waitForRemovalContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
		defer cancel()
		_, err = sm.Container.WaitForContainerByID(waitForRemovalContext, cont.ID, dockercontainer.WaitConditionRemoved)
		if err != nil && !errdefs.IsContainerNotFound(err) {
			return "", err
		}

		err = errdefs.ContainerNotFound(errors.New("container was recreated"))
	}
	if err != nil {
		if !errdefs.IsContainerNotFound(err) {
			return "", err
		}

		// A container removed after the agent folder moved gets the volumes
		// of the one before (recreate_marker.go).
		sm.carryRecordedVolumes(containerName, hConfig)
		defer func() {
			if containerID != "" {
				sm.markRecreated(containerName)
			}
		}()

		createContainerContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
		defer cancel()
		containerID, err = sm.Container.CreateContainer(createContainerContext, *cConfig, *hConfig, network.NetworkingConfig{}, containerName)
		if err != nil {
			if errdefs.IsImageNotFound(err) && app.Stage == common.DEV {
				imageNotFoundMessage := "The image " + payload.RegistryImageName.Dev + " was not found on the device, try building the app again..."
				_ = sm.LogManager.Write(containerName, imageNotFoundMessage)
			}
			if strings.Contains(err.Error(), "nvidia") {
				log.Debug().Msgf("Failed to launch container with NVIDIA Capabilities, retrying without... %s \n", err.Error())

				removeContainerByIdContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
				defer cancel()
				_ = sm.Container.RemoveContainerByID(removeContainerByIdContext, containerID, map[string]interface{}{"force": true})

				// remove NVIDIA host configuration
				hConfig.Runtime = ""
				hConfig.DeviceRequests = []dockercontainer.DeviceRequest{}

				createContainerContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
				defer cancel()
				containerID, err = sm.Container.CreateContainer(createContainerContext, *cConfig, *hConfig, network.NetworkingConfig{}, containerName)
				if err != nil {
					return "", err
				}
			} else {
				return "", err
			}
		}
	} else {
		containerID = cont.ID
		// A container that exists again (created by an earlier start that
		// was cut before it recorded so) has served the recreate marker too.
		sm.markRecreated(containerName)
	}

	return containerID, nil
}

func (sm *StateMachine) startContainer(payload common.TransitionPayload, app *common.App) (string, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		containerID, err := sm.startContainerOnce(payload, app)
		if err == nil {
			return containerID, nil
		}
		lastErr = err

		// The registry probes ports before handing them out, but a process
		// outside the agent can still grab one between probe and bind. Pick
		// fresh ports and recreate; createContainer notices the binding
		// mismatch and recreates the container.
		if !isPortAllocationError(err) || !sm.StateObserver.AppManager.reassignPortsAfterBindConflict(payload, err) {
			return "", err
		}
		log.Warn().Str("app", payload.AppName).Msg("Retrying container start with freshly assigned host ports")
	}
	return "", lastErr
}

func (sm *StateMachine) startContainerOnce(payload common.TransitionPayload, app *common.App) (string, error) {
	containerConfig, hostConfig, err := sm.computeContainerConfigs(payload, app)
	if err != nil {
		return "", err
	}

	containerID, err := sm.createContainer(payload, app, containerConfig, hostConfig)
	if err != nil {
		return "", err
	}

	startContainerContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	err = sm.Container.StartContainer(startContainerContext, containerID)
	if err != nil {
		if strings.Contains(err.Error(), "nvidia") {
			log.Debug().Msgf("Failed to launch container with NVIDIA Capabilities, retrying without... %s \n", err.Error())

			removeContainerByIdContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
			defer cancel()
			_ = sm.Container.RemoveContainerByID(removeContainerByIdContext, containerID, map[string]interface{}{"force": true})

			// remove nvidia device request
			hostConfig.DeviceRequests = []dockercontainer.DeviceRequest{}

			containerID, err = sm.createContainer(payload, app, containerConfig, hostConfig)
			if err != nil {
				return "", err
			}

			startContainerContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
			defer cancel()

			err = sm.Container.StartContainer(startContainerContext, containerID)
			if err != nil {
				return "", err
			}

		} else if isPortAllocationError(err) {
			// Surface host port conflicts so startContainer can reassign
			// ports and retry.
			return "", err
		}
	}

	return containerID, nil
}
