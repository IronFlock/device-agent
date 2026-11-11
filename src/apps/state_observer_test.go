package apps

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"reagent/common"
	"reagent/errdefs"
	"reagent/messenger/topics"
	"reagent/persistence"
	"reagent/store"
	"reagent/testutil/builders"
	"reagent/testutil/fakes"
	"reagent/testutil/mocks"

	containerpkg "reagent/container"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Observer test harness
//
// The StateObserver is a concrete struct, so we build it directly with a strict
// mocks.Container, a real in-memory store.AppStore (so Notify actually persists
// and we can read it back) and a fakes.Messenger (records the remote calls).
// We deliberately avoid the Compose()-coupled correction paths here — those
// shell out to `docker compose` and belong to integration tests.
// =============================================================================

func newObserverHarness(t *testing.T) (*StateObserver, *mocks.Container, *store.AppStore, *fakes.Messenger) {
	t.Helper()

	so, mockContainer, appStore, msg, _ := newObserverHarnessWithDB(t)
	return so, mockContainer, appStore, msg
}

// newObserverHarnessWithDB is newObserverHarness that also hands back the
// database, so a test can close it to make every local write fail.
func newObserverHarnessWithDB(t *testing.T) (*StateObserver, *mocks.Container, *store.AppStore, *fakes.Messenger, *persistence.AppStateDatabase) {
	t.Helper()

	mockContainer := mocks.NewContainer(t)

	cfg := builders.DefaultTestConfig()
	cfg.CommandLineArguments.DatabaseFileName = filepath.Join(t.TempDir(), "reagent_observer_test.db")
	db, err := persistence.NewSQLiteDb(cfg)
	require.NoError(t, err)
	require.NoError(t, db.Init())
	t.Cleanup(func() { _ = db.Close() })

	msg := fakes.NewMessenger()
	appStore := store.NewAppStore(db, msg)
	observer := NewObserver(mockContainer, &appStore, nil)

	return &observer, mockContainer, &appStore, msg, db
}

// observerSeedApp inserts an app row (non-compose) and returns the live
// pointer. requested is the backend-requested target state — it matters because
// UpsertAppState re-stamps the RequestedAppStates row with app.RequestedState on
// every local state write, so the cleanup gating in NotifyLocal keys off it.
func observerSeedApp(t *testing.T, st *store.AppStore, name string, state, requested common.AppState, stage common.Stage) *common.App {
	t.Helper()

	payload := builders.BuildTransitionPayload(name, requested, stage)
	payload.AppKey = 1
	payload.RequestedState = requested
	payload.CurrentState = state
	payload.DockerCompose = nil

	app, err := st.AddApp(payload)
	require.NoError(t, err)
	require.NotNil(t, app)
	return app
}

// remoteAppStates extracts every state pushed via SetActualAppOnDeviceState.
func remoteAppStates(msg *fakes.Messenger) []common.AppState {
	states := make([]common.AppState, 0)
	for _, call := range msg.GetCallCalls() {
		if call.Topic != topics.SetActualAppOnDeviceState {
			continue
		}
		if len(call.Args) == 0 {
			continue
		}
		if dict, ok := call.Args[0].(common.Dict); ok {
			if s, ok := dict["state"].(common.AppState); ok {
				states = append(states, s)
			}
		}
	}
	return states
}

// =============================================================================
// Notify — local persist + remote push
// =============================================================================

func TestObserverNotify(t *testing.T) {
	t.Run("persists locally and pushes to remote", func(t *testing.T) {
		so, _, st, msg := newObserverHarness(t)

		app := observerSeedApp(t, st, "notify-app", common.PRESENT, common.RUNNING, common.PROD)

		err := so.Notify(app, common.RUNNING)
		require.NoError(t, err)

		// In-memory pointer updated.
		app.StateLock.Lock()
		current := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.RUNNING, current)

		// Persisted to the DB.
		fromDB, err := st.GetApp(1, common.PROD)
		require.NoError(t, err)
		require.NotNil(t, fromDB)
		assert.Equal(t, common.RUNNING, fromDB.CurrentState)

		// Pushed to remote exactly once with RUNNING.
		states := remoteAppStates(msg)
		require.Len(t, states, 1)
		assert.Equal(t, common.RUNNING, states[0])
	})

	t.Run("propagates remote messenger errors", func(t *testing.T) {
		so, _, st, msg := newObserverHarness(t)

		app := observerSeedApp(t, st, "notify-err", common.PRESENT, common.RUNNING, common.PROD)
		msg.SetCallError(string(topics.SetActualAppOnDeviceState), errors.New("backend down"))

		err := so.Notify(app, common.RUNNING)
		require.Error(t, err)

		// Local state still got updated before the remote push failed.
		fromDB, dberr := st.GetApp(1, common.PROD)
		require.NoError(t, dberr)
		assert.Equal(t, common.RUNNING, fromDB.CurrentState)
	})

	// The field case: a DEV build fails with "no space left on device", and
	// the FAILED write to the agent's database fails on the same full disk.
	// The platform must still learn FAILED; before, it kept showing BUILDING.
	t.Run("reports to remote when the local write fails", func(t *testing.T) {
		so, _, st, msg, db := newObserverHarnessWithDB(t)

		app := observerSeedApp(t, st, "notify-disk-full", common.BUILDING, common.BUILT, common.DEV)
		require.NoError(t, db.Close())

		err := so.Notify(app, common.FAILED)
		require.Error(t, err, "the local write error is still returned")

		app.StateLock.Lock()
		current := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.FAILED, current)

		assert.Equal(t, []common.AppState{common.FAILED}, remoteAppStates(msg))
	})

	t.Run("returns both errors when local and remote fail", func(t *testing.T) {
		so, _, st, msg, db := newObserverHarnessWithDB(t)

		app := observerSeedApp(t, st, "notify-both-fail", common.BUILDING, common.BUILT, common.DEV)
		require.NoError(t, db.Close())
		remoteErr := errors.New("backend down")
		msg.SetCallError(string(topics.SetActualAppOnDeviceState), remoteErr)

		err := so.Notify(app, common.FAILED)
		require.Error(t, err)
		assert.ErrorIs(t, err, remoteErr)
		assert.Contains(t, err.Error(), "database is closed")
	})
}

// =============================================================================
// NotifyLocal — REMOVED-state DB cleanup gating
// =============================================================================

func TestObserverNotifyLocalRemovedCleanup(t *testing.T) {
	t.Run("deletes db rows when backend requested REMOVED", func(t *testing.T) {
		so, _, st, _ := newObserverHarness(t)

		// The app's backend-requested target is REMOVED, so the RequestedAppStates
		// row (re-stamped on every UpsertAppState) stays REMOVED.
		app := observerSeedApp(t, st, "cleanup-app", common.PRESENT, common.REMOVED, common.PROD)

		// Seed the matching RequestedAppStates row.
		reqPayload := builders.BuildTransitionPayload("cleanup-app", common.REMOVED, common.PROD)
		reqPayload.AppKey = 1
		reqPayload.RequestedState = common.REMOVED
		reqPayload.CurrentState = common.PRESENT
		require.NoError(t, st.UpdateLocalRequestedState(reqPayload))

		// Sanity: the requested state really is REMOVED before we notify.
		preReq, err := st.GetRequestedState(1, common.PROD)
		require.NoError(t, err)
		require.Equal(t, common.REMOVED, preReq.RequestedState)

		err = so.NotifyLocal(app, common.REMOVED)
		require.NoError(t, err)

		// Both the requested-state and app-state rows should now be gone:
		// GetRequestedState errors when there is no row for the key/stage.
		_, err = st.GetRequestedState(1, common.PROD)
		assert.Error(t, err, "requested state row should have been deleted")

		// And the in-memory store no longer has a row either: GetApp returns the
		// cached pointer (now REMOVED) but the underlying DB row is gone.
		fromDB, dberr := st.GetApp(1, common.PROD)
		require.NoError(t, dberr)
		require.NotNil(t, fromDB)
		assert.Equal(t, common.REMOVED, fromDB.CurrentState)
	})

	t.Run("keeps db rows when backend requested a non-removed state", func(t *testing.T) {
		so, _, st, _ := newObserverHarness(t)

		app := observerSeedApp(t, st, "keep-app", common.RUNNING, common.RUNNING, common.PROD)

		// Backend wants it RUNNING, even though it momentarily reached REMOVED.
		reqPayload := builders.BuildTransitionPayload("keep-app", common.RUNNING, common.PROD)
		reqPayload.AppKey = 1
		reqPayload.RequestedState = common.RUNNING
		reqPayload.CurrentState = common.RUNNING
		require.NoError(t, st.UpdateLocalRequestedState(reqPayload))

		err := so.NotifyLocal(app, common.REMOVED)
		require.NoError(t, err)

		// Requested state is preserved.
		got, err := st.GetRequestedState(1, common.PROD)
		require.NoError(t, err)
		assert.Equal(t, common.RUNNING, got.RequestedState)
	})

	t.Run("keeps db rows when there is no requested state at all", func(t *testing.T) {
		so, _, st, _ := newObserverHarness(t)

		// No RequestedAppStates row is seeded for this app at all.
		app := observerSeedApp(t, st, "orphan-app", common.PRESENT, common.PRESENT, common.PROD)

		err := so.NotifyLocal(app, common.REMOVED)
		require.NoError(t, err)

		// The app state row is still present in the DB (not cleaned up).
		got, err := st.GetApp(1, common.PROD)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, common.REMOVED, got.CurrentState)
	})
}

// =============================================================================
// addObserver / removeObserver — no-Compose container bookkeeping
// =============================================================================

func TestObserverAddObserver(t *testing.T) {
	t.Run("does not create an observer when the container is missing", func(t *testing.T) {
		so, mc, _, _ := newObserverHarness(t)

		containerName := common.BuildContainerName(common.PROD, 1, "ghost")
		mc.EXPECT().
			GetContainerState(mock.Anything, containerName).
			Return(containerpkg.ContainerState{}, errdefs.ContainerNotFound(errors.New("no such container"))).
			Once()

		created := so.addObserver(common.PROD, 1, "ghost")
		assert.False(t, created, "no observer should be created for a missing container")
	})

	t.Run("does not create an observer when the state lookup errors", func(t *testing.T) {
		so, mc, _, _ := newObserverHarness(t)

		containerName := common.BuildContainerName(common.PROD, 2, "boom")
		mc.EXPECT().
			GetContainerState(mock.Anything, containerName).
			Return(containerpkg.ContainerState{}, errors.New("docker daemon unreachable")).
			Once()

		created := so.addObserver(common.PROD, 2, "boom")
		assert.False(t, created)
	})
}

func TestObserverRemoveOwnObserver(t *testing.T) {
	t.Run("no-op when no observer is registered", func(t *testing.T) {
		so, _, _, _ := newObserverHarness(t)

		so.removeOwnObserver("prod_1_missing", context.Background())
		assert.Empty(t, so.activeObservers)
	})

	t.Run("removes and cancels its own entry", func(t *testing.T) {
		so, _, _, _ := newObserverHarness(t)

		ownerCtx, cancel := context.WithCancel(context.Background())
		so.activeObservers["prod_1_mine"] = &AppStateObserver{
			AppKey: 1, AppName: "mine", Stage: common.PROD,
			cancel: cancel, ctx: ownerCtx,
		}

		so.removeOwnObserver("prod_1_mine", ownerCtx)
		assert.Empty(t, so.activeObservers)
		assert.Error(t, ownerCtx.Err(), "own context must be cancelled on removal")
	})

	t.Run("leaves a replacement observer alone", func(t *testing.T) {
		so, _, _, _ := newObserverHarness(t)

		// A dying goroutine (old ctx) must not tear down the fresh observer
		// the event spawner registered under the same name.
		oldCtx, oldCancel := context.WithCancel(context.Background())
		oldCancel()
		newCtx, newCancel := context.WithCancel(context.Background())
		defer newCancel()
		so.activeObservers["prod_1_app"] = &AppStateObserver{
			AppKey: 1, AppName: "app", Stage: common.PROD,
			cancel: newCancel, ctx: newCtx,
		}

		so.removeOwnObserver("prod_1_app", oldCtx)
		assert.NotEmpty(t, so.activeObservers, "replacement observer must survive")
		assert.NoError(t, newCtx.Err(), "replacement observer must not be cancelled")
	})
}

// =============================================================================
// Container-name parsing — the spawner relies on these regexes + parsers to
// decide which (un)observe path to take for a docker event.
// =============================================================================

func TestObserverContainerNameRegex(t *testing.T) {
	t.Run("plain container name matches the plain regex", func(t *testing.T) {
		name := common.BuildContainerName(common.DEV, 1336, "myapp")
		match := ContainerNameRegexExp.FindStringSubmatch(name)
		require.NotEmpty(t, match)

		stage, key, parsedName, err := common.ParseContainerName(name)
		require.NoError(t, err)
		assert.Equal(t, common.DEV, stage)
		assert.Equal(t, uint64(1336), key)
		assert.Equal(t, "myapp", parsedName)
	})

	t.Run("compose container name matches the compose regex", func(t *testing.T) {
		// As emitted by docker for a compose service container.
		fullName := "dev_1336_markopetzold_compose-web-1"
		match := ComposeContainerNameRegexExp.FindStringSubmatch(fullName)
		require.NotEmpty(t, match, "expected the compose regex to match a compose service container")

		// The spawner trims everything after the first '-' before parsing.
		base := "dev_1336_markopetzold_compose"
		stage, key, parsedName, err := common.ParseComposeContainerName(base)
		require.NoError(t, err)
		assert.Equal(t, common.DEV, stage)
		assert.Equal(t, uint64(1336), key)
		assert.Equal(t, "markopetzold", parsedName)
	})

	t.Run("round-trips a built compose name through build+trim+parse", func(t *testing.T) {
		built := common.BuildComposeContainerName(common.PROD, 42, "svc")
		// BuildComposeContainerName lower-cases and appends _compose.
		assert.Equal(t, "prod_42_svc_compose", built)

		stage, key, parsedName, err := common.ParseComposeContainerName(built)
		require.NoError(t, err)
		assert.Equal(t, common.PROD, stage)
		assert.Equal(t, uint64(42), key)
		assert.Equal(t, "svc", parsedName)
	})

	t.Run("non-container names do not match either regex", func(t *testing.T) {
		assert.Empty(t, ComposeContainerNameRegexExp.FindStringSubmatch("not-a-container"))

		_, _, _, err := common.ParseContainerName("garbage")
		assert.Error(t, err)
	})
}

// =============================================================================
// composeServicesWithMissingImages — pure matching logic shared by the boot
// reconciler and the install path's download-phase decision (runProdComposeApp).
// =============================================================================

func TestComposeServicesWithMissingImages(t *testing.T) {
	compose := func(images ...string) map[string]interface{} {
		services := map[string]interface{}{}
		for i, image := range images {
			services[string(rune('a'+i))] = map[string]interface{}{"image": image}
		}
		return map[string]interface{}{"services": services}
	}

	local := []containerpkg.ImageResult{
		{RepoTags: []string{"registry.example.com/apps/prod_amd64_12_myapp:1.0.0"}},
		{RepoTags: []string{"eclipse-mosquitto:2"}},
		{RepoTags: nil}, // dangling image, no tags
	}

	t.Run("all images present", func(t *testing.T) {
		missing, err := composeServicesWithMissingImages(compose("registry.example.com/apps/prod_amd64_12_myapp:1.0.0", "eclipse-mosquitto:2"), local)
		require.NoError(t, err)
		assert.Empty(t, missing)
	})

	t.Run("only the services with missing images are returned", func(t *testing.T) {
		missing, err := composeServicesWithMissingImages(compose("eclipse-mosquitto:2", "dperson/samba:latest"), local)
		require.NoError(t, err)
		assert.Equal(t, []string{"b"}, missing)
	})

	t.Run("untagged reference matches tagged local image", func(t *testing.T) {
		missing, err := composeServicesWithMissingImages(compose("eclipse-mosquitto"), local)
		require.NoError(t, err)
		assert.Empty(t, missing)
	})

	t.Run("service without image field is skipped", func(t *testing.T) {
		dockerCompose := map[string]interface{}{
			"services": map[string]interface{}{
				"built": map[string]interface{}{"build": "."},
			},
		}
		missing, err := composeServicesWithMissingImages(dockerCompose, local)
		require.NoError(t, err)
		assert.Empty(t, missing)
	})

	t.Run("no local images at all returns every image service sorted", func(t *testing.T) {
		missing, err := composeServicesWithMissingImages(compose("eclipse-mosquitto:2", "dperson/samba:latest"), nil)
		require.NoError(t, err)
		assert.Equal(t, []string{"a", "b"}, missing)
	})

	t.Run("malformed compose errors", func(t *testing.T) {
		_, err := composeServicesWithMissingImages(map[string]interface{}{"services": "garbage"}, local)
		assert.Error(t, err)

		_, err = composeServicesWithMissingImages(map[string]interface{}{"services": map[string]interface{}{"a": "garbage"}}, local)
		assert.Error(t, err)
	})
}
