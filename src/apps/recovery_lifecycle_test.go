package apps

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	dockercontainer "github.com/docker/docker/api/types/container"

	"reagent/common"
	containerpkg "reagent/container"
	"reagent/testutil/builders"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// crashloop.go — backoff math + crash-loop registry bookkeeping
//
// These drive the pure backoff curve and the AppManager crash-loop registry
// (increment / clear) directly. The registry methods are reached through the
// amHarness AppManager from app_manager_test.go (which this file reuses).
// =============================================================================

func TestCalculateLoopSleepTime(t *testing.T) {
	t.Run("treats zero retries as one (non-zero, bounded)", func(t *testing.T) {
		d := calculateLoopSleepTime(0)
		// 5s * 1 * 1 with [0.8,1.2) jitter -> [4s, 6s).
		assert.GreaterOrEqual(t, d, 4*time.Second)
		assert.Less(t, d, 6*time.Second)
	})

	t.Run("grows quadratically for small retry counts", func(t *testing.T) {
		d := calculateLoopSleepTime(2)
		// 5s * 4 = 20s, jittered to [16s, 24s).
		assert.GreaterOrEqual(t, d, 16*time.Second)
		assert.Less(t, d, 24*time.Second)
	})

	t.Run("caps at roughly one hour (plus jitter) for large retry counts", func(t *testing.T) {
		d := calculateLoopSleepTime(100)
		// Capped at 1h before jitter; jitter widens it to [0.8h, 1.2h).
		assert.GreaterOrEqual(t, d, 48*time.Minute)
		assert.Less(t, d, 73*time.Minute)
	})

	t.Run("clamps retries above 100 to the cap as well", func(t *testing.T) {
		d := calculateLoopSleepTime(1000)
		assert.GreaterOrEqual(t, d, 48*time.Minute)
		assert.Less(t, d, 73*time.Minute)
	})
}

func TestCrashLoopRegistry(t *testing.T) {
	// NOTE: each crash loop spawns a single backoff goroutine that reads the
	// loop's fields without synchronization. To stay race-free we only call
	// incrementCrashLoop ONCE per app key, then immediately clearCrashLoop so the
	// goroutine takes its "loop no longer exists" exit path at the next wakeup.

	t.Run("incrementCrashLoop creates a new tracked loop", func(t *testing.T) {
		am, _, _, st, _, _ := amHarness(t)

		// Seed the app so the retry goroutine's GetApp lookup never nil-panics.
		amSeed(t, st, 1, "crasher", common.FAILED, common.PROD)

		payload := amPayload(1, "crasher", common.RUNNING, common.PROD)

		// incrementCrashLoop synchronously bumps the retry counter (retry() does
		// Retries++ before spawning its backoff goroutine), so even a brand-new
		// loop reports at least one retry, with a positive backoff sleep.
		retries, sleep := am.incrementCrashLoop(payload)
		assert.GreaterOrEqual(t, retries, uint(1), "a freshly-created loop has been retried once")
		assert.Greater(t, sleep, time.Duration(0))

		am.crashLoopLock.Lock()
		count := len(am.crashLoops)
		am.crashLoopLock.Unlock()
		assert.Equal(t, 1, count, "exactly one crash loop should be tracked")

		// Cleanup so the lingering retry goroutine exits at its next wakeup.
		am.clearCrashLoop(1, common.PROD)
	})

	t.Run("distinct apps get distinct tracked loops", func(t *testing.T) {
		am, _, _, st, _, _ := amHarness(t)

		amSeed(t, st, 3, "crash-a", common.FAILED, common.PROD)
		amSeed(t, st, 4, "crash-b", common.FAILED, common.PROD)

		am.incrementCrashLoop(amPayload(3, "crash-a", common.RUNNING, common.PROD))
		am.incrementCrashLoop(amPayload(4, "crash-b", common.RUNNING, common.PROD))

		am.crashLoopLock.Lock()
		count := len(am.crashLoops)
		am.crashLoopLock.Unlock()
		assert.Equal(t, 2, count, "two different apps should yield two crash loops")

		am.clearCrashLoop(3, common.PROD)
		am.clearCrashLoop(4, common.PROD)
	})

	t.Run("clearCrashLoop removes a tracked loop and is a no-op when none exists", func(t *testing.T) {
		am, _, _, st, _, _ := amHarness(t)
		amSeed(t, st, 2, "clearme", common.FAILED, common.PROD)

		payload := amPayload(2, "clearme", common.RUNNING, common.PROD)
		am.incrementCrashLoop(payload)

		am.crashLoopLock.Lock()
		require.Equal(t, 1, len(am.crashLoops))
		am.crashLoopLock.Unlock()

		am.clearCrashLoop(2, common.PROD)

		am.crashLoopLock.Lock()
		assert.Equal(t, 0, len(am.crashLoops))
		am.crashLoopLock.Unlock()

		// Clearing again (nothing to clear) is harmless.
		am.clearCrashLoop(2, common.PROD)
		am.clearCrashLoop(999, common.DEV)
	})
}

// =============================================================================
// uninstall_app.go — removeApp + UNINSTALLED + filesystem cleanup
// =============================================================================

func TestUninstallApp(t *testing.T) {
	t.Run("prod: removes container/image, sets UNINSTALLED, deletes data dir", func(t *testing.T) {
		sm, mc, st, msg := wiredStateMachine(t)

		// wiredStateMachine's container mock has no GetConfig wiring, but
		// uninstallApp calls GetConfig() for the data-dir paths, so add it.
		cfg := amTempConfig(t)
		mc.EXPECT().GetConfig().Return(cfg).Maybe()

		// Pre-create the data dir so RemoveAll has something to delete.
		dataDir := filepath.Join(cfg.CommandLineArguments.AppsDirectory, "prod", "uninstall-prod")
		require.NoError(t, os.MkdirAll(dataDir, 0o755))

		app := seedApp(t, st, "uninstall-prod", common.PRESENT, common.PROD)
		payload := execPayload("uninstall-prod", common.UNINSTALLED, common.PROD)

		// removeProdApp path: no live container -> only image removal.
		mc.EXPECT().
			GetContainer(mock.Anything, payload.ContainerName.Prod).
			Return(dockercontainer.Summary{}, notFoundErr()).
			Once()
		mc.EXPECT().
			RemoveImagesByName(mock.Anything, payload.RegistryImageName.Prod, mock.Anything).
			Return(nil).
			Once()

		err := sm.uninstallApp(payload, app)
		require.NoError(t, err)

		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.UNINSTALLED, final)

		// Data dir was removed.
		_, statErr := os.Stat(dataDir)
		assert.True(t, os.IsNotExist(statErr), "data dir should have been removed")

		states := remoteStatesFor(msg)
		assert.Contains(t, states, common.UNINSTALLED)
	})

	t.Run("aborts with the removeApp error before setting UNINSTALLED", func(t *testing.T) {
		sm, mc, st, _ := wiredStateMachine(t)
		cfg := amTempConfig(t)
		mc.EXPECT().GetConfig().Return(cfg).Maybe()

		app := seedApp(t, st, "uninstall-fail", common.PRESENT, common.PROD)
		payload := execPayload("uninstall-fail", common.UNINSTALLED, common.PROD)

		boom := errors.New("image removal failed")
		mc.EXPECT().
			GetContainer(mock.Anything, payload.ContainerName.Prod).
			Return(dockercontainer.Summary{}, notFoundErr()).
			Once()
		mc.EXPECT().
			RemoveImagesByName(mock.Anything, payload.RegistryImageName.Prod, mock.Anything).
			Return(boom).
			Once()

		err := sm.uninstallApp(payload, app)
		require.Error(t, err)
		assert.ErrorIs(t, err, boom)

		// removeApp set DELETING and never reached UNINSTALLED.
		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.NotEqual(t, common.UNINSTALLED, final)
	})

	t.Run("dev (non-compose): also removes the build zip", func(t *testing.T) {
		sm, mc, st, _ := wiredStateMachine(t)
		cfg := amTempConfig(t)
		mc.EXPECT().GetConfig().Return(cfg).Maybe()

		// Seed the build zip on disk.
		require.NoError(t, os.MkdirAll(cfg.CommandLineArguments.AppsBuildDir, 0o755))
		zipPath := filepath.Join(cfg.CommandLineArguments.AppsBuildDir,
			"uninstall-dev."+cfg.CommandLineArguments.CompressedBuildExtension)
		require.NoError(t, os.WriteFile(zipPath, []byte("zip"), 0o644))

		app := seedApp(t, st, "uninstall-dev", common.PRESENT, common.DEV)
		payload := execPayload("uninstall-dev", common.UNINSTALLED, common.DEV)

		// removeDevApp path: no live container -> only dev image removal.
		mc.EXPECT().
			GetContainer(mock.Anything, payload.ContainerName.Dev).
			Return(dockercontainer.Summary{}, notFoundErr()).
			Once()
		mc.EXPECT().
			RemoveImagesByName(mock.Anything, payload.RegistryImageName.Dev, mock.Anything).
			Return(nil).
			Once()

		err := sm.uninstallApp(payload, app)
		require.NoError(t, err)

		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.UNINSTALLED, final)

		_, statErr := os.Stat(zipPath)
		assert.True(t, os.IsNotExist(statErr), "build zip should have been removed")
	})
}

// =============================================================================
// fail_run.go — recoverFailToRunningHandler
// =============================================================================

func TestRecoverFailToRunningHandler(t *testing.T) {
	t.Run("non-compose prod: removes stale container then runs to RUNNING", func(t *testing.T) {
		sm, mc, st, _, _ := wiredRunBuildSM(t)

		app := seedApp(t, st, "failrun-prod", common.FAILED, common.PROD)
		payload := execPayload("failrun-prod", common.RUNNING, common.PROD)

		const containerID = "failrun-cid"

		// First: defensive force-remove of any existing container (by NAME).
		mc.EXPECT().
			RemoveContainerByID(mock.Anything, payload.ContainerName.Prod, map[string]interface{}{"force": true}).
			Return(nil).
			Once()

		// Then runApp -> runProdApp pipeline (image already present).
		mc.EXPECT().
			GetImage(mock.Anything, payload.RegistryImageName.Prod, mock.Anything).
			Return(containerpkg.ImageResult{}, nil).
			Once()
		mc.EXPECT().
			GetContainer(mock.Anything, payload.ContainerName.Prod).
			Return(dockercontainer.Summary{}, notFoundErr()).
			Once()
		mc.EXPECT().
			CreateContainer(mock.Anything, mock.Anything, mock.Anything, mock.Anything, payload.ContainerName.Prod).
			Return(containerID, nil).
			Once()
		mc.EXPECT().
			StartContainer(mock.Anything, containerID).
			Return(nil).
			Once()

		running, errC := fwdRunningSignal()
		mc.EXPECT().
			WaitForRunning(mock.Anything, containerID, mock.Anything).
			Return(running, errC).
			Once()

		fwdAllowLogs(mc)

		err := sm.recoverFailToRunningHandler(payload, app)
		require.NoError(t, err)

		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.RUNNING, final)
	})

	t.Run("unknown stage: removes stale container then runApp is a no-op", func(t *testing.T) {
		sm, mc, _, _, _ := wiredRunBuildSM(t)

		// In-memory app only (the DB CHECK allows DEV/PROD); the unknown-stage
		// path removes a container then no-ops in runApp without touching the store.
		app := builders.BuildApp("failrun-noop", common.FAILED, common.Stage(""))
		payload := execPayload("failrun-noop", common.RUNNING, common.Stage(""))
		payload.DockerCompose = nil
		// Unknown stage -> containerToRemove is the Prod name (else-branch).
		mc.EXPECT().
			RemoveContainerByID(mock.Anything, payload.ContainerName.Prod, map[string]interface{}{"force": true}).
			Return(nil).
			Once()

		err := sm.recoverFailToRunningHandler(payload, app)
		require.NoError(t, err)
	})
}

// =============================================================================
// fail_present.go — recoverFailToPresentHandler
// =============================================================================

func TestRecoverFailToPresentHandler(t *testing.T) {
	t.Run("prod with an existing image short-circuits to PRESENT", func(t *testing.T) {
		sm, mc, st, _, _ := wiredRunBuildSM(t)

		app := seedApp(t, st, "failpresent-prod", common.FAILED, common.PROD)
		payload := execPayload("failpresent-prod", common.PRESENT, common.PROD)

		// Defensive remove of any stale container (by name).
		mc.EXPECT().
			RemoveContainerByID(mock.Anything, payload.ContainerName.Prod, map[string]interface{}{"force": true}).
			Return(nil).
			Once()
		// GetImages reports the image is present -> setState(PRESENT), no pull.
		mc.EXPECT().
			GetImages(mock.Anything, payload.RegistryImageName.Prod).
			Return([]containerpkg.ImageResult{{}}, nil).
			Once()

		err := sm.recoverFailToPresentHandler(payload, app)
		require.NoError(t, err)

		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.PRESENT, final)
	})

	t.Run("prod with no local image falls through to pullApp", func(t *testing.T) {
		sm, mc, st, _, _ := wiredRunBuildSM(t)

		app := seedApp(t, st, "failpresent-pull", common.FAILED, common.PROD)
		payload := execPayload("failpresent-pull", common.PRESENT, common.PROD)
		payload.NewestVersion = "2.0.0"

		mc.EXPECT().
			RemoveContainerByID(mock.Anything, payload.ContainerName.Prod, map[string]interface{}{"force": true}).
			Return(nil).
			Once()
		// No image locally -> pullApp runs.
		mc.EXPECT().
			GetImages(mock.Anything, payload.RegistryImageName.Prod).
			Return([]containerpkg.ImageResult{}, nil).
			Once()
		mc.EXPECT().HandleRegistryLogins(mock.Anything).Return(nil).Once()
		mc.EXPECT().
			Pull(mock.Anything, mock.Anything, mock.Anything).
			Return(fwdDockerStream(), nil).
			Once()

		fwdAllowLogs(mc)

		err := sm.recoverFailToPresentHandler(payload, app)
		require.NoError(t, err)

		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.PRESENT, final)
	})

	t.Run("GetImages failure is propagated", func(t *testing.T) {
		sm, mc, st, _, _ := wiredRunBuildSM(t)

		app := seedApp(t, st, "failpresent-err", common.FAILED, common.PROD)
		payload := execPayload("failpresent-err", common.PRESENT, common.PROD)

		boom := errors.New("image inspect failed")
		mc.EXPECT().
			RemoveContainerByID(mock.Anything, payload.ContainerName.Prod, map[string]interface{}{"force": true}).
			Return(nil).
			Once()
		mc.EXPECT().
			GetImages(mock.Anything, payload.RegistryImageName.Prod).
			Return(nil, boom).
			Once()

		err := sm.recoverFailToPresentHandler(payload, app)
		require.Error(t, err)
		assert.ErrorIs(t, err, boom)
	})

	t.Run("dev: defensively removes then delegates to buildApp", func(t *testing.T) {
		sm, mc, st, _, _ := wiredRunBuildSM(t)

		app := seedApp(t, st, "failpresent-dev", common.FAILED, common.DEV)
		payload := execPayload("failpresent-dev", common.PRESENT, common.DEV)

		mc.EXPECT().
			RemoveContainerByID(mock.Anything, payload.ContainerName.Dev, map[string]interface{}{"force": true}).
			Return(nil).
			Once()
		// buildApp (dev) path.
		mc.EXPECT().
			Build(mock.Anything, mock.Anything, mock.Anything).
			Return(fwdDockerStream(`{"stream":"built"}`), nil).
			Once()

		fwdAllowLogs(mc)

		err := sm.recoverFailToPresentHandler(payload, app)
		require.NoError(t, err)

		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.BUILT, final)
	})
}

// =============================================================================
// update_app.go (non-compose) — guard rails + happy path
// =============================================================================

func TestUpdateApp(t *testing.T) {
	t.Run("rejects dev apps", func(t *testing.T) {
		sm, _, _, _, _ := wiredRunBuildSM(t)

		app := builders.BuildApp("update-dev", common.PRESENT, common.DEV)
		payload := execPayload("update-dev", common.PRESENT, common.DEV)

		err := sm.updateApp(payload, app)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot update dev app")
	})

	t.Run("rejects an update to the already-installed version", func(t *testing.T) {
		sm, _, st, _, _ := wiredRunBuildSM(t)

		app := seedApp(t, st, "update-same", common.PRESENT, common.PROD)
		app.StateLock.Lock()
		app.Version = "1.0.0"
		app.StateLock.Unlock()

		payload := execPayload("update-same", common.PRESENT, common.PROD)
		payload.NewestVersion = "1.0.0"

		err := sm.updateApp(payload, app)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already equal to the newest version")
	})

	t.Run("happy path: no running container -> pull, set PRESENT, remove old image", func(t *testing.T) {
		sm, mc, st, _, _ := wiredRunBuildSM(t)

		app := seedApp(t, st, "update-prod", common.PRESENT, common.PROD)
		app.StateLock.Lock()
		app.Version = "1.0.0"
		app.StateLock.Unlock()

		payload := execPayload("update-prod", common.PRESENT, common.PROD)
		payload.NewestVersion = "2.0.0"
		payload.PresentVersion = "1.0.0"

		// No existing container -> the remove/poll block is skipped.
		mc.EXPECT().
			GetContainer(mock.Anything, payload.ContainerName.Prod).
			Return(dockercontainer.Summary{}, notFoundErr()).
			Once()
		mc.EXPECT().HandleRegistryLogins(mock.Anything).Return(nil).Once()
		mc.EXPECT().
			Pull(mock.Anything, mock.Anything, mock.Anything).
			Return(fwdDockerStream(), nil).
			Once()
		// Old image cleanup (best-effort; return is ignored by the handler).
		mc.EXPECT().
			RemoveImageByName(mock.Anything, payload.RegistryImageName.Prod, "1.0.0", mock.Anything).
			Return(nil).
			Once()

		fwdAllowLogs(mc)

		err := sm.updateApp(payload, app)
		require.NoError(t, err)

		app.StateLock.Lock()
		final := app.CurrentState
		version := app.Version
		app.StateLock.Unlock()

		assert.Equal(t, common.PRESENT, final)
		assert.Equal(t, "2.0.0", version, "app version should be bumped to the newest")
	})

	t.Run("pull error aborts after UPDATING and before PRESENT", func(t *testing.T) {
		sm, mc, st, _, _ := wiredRunBuildSM(t)

		app := seedApp(t, st, "update-pullfail", common.PRESENT, common.PROD)
		app.StateLock.Lock()
		app.Version = "1.0.0"
		app.StateLock.Unlock()

		payload := execPayload("update-pullfail", common.PRESENT, common.PROD)
		payload.NewestVersion = "2.0.0"

		boom := errors.New("registry unreachable")
		// No GetContainer/RemoveContainerByID expectations: the pull runs
		// BEFORE the teardown, so a failed download must abort without the old
		// container ever being looked up — the strict mock enforces it.
		mc.EXPECT().HandleRegistryLogins(mock.Anything).Return(nil).Once()
		mc.EXPECT().
			Pull(mock.Anything, mock.Anything, mock.Anything).
			Return(nil, boom).
			Once()

		fwdAllowLogs(mc)

		err := sm.updateApp(payload, app)
		require.Error(t, err)
		assert.ErrorIs(t, err, boom)

		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.NotEqual(t, common.PRESENT, final)
	})

	t.Run("getUpdateTransition returns the updateApp handler", func(t *testing.T) {
		sm, _, _, _, _ := wiredRunBuildSM(t)
		app := builders.BuildApp("update-lookup", common.PRESENT, common.PROD)
		payload := execPayload("update-lookup", common.PRESENT, common.PROD)

		fn := sm.getUpdateTransition(payload, app)
		assert.NotNil(t, fn)
	})
}
