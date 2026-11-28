package apps

import (
	"context"
	"os"
	"reagent/common"
	"reagent/errdefs"
	"time"

	"github.com/rs/zerolog/log"
)

// A release can switch the flow its app runs under: adding a docker-compose.yml
// turns a single-container ("legacy") app into a compose project, and removing
// it turns the project back into a single container. The update transition then
// has to install the new release AND dismantle the predecessor the other flow
// left behind — nothing else in the lifecycle knows about the old shape once
// the payload's flow marker has been promoted (persistPostUpdateRequestedState).
//
// The helpers below are that dismantling half. They run only after the new
// release's images are local, so a failure to reach the registry still leaves
// the old version running.

// composeInstallFilePath is the rendered compose file that currently manages
// the app, and whether it is on disk.
//
// Deliberately NOT re-rendered through SetupComposeFiles: that writes the app's
// env-file mirror from the payload, and the payload here describes the release
// that is replacing this project. The file left by the install/start is the one
// the running containers were created from, which is exactly what a teardown
// has to address.
func (sm *StateMachine) composeInstallFilePath(payload common.TransitionPayload, app *common.App) (string, bool) {
	config := sm.Container.GetConfig()

	dir := config.CommandLineArguments.AppsComposeDir
	if payload.Stage == common.DEV {
		dir = config.CommandLineArguments.AppsBuildDir
	}

	path := dir + "/" + app.AppName + "/" + DockerFileName
	if _, err := os.Stat(path); err != nil {
		return "", false
	}

	return path, true
}

// tearDownComposeInstall destroys the compose project the app currently runs
// as, so a single-container release can take over its name, ports and data.
// Volumes are preserved (no `-v`) — this is an update, not an uninstall, and
// the new version inherits the app's data.
func (sm *StateMachine) tearDownComposeInstall(payload common.TransitionPayload, app *common.App) error {
	config := sm.Container.GetConfig()
	compose := sm.Container.Compose()
	if !compose.Supported() {
		// Without the CLI the project cannot have been started by this agent,
		// and there is nothing this teardown could reach. Not an error: the
		// release being installed does not need compose at all.
		return nil
	}

	dockerComposePath, ok := sm.composeInstallFilePath(payload, app)
	if !ok {
		return nil
	}

	// Cancelable like every other compose command in an update: a cancel
	// request must be able to kill a hung `compose down` instead of holding
	// the transition lock forever.
	ctx, cancel := context.WithCancel(context.Background())
	sm.registerComposeTransitionCancel(payload.Stage, payload.AppKey, cancel)
	defer func() {
		sm.clearComposeTransitionCancel(payload.Stage, payload.AppKey)
		cancel()
	}()

	err := compose.DownRemoveOrphansContext(ctx, dockerComposePath)
	if err != nil {
		return composeTransitionErr(ctx, err)
	}

	// Image cleanup is BEST-EFFORT, same contract as removeComposeApp: the
	// containers are already gone, the app is functionally migrated, and no
	// leftover image may fail an update whose new version is installed.
	images, err := compose.ListImages(payload.DockerCompose)
	if err != nil {
		log.Warn().Err(err).Str("app", payload.AppName).Msg("Could not list the superseded compose images; leaving them for a later prune")
		return nil
	}

	for _, imageName := range images {
		removeImageContext, cancelRemove := context.WithTimeout(context.Background(), time.Second*30)
		err := sm.Container.RemoveImage(removeImageContext, imageName, map[string]interface{}{"force": true})
		cancelRemove()
		if err != nil && !errdefs.IsImageNotFound(err) {
			log.Warn().Err(err).Str("app", payload.AppName).Str("image", imageName).Msg("Could not remove a superseded compose image; leaving it for a later prune")
		}
	}

	// Drop the project directory too, as uninstallApp does. Left behind it is
	// mostly inert — nothing routes on it once the flow marker is promoted —
	// but diskguard reads these directories as "this project still exists" and
	// would protect the dead project's named volumes from reclamation for good.
	//
	// PROD only: the DEV compose directory under AppsBuildDir is also the
	// extracted build context, and DEV apps never update (updateApp refuses
	// them) — this is belt and braces for a future caller.
	if payload.Stage == common.PROD {
		err = os.RemoveAll(config.CommandLineArguments.AppsComposeDir + "/" + app.AppName)
		if err != nil {
			log.Warn().Err(err).Str("app", payload.AppName).Msg("Could not remove the superseded compose project directory")
		}
	}

	return nil
}

// removeLegacyProdContainer removes the app's single PROD container and waits
// for docker to actually be rid of it. Shared by the plain legacy update and
// the legacy -> compose migration; a missing container is success.
func (sm *StateMachine) removeLegacyProdContainer(payload common.TransitionPayload) error {
	getContainerContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	cont, err := sm.Container.GetContainer(getContainerContext, payload.ContainerName.Prod)
	if err != nil {
		return nil
	}

	removeContainerByIdContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	err = sm.Container.RemoveContainerByID(removeContainerByIdContext, cont.ID, map[string]interface{}{"force": true})
	if err != nil {
		return err
	}

	pollContainerStateContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	// should return 'container not found' error, this way we know it's removed successfully
	_, errC := sm.Container.PollContainerState(pollContainerStateContext, cont.ID, time.Second)
	err = <-errC
	if !errdefs.IsContainerNotFound(err) {
		return err
	}

	return nil
}

// removeLegacyProdInstall removes the single-container install a compose
// release is replacing: the container, and then every tag of its image — the
// app is leaving the legacy flow entirely, so no version of that image will be
// run again. The image half is best-effort for the same reason as above.
func (sm *StateMachine) removeLegacyProdInstall(payload common.TransitionPayload) error {
	err := sm.removeLegacyProdContainer(payload)
	if err != nil {
		return err
	}

	err = sm.removeProdImageRepositories(payload, map[string]interface{}{"force": true})
	if err != nil && !errdefs.IsImageNotFound(err) {
		log.Warn().Err(err).Str("app", payload.AppName).Str("image", payload.RegistryImageName.Prod).Msg("Could not remove the superseded legacy image; leaving it for a later prune")
	}

	return nil
}

// releaseSupersededHostPorts drops the host-port reservations the app held
// under the flow it is leaving. The two flows key reservations differently —
// compose entries carry the service name, single-container ones do not — so
// after a migration the old keys can never be recovered again and would hold
// their pool ports until the agent restarts.
//
// Only the departing flow is released, never the whole app: the compose path
// renders (and reserves) the new project's ports before the old install is torn
// down, and a blanket release would hand back ports the new compose file
// already names. The incoming flow reserves the rest at launch, and
// syncPortState replaces any tunnel whose local port moved as a result.
func (sm *StateMachine) releaseSupersededHostPorts(payload common.TransitionPayload, composeFlow bool) {
	if am := sm.StateObserver.AppManager; am != nil {
		am.hostPorts.ReleaseAppFlow(payload.Stage, payload.AppKey, composeFlow)
	}
}
