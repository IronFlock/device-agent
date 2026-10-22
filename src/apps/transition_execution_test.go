package apps

import (
	"errors"
	"path/filepath"
	"testing"

	dockercontainer "github.com/docker/docker/api/types/container"

	"reagent/common"
	"reagent/container"
	"reagent/errdefs"
	"reagent/logging"
	"reagent/messenger/topics"
	"reagent/persistence"
	"reagent/store"
	"reagent/testutil/builders"
	"reagent/testutil/fakes"
	"reagent/testutil/mocks"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Fully-wired StateMachine harness
//
// setupTestStateMachine (in state_machine_test.go) deliberately wires nil for
// the LogManager / AppStore database, which is enough for transition-function
// *lookup* tests but panics the moment a real handler calls setState() or
// LogManager. The handlers exercised here need those to actually work, so this
// file builds a StateMachine backed by:
//   - a strict mocks.Container (asserts the exact container interactions),
//   - a real, isolated in-memory sqlite store.AppStore (lets us assert the
//     persisted final state),
//   - a real LogManager fed by a fakes.Messenger (so Write/ClearLogHistory are
//     no-ops that record, instead of nil-panicking).
// =============================================================================

// newExecTestDB builds a real, isolated SQLite-backed persistence DB in a temp
// dir (Init already run) and registers a cleanup that closes it.
func newExecTestDB(t *testing.T) persistence.Database {
	t.Helper()

	cfg := builders.DefaultTestConfig()
	cfg.CommandLineArguments.DatabaseFileName = filepath.Join(t.TempDir(), "reagent_apps_test.db")

	db, err := persistence.NewSQLiteDb(cfg)
	require.NoError(t, err)
	require.NoError(t, db.Init())

	t.Cleanup(func() { _ = db.Close() })

	return db
}

// wiredStateMachine returns a StateMachine with real store + log manager and a
// strict container mock. The returned store lets tests read back persisted
// state; the messenger records the remote Notify calls.
func wiredStateMachine(t *testing.T) (*StateMachine, *mocks.Container, *store.AppStore, *fakes.Messenger) {
	t.Helper()

	mockContainer := mocks.NewContainer(t)
	db := newExecTestDB(t)
	msg := fakes.NewMessenger()

	appStore := store.NewAppStore(db, msg)
	observer := NewObserver(mockContainer, &appStore, nil)
	logManager := logging.NewLogManager(mockContainer, msg, db, appStore)

	sm := NewStateMachine(mockContainer, &logManager, &observer, nil)

	return &sm, mockContainer, &appStore, msg
}

// seedApp inserts an app into the store/DB so the observer's Notify path can
// upsert against an existing row, and returns the live *common.App pointer.
func seedApp(t *testing.T, st *store.AppStore, name string, state common.AppState, stage common.Stage) *common.App {
	t.Helper()

	payload := builders.BuildTransitionPayload(name, common.PRESENT, stage)
	payload.AppKey = 1
	payload.CurrentState = state
	payload.DockerCompose = nil // force the non-compose container path

	app, err := st.AddApp(payload)
	require.NoError(t, err)
	require.NotNil(t, app)
	return app
}

// execPayload mirrors seedApp's identity but as a TransitionPayload, with the
// stage-based container/image names populated the way the real flow expects.
func execPayload(name string, requested common.AppState, stage common.Stage) common.TransitionPayload {
	p := builders.BuildTransitionPayload(name, requested, stage)
	p.AppKey = 1
	p.DockerCompose = nil
	// A valid (non-empty) current_state so the RequestedAppStates CHECK
	// constraint is satisfied if this payload is persisted as a requested state.
	p.CurrentState = common.PRESENT
	p.ContainerName = common.StageBasedResult{
		Dev:  common.BuildContainerName(common.DEV, 1, name),
		Prod: common.BuildContainerName(common.PROD, 1, name),
	}
	p.RegistryImageName = common.StageBasedResult{
		Dev:  "registry.test/dev/" + name,
		Prod: "registry.test/prod/" + name,
	}
	return p
}

// notFoundErr is a canonical "container not found" error the handlers treat as
// the benign "nothing to remove" case.
func notFoundErr() error {
	return errdefs.ContainerNotFound(errors.New("no such container"))
}

// =============================================================================
// No-op transition
// =============================================================================

func TestExecuteNoActionTransition(t *testing.T) {
	t.Run("noActionTransitionFunc never touches the container", func(t *testing.T) {
		// A strict mock with zero EXPECT() calls fails if any container method
		// is invoked, proving the no-op truly does nothing.
		sm, _, _, _ := wiredStateMachine(t)

		app := builders.BuildApp("noop-app", common.PRESENT, common.PROD)
		payload := builders.BuildTransitionPayload("noop-app", common.PRESENT, common.PROD)

		err := sm.noActionTransitionFunc(payload, app)

		require.Error(t, err)
		assert.True(t, errdefs.IsNoActionTransition(err), "expected a NoActionTransition error, got %v", err)
	})
}

// =============================================================================
// removeApp (prod, non-compose) — real handler driven end to end
// =============================================================================

func TestRemoveProdApp(t *testing.T) {
	t.Run("no running container: removes image and persists REMOVED", func(t *testing.T) {
		sm, mc, st, msg := wiredStateMachine(t)

		app := seedApp(t, st, "remove-prod", common.PRESENT, common.PROD)
		payload := execPayload("remove-prod", common.REMOVED, common.PROD)

		// Backend requested REMOVED so the observer is allowed to clean up the
		// DB rows once REMOVED is reached.
		require.NoError(t, st.UpdateLocalRequestedState(payload))

		// GetContainer reports "not found" -> the container-removal block is
		// skipped entirely; only the image gets removed.
		mc.EXPECT().
			GetContainer(mock.Anything, payload.ContainerName.Prod).
			Return(dockercontainer.Summary{}, notFoundErr()).
			Once()
		mc.EXPECT().
			RemoveImagesByName(mock.Anything, payload.RegistryImageName.Prod, mock.Anything).
			Return(nil).
			Once()

		err := sm.removeApp(payload, app)
		require.NoError(t, err)

		// In-memory app pointer should now be REMOVED.
		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.REMOVED, final)

		// And the remote Notify happened: DELETING then REMOVED were both
		// pushed to the backend via SetActualAppOnDeviceState.
		states := remoteStatesFor(msg)
		assert.Contains(t, states, common.DELETING)
		assert.Contains(t, states, common.REMOVED)
	})

	t.Run("running container is stopped/removed before image removal", func(t *testing.T) {
		sm, mc, st, _ := wiredStateMachine(t)

		app := seedApp(t, st, "remove-prod2", common.RUNNING, common.PROD)
		payload := execPayload("remove-prod2", common.REMOVED, common.PROD)

		const containerID = "cid-123"

		mc.EXPECT().
			GetContainer(mock.Anything, payload.ContainerName.Prod).
			Return(dockercontainer.Summary{ID: containerID}, nil).
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
			RemoveImagesByName(mock.Anything, payload.RegistryImageName.Prod, mock.Anything).
			Return(nil).
			Once()

		err := sm.removeApp(payload, app)
		require.NoError(t, err)

		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.REMOVED, final)
	})

	t.Run("image removal failure propagates and state stays DELETING", func(t *testing.T) {
		sm, mc, st, _ := wiredStateMachine(t)

		app := seedApp(t, st, "remove-prod3", common.PRESENT, common.PROD)
		payload := execPayload("remove-prod3", common.REMOVED, common.PROD)

		boom := errors.New("registry unreachable")

		mc.EXPECT().
			GetContainer(mock.Anything, payload.ContainerName.Prod).
			Return(dockercontainer.Summary{}, notFoundErr()).
			Once()
		mc.EXPECT().
			RemoveImagesByName(mock.Anything, payload.RegistryImageName.Prod, mock.Anything).
			Return(boom).
			Once()

		err := sm.removeApp(payload, app)
		require.Error(t, err)
		assert.ErrorIs(t, err, boom)

		// The handler set DELETING before the failing image removal, and never
		// reached the final REMOVED set.
		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.DELETING, final)
	})
}

// =============================================================================
// stopApp — exercises both the no-op stage branch and a real prod stop
// =============================================================================

func TestStopApp(t *testing.T) {
	t.Run("unknown stage is a no-op and never touches the container", func(t *testing.T) {
		sm, _, _, _ := wiredStateMachine(t)

		app := builders.BuildApp("stop-noop", common.RUNNING, common.Stage(""))
		payload := execPayload("stop-noop", common.PRESENT, common.Stage(""))

		err := sm.stopApp(payload, app)
		require.NoError(t, err)
	})

	t.Run("prod stop stops, removes and ends in PRESENT", func(t *testing.T) {
		sm, mc, st, msg := wiredStateMachine(t)

		app := seedApp(t, st, "stop-prod", common.RUNNING, common.PROD)
		payload := execPayload("stop-prod", common.PRESENT, common.PROD)

		const containerID = "stop-cid"

		mc.EXPECT().
			GetContainer(mock.Anything, payload.ContainerName.Prod).
			Return(dockercontainer.Summary{ID: containerID}, nil).
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

		// PollContainerState returns a channel that immediately yields a
		// container-not-found error, which the handler treats as "removed OK".
		errC := make(chan error, 1)
		errC <- notFoundErr()
		close(errC)
		stateC := make(chan container.ContainerState)
		mc.EXPECT().
			PollContainerState(mock.Anything, containerID, mock.Anything).
			Return(stateC, errC).
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
}

// =============================================================================
// cancel* handlers — cancel a stream then notify a stable state
// =============================================================================

func TestCancelHandlers(t *testing.T) {
	t.Run("cancelPull cancels stream and sets REMOVED (prod)", func(t *testing.T) {
		sm, mc, st, _ := wiredStateMachine(t)

		app := seedApp(t, st, "cancel-pull", common.DOWNLOADING, common.PROD)
		payload := execPayload("cancel-pull", common.REMOVED, common.PROD)

		mc.EXPECT().CancelStream(mock.Anything).Return(nil).Once()

		err := sm.cancelPull(payload, app)
		require.NoError(t, err)

		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.REMOVED, final)
	})

	t.Run("cancelPull rejects dev stage without touching the container", func(t *testing.T) {
		sm, _, _, _ := wiredStateMachine(t)

		app := builders.BuildApp("cancel-pull-dev", common.DOWNLOADING, common.DEV)
		payload := execPayload("cancel-pull-dev", common.REMOVED, common.DEV)

		err := sm.cancelPull(payload, app)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot pull dev apps")
	})

	t.Run("cancelPush cancels stream and sets REMOVED", func(t *testing.T) {
		sm, mc, st, _ := wiredStateMachine(t)

		app := seedApp(t, st, "cancel-push", common.PUBLISHING, common.DEV)
		payload := execPayload("cancel-push", common.REMOVED, common.DEV)

		mc.EXPECT().CancelStream(mock.Anything).Return(nil).Once()

		err := sm.cancelPush(payload, app)
		require.NoError(t, err)

		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.REMOVED, final)
	})

	t.Run("cancelUpdate cancels stream, marks CANCELED, sets PRESENT", func(t *testing.T) {
		sm, mc, st, _ := wiredStateMachine(t)

		app := seedApp(t, st, "cancel-update", common.UPDATING, common.PROD)
		payload := execPayload("cancel-update", common.PRESENT, common.PROD)

		mc.EXPECT().CancelStream(mock.Anything).Return(nil).Once()

		err := sm.cancelUpdate(payload, app)
		require.NoError(t, err)

		app.StateLock.Lock()
		final := app.CurrentState
		updStatus := app.UpdateStatus
		app.StateLock.Unlock()
		assert.Equal(t, common.PRESENT, final)
		assert.Equal(t, common.CANCELED, updStatus)
	})

	t.Run("cancelBuild rejects prod apps", func(t *testing.T) {
		sm, _, _, _ := wiredStateMachine(t)

		app := builders.BuildApp("cancel-build-prod", common.BUILDING, common.PROD)
		payload := execPayload("cancel-build-prod", common.REMOVED, common.PROD)
		payload.DockerCompose = nil

		err := sm.cancelBuild(payload, app)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot build prod apps")
	})

	t.Run("cancelBuild (dev, non-compose) cancels stream and sets REMOVED", func(t *testing.T) {
		sm, mc, st, _ := wiredStateMachine(t)

		app := seedApp(t, st, "cancel-build-dev", common.BUILDING, common.DEV)
		payload := execPayload("cancel-build-dev", common.REMOVED, common.DEV)

		mc.EXPECT().CancelStream(mock.Anything).Return(nil).Once()

		err := sm.cancelBuild(payload, app)
		require.NoError(t, err)

		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.REMOVED, final)
	})
}

// =============================================================================
// Small mock-arg helpers
// =============================================================================

func remoteStatesFor(msg *fakes.Messenger) []common.AppState {
	states := make([]common.AppState, 0)
	for _, call := range msg.CallCalls {
		if string(call.Topic) != string(topics.SetActualAppOnDeviceState) {
			continue
		}
		if len(call.Args) == 0 {
			continue
		}
		dict, ok := call.Args[0].(common.Dict)
		if !ok {
			continue
		}
		if s, ok := dict["state"].(common.AppState); ok {
			states = append(states, s)
		}
	}
	return states
}
