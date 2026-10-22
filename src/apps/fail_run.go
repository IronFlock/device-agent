package apps

import (
	"context"
	"reagent/common"
	"time"
)

func (sm *StateMachine) recoverFailToRunningHandler(payload common.TransitionPayload, app *common.App) error {

	var containerToRemove string
	if payload.Stage == common.DEV {
		containerToRemove = payload.ContainerName.Dev
	} else {
		containerToRemove = payload.ContainerName.Prod
	}

	if payload.DockerCompose == nil {
		removeContainerByIdContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
		defer cancel()
		// remove any existing container to ensure environment variables are set
		_ = sm.Container.RemoveContainerByID(removeContainerByIdContext, containerToRemove, map[string]interface{}{"force": true})
	}

	return sm.runApp(payload, app)
}
