package apps

import (
	"errors"
	"testing"

	"github.com/docker/docker/api/types/container"

	"reagent/common"
	"reagent/testutil/builders"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// stop_app.go — branches the existing TestStopApp did not hit
//
// The existing transition_execution_test.go covers the prod happy path. These
// add the DEV dispatch, the GetContainer non-not-found error, and the
// stop/wait failure branches for both stages.
// =============================================================================

func TestStopAppExtraBranches(t *testing.T) {
	t.Run("dev (non-compose): stops, removes and ends in PRESENT", func(t *testing.T) {
		sm, mc, st, msg := wiredStateMachine(t)

		app := seedApp(t, st, "stop-dev", common.RUNNING, common.DEV)
		payload := execPayload("stop-dev", common.PRESENT, common.DEV)

		const containerID = "stop-dev-cid"

		mc.EXPECT().
			GetContainer(mock.Anything, payload.ContainerName.Dev).
			Return(container.Summary{ID: containerID}, nil).
			Once()
		mc.EXPECT().
			StopContainerByID(mock.Anything, containerID, mock.Anything).
			Return(nil).
			Once()
		mc.EXPECT().
			WaitForContainerByID(mock.Anything, containerID, mock.Anything).
			Return(int64(0), nil).
			Once()
		mc.EXPECT().
			RemoveContainerByID(mock.Anything, containerID, mock.Anything).
			Return(nil).
			Once()

		errC := make(chan error, 1)
		errC <- notFoundErr()
		close(errC)
		mc.EXPECT().
			PollContainerState(mock.Anything, containerID, mock.Anything).
			Return(nil, errC).
			Once()

		err := sm.stopApp(payload, app)
		require.NoError(t, err)

		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.PRESENT, final)

		states := remoteStatesFor(msg)
		assert.Contains(t, states, common.STOPPING)
		assert.Contains(t, states, common.PRESENT)
	})

	t.Run("prod: a non-not-found GetContainer error is wrapped and returned", func(t *testing.T) {
		sm, mc, st, _ := wiredStateMachine(t)

		app := seedApp(t, st, "stop-geterr", common.RUNNING, common.PROD)
		payload := execPayload("stop-geterr", common.PRESENT, common.PROD)

		boom := errors.New("docker daemon down")
		mc.EXPECT().
			GetContainer(mock.Anything, payload.ContainerName.Prod).
			Return(container.Summary{}, boom).
			Once()

		err := sm.stopApp(payload, app)
		require.Error(t, err)
		assert.ErrorIs(t, err, boom)
	})

	t.Run("prod: a StopContainerByID error is wrapped and returned", func(t *testing.T) {
		sm, mc, st, _ := wiredStateMachine(t)

		app := seedApp(t, st, "stop-stoperr", common.RUNNING, common.PROD)
		payload := execPayload("stop-stoperr", common.PRESENT, common.PROD)

		const containerID = "stop-stoperr-cid"
		boom := errors.New("stop refused")

		mc.EXPECT().
			GetContainer(mock.Anything, payload.ContainerName.Prod).
			Return(container.Summary{ID: containerID}, nil).
			Once()
		mc.EXPECT().
			StopContainerByID(mock.Anything, containerID, mock.Anything).
			Return(boom).
			Once()

		err := sm.stopApp(payload, app)
		require.Error(t, err)
		assert.ErrorIs(t, err, boom)

		// Reached STOPPING but never PRESENT.
		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.STOPPING, final)
	})

	t.Run("dev: a WaitForContainerByID error is returned", func(t *testing.T) {
		sm, mc, st, _ := wiredStateMachine(t)

		app := seedApp(t, st, "stop-waiterr", common.RUNNING, common.DEV)
		payload := execPayload("stop-waiterr", common.PRESENT, common.DEV)

		const containerID = "stop-waiterr-cid"
		boom := errors.New("wait failed")

		mc.EXPECT().
			GetContainer(mock.Anything, payload.ContainerName.Dev).
			Return(container.Summary{ID: containerID}, nil).
			Once()
		mc.EXPECT().
			StopContainerByID(mock.Anything, containerID, mock.Anything).
			Return(nil).
			Once()
		mc.EXPECT().
			WaitForContainerByID(mock.Anything, containerID, mock.Anything).
			Return(int64(0), boom).
			Once()

		err := sm.stopApp(payload, app)
		require.Error(t, err)
		assert.ErrorIs(t, err, boom)
	})
}

// =============================================================================
// remove_app.go — branches the existing TestRemoveProdApp did not hit
//
// Existing coverage: prod no-container, prod running-container, prod
// image-removal failure. These add the DEV dispatch and the error branches
// inside the container-exists block.
// =============================================================================

func TestRemoveAppExtraBranches(t *testing.T) {
	t.Run("dev: removes a running container, its image, ends REMOVED", func(t *testing.T) {
		sm, mc, st, msg := wiredStateMachine(t)

		app := seedApp(t, st, "remove-dev", common.RUNNING, common.DEV)
		payload := execPayload("remove-dev", common.REMOVED, common.DEV)

		const containerID = "remove-dev-cid"

		mc.EXPECT().
			GetContainer(mock.Anything, payload.ContainerName.Dev).
			Return(container.Summary{ID: containerID}, nil).
			Once()
		mc.EXPECT().
			RemoveContainerByID(mock.Anything, containerID, mock.Anything).
			Return(nil).
			Once()
		mc.EXPECT().
			WaitForContainerByID(mock.Anything, containerID, mock.Anything).
			Return(int64(0), notFoundErr()).
			Once()
		mc.EXPECT().
			RemoveImagesByName(mock.Anything, payload.RegistryImageName.Dev, mock.Anything).
			Return(nil).
			Once()

		err := sm.removeApp(payload, app)
		require.NoError(t, err)

		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.REMOVED, final)

		states := remoteStatesFor(msg)
		assert.Contains(t, states, common.DELETING)
		assert.Contains(t, states, common.REMOVED)
	})

	t.Run("dev: no container present -> only image removal", func(t *testing.T) {
		sm, mc, st, _ := wiredStateMachine(t)

		app := seedApp(t, st, "remove-dev-noctr", common.PRESENT, common.DEV)
		payload := execPayload("remove-dev-noctr", common.REMOVED, common.DEV)

		mc.EXPECT().
			GetContainer(mock.Anything, payload.ContainerName.Dev).
			Return(container.Summary{}, notFoundErr()).
			Once()
		mc.EXPECT().
			RemoveImagesByName(mock.Anything, payload.RegistryImageName.Dev, mock.Anything).
			Return(nil).
			Once()

		err := sm.removeApp(payload, app)
		require.NoError(t, err)

		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.REMOVED, final)
	})

	t.Run("prod: a non-not-found RemoveContainerByID error aborts", func(t *testing.T) {
		sm, mc, st, _ := wiredStateMachine(t)

		app := seedApp(t, st, "remove-prod-rmerr", common.RUNNING, common.PROD)
		payload := execPayload("remove-prod-rmerr", common.REMOVED, common.PROD)

		const containerID = "remove-prod-rmerr-cid"
		boom := errors.New("remove refused")

		mc.EXPECT().
			GetContainer(mock.Anything, payload.ContainerName.Prod).
			Return(container.Summary{ID: containerID}, nil).
			Once()
		mc.EXPECT().
			RemoveContainerByID(mock.Anything, containerID, mock.Anything).
			Return(boom).
			Once()

		err := sm.removeApp(payload, app)
		require.Error(t, err)
		assert.ErrorIs(t, err, boom)

		// Set DELETING but never REMOVED.
		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.DELETING, final)
	})

	t.Run("dev: image removal failure aborts at DELETING", func(t *testing.T) {
		sm, mc, st, _ := wiredStateMachine(t)

		app := seedApp(t, st, "remove-dev-imgerr", common.PRESENT, common.DEV)
		payload := execPayload("remove-dev-imgerr", common.REMOVED, common.DEV)

		boom := errors.New("image removal failed")
		mc.EXPECT().
			GetContainer(mock.Anything, payload.ContainerName.Dev).
			Return(container.Summary{}, notFoundErr()).
			Once()
		mc.EXPECT().
			RemoveImagesByName(mock.Anything, payload.RegistryImageName.Dev, mock.Anything).
			Return(boom).
			Once()

		err := sm.removeApp(payload, app)
		require.Error(t, err)
		assert.ErrorIs(t, err, boom)

		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.DELETING, final)
	})

	t.Run("unknown stage is a no-op", func(t *testing.T) {
		// A strict mock with no container expectations proves removeApp does
		// nothing for an unknown stage. The app is built in-memory only (the DB
		// CHECK constraint only allows DEV/PROD), which is all removeApp reads.
		sm, _, _, _ := wiredStateMachine(t)

		app := builders.BuildApp("remove-noop", common.PRESENT, common.Stage("weird"))
		payload := execPayload("remove-noop", common.REMOVED, common.Stage("weird"))
		payload.DockerCompose = nil

		err := sm.removeApp(payload, app)
		require.NoError(t, err)
	})
}
