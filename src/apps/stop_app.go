package apps

import (
	"context"
	"fmt"
	"reagent/common"
	"reagent/errdefs"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/pkg/errors"
)

func (sm *StateMachine) stopApp(payload common.TransitionPayload, app *common.App) error {
	if payload.Stage == common.DEV {
		return sm.stopDevApp(payload, app)
	} else if payload.Stage == common.PROD {
		return sm.stopProdApp(payload, app)
	}
	return nil
}

func (sm *StateMachine) stopProdApp(payload common.TransitionPayload, app *common.App) error {
	if payload.DockerCompose != nil {
		return sm.stopProdComposeApp(payload, app)
	}

	err := sm.setState(app, common.STOPPING)
	if err != nil {
		return err
	}

	getContainerContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	// for now to resolve the issue regarding env variables, we should remove the container on stop
	cont, err := sm.Container.GetContainer(getContainerContext, payload.ContainerName.Prod)
	if err != nil {
		if !errdefs.IsContainerNotFound(err) {
			return errors.Wrap(err, "failed to getContainer during stopDevApp")
		}
	}

	err = sm.LogManager.Write(payload.ContainerName.Prod, fmt.Sprintf("Received stop signal for %s", payload.AppName))
	if err != nil {
		return err
	}

	err = sm.setState(app, common.STOPPING)
	if err != nil {
		return err
	}

	stopContainerContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	err = sm.Container.StopContainerByID(stopContainerContext, cont.ID, time.Second*1)
	if err != nil {
		return errors.Wrap(err, "failed to stop container by ID during stopProdApp")
	}

	waitForContainerByIDContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	_, err = sm.Container.WaitForContainerByID(waitForContainerByIDContext, cont.ID, container.WaitConditionNotRunning)
	if err != nil {
		return err
	}

	removeContainerByIDContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	err = sm.Container.RemoveContainerByID(removeContainerByIDContext, cont.ID, map[string]interface{}{"force": true})
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

	err = sm.setState(app, common.PRESENT)
	if err != nil {
		return err
	}

	return sm.LogManager.Write(payload.ContainerName.Prod, fmt.Sprintf("Successfully stopped %s", payload.AppName))
}

func (sm *StateMachine) stopDevComposeApp(payload common.TransitionPayload, app *common.App) error {
	dockerComposePath, err := sm.SetupComposeFiles(payload, app, false)
	if err != nil {
		return err
	}

	err = sm.LogManager.Write(payload.ContainerName.Dev, fmt.Sprintf("Received stop signal for %s", payload.AppName))
	if err != nil {
		return err
	}

	err = sm.setState(app, common.STOPPING)
	if err != nil {
		return err
	}

	err = sm.teardownComposeProject(payload, app, dockerComposePath)
	if err != nil {
		return err
	}

	err = sm.setState(app, common.PRESENT)
	if err != nil {
		return err
	}

	return sm.LogManager.Write(payload.ContainerName.Dev, fmt.Sprintf("Successfully stopped %s", payload.AppName))
}

func (sm *StateMachine) stopProdComposeApp(payload common.TransitionPayload, app *common.App) error {
	dockerComposePath, err := sm.SetupComposeFiles(payload, app, false)
	if err != nil {
		return err
	}

	err = sm.LogManager.Write(payload.ContainerName.Prod, fmt.Sprintf("Received stop signal for %s", payload.AppName))
	if err != nil {
		return err
	}

	err = sm.setState(app, common.STOPPING)
	if err != nil {
		return err
	}

	err = sm.teardownComposeProject(payload, app, dockerComposePath)
	if err != nil {
		return err
	}

	err = sm.setState(app, common.PRESENT)
	if err != nil {
		return err
	}

	return sm.LogManager.Write(payload.ContainerName.Prod, fmt.Sprintf("Successfully stopped %s", payload.AppName))
}

// dropDevTunnels removes a development run's tunnels: they live only while
// its container runs (see syncPortState), and a stop or a removal ends that.
// Unconditional, also when the stop or removal fails half-way: a container
// that is in fact still running is seen by its state observer, which brings
// the tunnels back (StateObserver.followDevRun).
func (sm *StateMachine) dropDevTunnels(payload common.TransitionPayload) {
	if payload.Stage != common.DEV || sm.StateObserver == nil || sm.StateObserver.AppManager == nil {
		return
	}
	sm.StateObserver.AppManager.RemoveAppTunnels(payload.AppName, common.DEV)
}

func (sm *StateMachine) stopDevApp(payload common.TransitionPayload, app *common.App) error {
	defer sm.dropDevTunnels(payload)

	if payload.DockerCompose != nil {
		return sm.stopDevComposeApp(payload, app)
	}

	getContainerContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	cont, err := sm.Container.GetContainer(getContainerContext, payload.ContainerName.Dev)
	if err != nil {
		if !errdefs.IsContainerNotFound(err) {
			return errors.Wrap(err, "failed to getContainer during stopDevApp")
		}
	}

	err = sm.LogManager.Write(payload.ContainerName.Dev, fmt.Sprintf("Received stop signal for %s", payload.AppName))
	if err != nil {
		return err
	}

	err = sm.setState(app, common.STOPPING)
	if err != nil {
		return err
	}

	stopContainerByIDrContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	err = sm.Container.StopContainerByID(stopContainerByIDrContext, cont.ID, time.Second*1)
	if err != nil {
		return errors.Wrap(err, "failed to stop container by ID during stopDevApp")
	}

	waitForContainerByIDContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	_, err = sm.Container.WaitForContainerByID(waitForContainerByIDContext, cont.ID, container.WaitConditionNotRunning)
	if err != nil {
		return err
	}

	removeContainerByIDContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	err = sm.Container.RemoveContainerByID(removeContainerByIDContext, cont.ID, map[string]interface{}{"force": true})
	if err != nil {
		return err
	}

	// should return 'container not found' error, this way we know it's removed successfully
	pollContainerStateContext, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	_, errC := sm.Container.PollContainerState(pollContainerStateContext, cont.ID, time.Second)
	err = <-errC
	if !errdefs.IsContainerNotFound(err) {
		return err
	}

	err = sm.setState(app, common.PRESENT)
	if err != nil {
		return err
	}

	return sm.LogManager.Write(payload.ContainerName.Dev, fmt.Sprintf("Successfully stopped %s", payload.AppName))
}
