package apps

import (
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"

	"reagent/common"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Crashloop wake-up decisions & teardown-vs-lock behavior
//
// Field incident behind these tests: a compose app crashlooping on an
// unreachable registry could not be uninstalled for 40+ minutes. Two mechanisms
// conspired:
//
//  1. The crashloop retried the RUNNING payload it captured when the app first
//     failed, never noticing the requested state had since changed to
//     UNINSTALLED — so it re-drove RUNNING forever.
//  2. The genuine UNINSTALLED pushes arrived while a retry's transition held
//     the app's transition lock (each attempt spent 10s in a registry-login
//     timeout) and were silently dropped by RequestAppState.
//
// A third, smaller defect made the loop noisier: a crashloop goroutine woken
// from its backoff re-found "its" task by app key alone, so a goroutine whose
// loop had been cleared latched onto a NEWER loop for the same app and
// double-drove its retries.
//
// Helpers (amHarness, amSeed, amPayload, notFoundErr, fwdAllowLogs) come from
// the other files in this package.
// =============================================================================

// crashSeedTeardownRow persists the app's requested state as UNINSTALLED (both
// in memory and in the requested-state row), exactly as CreateOrUpdateApp does
// when an uninstall push arrives.
func crashSeedTeardownRow(t *testing.T, am *AppManager, app *common.App, payload common.TransitionPayload) {
	t.Helper()

	app.StateLock.Lock()
	app.RequestedState = payload.RequestedState
	app.StateLock.Unlock()

	require.NoError(t, am.AppStore.UpdateLocalRequestedState(payload))
}

func TestCrashLoopWakeExitsWhenItsTaskWasReplaced(t *testing.T) {
	am, _, _, st, _, _ := amHarness(t)

	amSeed(t, st, 71, "ghost", common.FAILED, common.PROD)

	// The ghost goroutine's task was cleared while it slept, and a NEW loop for
	// the same app was created since (same app key, different task).
	ghostPayload := amPayload(71, "ghost", common.RUNNING, common.PROD)
	ghostPayload.Retrying = true
	ghost := &CrashLoop{Payload: ghostPayload, Retries: 11}

	replacementPayload := amPayload(71, "ghost", common.RUNNING, common.PROD)
	replacementPayload.Retrying = true
	replacement := &CrashLoop{Payload: replacementPayload, Retries: 2}

	am.crashLoopLock.Lock()
	am.crashLoops[replacement] = struct{}{}
	am.crashLoopLock.Unlock()

	// No container or tunnel expectations at all: the strict mocks prove the
	// ghost drives nothing. The old key-based lookup would have matched the
	// replacement task and re-driven the RUNNING payload here.
	am.crashLoopWake(ghost)

	am.crashLoopLock.Lock()
	_, replacementAlive := am.crashLoops[replacement]
	loopCount := len(am.crashLoops)
	retries := replacement.Retries
	am.crashLoopLock.Unlock()

	assert.True(t, replacementAlive, "the replacement loop must survive a ghost wake-up")
	assert.Equal(t, 1, loopCount, "the ghost must not add or remove loops")
	assert.Equal(t, uint(2), retries, "the ghost must not touch the replacement's backoff counter")
}

// The core of the field incident: the requested state changed to UNINSTALLED
// while the loop was sleeping. The wake must drop the stale RUNNING retry,
// clear the loop, and drive the app toward the NEW requested state.
func TestCrashLoopWakeDropsStaleRetryAndRunsTheNewTeardown(t *testing.T) {
	am, mc, mt, st, _, _ := amHarness(t)

	app := amSeed(t, st, 72, "stale", common.FAILED, common.PROD)

	uninstall := amPayload(72, "stale", common.UNINSTALLED, common.PROD)
	crashSeedTeardownRow(t, am, app, uninstall)

	captured := amPayload(72, "stale", common.RUNNING, common.PROD)
	captured.Retrying = true
	task := &CrashLoop{Payload: captured, Retries: 5}
	am.crashLoopLock.Lock()
	am.crashLoops[task] = struct{}{}
	am.crashLoopLock.Unlock()

	// Only teardown work may happen. A re-driven RUNNING retry would hit
	// registry logins / pulls, which the strict mock would reject.
	mt.EXPECT().TunnelCapable().Return(false).Maybe()
	mt.EXPECT().GetState().Return(nil, nil).Maybe()
	mc.EXPECT().
		GetContainer(mock.Anything, captured.ContainerName.Prod).
		Return(container.Summary{}, notFoundErr()).
		Once()
	// The wake re-drives with a payload rebuilt from the requested-state row,
	// whose registry image name is config-derived — not the hand-set fixture
	// value — so match loosely.
	mc.EXPECT().
		RemoveImagesByName(mock.Anything, mock.Anything, mock.Anything).
		Return(nil).
		Once()
	fwdAllowLogs(mc)

	am.crashLoopWake(task)

	app.StateLock.Lock()
	state := app.CurrentState
	app.StateLock.Unlock()
	assert.Equal(t, common.UNINSTALLED, state,
		"the wake must drive the app to the NEW requested state, not retry the captured one")

	am.crashLoopLock.Lock()
	remaining := len(am.crashLoops)
	am.crashLoopLock.Unlock()
	assert.Equal(t, 0, remaining, "the stale loop must be cleared")
}

// Once the app reaches the loop's target on its own, the wake clears the loop
// and does nothing else.
func TestCrashLoopWakeClearsTheLoopOnceTheTargetIsReached(t *testing.T) {
	am, _, _, st, _, _ := amHarness(t)

	app := amSeed(t, st, 73, "settled", common.RUNNING, common.PROD)
	app.StateLock.Lock()
	app.RequestedState = common.RUNNING
	app.StateLock.Unlock()

	captured := amPayload(73, "settled", common.RUNNING, common.PROD)
	captured.Retrying = true
	task := &CrashLoop{Payload: captured, Retries: 3}
	am.crashLoopLock.Lock()
	am.crashLoops[task] = struct{}{}
	am.crashLoopLock.Unlock()

	// No expectations: reaching the target must not drive any work.
	am.crashLoopWake(task)

	am.crashLoopLock.Lock()
	remaining := len(am.crashLoops)
	am.crashLoopLock.Unlock()
	assert.Equal(t, 0, remaining, "a satisfied loop must clear itself")
}

// The second half of the field incident: an UNINSTALLED request arriving while
// a transition holds the lock must not be dropped. It interrupts the in-flight
// transition's cancelable work, waits for the lock, and runs the teardown.
func TestTeardownRequestWaitsForTheInFlightTransition(t *testing.T) {
	am, mc, mt, st, _, _ := amHarness(t)

	app := amSeed(t, st, 74, "busyremove", common.FAILED, common.PROD)

	uninstall := amPayload(74, "busyremove", common.UNINSTALLED, common.PROD)
	crashSeedTeardownRow(t, am, app, uninstall)

	// Model the in-flight FAILED -> RUNNING attempt by holding the transition
	// lock, exactly as RequestAppState holds it for a transition's duration.
	require.False(t, app.SecureTransition(), "precondition: the transition lock must start free")

	// The teardown first interrupts the transition's cancelable work (for a
	// non-compose app: the docker pull stream)...
	mc.EXPECT().CancelStream(mock.Anything).Return(nil).Once()
	// ...then waits for the lock and runs the full non-compose PROD teardown.
	mt.EXPECT().TunnelCapable().Return(false).Maybe()
	mt.EXPECT().GetState().Return(nil, nil).Maybe()
	mc.EXPECT().
		GetContainer(mock.Anything, uninstall.ContainerName.Prod).
		Return(container.Summary{}, notFoundErr()).
		Once()
	mc.EXPECT().
		RemoveImagesByName(mock.Anything, uninstall.RegistryImageName.Prod, mock.Anything).
		Return(nil).
		Once()
	fwdAllowLogs(mc)

	released := make(chan struct{})
	go func() {
		time.Sleep(200 * time.Millisecond)
		app.UnlockTransition() // the interrupted transition unwinds
		close(released)
	}()

	require.NoError(t, am.RequestAppState(uninstall),
		"the teardown must run once the lock frees instead of being dropped")
	<-released

	app.StateLock.Lock()
	state := app.CurrentState
	app.StateLock.Unlock()
	assert.Equal(t, common.UNINSTALLED, state, "the app must end up uninstalled")
}

// When the in-flight transition never releases the lock, the teardown gives up
// after the acquire timeout and reports an ERROR — never silent success, so
// callers and logs show the uninstall did not happen.
func TestTeardownRequestGivesUpWhenTheTransitionNeverUnwinds(t *testing.T) {
	am, mc, mt, st, _, _ := amHarness(t)

	oldTimeout := teardownLockAcquireTimeout
	teardownLockAcquireTimeout = 100 * time.Millisecond
	t.Cleanup(func() { teardownLockAcquireTimeout = oldTimeout })

	app := amSeed(t, st, 75, "wedged", common.FAILED, common.PROD)

	uninstall := amPayload(75, "wedged", common.UNINSTALLED, common.PROD)
	crashSeedTeardownRow(t, am, app, uninstall)

	require.False(t, app.SecureTransition(), "precondition: the transition lock must start free")
	// The lock is deliberately never released.

	mt.EXPECT().TunnelCapable().Return(false).Maybe()
	mc.EXPECT().CancelStream(mock.Anything).Return(nil).Once()

	err := am.RequestAppState(uninstall)
	require.Error(t, err, "an unexecuted teardown must surface as an error, not silent success")

	app.StateLock.Lock()
	state := app.CurrentState
	app.StateLock.Unlock()
	assert.Equal(t, common.FAILED, state, "the app must be left untouched")
}

// An uninstall that lands while the app is DOWNLOADING must run the FULL
// teardown, not stop at the cancel's REMOVED settle. Settling at REMOVED never
// converges: the observer deletes both DB rows the moment REMOVED is reached
// with an UNINSTALLED requested-state row, so nothing can re-drive the rest —
// the data dir, tunnels and host ports would leak silently.
func TestUninstallDuringDownloadRunsFullTeardown(t *testing.T) {
	am, mc, mt, st, _, _ := amHarness(t)

	app := amSeed(t, st, 81, "middownload", common.DOWNLOADING, common.PROD)

	uninstall := amPayload(81, "middownload", common.UNINSTALLED, common.PROD)
	crashSeedTeardownRow(t, am, app, uninstall)

	mt.EXPECT().TunnelCapable().Return(false).Maybe()
	mt.EXPECT().GetState().Return(nil, nil).Maybe()
	// cancelPull cancels the in-flight docker pull stream...
	mc.EXPECT().CancelStream(mock.Anything).Return(nil).Once()
	// ...and must then run the full non-compose PROD uninstall.
	mc.EXPECT().
		GetContainer(mock.Anything, uninstall.ContainerName.Prod).
		Return(container.Summary{}, notFoundErr()).
		Once()
	mc.EXPECT().
		RemoveImagesByName(mock.Anything, uninstall.RegistryImageName.Prod, mock.Anything).
		Return(nil).
		Once()
	fwdAllowLogs(mc)

	require.NoError(t, am.RequestAppState(uninstall))

	app.StateLock.Lock()
	state := app.CurrentState
	app.StateLock.Unlock()
	assert.Equal(t, common.UNINSTALLED, state,
		"an uninstall during DOWNLOADING must reach UNINSTALLED, not stall at REMOVED")
}

// The same guarantee for a DEV app whose build is canceled by an uninstall
// (BUILDING -> UNINSTALLED is one of the transition-map rows added for this).
func TestUninstallDuringBuildRunsFullTeardown(t *testing.T) {
	am, mc, mt, st, _, _ := amHarness(t)

	app := amSeed(t, st, 82, "midbuild", common.BUILDING, common.DEV)

	uninstall := amPayload(82, "midbuild", common.UNINSTALLED, common.DEV)
	crashSeedTeardownRow(t, am, app, uninstall)

	mt.EXPECT().TunnelCapable().Return(false).Maybe()
	mt.EXPECT().GetState().Return(nil, nil).Maybe()
	// cancelBuild cancels the build stream, then the full DEV uninstall runs.
	mc.EXPECT().CancelStream(mock.Anything).Return(nil).Once()
	mc.EXPECT().
		GetContainer(mock.Anything, uninstall.ContainerName.Dev).
		Return(container.Summary{}, notFoundErr()).
		Once()
	mc.EXPECT().
		RemoveImagesByName(mock.Anything, uninstall.RegistryImageName.Dev, mock.Anything).
		Return(nil).
		Once()
	fwdAllowLogs(mc)

	require.NoError(t, am.RequestAppState(uninstall))

	app.StateLock.Lock()
	state := app.CurrentState
	app.StateLock.Unlock()
	assert.Equal(t, common.UNINSTALLED, state,
		"an uninstall during BUILDING must reach UNINSTALLED, not stall at REMOVED")
}

// When the in-memory requested state diverges from the row (a row write failed
// — e.g. disk-full — after memory was updated), the wake must NOT treat the
// divergence as new intent: that would clear and re-create the loop every
// cycle, resetting the quadratic backoff to seconds forever. The row is the
// deciding authority; a same-target row keeps the loop (and its backoff)
// alive.
func TestCrashLoopWakeKeepsBackoffWhenMemoryDivergesFromRow(t *testing.T) {
	am, _, mt, st, _, _ := amHarness(t)

	app := amSeed(t, st, 83, "diverged", common.BUILT, common.PROD)

	// Row: requested PRESENT (same as the captured payload).
	rowPayload := amPayload(83, "diverged", common.PRESENT, common.PROD)
	require.NoError(t, st.UpdateLocalRequestedState(rowPayload))

	// Memory: diverged to RUNNING (its row write "failed").
	app.StateLock.Lock()
	app.RequestedState = common.RUNNING
	app.StateLock.Unlock()

	captured := amPayload(83, "diverged", common.PRESENT, common.PROD)
	captured.Retrying = true
	task := &CrashLoop{Payload: captured, Retries: 4}
	am.crashLoopLock.Lock()
	am.crashLoops[task] = struct{}{}
	am.crashLoopLock.Unlock()

	// The same-target retry resolves to the no-action PRESENT transition; no
	// container work happens (strict mock), and crucially the loop survives.
	mt.EXPECT().TunnelCapable().Return(false).Maybe()
	mt.EXPECT().GetState().Return(nil, nil).Maybe()

	am.crashLoopWake(task)

	am.crashLoopLock.Lock()
	_, alive := am.crashLoops[task]
	retries := task.Retries
	am.crashLoopLock.Unlock()

	assert.True(t, alive, "a memory/row divergence must not clear the loop — backoff would reset every wake")
	assert.Equal(t, uint(4), retries, "the wake itself must not touch the backoff counter")
}

// The requested-state row never persists push-only fields; a crashloop retry
// rebuilt from the row must carry them over from the captured payload, or the
// app loses its third-party registry credentials, instance identity, and
// credential epoch on every retry.
func TestCarryPushOnlyFields(t *testing.T) {
	captured := amPayload(84, "pushfields", common.RUNNING, common.PROD)
	captured.DockerCredentials = map[string]common.DockerCredential{
		"registry.example.com": {Username: "u", Password: "p"},
	}
	captured.InstanceKey = 42
	captured.AppCredEpoch = 3
	captured.DeviceToAppKey = 7

	fromRow := amPayload(84, "pushfields", common.RUNNING, common.PROD)

	carryPushOnlyFields(&fromRow, captured)

	assert.Equal(t, captured.DockerCredentials, fromRow.DockerCredentials)
	assert.Equal(t, uint64(42), fromRow.InstanceKey)
	assert.Equal(t, uint64(3), fromRow.AppCredEpoch)
	assert.Equal(t, uint64(7), fromRow.DeviceToAppKey)
}

// Non-teardown requests keep the existing drop semantics while a transition is
// in flight (TestRequestAppStateDropsUpdateWhileTransitioning pins the update
// flavor of this; here the plain state-change flavor).
func TestNonTeardownRequestIsStillDroppedWhileTransitioning(t *testing.T) {
	am, _, mt, st, _, _ := amHarness(t)

	app := amSeed(t, st, 76, "busykeep", common.PRESENT, common.PROD)

	mt.EXPECT().TunnelCapable().Return(false).Maybe()

	require.False(t, app.SecureTransition(), "precondition: the transition lock must start free")

	payload := amPayload(76, "busykeep", common.RUNNING, common.PROD)

	// No container expectations and no CancelStream: a non-teardown request
	// must neither interrupt the in-flight transition nor wait for it.
	require.NoError(t, am.RequestAppState(payload), "a dropped request is reported as success")

	app.StateLock.Lock()
	state := app.CurrentState
	app.StateLock.Unlock()
	assert.Equal(t, common.PRESENT, state, "the dropped request must not have touched the app")
}
