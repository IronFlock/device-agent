package apps

import (
	"context"
	"reagent/common"
	"time"
)

func (sm *StateMachine) recoverFailToPresentHandler(payload common.TransitionPayload, app *common.App) error {
	if payload.Stage == common.PROD && payload.DockerCompose != nil {
		return sm.recoverFailedComposeToPresent(payload, app)
	}

	var containerToRemove string
	if payload.Stage == common.DEV {
		containerToRemove = payload.ContainerName.Dev
	} else {
		containerToRemove = payload.ContainerName.Prod
	}

	removeContainerByIdContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	// remove any existing container to ensure environment variables are set
	_ = sm.Container.RemoveContainerByID(removeContainerByIdContext, containerToRemove, map[string]interface{}{"force": true})

	if payload.Stage == common.DEV {
		return sm.buildApp(payload, app)
	}

	// Check if the image exists
	var fullImageName string
	if payload.Stage == common.DEV {
		fullImageName = payload.RegistryImageName.Dev
	} else if payload.Stage == common.PROD {
		fullImageName = payload.RegistryImageName.Prod
	}

	getImagesContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	images, err := sm.Container.GetImages(getImagesContext, fullImageName)
	if err != nil {
		return err
	}

	if len(images) != 0 {
		return sm.setState(app, common.PRESENT)
	}

	return sm.pullApp(payload, app)
}

// recoverFailedComposeToPresent settles a FAILED PROD compose app at PRESENT.
// The generic handler's image check compares against the single-container
// registry image name, which never matches a compose app's service images —
// so every stop of a failed compose app re-entered DOWNLOADING and re-pulled
// images that were already on the device. Check the actual service images
// instead: when they are all present, tear the failed project's containers
// down and settle at PRESENT; only genuinely missing images go through the
// pull (DOWNLOADING) path.
func (sm *StateMachine) recoverFailedComposeToPresent(payload common.TransitionPayload, app *common.App) error {
	getImagesContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
	images, err := sm.Container.GetImages(getImagesContext, "")
	cancel()

	if err == nil {
		missing, missingErr := composeServicesWithMissingImages(payload.DockerCompose, images)
		if missingErr == nil && len(missing) == 0 {
			dockerComposePath, setupErr := sm.SetupComposeFiles(payload, app, false)
			if setupErr != nil {
				return setupErr
			}

			teardownErr := sm.teardownComposeProject(payload, app, dockerComposePath)
			if teardownErr != nil {
				return teardownErr
			}

			return sm.setState(app, common.PRESENT)
		}
	}

	return sm.pullComposeApp(payload, app)
}
