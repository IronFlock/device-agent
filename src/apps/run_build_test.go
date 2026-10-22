package apps

import (
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dockercontainer "github.com/docker/docker/api/types/container"

	"reagent/common"
	"reagent/config"
	containerpkg "reagent/container"
	"reagent/errdefs"
	"reagent/logging"
	"reagent/store"
	"reagent/testutil/builders"
	"reagent/testutil/fakes"
	"reagent/testutil/mocks"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Forward build/run pipeline harness
//
// These tests drive the *non-compose* forward lifecycle handlers (runApp /
// pullApp / buildApp / publishApp and their compositions) end-to-end against a
// real in-memory store + LogManager and a strict mocks.Container.
//
// The wiredStateMachine / seedApp / execPayload / notFoundErr / remoteStatesFor
// helpers live in transition_execution_test.go and are reused here. This file
// only adds NEW, uniquely-named helpers (prefixed runbuild* / fwd*).
// =============================================================================

// runbuildTempConfig returns a *config.Config whose Apps* directories all point
// at fresh temp dirs, so the handlers' filesystem side-effects (env-var files,
// bind-mount dirs) land somewhere writable and isolated. The container mock's
// GetConfig() is wired to return this.
func runbuildTempConfig(t *testing.T) *config.Config {
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

// wiredRunBuildSM mirrors wiredStateMachine but additionally wires GetConfig()
// (used by computeContainerConfigs / HandleRegistryLoginsWithDefault /
// SetupComposeFiles) to a temp-dir-backed config, and returns that config so a
// test can read back the dirs if needed. GetConfig is registered .Maybe()
// because not every handler reaches it.
func wiredRunBuildSM(t *testing.T) (*StateMachine, *mocks.Container, *store.AppStore, *fakes.Messenger, *config.Config) {
	t.Helper()

	mockContainer := mocks.NewContainer(t)
	db := newExecTestDB(t)

	// The forward stream pipelines (build/pull/push) drive LogManager.emitStream,
	// whose deferred cleanup spawns fire-and-forget goroutines that write log
	// history back to the DB. newExecTestDB registers a db.Close() cleanup;
	// because t.Cleanup runs LIFO, this drain — registered *after* it — runs
	// *before* the close, giving those goroutines a moment to finish so they
	// never hit a closed DB (which would os.Exit via safe.Go's log.Fatal).
	t.Cleanup(func() { time.Sleep(250 * time.Millisecond) })

	msg := fakes.NewMessenger()
	cfg := runbuildTempConfig(t)

	mockContainer.EXPECT().GetConfig().Return(cfg).Maybe()

	appStore := store.NewAppStore(db, msg)
	observer := NewObserver(mockContainer, &appStore, nil)
	logManager := logging.NewLogManager(mockContainer, msg, db, appStore)

	sm := NewStateMachine(mockContainer, &logManager, &observer, nil)

	return &sm, mockContainer, &appStore, msg, cfg
}

// fwdDockerStream builds an io.ReadCloser over a small newline-delimited docker
// JSON progress stream. The LogManager scans it line by line; the LAST line must
// NOT be a JSON {"error": ...} object or emitStream treats the whole stream as a
// failure. These lines are benign status messages.
func fwdDockerStream(lines ...string) io.ReadCloser {
	if len(lines) == 0 {
		lines = []string{
			`{"status":"Pulling from library/test"}`,
			`{"status":"Download complete"}`,
		}
	}
	return io.NopCloser(strings.NewReader(strings.Join(lines, "\n") + "\n"))
}

// fwdErrorStream builds a docker stream whose final line is an {"error": ...}
// chunk, which emitStream surfaces as a non-canceled stream error.
func fwdErrorStream(msg string) io.ReadCloser {
	return io.NopCloser(strings.NewReader(
		`{"status":"working"}` + "\n" + `{"error":"` + msg + `"}` + "\n"))
}

// fwdAllowLogs registers Logs() as an always-allowed no-op. The forward run
// pipelines fire an async LogManager.Stream() at the end (and emitStream's
// cleanup goroutine may call Logs too); since the strict mock would fail on an
// unexpected Logs call, allow it to return an empty reader.
func fwdAllowLogs(mc *mocks.Container) {
	mc.EXPECT().
		Logs(mock.Anything, mock.Anything, mock.Anything).
		Return(io.NopCloser(strings.NewReader("")), nil).
		Maybe()
}

// fwdRunningSignal returns the (runningSignal, errC) pair a happy-path
// WaitForRunning should yield: a buffered struct{} channel already carrying a
// value, and an empty (never-fired) error channel so the select picks running.
func fwdRunningSignal() (<-chan struct{}, <-chan error) {
	running := make(chan struct{}, 1)
	running <- struct{}{}
	errC := make(chan error) // never receives -> select takes running
	return running, errC
}

// fwdFailSignal returns a WaitForRunning result that reports the container
// exited/failed: an empty running channel and an error channel carrying err.
func fwdFailSignal(err error) (<-chan struct{}, <-chan error) {
	running := make(chan struct{}) // never receives
	errC := make(chan error, 1)
	errC <- err
	return running, errC
}

// =============================================================================
// runProdApp (non-compose) — pull-already-present -> create -> start -> running
// =============================================================================

func TestRunProdApp(t *testing.T) {
	t.Run("happy path: create, start, wait-for-running ends RUNNING", func(t *testing.T) {
		sm, mc, st, _, _ := wiredRunBuildSM(t)

		app := seedApp(t, st, "run-prod", common.PRESENT, common.PROD)
		payload := execPayload("run-prod", common.RUNNING, common.PROD)

		const containerID = "run-cid"

		// computeContainerConfigs (PROD) probes for a local image; nil error means
		// "already present", so no internal pull happens.
		mc.EXPECT().
			GetImage(mock.Anything, payload.RegistryImageName.Prod, mock.Anything).
			Return(containerpkg.ImageResult{}, nil).
			Once()

		// createContainer: no existing container -> create a fresh one.
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

		err := sm.runProdApp(payload, app)
		require.NoError(t, err)

		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.RUNNING, final)

		// Persisted RUNNING. (The remote Notify pushes for STARTING/RUNNING happen
		// synchronously via setState; we don't read msg.CallCalls here because the
		// handler's trailing async LogManager.Stream() writes to it concurrently.)
		fromDB, dberr := st.GetApp(1, common.PROD)
		require.NoError(t, dberr)
		require.NotNil(t, fromDB)
		assert.Equal(t, common.RUNNING, fromDB.CurrentState)
	})

	t.Run("reuses an existing container (skips create) and ends RUNNING", func(t *testing.T) {
		sm, mc, st, _, _ := wiredRunBuildSM(t)

		app := seedApp(t, st, "run-prod-existing", common.PRESENT, common.PROD)
		payload := execPayload("run-prod-existing", common.RUNNING, common.PROD)

		const containerID = "existing-cid"

		mc.EXPECT().
			GetImage(mock.Anything, payload.RegistryImageName.Prod, mock.Anything).
			Return(containerpkg.ImageResult{}, nil).
			Once()
		// GetContainer returns a real container -> createContainer returns its ID
		// without calling CreateContainer.
		mc.EXPECT().
			GetContainer(mock.Anything, payload.ContainerName.Prod).
			Return(dockercontainer.Summary{ID: containerID}, nil).
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

		err := sm.runProdApp(payload, app)
		require.NoError(t, err)

		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.RUNNING, final)
	})

	t.Run("create error aborts before start and stays STARTING", func(t *testing.T) {
		sm, mc, st, msg, _ := wiredRunBuildSM(t)

		app := seedApp(t, st, "run-prod-createfail", common.PRESENT, common.PROD)
		payload := execPayload("run-prod-createfail", common.RUNNING, common.PROD)

		boom := errors.New("create blew up")

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
			Return("", boom).
			Once()

		err := sm.runProdApp(payload, app)
		require.Error(t, err)
		assert.ErrorIs(t, err, boom)

		// setState(STARTING) ran, but the container never started -> never RUNNING.
		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.STARTING, final)

		states := remoteStatesFor(msg)
		assert.Contains(t, states, common.STARTING)
		assert.NotContains(t, states, common.RUNNING)
	})

	t.Run("nvidia start failure retries without nvidia, second start error aborts at STARTING", func(t *testing.T) {
		sm, mc, st, _, _ := wiredRunBuildSM(t)

		app := seedApp(t, st, "run-prod-startfail", common.PRESENT, common.PROD)
		payload := execPayload("run-prod-startfail", common.RUNNING, common.PROD)

		const containerID = "startfail-cid"
		// A plain (non-nvidia) StartContainer error is swallowed by startContainer,
		// so to exercise a *real* start failure we drive the nvidia retry branch:
		// the first start fails with an nvidia error, triggering remove + recreate +
		// retry, and the retry then fails for good.
		nvidiaErr := errors.New("could not select device driver with capabilities: nvidia")
		retryErr := errors.New("start still refused")

		mc.EXPECT().
			GetImage(mock.Anything, payload.RegistryImageName.Prod, mock.Anything).
			Return(containerpkg.ImageResult{}, nil).
			Once()
		// createContainer is invoked twice (initial + nvidia-retry recreate).
		mc.EXPECT().
			GetContainer(mock.Anything, payload.ContainerName.Prod).
			Return(dockercontainer.Summary{}, notFoundErr()).
			Twice()
		mc.EXPECT().
			CreateContainer(mock.Anything, mock.Anything, mock.Anything, mock.Anything, payload.ContainerName.Prod).
			Return(containerID, nil).
			Twice()
		// Remove between the failed start and the recreate.
		mc.EXPECT().
			RemoveContainerByID(mock.Anything, containerID, mock.Anything).
			Return(nil).
			Once()
		// First start fails with nvidia error -> retry; retry start fails for good.
		mc.EXPECT().
			StartContainer(mock.Anything, containerID).
			Return(nvidiaErr).
			Once()
		mc.EXPECT().
			StartContainer(mock.Anything, containerID).
			Return(retryErr).
			Once()

		err := sm.runProdApp(payload, app)
		require.Error(t, err)
		assert.ErrorIs(t, err, retryErr)

		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.STARTING, final)
	})

	t.Run("container exits during wait-for-running: returns the failure", func(t *testing.T) {
		sm, mc, st, _, _ := wiredRunBuildSM(t)

		app := seedApp(t, st, "run-prod-exit", common.PRESENT, common.PROD)
		payload := execPayload("run-prod-exit", common.RUNNING, common.PROD)

		const containerID = "exit-cid"
		failure := errors.New("container exited with code 1")

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

		running, errC := fwdFailSignal(failure)
		mc.EXPECT().
			WaitForRunning(mock.Anything, containerID, mock.Anything).
			Return(running, errC).
			Once()

		// On failure the handler fetches the container Logs to stream them out.
		mc.EXPECT().
			Logs(mock.Anything, containerID, mock.Anything).
			Return(fwdDockerStream(`app crashed`), nil).
			Once()

		err := sm.runProdApp(payload, app)
		require.Error(t, err)
		assert.ErrorIs(t, err, failure)

		// Never reached RUNNING.
		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.STARTING, final)
	})
}

// =============================================================================
// runDevApp (non-compose) — remove stale -> create -> start -> running
// =============================================================================

func TestRunDevApp(t *testing.T) {
	t.Run("happy path: no stale container, create+start ends RUNNING", func(t *testing.T) {
		sm, mc, st, _, _ := wiredRunBuildSM(t)

		app := seedApp(t, st, "run-dev", common.PRESENT, common.DEV)
		payload := execPayload("run-dev", common.RUNNING, common.DEV)

		const containerID = "run-dev-cid"

		// First GetContainer: the "remove stale container" probe -> not found, so
		// the removal block is skipped.
		// Second GetContainer: inside createContainer -> not found -> create fresh.
		mc.EXPECT().
			GetContainer(mock.Anything, payload.ContainerName.Dev).
			Return(dockercontainer.Summary{}, notFoundErr()).
			Twice()
		mc.EXPECT().
			CreateContainer(mock.Anything, mock.Anything, mock.Anything, mock.Anything, payload.ContainerName.Dev).
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

		err := sm.runDevApp(payload, app)
		require.NoError(t, err)

		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.RUNNING, final)

		// (No remoteStatesFor read here: the trailing async LogManager.Stream()
		// writes to msg.CallCalls concurrently.)
		fromDB, dberr := st.GetApp(1, common.DEV)
		require.NoError(t, dberr)
		require.NotNil(t, fromDB)
		assert.Equal(t, common.RUNNING, fromDB.CurrentState)
	})

	t.Run("removes a stale container before recreating", func(t *testing.T) {
		sm, mc, st, _, _ := wiredRunBuildSM(t)

		app := seedApp(t, st, "run-dev-stale", common.PRESENT, common.DEV)
		payload := execPayload("run-dev-stale", common.RUNNING, common.DEV)

		const staleID = "stale-cid"
		const newID = "fresh-cid"

		// First GetContainer finds a stale container -> remove + wait-for-removed.
		mc.EXPECT().
			GetContainer(mock.Anything, payload.ContainerName.Dev).
			Return(dockercontainer.Summary{ID: staleID}, nil).
			Once()
		mc.EXPECT().
			RemoveContainerByID(mock.Anything, staleID, mock.Anything).
			Return(nil).
			Once()
		mc.EXPECT().
			WaitForContainerByID(mock.Anything, staleID, mock.Anything).
			Return(int64(0), notFoundErr()).
			Once()
		// Second GetContainer (inside createContainer) -> not found -> create fresh.
		mc.EXPECT().
			GetContainer(mock.Anything, payload.ContainerName.Dev).
			Return(dockercontainer.Summary{}, notFoundErr()).
			Once()
		mc.EXPECT().
			CreateContainer(mock.Anything, mock.Anything, mock.Anything, mock.Anything, payload.ContainerName.Dev).
			Return(newID, nil).
			Once()
		mc.EXPECT().
			StartContainer(mock.Anything, newID).
			Return(nil).
			Once()

		running, errC := fwdRunningSignal()
		mc.EXPECT().
			WaitForRunning(mock.Anything, newID, mock.Anything).
			Return(running, errC).
			Once()

		fwdAllowLogs(mc)

		err := sm.runDevApp(payload, app)
		require.NoError(t, err)

		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.RUNNING, final)
	})
}

// =============================================================================
// runApp dispatch — unknown stage is a no-op
// =============================================================================

func TestRunAppDispatch(t *testing.T) {
	t.Run("unknown stage is a no-op and never touches the container", func(t *testing.T) {
		sm, _, _, _, _ := wiredRunBuildSM(t)

		app := builders.BuildApp("run-noop", common.PRESENT, common.Stage(""))
		payload := execPayload("run-noop", common.RUNNING, common.Stage(""))

		err := sm.runApp(payload, app)
		require.NoError(t, err)
	})
}

// =============================================================================
// removedToRunning / removedToPresent — thin dispatchers over runApp / pullApp
// =============================================================================

func TestRemovedToTransitions(t *testing.T) {
	t.Run("removedToPresent on a dev app is a no-op transition", func(t *testing.T) {
		sm, _, _, _, _ := wiredRunBuildSM(t)

		app := builders.BuildApp("rmtopresent-dev", common.REMOVED, common.DEV)
		payload := execPayload("rmtopresent-dev", common.PRESENT, common.DEV)

		err := sm.removedToPresent(payload, app)
		require.Error(t, err)
		assert.True(t, errdefs.IsNoActionTransition(err))
	})

	t.Run("removedToRunning dispatches into runApp (unknown stage no-op)", func(t *testing.T) {
		sm, _, _, _, _ := wiredRunBuildSM(t)

		app := builders.BuildApp("rmtorunning", common.REMOVED, common.Stage(""))
		payload := execPayload("rmtorunning", common.RUNNING, common.Stage(""))

		err := sm.removedToRunning(payload, app)
		require.NoError(t, err)
	})
}

// =============================================================================
// pullApp (non-compose, prod) — auth -> pull -> stream -> PRESENT
// =============================================================================

func TestPullApp(t *testing.T) {
	t.Run("happy path: pulls, streams, persists PRESENT", func(t *testing.T) {
		sm, mc, st, msg, _ := wiredRunBuildSM(t)

		app := seedApp(t, st, "pull-prod", common.REMOVED, common.PROD)
		payload := execPayload("pull-prod", common.PRESENT, common.PROD)
		payload.NewestVersion = "2.0.0"

		mc.EXPECT().HandleRegistryLogins(mock.Anything).Return(nil).Once()
		mc.EXPECT().
			Pull(mock.Anything, mock.Anything, mock.Anything).
			Return(fwdDockerStream(), nil).
			Once()

		fwdAllowLogs(mc)

		err := sm.pullApp(payload, app)
		require.NoError(t, err)

		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.PRESENT, final)

		states := remoteStatesFor(msg)
		assert.Contains(t, states, common.DOWNLOADING)
		assert.Contains(t, states, common.PRESENT)
	})

	t.Run("rejects dev apps without touching the container", func(t *testing.T) {
		sm, _, _, _, _ := wiredRunBuildSM(t)

		app := builders.BuildApp("pull-dev", common.REMOVED, common.DEV)
		payload := execPayload("pull-dev", common.PRESENT, common.DEV)

		err := sm.pullApp(payload, app)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot pull dev apps")
	})

	t.Run("pull error is returned and state never advances to PRESENT", func(t *testing.T) {
		sm, mc, st, _, _ := wiredRunBuildSM(t)

		app := seedApp(t, st, "pull-prod-err", common.REMOVED, common.PROD)
		payload := execPayload("pull-prod-err", common.PRESENT, common.PROD)

		boom := errors.New("registry unreachable")

		mc.EXPECT().HandleRegistryLogins(mock.Anything).Return(nil).Once()
		mc.EXPECT().
			Pull(mock.Anything, mock.Anything, mock.Anything).
			Return(nil, boom).
			Once()

		err := sm.pullApp(payload, app)
		require.Error(t, err)
		assert.ErrorIs(t, err, boom)

		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.NotEqual(t, common.PRESENT, final)
	})

	t.Run("registry login failure aborts before pull", func(t *testing.T) {
		sm, mc, st, _, _ := wiredRunBuildSM(t)

		app := seedApp(t, st, "pull-prod-auth", common.REMOVED, common.PROD)
		payload := execPayload("pull-prod-auth", common.PRESENT, common.PROD)

		boom := errors.New("bad credentials")
		mc.EXPECT().HandleRegistryLogins(mock.Anything).Return(boom).Once()

		err := sm.pullApp(payload, app)
		require.Error(t, err)
		assert.ErrorIs(t, err, boom)
	})

	t.Run("canceled pull stream is surfaced as a stream-canceled error", func(t *testing.T) {
		sm, mc, st, _, _ := wiredRunBuildSM(t)

		app := seedApp(t, st, "pull-prod-cancel", common.REMOVED, common.PROD)
		payload := execPayload("pull-prod-cancel", common.PRESENT, common.PROD)

		mc.EXPECT().HandleRegistryLogins(mock.Anything).Return(nil).Once()
		// A stream that yields a "use of closed network connection" scanner error
		// is mapped to DockerStreamCanceled by emitStream.
		mc.EXPECT().
			Pull(mock.Anything, mock.Anything, mock.Anything).
			Return(fwdCanceledStream(), nil).
			Once()

		fwdAllowLogs(mc)

		err := sm.pullApp(payload, app)
		require.Error(t, err)
		assert.True(t, errdefs.IsDockerStreamCanceled(err), "expected a DockerStreamCanceled error, got %v", err)

		// On cancel the handler does not advance to PRESENT.
		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.NotEqual(t, common.PRESENT, final)
	})
}

// fwdCanceledStream returns a reader whose Read fails with a "use of closed
// network connection" error, which emitStream maps to DockerStreamCanceled.
func fwdCanceledStream() io.ReadCloser {
	return io.NopCloser(&fwdClosedConnReader{})
}

type fwdClosedConnReader struct{ done bool }

func (r *fwdClosedConnReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, errors.New("read tcp 1.2.3.4:5->6.7.8.9:10: use of closed network connection")
	}
	// emit one benign line first, then fail on the next read so the scanner
	// surfaces the connection error.
	r.done = true
	n := copy(p, []byte("{\"status\":\"downloading\"}\n"))
	return n, nil
}

// =============================================================================
// pullAndRunApp wrappers
// =============================================================================

func TestRemovedToRunningPullThenRun(t *testing.T) {
	// removedToRunning -> runApp; for a prod app with image already present this
	// exercises the full pull-skipped -> create -> start -> running pipeline.
	t.Run("prod removed->running runs the create/start pipeline", func(t *testing.T) {
		sm, mc, st, _, _ := wiredRunBuildSM(t)

		app := seedApp(t, st, "rmrun-prod", common.PRESENT, common.PROD)
		payload := execPayload("rmrun-prod", common.RUNNING, common.PROD)

		const containerID = "rmrun-cid"

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

		err := sm.removedToRunning(payload, app)
		require.NoError(t, err)

		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.RUNNING, final)

		fromDB, dberr := st.GetApp(1, common.PROD)
		require.NoError(t, dberr)
		require.NotNil(t, fromDB)
		assert.Equal(t, common.RUNNING, fromDB.CurrentState)
	})
}

// =============================================================================
// buildApp (non-compose, dev) — build -> stream -> BUILT
// =============================================================================

func TestBuildApp(t *testing.T) {
	t.Run("rejects non-dev (prod) apps", func(t *testing.T) {
		sm, _, _, _, _ := wiredRunBuildSM(t)

		app := builders.BuildApp("build-prod", common.REMOVED, common.PROD)
		payload := execPayload("build-prod", common.BUILT, common.PROD)

		err := sm.buildApp(payload, app)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "can only build dev apps")
	})

	t.Run("happy path: builds, streams, persists BUILT", func(t *testing.T) {
		sm, mc, st, msg, _ := wiredRunBuildSM(t)

		app := seedApp(t, st, "build-dev", common.REMOVED, common.DEV)
		payload := execPayload("build-dev", common.BUILT, common.DEV)

		mc.EXPECT().
			Build(mock.Anything, mock.Anything, mock.Anything).
			Return(fwdDockerStream(`{"stream":"Step 1/1 : FROM scratch"}`, `{"stream":"Successfully built abc123"}`), nil).
			Once()

		fwdAllowLogs(mc)

		err := sm.buildApp(payload, app)
		require.NoError(t, err)

		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.BUILT, final)

		states := remoteStatesFor(msg)
		assert.Contains(t, states, common.REMOVED)
		assert.Contains(t, states, common.BUILDING)
		assert.Contains(t, states, common.BUILT)
	})

	t.Run("build error aborts and never reaches BUILT", func(t *testing.T) {
		sm, mc, st, _, _ := wiredRunBuildSM(t)

		app := seedApp(t, st, "build-dev-err", common.REMOVED, common.DEV)
		payload := execPayload("build-dev-err", common.BUILT, common.DEV)

		boom := errors.New("docker build failed")
		mc.EXPECT().
			Build(mock.Anything, mock.Anything, mock.Anything).
			Return(nil, boom).
			Once()

		fwdAllowLogs(mc)

		err := sm.buildApp(payload, app)
		require.Error(t, err)
		assert.ErrorIs(t, err, boom)

		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.NotEqual(t, common.BUILT, final)
	})

	t.Run("error chunk in the build stream surfaces as a build failure", func(t *testing.T) {
		sm, mc, st, _, _ := wiredRunBuildSM(t)

		app := seedApp(t, st, "build-dev-streamerr", common.REMOVED, common.DEV)
		payload := execPayload("build-dev-streamerr", common.BUILT, common.DEV)

		mc.EXPECT().
			Build(mock.Anything, mock.Anything, mock.Anything).
			Return(fwdErrorStream("compilation error"), nil).
			Once()

		fwdAllowLogs(mc)

		err := sm.buildApp(payload, app)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "compilation error")

		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.NotEqual(t, common.BUILT, final)
	})
}

// =============================================================================
// publishApp (non-compose, dev) — build -> tag -> push -> PUBLISHED
// =============================================================================

func TestPublishApp(t *testing.T) {
	t.Run("happy path: builds, tags, pushes, persists PUBLISHED", func(t *testing.T) {
		sm, mc, st, msg, _ := wiredRunBuildSM(t)

		app := seedApp(t, st, "publish-dev", common.REMOVED, common.DEV)
		payload := execPayload("publish-dev", common.PUBLISHED, common.DEV)
		payload.PublishContainerName = "pub_1_publish-dev"

		// buildDevApp (releaseBuild=true) runs first.
		mc.EXPECT().
			Build(mock.Anything, mock.Anything, mock.Anything).
			Return(fwdDockerStream(`{"stream":"Successfully built abc"}`), nil).
			Once()

		mc.EXPECT().
			Tag(mock.Anything, payload.RegistryImageName.Dev, mock.Anything).
			Return(nil).
			Once()
		mc.EXPECT().HandleRegistryLogins(mock.Anything).Return(nil).Once()
		mc.EXPECT().
			Push(mock.Anything, mock.Anything, mock.Anything).
			Return(fwdDockerStream(`{"status":"Pushed"}`), nil).
			Once()

		fwdAllowLogs(mc)

		err := sm.publishApp(payload, app)
		require.NoError(t, err)

		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.PUBLISHED, final)

		states := remoteStatesFor(msg)
		assert.Contains(t, states, common.BUILT)
		assert.Contains(t, states, common.PUBLISHING)
		assert.Contains(t, states, common.PUBLISHED)
	})

	t.Run("tag error aborts before publishing", func(t *testing.T) {
		sm, mc, st, _, _ := wiredRunBuildSM(t)

		app := seedApp(t, st, "publish-dev-tagfail", common.REMOVED, common.DEV)
		payload := execPayload("publish-dev-tagfail", common.PUBLISHED, common.DEV)
		payload.PublishContainerName = "pub_1_publish-dev-tagfail"

		boom := errors.New("tag failed")
		mc.EXPECT().
			Build(mock.Anything, mock.Anything, mock.Anything).
			Return(fwdDockerStream(`{"stream":"built"}`), nil).
			Once()
		mc.EXPECT().
			Tag(mock.Anything, payload.RegistryImageName.Dev, mock.Anything).
			Return(boom).
			Once()

		fwdAllowLogs(mc)

		err := sm.publishApp(payload, app)
		require.Error(t, err)
		assert.ErrorIs(t, err, boom)

		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.NotEqual(t, common.PUBLISHED, final)
	})

	t.Run("push error aborts after publishing state was set", func(t *testing.T) {
		sm, mc, st, _, _ := wiredRunBuildSM(t)

		app := seedApp(t, st, "publish-dev-pushfail", common.REMOVED, common.DEV)
		payload := execPayload("publish-dev-pushfail", common.PUBLISHED, common.DEV)
		payload.PublishContainerName = "pub_1_publish-dev-pushfail"

		boom := errors.New("push rejected")
		mc.EXPECT().
			Build(mock.Anything, mock.Anything, mock.Anything).
			Return(fwdDockerStream(`{"stream":"built"}`), nil).
			Once()
		mc.EXPECT().
			Tag(mock.Anything, payload.RegistryImageName.Dev, mock.Anything).
			Return(nil).
			Once()
		mc.EXPECT().HandleRegistryLogins(mock.Anything).Return(nil).Once()
		mc.EXPECT().
			Push(mock.Anything, mock.Anything, mock.Anything).
			Return(nil, boom).
			Once()

		fwdAllowLogs(mc)

		err := sm.publishApp(payload, app)
		require.Error(t, err)
		assert.ErrorIs(t, err, boom)

		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.PUBLISHING, final)
	})
}

// =============================================================================
// stopAndBuildApp / removeAndPublishApp — compositions of stop/remove + build
// =============================================================================

func TestStopAndBuildApp(t *testing.T) {
	t.Run("dev: stops the container then builds to BUILT", func(t *testing.T) {
		sm, mc, st, msg, _ := wiredRunBuildSM(t)

		app := seedApp(t, st, "stopbuild-dev", common.RUNNING, common.DEV)
		payload := execPayload("stopbuild-dev", common.BUILT, common.DEV)

		const containerID = "stopbuild-cid"

		// stopDevApp (non-compose) path.
		mc.EXPECT().
			GetContainer(mock.Anything, payload.ContainerName.Dev).
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

		errC := make(chan error, 1)
		errC <- notFoundErr()
		close(errC)
		mc.EXPECT().
			PollContainerState(mock.Anything, containerID, mock.Anything).
			Return(nil, errC).
			Once()

		// buildDevApp path.
		mc.EXPECT().
			Build(mock.Anything, mock.Anything, mock.Anything).
			Return(fwdDockerStream(`{"stream":"built"}`), nil).
			Once()

		fwdAllowLogs(mc)

		err := sm.stopAndBuildApp(payload, app)
		require.NoError(t, err)

		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.BUILT, final)

		states := remoteStatesFor(msg)
		assert.Contains(t, states, common.STOPPING)
		assert.Contains(t, states, common.BUILT)
	})
}

func TestRemoveAndPublishApp(t *testing.T) {
	t.Run("prod stage is a no-op (only dev apps are removed+published)", func(t *testing.T) {
		// A strict mock with no expectations proves no container interaction.
		sm, _, _, _, _ := wiredRunBuildSM(t)

		app := builders.BuildApp("rmpub-prod", common.RUNNING, common.PROD)
		payload := execPayload("rmpub-prod", common.PUBLISHED, common.PROD)

		err := sm.removeAndPublishApp(payload, app)
		require.NoError(t, err)
	})

	t.Run("dev: removes then publishes to PUBLISHED", func(t *testing.T) {
		sm, mc, st, msg, _ := wiredRunBuildSM(t)

		app := seedApp(t, st, "rmpub-dev", common.RUNNING, common.DEV)
		payload := execPayload("rmpub-dev", common.PUBLISHED, common.DEV)
		payload.PublishContainerName = "pub_1_rmpub-dev"

		const containerID = "rmpub-cid"

		// removeDevApp path.
		mc.EXPECT().
			GetContainer(mock.Anything, payload.ContainerName.Dev).
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
			RemoveImagesByName(mock.Anything, payload.RegistryImageName.Dev, mock.Anything).
			Return(nil).
			Once()

		// publishApp path: build -> tag -> push.
		mc.EXPECT().
			Build(mock.Anything, mock.Anything, mock.Anything).
			Return(fwdDockerStream(`{"stream":"built"}`), nil).
			Once()
		mc.EXPECT().
			Tag(mock.Anything, payload.RegistryImageName.Dev, mock.Anything).
			Return(nil).
			Once()
		mc.EXPECT().HandleRegistryLogins(mock.Anything).Return(nil).Once()
		mc.EXPECT().
			Push(mock.Anything, mock.Anything, mock.Anything).
			Return(fwdDockerStream(`{"status":"Pushed"}`), nil).
			Once()

		fwdAllowLogs(mc)

		err := sm.removeAndPublishApp(payload, app)
		require.NoError(t, err)

		app.StateLock.Lock()
		final := app.CurrentState
		app.StateLock.Unlock()
		assert.Equal(t, common.PUBLISHED, final)

		states := remoteStatesFor(msg)
		assert.Contains(t, states, common.DELETING)
		assert.Contains(t, states, common.PUBLISHED)
	})
}
