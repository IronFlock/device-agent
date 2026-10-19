package apps

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"reagent/common"
	"reagent/config"
	"reagent/logging"
	"reagent/persistence"
	"reagent/store"
	"reagent/testutil/builders"
	"reagent/testutil/fakes"
	"reagent/testutil/mocks"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// AppManager harness
//
// The AppManager is a concrete struct that owns the StateMachine + StateObserver
// + AppStore + crash-loop bookkeeping. We wire it against:
//   - a strict mocks.Container (so cleanup paths' container interactions are
//     asserted exactly),
//   - a strict mocks.TunnelManager (only the tunnel-state push path touches it),
//   - a real, isolated in-memory sqlite store.AppStore (so DB cleanup is real),
//   - a fakes.Messenger (records remote publishes/calls).
//
// All helpers in this file are uniquely named (prefixed `am*`) so they never
// collide with the wiredStateMachine / seedApp / execPayload helpers in
// transition_execution_test.go and run_build_test.go, which this file does NOT
// redefine.
// =============================================================================

// amTempConfig returns a config whose Apps* dirs and registry point at isolated
// temp locations so any filesystem side-effect is harmless.
func amTempConfig(t *testing.T) *config.Config {
	t.Helper()

	cfg := builders.DefaultTestConfig()
	base := t.TempDir()
	cfg.CommandLineArguments.AppsDirectory = filepath.Join(base, "apps")
	cfg.CommandLineArguments.AppsBuildDir = filepath.Join(base, "build")
	cfg.CommandLineArguments.AppsComposeDir = filepath.Join(base, "compose")
	cfg.CommandLineArguments.AppsSharedDir = filepath.Join(base, "shared")
	cfg.CommandLineArguments.CompressedBuildExtension = "tgz"
	cfg.ReswarmConfig.DockerRegistryURL = "registry.test"
	return cfg
}

// amNewDB builds a real isolated sqlite DB (Init already run) and registers a
// close cleanup.
func amNewDB(t *testing.T) persistence.Database {
	t.Helper()

	cfg := builders.DefaultTestConfig()
	cfg.CommandLineArguments.DatabaseFileName = filepath.Join(t.TempDir(), "reagent_am_test.db")

	db, err := persistence.NewSQLiteDb(cfg)
	require.NoError(t, err)
	require.NoError(t, db.Init())

	t.Cleanup(func() { _ = db.Close() })
	return db
}

// amHarness wires a complete AppManager. Returns the manager plus the doubles a
// test asserts against.
func amHarness(t *testing.T) (*AppManager, *mocks.Container, *mocks.TunnelManager, *store.AppStore, *fakes.Messenger, *config.Config) {
	t.Helper()

	mockContainer := mocks.NewContainer(t)
	mockTunnel := mocks.NewTunnelManager(t)
	db := amNewDB(t)
	msg := fakes.NewMessenger()
	cfg := amTempConfig(t)

	// Several manager paths (and the forward stream cleanup goroutines) read the
	// config; allow it everywhere.
	mockContainer.EXPECT().GetConfig().Return(cfg).Maybe()

	appStore := store.NewAppStore(db, msg)
	observer := NewObserver(mockContainer, &appStore, nil)
	logManager := logging.NewLogManager(mockContainer, msg, db, appStore)
	sm := NewStateMachine(mockContainer, &logManager, &observer, nil)

	am := NewAppManager(&sm, &appStore, &observer, mockTunnel)

	// No test may arm a real port-sync retry timer: it could fire into a
	// finished test's strict mocks. Tests that inspect or fire the retries
	// install their own capture on top of this one.
	amCapturePortSyncRetries(t)

	// give any async log-history drains a moment before the DB closes
	t.Cleanup(func() { time.Sleep(150 * time.Millisecond) })

	return am, mockContainer, mockTunnel, &appStore, msg, cfg
}

// amPortSyncRetries records the port-sync retries an AppManager under test
// schedules. None of them ever fires on its own; a test fires one by hand.
// The package's tests do not run in parallel, so swapping the package-level
// timer seam per test is safe.
type amPortSyncRetries struct {
	mu        sync.Mutex
	scheduled []amPortSyncRetry
}

type amPortSyncRetry struct {
	after time.Duration
	fire  func()
}

// amCapturePortSyncRetries replaces the port-sync retry timer with a capture
// for the rest of the test and returns it.
func amCapturePortSyncRetries(t *testing.T) *amPortSyncRetries {
	t.Helper()

	captured := &amPortSyncRetries{}
	restore := portSyncRetryAfterFunc
	portSyncRetryAfterFunc = func(after time.Duration, fire func()) *time.Timer {
		captured.mu.Lock()
		captured.scheduled = append(captured.scheduled, amPortSyncRetry{after: after, fire: fire})
		captured.mu.Unlock()

		// A real timer the manager may Stop, already stopped: it never fires.
		timer := time.AfterFunc(time.Hour, func() {})
		timer.Stop()
		return timer
	}
	t.Cleanup(func() { portSyncRetryAfterFunc = restore })

	return captured
}

// all returns the retries scheduled so far, oldest first.
func (r *amPortSyncRetries) all() []amPortSyncRetry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]amPortSyncRetry(nil), r.scheduled...)
}

// amSeed inserts an app row (non-compose) and returns the live pointer.
func amSeed(t *testing.T, st *store.AppStore, key uint64, name string, state common.AppState, stage common.Stage) *common.App {
	t.Helper()

	payload := builders.BuildTransitionPayload(name, common.PRESENT, stage)
	payload.AppKey = key
	payload.CurrentState = state
	payload.DockerCompose = nil

	app, err := st.AddApp(payload)
	require.NoError(t, err)
	require.NotNil(t, app)
	return app
}

// amPayload builds a transition payload with the same identity as amSeed.
func amPayload(key uint64, name string, requested common.AppState, stage common.Stage) common.TransitionPayload {
	p := builders.BuildTransitionPayload(name, requested, stage)
	p.AppKey = key
	p.DockerCompose = nil
	p.CurrentState = common.PRESENT
	p.ContainerName = common.StageBasedResult{
		Dev:  common.BuildContainerName(common.DEV, key, name),
		Prod: common.BuildContainerName(common.PROD, key, name),
	}
	p.RegistryImageName = common.StageBasedResult{
		Dev:  "registry.test/dev/" + name,
		Prod: "registry.test/prod/" + name,
	}
	return p
}

// =============================================================================
// NewAppManager — wiring
// =============================================================================

func TestNewAppManager(t *testing.T) {
	t.Run("wires the back-reference and initializes the crash-loop map", func(t *testing.T) {
		am, _, _, st, _, _ := amHarness(t)

		require.NotNil(t, am)
		assert.NotNil(t, am.StateMachine)
		assert.NotNil(t, am.StateObserver)
		assert.Same(t, st, am.AppStore)
		// NewAppManager sets observer.AppManager back to the manager.
		assert.Same(t, am, am.StateObserver.AppManager)
		// The crash-loop registry is ready (empty).
		require.NotNil(t, am.crashLoops)
		assert.Len(t, am.crashLoops, 0)
	})
}

// =============================================================================
// CleanupOrphanedContainers
// =============================================================================

func TestCleanupOrphanedContainers(t *testing.T) {
	t.Run("removes a container that is not backed by any DB app", func(t *testing.T) {
		am, mc, _, st, _, _ := amHarness(t)

		// One known app -> its container name is expected and must be kept.
		amSeed(t, st, 1, "keepme", common.RUNNING, common.PROD)
		keptName := "/" + common.BuildContainerName(common.PROD, 1, "keepme")

		// An orphan: a well-formed container name with no matching DB row.
		orphanName := "/" + common.BuildContainerName(common.PROD, 99, "ghost")

		mc.EXPECT().
			GetContainers(mock.Anything).
			Return([]dockertypes.Container{
				{ID: "kept-container-id", Names: []string{keptName}},
				{ID: "orphan-container-id", Names: []string{orphanName}},
				// A foreign container we don't manage -> ParseContainerName fails.
				{ID: "foreign-id", Names: []string{"/some-random-thing"}},
			}, nil).
			Once()

		// Only the orphan should be force-removed.
		mc.EXPECT().
			RemoveContainerByID(mock.Anything, "orphan-container-id", map[string]interface{}{"force": true}).
			Return(nil).
			Once()

		err := am.CleanupOrphanedContainers()
		require.NoError(t, err)
	})

	t.Run("propagates a list error", func(t *testing.T) {
		am, mc, _, _, _, _ := amHarness(t)

		boom := errors.New("docker daemon unreachable")
		mc.EXPECT().GetContainers(mock.Anything).Return(nil, boom).Once()

		err := am.CleanupOrphanedContainers()
		require.Error(t, err)
		assert.ErrorIs(t, err, boom)
	})

	t.Run("swallows a remove failure and keeps going", func(t *testing.T) {
		am, mc, _, _, _, _ := amHarness(t)

		orphanName := "/" + common.BuildContainerName(common.DEV, 7, "orphan")
		mc.EXPECT().
			GetContainers(mock.Anything).
			Return([]dockertypes.Container{
				{ID: "orphan-id-xyz", Names: []string{orphanName}},
			}, nil).
			Once()

		// Remove fails, but CleanupOrphanedContainers logs and returns nil overall.
		mc.EXPECT().
			RemoveContainerByID(mock.Anything, "orphan-id-xyz", map[string]interface{}{"force": true}).
			Return(errors.New("remove blew up")).
			Once()

		err := am.CleanupOrphanedContainers()
		require.NoError(t, err)
	})

	t.Run("keeps every container while a fresh database is unsynced", func(t *testing.T) {
		am, mc, _, _, _, _ := amHarness(t)

		// A database this start created: no Docker call at all, so no
		// container can be removed (the strict mock fails on any call).
		am.MarkDatabaseUnsynced()
		require.NoError(t, am.CleanupOrphanedContainers())

		// After the backend sync, cleanup works again.
		am.MarkDatabaseSynced()
		orphanName := "/" + common.BuildContainerName(common.PROD, 99, "ghost")
		mc.EXPECT().
			GetContainers(mock.Anything).
			Return([]dockertypes.Container{{ID: "orphan-id", Names: []string{orphanName}}}, nil).
			Once()
		mc.EXPECT().
			RemoveContainerByID(mock.Anything, "orphan-id", map[string]interface{}{"force": true}).
			Return(nil).
			Once()
		require.NoError(t, am.CleanupOrphanedContainers())
	})

	t.Run("no containers is a clean no-op", func(t *testing.T) {
		am, mc, _, _, _, _ := amHarness(t)

		mc.EXPECT().GetContainers(mock.Anything).Return([]dockertypes.Container{}, nil).Once()

		err := am.CleanupOrphanedContainers()
		require.NoError(t, err)
	})

	// Regression: compose names its containers "<project>-<service>-<index>",
	// which ParseContainerName parses as a legacy container whose name is never
	// in the keep-set. That force-removed every service container of every
	// compose app on each WAMP reconnect, and the state correction right after
	// reinstalled the project — a full app restart per reconnect (seen in the
	// field on a proxied appliance whose session was capped every 15 minutes).
	t.Run("keeps every service container of a known compose app", func(t *testing.T) {
		am, mc, _, st, _, _ := amHarness(t)

		amSeed(t, st, 6, "trumpfqds", common.RUNNING, common.PROD)
		project := common.BuildComposeContainerName(common.PROD, 6, "trumpfqds")

		// The strict mock proves it: no RemoveContainerByID expectation is set,
		// so any removal fails the test.
		mc.EXPECT().
			GetContainers(mock.Anything).
			Return([]dockertypes.Container{
				{
					ID:     "svc-nginx-id",
					Names:  []string{"/" + project + "-nginx-1"},
					Labels: map[string]string{composeProjectLabel: project},
				},
				{
					ID:     "svc-collector-id",
					Names:  []string{"/" + project + "-collector-1"},
					Labels: map[string]string{composeProjectLabel: project},
				},
			}, nil).
			Once()

		err := am.CleanupOrphanedContainers()
		require.NoError(t, err)
	})

	t.Run("removes service containers of a compose app that is gone from the DB", func(t *testing.T) {
		am, mc, _, st, _, _ := amHarness(t)

		// A different app is known; the ghost project has no DB row.
		amSeed(t, st, 6, "trumpfqds", common.RUNNING, common.PROD)
		ghost := common.BuildComposeContainerName(common.PROD, 42, "ghost")

		mc.EXPECT().
			GetContainers(mock.Anything).
			Return([]dockertypes.Container{
				{
					ID:     "ghost-svc-id",
					Names:  []string{"/" + ghost + "-web-1"},
					Labels: map[string]string{composeProjectLabel: ghost},
				},
			}, nil).
			Once()

		mc.EXPECT().
			RemoveContainerByID(mock.Anything, "ghost-svc-id", map[string]interface{}{"force": true}).
			Return(nil).
			Once()

		err := am.CleanupOrphanedContainers()
		require.NoError(t, err)
	})

	t.Run("never touches a foreign compose project", func(t *testing.T) {
		am, mc, _, _, _, _ := amHarness(t)

		// The appliance's own stack, and a short project name that would have
		// panicked ParseComposeContainerName's unguarded index.
		mc.EXPECT().
			GetContainers(mock.Anything).
			Return([]dockertypes.Container{
				{
					ID:     "appliance-backend-id",
					Names:  []string{"/ironflock-backend-1"},
					Labels: map[string]string{composeProjectLabel: "ironflock"},
				},
				{
					ID:     "other-id",
					Names:  []string{"/x-y-1"},
					Labels: map[string]string{composeProjectLabel: "x"},
				},
			}, nil).
			Once()

		err := am.CleanupOrphanedContainers()
		require.NoError(t, err)
	})
}

// =============================================================================
// CleanupOrphanedAppsFromDatabase
// =============================================================================

func TestCleanupOrphanedAppsFromDatabase(t *testing.T) {
	t.Run("keeps apps still present in the backend sync", func(t *testing.T) {
		am, _, _, st, _, _ := amHarness(t)

		amSeed(t, st, 1, "synced", common.RUNNING, common.PROD)

		// The backend still lists this app -> nothing is removed, no container
		// interaction at all (strict mock proves it).
		remote := []common.TransitionPayload{amPayload(1, "synced", common.RUNNING, common.PROD)}

		err := am.CleanupOrphanedAppsFromDatabase(remote)
		require.NoError(t, err)

		// Row still present.
		got, err := st.GetApp(1, common.PROD)
		require.NoError(t, err)
		require.NotNil(t, got)
	})

	t.Run("removes a prod app missing from the backend sync (no container)", func(t *testing.T) {
		am, mc, _, st, _, _ := amHarness(t)

		amSeed(t, st, 5, "stale-prod", common.PRESENT, common.PROD)

		// Backend no longer lists this app -> it must be torn down.
		// Both the single and compose container probes report "not found", so
		// RemoveContainerByID is never called. PROD skips image removal.
		mc.EXPECT().
			GetContainer(mock.Anything, common.BuildContainerName(common.PROD, 5, "stale-prod")).
			Return(dockertypes.Container{}, notFoundErr()).
			Once()
		mc.EXPECT().
			GetContainer(mock.Anything, common.BuildComposeContainerName(common.PROD, 5, "stale-prod")).
			Return(dockertypes.Container{}, notFoundErr()).
			Once()

		err := am.CleanupOrphanedAppsFromDatabase([]common.TransitionPayload{})
		require.NoError(t, err)

		// Database rows for the app should be gone.
		fromDB, dberr := st.GetApp(5, common.PROD)
		require.NoError(t, dberr)
		// GetApp may still return the in-memory cached pointer, but the persisted
		// requested-state row must be deleted.
		_, reqErr := st.GetRequestedState(5, common.PROD)
		assert.Error(t, reqErr, "requested state row should have been deleted")
		_ = fromDB
	})

	t.Run("removes a stale dev app with a live container and its image", func(t *testing.T) {
		am, mc, _, st, _, _ := amHarness(t)

		amSeed(t, st, 8, "stale-dev", common.RUNNING, common.DEV)

		const containerID = "stale-dev-cid"
		// Single container exists -> force remove.
		mc.EXPECT().
			GetContainer(mock.Anything, common.BuildContainerName(common.DEV, 8, "stale-dev")).
			Return(dockertypes.Container{ID: containerID}, nil).
			Once()
		mc.EXPECT().
			RemoveContainerByID(mock.Anything, containerID, map[string]interface{}{"force": true}).
			Return(nil).
			Once()
		// Compose container probe -> not found.
		mc.EXPECT().
			GetContainer(mock.Anything, common.BuildComposeContainerName(common.DEV, 8, "stale-dev")).
			Return(dockertypes.Container{}, notFoundErr()).
			Once()
		// DEV apps get their image removed.
		mc.EXPECT().
			RemoveImagesByName(mock.Anything, common.BuildImageName(common.DEV, "amd64", 8, "stale-dev"), map[string]interface{}{"force": true}).
			Return(nil).
			Once()

		err := am.CleanupOrphanedAppsFromDatabase([]common.TransitionPayload{})
		require.NoError(t, err)

		_, reqErr := st.GetRequestedState(8, common.DEV)
		assert.Error(t, reqErr, "requested state row should have been deleted")
	})
}

// =============================================================================
// IsInvalidOfflineTransition
// =============================================================================

func TestIsInvalidOfflineTransition(t *testing.T) {
	tests := []struct {
		name      string
		current   common.AppState
		requested common.AppState
		appReq    common.AppState
		stage     common.Stage
		want      bool
	}{
		{"build request is always invalid offline", common.PRESENT, common.PRESENT, common.BUILT, common.DEV, true},
		{"prod not-installed non-removal needs internet", common.REMOVED, common.RUNNING, common.RUNNING, common.PROD, true},
		{"prod not-installed removal is allowed offline", common.REMOVED, common.REMOVED, common.REMOVED, common.PROD, false},
		{"publish is invalid offline", common.RUNNING, common.PUBLISHED, common.RUNNING, common.DEV, true},
		{"present-with-update is invalid offline", common.RUNNING, common.PRESENT, common.RUNNING, common.PROD, true},
		{"running dev stop is fine offline", common.RUNNING, common.PRESENT, common.RUNNING, common.DEV, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := builders.BuildApp("offline-"+tt.name, tt.current, tt.stage)
			app.RequestedState = tt.appReq

			payload := amPayload(1, "offline", tt.requested, tt.stage)
			payload.RequestedState = tt.requested
			// only the "present-with-update" case requires RequestUpdate.
			if tt.requested == common.PRESENT && tt.want && tt.current == common.RUNNING && tt.stage == common.PROD {
				payload.RequestUpdate = true
			}

			got := IsInvalidOfflineTransition(app, payload)
			assert.Equal(t, tt.want, got)
		})
	}
}

// =============================================================================
// UpdateTunnelState — publishes whatever the tunnel manager reports
// =============================================================================

func TestUpdateTunnelState(t *testing.T) {
	t.Run("propagates a GetState error", func(t *testing.T) {
		am, _, mt, _, _, _ := amHarness(t)

		boom := errors.New("frpc not running")
		mt.EXPECT().GetState().Return(nil, boom).Once()

		err := am.UpdateTunnelState()
		require.Error(t, err)
		assert.ErrorIs(t, err, boom)
	})

	t.Run("publishes the tunnel states to the backend", func(t *testing.T) {
		am, _, mt, _, msg, _ := amHarness(t)

		mt.EXPECT().GetState().Return(nil, nil).Once()

		err := am.UpdateTunnelState()
		require.NoError(t, err)

		// A publish happened (the tunnel-state update topic).
		assert.GreaterOrEqual(t, msg.GetPublishCount(), 1)
	})
}
