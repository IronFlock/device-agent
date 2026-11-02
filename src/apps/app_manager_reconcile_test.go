package apps

import (
	"testing"

	"reagent/common"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// app_manager.go — local DB reconciliation helpers
//
// These exercise the pure store-backed reconciliation methods (no docker /
// tunnel / network), reusing the amHarness AppManager from app_manager_test.go.
// =============================================================================

func TestCreateOrUpdateApp(t *testing.T) {
	t.Run("creates a new app row and records the requested state", func(t *testing.T) {
		am, _, _, st, _, _ := amHarness(t)

		payload := amPayload(10, "new-app", common.RUNNING, common.PROD)
		payload.RequestedState = common.RUNNING

		err := am.CreateOrUpdateApp(payload)
		require.NoError(t, err)

		app, err := st.GetApp(10, common.PROD)
		require.NoError(t, err)
		require.NotNil(t, app)
		app.StateLock.Lock()
		reqState := app.RequestedState
		app.StateLock.Unlock()
		// RUNNING is a stable actual state -> TransientToActualState keeps it.
		assert.Equal(t, common.RUNNING, reqState)

		// And the requested-state row was persisted.
		got, err := st.GetRequestedState(10, common.PROD)
		require.NoError(t, err)
		assert.Equal(t, common.RUNNING, got.RequestedState)
	})

	t.Run("normalizes a transient requested state to its actual target", func(t *testing.T) {
		am, _, _, st, _, _ := amHarness(t)

		// DOWNLOADING is transient -> normalized to PRESENT.
		payload := amPayload(11, "transient-app", common.DOWNLOADING, common.PROD)
		payload.RequestedState = common.DOWNLOADING

		err := am.CreateOrUpdateApp(payload)
		require.NoError(t, err)

		app, err := st.GetApp(11, common.PROD)
		require.NoError(t, err)
		require.NotNil(t, app)
		app.StateLock.Lock()
		reqState := app.RequestedState
		app.StateLock.Unlock()
		assert.Equal(t, common.PRESENT, reqState)
	})

	t.Run("does not change the requested state of an already-BUILT app awaiting promotion", func(t *testing.T) {
		am, _, _, st, _, _ := amHarness(t)

		// The guard fires when the app's CurrentState is BUILT and its existing
		// RequestedState is NOT BUILT (it has finished building and is waiting to
		// be promoted to PRESENT). A new requested state must not clobber it.
		app := amSeed(t, st, 12, "built-app", common.BUILT, common.DEV)
		app.StateLock.Lock()
		app.RequestedState = common.PRESENT
		app.StateLock.Unlock()

		payload := amPayload(12, "built-app", common.RUNNING, common.DEV)
		payload.RequestedState = common.RUNNING

		err := am.CreateOrUpdateApp(payload)
		require.NoError(t, err)

		app.StateLock.Lock()
		reqState := app.RequestedState
		app.StateLock.Unlock()
		assert.Equal(t, common.PRESENT, reqState, "requested state should be left untouched")
	})

	t.Run("honours an explicit teardown request for an already-BUILT app", func(t *testing.T) {
		am, _, _, st, _, _ := amHarness(t)

		// Same setup as the guard above (BUILT, awaiting promotion to PRESENT),
		// but an explicit UNINSTALLED must override the guard — otherwise a BUILT
		// app can never be uninstalled.
		app := amSeed(t, st, 13, "built-teardown", common.BUILT, common.DEV)
		app.StateLock.Lock()
		app.RequestedState = common.PRESENT
		app.StateLock.Unlock()

		payload := amPayload(13, "built-teardown", common.UNINSTALLED, common.DEV)
		payload.RequestedState = common.UNINSTALLED

		err := am.CreateOrUpdateApp(payload)
		require.NoError(t, err)

		app.StateLock.Lock()
		reqState := app.RequestedState
		app.StateLock.Unlock()
		assert.Equal(t, common.UNINSTALLED, reqState, "teardown must override the BUILT guard")

		got, err := st.GetRequestedState(13, common.DEV)
		require.NoError(t, err)
		assert.Equal(t, common.UNINSTALLED, got.RequestedState)
	})
}

func TestUpdateCurrentAppState(t *testing.T) {
	t.Run("persists the in-memory current state without changing a RUNNING app", func(t *testing.T) {
		am, _, _, st, _, _ := amHarness(t)

		amSeed(t, st, 20, "cur-app", common.RUNNING, common.PROD)

		payload := amPayload(20, "cur-app", common.RUNNING, common.PROD)
		// A RUNNING app is not BUILT/PUBLISHED, so CurrentState is left untouched
		// even though the payload carries one.
		payload.CurrentState = common.PRESENT

		err := am.UpdateCurrentAppState(payload)
		require.NoError(t, err)

		got, err := st.GetApp(20, common.PROD)
		require.NoError(t, err)
		got.StateLock.Lock()
		cur := got.CurrentState
		got.StateLock.Unlock()
		assert.Equal(t, common.RUNNING, cur)
	})

	t.Run("adopts the payload current state when the app is BUILT", func(t *testing.T) {
		am, _, _, st, _, _ := amHarness(t)

		app := amSeed(t, st, 21, "built-cur", common.BUILT, common.DEV)
		app.StateLock.Lock()
		app.RequestedState = common.RUNNING
		app.StateLock.Unlock()

		payload := amPayload(21, "built-cur", common.RUNNING, common.DEV)
		payload.CurrentState = common.RUNNING
		payload.PresentVersion = "9.9.9"

		err := am.UpdateCurrentAppState(payload)
		require.NoError(t, err)

		app.StateLock.Lock()
		cur := app.CurrentState
		ver := app.Version
		app.StateLock.Unlock()
		assert.Equal(t, common.RUNNING, cur, "a BUILT app adopts the payload's current state")
		assert.Equal(t, "9.9.9", ver, "the present version is applied")
	})

	t.Run("adopts the backend's name for a reused app key, in memory and on disk", func(t *testing.T) {
		am, _, _, st, _, _ := amHarness(t)

		app := amSeed(t, st, 22, "stale-name", common.RUNNING, common.PROD)

		require.NoError(t, am.UpdateCurrentAppState(amPayload(22, "backend-name", common.RUNNING, common.PROD)))

		app.StateLock.Lock()
		name := app.AppName
		app.StateLock.Unlock()
		assert.Equal(t, "backend-name", name, "the observer and transitions must use the backend's name")

		// The orphan sweep reads the names from the database.
		all, err := st.GetAllApps()
		require.NoError(t, err)
		require.Len(t, all, 1)
		assert.Equal(t, "backend-name", all[0].AppName)
	})
}

func TestUpdateLocalRequestedAppStatesWithRemote(t *testing.T) {
	t.Run("creates prod apps from the remote payloads and ignores dev", func(t *testing.T) {
		am, _, _, st, _, _ := amHarness(t)

		prod := amPayload(30, "remote-prod", common.RUNNING, common.PROD)
		prod.RequestedState = common.RUNNING
		dev := amPayload(31, "remote-dev", common.BUILT, common.DEV)
		dev.RequestedState = common.BUILT

		err := am.UpdateLocalRequestedAppStatesWithRemote([]common.TransitionPayload{prod, dev})
		require.NoError(t, err)

		// The prod app now exists locally.
		gotProd, err := st.GetApp(30, common.PROD)
		require.NoError(t, err)
		require.NotNil(t, gotProd)

		// DEV apps are NOT materialized by the bulk sync — they are driven by the
		// developer's interactive actions, not overwritten from the cloud sync.
		gotDev, err := st.GetApp(31, common.DEV)
		require.NoError(t, err)
		assert.Nil(t, gotDev, "dev payloads are not materialized by this method")
	})

	t.Run("applies a stop the cloud holds to a DEV app that still runs", func(t *testing.T) {
		am, _, _, st, _, _ := amHarness(t)

		// The stop was recorded in the cloud while the device was offline, so
		// the push never arrived: locally the DEV app is still asked to run.
		app := amSeed(t, st, 32, "dev-running", common.RUNNING, common.DEV)
		app.StateLock.Lock()
		app.RequestedState = common.RUNNING
		app.StateLock.Unlock()

		remote := amPayload(32, "dev-running", common.PRESENT, common.DEV)
		require.NoError(t, am.UpdateLocalRequestedAppStatesWithRemote([]common.TransitionPayload{remote}))

		app.StateLock.Lock()
		reqState := app.RequestedState
		app.StateLock.Unlock()
		assert.Equal(t, common.PRESENT, reqState)
		got, err := st.GetRequestedState(32, common.DEV)
		require.NoError(t, err)
		assert.Equal(t, common.PRESENT, got.RequestedState)
	})

	t.Run("leaves DEV run and build requests to the developer", func(t *testing.T) {
		am, _, _, st, _, _ := amHarness(t)

		stopped := amSeed(t, st, 33, "dev-stopped", common.PRESENT, common.DEV)
		running := amSeed(t, st, 34, "dev-building", common.RUNNING, common.DEV)
		running.StateLock.Lock()
		running.RequestedState = common.RUNNING
		running.StateLock.Unlock()

		run := amPayload(33, "dev-stopped", common.RUNNING, common.DEV)
		build := amPayload(34, "dev-building", common.BUILT, common.DEV)
		require.NoError(t, am.UpdateLocalRequestedAppStatesWithRemote([]common.TransitionPayload{run, build}))

		stopped.StateLock.Lock()
		assert.Equal(t, common.PRESENT, stopped.RequestedState)
		stopped.StateLock.Unlock()
		running.StateLock.Lock()
		assert.Equal(t, common.RUNNING, running.RequestedState)
		running.StateLock.Unlock()
	})

	t.Run("empty remote list is a clean no-op", func(t *testing.T) {
		am, _, _, _, _, _ := amHarness(t)

		err := am.UpdateLocalRequestedAppStatesWithRemote([]common.TransitionPayload{})
		require.NoError(t, err)
	})
}

func TestDevStopOutstanding(t *testing.T) {
	for _, tc := range []struct {
		requested, current common.AppState
		want               bool
	}{
		{common.PRESENT, common.RUNNING, true},
		{common.REMOVED, common.RUNNING, true},
		{common.UNINSTALLED, common.RUNNING, true},
		{common.PRESENT, common.PRESENT, false},
		{common.PRESENT, common.BUILDING, false},
		{common.REMOVED, common.PRESENT, false},
		{common.RUNNING, common.PRESENT, false},
		{common.BUILT, common.RUNNING, false},
		{common.PUBLISHED, common.RUNNING, false},
	} {
		assert.Equal(t, tc.want, devStopOutstanding(tc.requested, tc.current), "%s while %s", tc.requested, tc.current)
	}
}

func TestHandleTransferFailure(t *testing.T) {
	t.Run("resets a TRANSFERING app to REMOVED via the observer", func(t *testing.T) {
		am, _, _, st, msg, _ := amHarness(t)

		app := amSeed(t, st, 40, "transfer-app", common.TRANSFERING, common.DEV)
		containerName := common.BuildContainerName(common.DEV, 40, "transfer-app")

		am.HandleTransferFailure(containerName, assertErr("transfer canceled"))

		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.REMOVED, final)

		states := remoteStatesFor(msg)
		assert.Contains(t, states, common.REMOVED)
	})

	t.Run("does nothing when the app is not in TRANSFERING state", func(t *testing.T) {
		am, _, _, st, msg, _ := amHarness(t)

		app := amSeed(t, st, 41, "running-app", common.RUNNING, common.DEV)
		containerName := common.BuildContainerName(common.DEV, 41, "running-app")

		before := msg.GetCallCount()
		am.HandleTransferFailure(containerName, assertErr("transfer canceled"))

		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.RUNNING, final, "a non-TRANSFERING app is left untouched")
		assert.Equal(t, before, msg.GetCallCount(), "no remote notification should be sent")
	})

	t.Run("is a no-op for an unknown container", func(t *testing.T) {
		am, _, _, _, _, _ := amHarness(t)

		// No app seeded -> GetAppByContainerName returns nil -> early return.
		am.HandleTransferFailure("dev_999_nope", assertErr("transfer canceled"))
	})
}

// assertErr is a tiny local error constructor (kept distinct from the
// notFoundErr/boom helpers used elsewhere in the package's tests).
func assertErr(msg string) error { return &simpleErr{msg} }

type simpleErr struct{ s string }

func (e *simpleErr) Error() string { return e.s }
