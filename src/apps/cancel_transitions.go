package apps

import (
	"errors"
	"reagent/common"
)

// settleCanceledTransition finishes a canceled in-flight transition at the
// teardown the payload actually asked for: the full uninstall when the request
// was UNINSTALLED, the plain REMOVED settle otherwise. Merely publishing
// REMOVED for an UNINSTALLED request never converges — the state observer
// deletes both database rows the moment REMOVED is reached with an UNINSTALLED
// requested-state row, so no later re-drive can finish the job and the app's
// data directory, tunnels and host ports leak. Mirrors what cancelPull's
// compose branch and the cancelUpdateAnd* handlers already do.
func (sm *StateMachine) settleCanceledTransition(payload common.TransitionPayload, app *common.App) error {
	if payload.RequestedState == common.UNINSTALLED {
		return sm.uninstallApp(payload, app)
	}
	return sm.setState(app, common.REMOVED)
}

func (sm *StateMachine) cancelBuild(payload common.TransitionPayload, app *common.App) error {
	if payload.Stage == common.PROD {
		return errors.New("cannot build prod apps")
	}

	buildID := common.BuildDockerBuildID(app.AppKey, app.AppName)

	if payload.DockerCompose != nil {
		compose := sm.Container.Compose()
		_ = compose.CancelBuild(buildID) // ignore error — build may have already finished
		return sm.settleCanceledTransition(payload, app)
	}

	_ = sm.Container.CancelStream(buildID)

	return sm.settleCanceledTransition(payload, app)
}

func (sm *StateMachine) cancelPull(payload common.TransitionPayload, app *common.App) error {
	if payload.Stage != common.PROD {
		return errors.New("cannot pull dev apps")
	}

	if payload.DockerCompose != nil {
		// Compose pulls run via the docker compose CLI, not a docker-API
		// stream, so CancelStream can't reach them — cancel through the
		// transition's registered context instead, which kills the CLI. Unlike
		// a canceled docker-API pull there may already be compose files, env
		// mirrors and fully pulled service images on disk, so settle with a
		// real teardown matching the requested state (like the STARTING row
		// does) instead of only publishing REMOVED. Mirrors
		// cancelUpdateAndRemove/cancelUpdateAndUninstall for compose updates.
		sm.cancelComposeTransition(payload.Stage, payload.AppKey)

		if payload.RequestedState == common.UNINSTALLED {
			return sm.uninstallApp(payload, app)
		}
		return sm.removeApp(payload, app)
	}

	pullID := common.BuildDockerPullID(payload.AppKey, payload.AppName)

	_ = sm.Container.CancelStream(pullID)

	return sm.settleCanceledTransition(payload, app)
}

func (sm *StateMachine) cancelPush(payload common.TransitionPayload, app *common.App) error {
	pushID := common.BuildDockerPushID(payload.AppKey, payload.AppName)

	_ = sm.Container.CancelStream(pushID)

	return sm.settleCanceledTransition(payload, app)
}

// interruptActiveTransition cancels the cancelable work of whatever transition
// is in flight for this app so it unwinds and releases the transition lock.
// Compose transitions (pull/up/update) run via the docker compose CLI (not a
// docker-API stream), so CancelStream can't reach them — they are canceled
// through the transition's registered context, which kills the CLI. The
// single-container flow cancels the docker-API pull stream; DEV apps
// additionally get their in-flight build canceled. No-op if nothing is in
// flight, and a best effort otherwise: work outside these cancel points (a
// registry login, a compose up/down) finishes or times out on its own.
//
// BOTH mechanisms are fired rather than picking one by payload.DockerCompose.
// That marker describes the INSTALLED release, and an update that migrates the
// app between the flows runs the other flow's cancelable work: a legacy install
// updating to a compose release is inside `docker compose pull`, and a compose
// install updating to a single-container release is inside a docker-API pull.
// Choosing by the marker addressed the wrong one in exactly those two cases and
// the cancel was silently lost — the user's stop did nothing and the transition
// ran to completion. Each cancel is a no-op when its side has nothing
// registered, so firing both costs nothing.
func (sm *StateMachine) interruptActiveTransition(payload common.TransitionPayload) {
	sm.cancelComposeTransition(payload.Stage, payload.AppKey)

	pullID := common.BuildDockerPullID(payload.AppKey, payload.AppName)
	_ = sm.Container.CancelStream(pullID)

	if payload.Stage == common.DEV {
		buildID := common.BuildDockerBuildID(payload.AppKey, payload.AppName)
		_ = sm.Container.Compose().CancelBuild(buildID) // ignore error — no build may be in flight
		_ = sm.Container.CancelStream(buildID)
	}
}

func (sm *StateMachine) cancelUpdate(payload common.TransitionPayload, app *common.App) error {
	sm.interruptActiveTransition(payload)

	// let the backend know the update has been canceled
	app.UpdateStatus = common.CANCELED

	return sm.setState(app, common.PRESENT)
}

func (sm *StateMachine) cancelUpdateAndRemove(payload common.TransitionPayload, app *common.App) error {
	sm.interruptActiveTransition(payload)

	app.UpdateStatus = common.CANCELED

	return sm.removeApp(payload, app)
}

// cancelUpdateAndUninstall cancels an in-flight update and then uninstalls the
// app. It mirrors cancelUpdateAndRemove for the UPDATING -> UNINSTALLED path, so a
// teardown requested mid-update aborts the update first.
func (sm *StateMachine) cancelUpdateAndUninstall(payload common.TransitionPayload, app *common.App) error {
	sm.interruptActiveTransition(payload)

	app.UpdateStatus = common.CANCELED

	return sm.uninstallApp(payload, app)
}

func (sm *StateMachine) cancelTransfer(payload common.TransitionPayload, app *common.App) error {
	if payload.Stage != common.DEV {
		return errors.New("file transfer is only for dev apps")
	}

	sm.Filesystem.CancelFileTransfer(payload.ContainerName.Dev)

	return sm.settleCanceledTransition(payload, app)
}
