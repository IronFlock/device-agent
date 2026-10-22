package apps

import (
	"testing"
	"time"

	dockercontainer "github.com/docker/docker/api/types/container"

	"reagent/common"
	containerpkg "reagent/container"
	"reagent/errdefs"
	"reagent/messenger/topics"
	"reagent/testutil/fakes"
	"reagent/testutil/mocks"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// The update trigger path
//
// "Update this app" travels through four seams before anything is downloaded,
// and a break in ANY of them looks identical on a device: no download happens
// and the app keeps running its old release.
//
//	cloud RPC -> CreateOrUpdateApp     persists the requested state + versions
//	          -> RequestAppState       DROPS the request if a transition is
//	                                   already in flight (it does not queue)
//	          -> UpdateCurrentAppState overwrites app.Version from the payload
//	          -> InitTransition        the gate that picks updateApp over runApp
//
// The gate in state_machine.go is the decision point:
//
//	payload.RequestUpdate && payload.NewestVersion != app.Version && ...
//
// Because UpdateCurrentAppState runs first and assigns app.Version =
// payload.PresentVersion, the gate is in practice
// `RequestUpdate && newest != present` — both operands supplied by the cloud.
// The exception is an empty present_version, where the locally persisted
// version survives and becomes the operand instead.
//
// These tests use "was anything pulled?" as the observable, because a download
// is exactly what an operator does or does not see when they request an update.
// The strict mocks.Container makes that assertion for free: an unexpected Pull
// fails the test, and a missing expected Pull fails it too.
//
// Helpers here are prefixed upd*; the harnesses (wiredRunBuildSM, amHarness)
// and fixtures (seedApp, execPayload, amSeed, amPayload, fwdDockerStream,
// fwdAllowLogs, notFoundErr) come from the other files in this package.
// =============================================================================

// updRequest builds the payload the cloud sends for an app update: the
// request_update flag, the version currently recorded against the device, and
// the newer release to move to.
func updRequest(name string, requested common.AppState, present, newest string) common.TransitionPayload {
	p := execPayload(name, requested, common.PROD)
	p.RequestUpdate = true
	p.PresentVersion = present
	p.NewestVersion = newest
	p.NewReleaseKey = 202
	return p
}

// updExpectPull wires the container calls updateApp makes on its happy path:
// a registry login, the pull itself, then the teardown probe (finding no
// existing container to remove) and the best-effort removal of the superseded
// image. The pull runs BEFORE the teardown — TestUpdateAppPullsBeforeTeardown
// asserts that order explicitly.
func updExpectPull(mc *mocks.Container, payload common.TransitionPayload, supersededVersion string) {
	mc.EXPECT().
		GetContainer(mock.Anything, payload.ContainerName.Prod).
		Return(dockercontainer.Summary{}, notFoundErr()).
		Once()
	mc.EXPECT().HandleRegistryLogins(mock.Anything).Return(nil).Once()
	mc.EXPECT().
		Pull(mock.Anything, mock.Anything, mock.Anything).
		Return(fwdDockerStream(), nil).
		Once()
	mc.EXPECT().
		RemoveImageByName(mock.Anything, payload.RegistryImageName.Prod, supersededVersion, mock.Anything).
		Return(nil).
		Once()

	fwdAllowLogs(mc)
}

// updSentUpdateStatuses extracts the updateStatus field of every app-state
// report pushed to the backend, so a test can assert that an update was (or was
// not) confirmed. Mirrors remoteStatesFor, which reads the state field instead.
func updSentUpdateStatuses(msg *fakes.Messenger) []common.UpdateStatus {
	statuses := make([]common.UpdateStatus, 0)
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
		if s, ok := dict["updateStatus"].(common.UpdateStatus); ok {
			statuses = append(statuses, s)
		}
	}
	return statuses
}

// updSeedAt seeds a PROD app already carrying an installed version.
func updSeedAt(t *testing.T, sm *StateMachine, name string, state common.AppState, version string) *common.App {
	t.Helper()

	app := seedApp(t, sm.StateObserver.AppStore, name, state, common.PROD)
	app.StateLock.Lock()
	app.Version = version
	app.StateLock.Unlock()
	return app
}

// =============================================================================
// InitTransition — the update gate
//
// Every negative case below deliberately parks the app at PRESENT and requests
// PRESENT, so the non-update branch resolves to noActionTransitionFunc: the
// transition then performs NO container work at all, and the strict mock proves
// that nothing was downloaded.
// =============================================================================

func TestInitTransitionUpdateGate(t *testing.T) {
	t.Run("request_update with a newer version downloads the new release", func(t *testing.T) {
		sm, mc, _, _, _ := wiredRunBuildSM(t)

		app := updSeedAt(t, sm, "gate-update", common.PRESENT, "1.0.0")
		payload := updRequest("gate-update", common.PRESENT, "1.0.0", "2.0.0")

		updExpectPull(mc, payload, "1.0.0")

		errC := sm.InitTransition(app, payload)
		require.NotNil(t, errC, "an update request must produce a transition")
		require.NoError(t, <-errC)

		app.StateLock.Lock()
		version := app.Version
		app.StateLock.Unlock()

		assert.Equal(t, "2.0.0", version, "the update must land the newest version")
	})

	t.Run("request_update=false never downloads, even when a newer version exists", func(t *testing.T) {
		sm, _, _, _, _ := wiredRunBuildSM(t)

		app := updSeedAt(t, sm, "gate-no-flag", common.PRESENT, "1.0.0")
		payload := updRequest("gate-no-flag", common.PRESENT, "1.0.0", "2.0.0")
		payload.RequestUpdate = false

		// No container expectations at all: the strict mock fails the test if
		// anything is pulled.
		errC := sm.InitTransition(app, payload)
		require.NotNil(t, errC)

		err := <-errC
		assert.True(t, errdefs.IsNoActionTransition(err),
			"expected the plain PRESENT->PRESENT no-op, got %v", err)
	})

	t.Run("a newest version equal to the installed one never downloads", func(t *testing.T) {
		sm, _, _, _, _ := wiredRunBuildSM(t)

		app := updSeedAt(t, sm, "gate-same", common.PRESENT, "2.0.0")
		payload := updRequest("gate-same", common.PRESENT, "2.0.0", "2.0.0")

		errC := sm.InitTransition(app, payload)
		require.NotNil(t, errC)

		err := <-errC
		assert.True(t, errdefs.IsNoActionTransition(err),
			"nothing to update to, so no transition should run; got %v", err)
	})

	t.Run("an UNINSTALLED request is never converted into an update", func(t *testing.T) {
		sm, _, _, _, _ := wiredRunBuildSM(t)

		// A pending update must not resurrect an app that is being removed.
		app := updSeedAt(t, sm, "gate-uninstall", common.UNINSTALLED, "1.0.0")
		payload := updRequest("gate-uninstall", common.UNINSTALLED, "1.0.0", "2.0.0")

		errC := sm.InitTransition(app, payload)
		require.NotNil(t, errC)

		err := <-errC
		assert.True(t, errdefs.IsNoActionTransition(err),
			"an UNINSTALLED target must bypass the update path; got %v", err)
	})

	t.Run("returns a nil channel when no transition function matches", func(t *testing.T) {
		sm, _, _, _, _ := wiredRunBuildSM(t)

		app := updSeedAt(t, sm, "gate-nil", common.DOWNLOADING, "1.0.0")
		payload := execPayload("gate-nil", common.BUILT, common.PROD)

		assert.Nil(t, sm.InitTransition(app, payload),
			"DOWNLOADING->BUILT is not a modelled transition")
	})
}

// =============================================================================
// UpdateCurrentAppState — which version the gate ends up comparing
//
// This runs immediately before InitTransition inside RequestAppState, so it
// decides the `app.Version` operand of the gate.
// =============================================================================

func TestUpdateCurrentAppStateDecidesTheGateVersionOperand(t *testing.T) {
	t.Run("adopts the version the cloud reports as installed", func(t *testing.T) {
		am, _, _, st, _, _ := amHarness(t)

		app := amSeed(t, st, 11, "operand-cloud", common.PRESENT, common.PROD)
		app.StateLock.Lock()
		app.Version = "9.9.9" // a stale local value
		app.StateLock.Unlock()

		payload := amPayload(11, "operand-cloud", common.PRESENT, common.PROD)
		payload.PresentVersion = "1.0.0"

		require.NoError(t, am.UpdateCurrentAppState(payload))

		app.StateLock.Lock()
		version := app.Version
		app.StateLock.Unlock()

		assert.Equal(t, "1.0.0", version,
			"the cloud's present_version is authoritative and overwrites the local one")
	})

	t.Run("keeps the locally stored version when the cloud sends none", func(t *testing.T) {
		am, _, _, st, _, _ := amHarness(t)

		app := amSeed(t, st, 12, "operand-local", common.PRESENT, common.PROD)
		app.StateLock.Lock()
		app.Version = "2.0.0"
		app.StateLock.Unlock()

		payload := amPayload(12, "operand-local", common.PRESENT, common.PROD)
		payload.PresentVersion = "" // e.g. the device has no release row yet

		require.NoError(t, am.UpdateCurrentAppState(payload))

		app.StateLock.Lock()
		version := app.Version
		app.StateLock.Unlock()

		assert.Equal(t, "2.0.0", version,
			"with no cloud present_version the local one survives and feeds the gate")
	})
}

// A locally stored version equal to the newest release blocks the update, but
// only when the cloud sends no present_version to overwrite it. This is the one
// way local state alone can wedge the trigger, so it is pinned explicitly.
func TestInitTransitionLocalVersionBlocksUpdateWhenCloudSendsNoPresentVersion(t *testing.T) {
	sm, _, _, _, _ := wiredRunBuildSM(t)

	app := updSeedAt(t, sm, "gate-local-wedge", common.PRESENT, "2.0.0")

	payload := updRequest("gate-local-wedge", common.PRESENT, "", "2.0.0")

	errC := sm.InitTransition(app, payload)
	require.NotNil(t, errC)

	err := <-errC
	assert.True(t, errdefs.IsNoActionTransition(err),
		"a local version already equal to newest suppresses the update; got %v", err)
}

// =============================================================================
// RequestAppState — an update arriving mid-transition is discarded
// =============================================================================

// RequestAppState does not queue: if the app is already transitioning it logs
// and returns nil, so the update request is lost. The cloud does not resend it,
// which makes this a silent way for an update to never happen.
func TestRequestAppStateDropsUpdateWhileTransitioning(t *testing.T) {
	am, _, mt, st, _, _ := amHarness(t)

	app := amSeed(t, st, 21, "drop-update", common.PRESENT, common.PROD)
	app.StateLock.Lock()
	app.Version = "1.0.0"
	app.StateLock.Unlock()

	// syncPortState runs before the transition lock is tested; short-circuit it.
	mt.EXPECT().TunnelCapable().Return(false).Once()

	// SecureTransition acquires on the first call and reports "was already
	// locked" thereafter — so this both asserts the precondition and stands in
	// for the transition that is supposedly in flight.
	require.False(t, app.SecureTransition(), "precondition: the transition lock must start free")

	payload := amPayload(21, "drop-update", common.PRESENT, common.PROD)
	payload.RequestUpdate = true
	payload.PresentVersion = "1.0.0"
	payload.NewestVersion = "2.0.0"

	// No container expectations: the strict mock proves nothing was downloaded.
	require.NoError(t, am.RequestAppState(payload),
		"a dropped request is reported as success")

	app.StateLock.Lock()
	version := app.Version
	app.StateLock.Unlock()

	assert.Equal(t, "1.0.0", version, "the dropped update must not have touched the app")
}

// The full scenario behind a stalled update, end to end. An app is mid-transition
// PRESENT -> RUNNING (that transition holds the lock). An update_request arrives
// during that window, exactly as the WAMP handler delivers it:
//
//   - CreateOrUpdateApp persists the intent to the requested-state row — the
//     agent's durable "queue" of the latest desired state — even though the
//     follow-up action is about to be dropped.
//   - RequestAppState hits the held lock (SecureTransition) and is dropped: no
//     download, reported as success, nothing retried.
//
// DESIRED behaviour (this test is currently RED): when the transition finishes,
// the post-transition reconcile must notice the pending update still sitting in
// the row and re-drive it. Today VerifyState only re-drives on a STATE mismatch
// and never inspects request_update, so a dropped update whose target state
// already matches (RUNNING == RUNNING) is stranded. The fix is to reconcile the
// whole row — request_update included — after a transition completes.
//
// The recovered update is stopped at the pull with a canceled stream so the
// test observes the recovery (the app re-enters UPDATING) without cascading into
// a full run.
func TestDroppedUpdateIsRecoveredAfterTransitionCompletes(t *testing.T) {
	am, mc, mt, st, _, _ := amHarness(t)

	app := amSeed(t, st, 42, "busy", common.PRESENT, common.PROD)
	app.StateLock.Lock()
	app.Version = "1.0.0"
	app.RequestedState = common.RUNNING // the target of the in-flight transition
	app.StateLock.Unlock()

	// Model the in-flight PRESENT -> RUNNING transition by holding the lock, just
	// as RequestAppState holds it for a real transition's whole duration.
	require.False(t, app.SecureTransition(),
		"precondition: lock free; acquiring it now models the running transition")

	mt.EXPECT().TunnelCapable().Return(false).Maybe() // syncPortState short-circuit
	mt.EXPECT().GetState().Return(nil, nil).Maybe()   // UpdateTunnelState after the recovered transition

	// The cloud pushes an update mid-transition. The handler persists intent...
	update := amPayload(42, "busy", common.RUNNING, common.PROD)
	update.RequestUpdate = true
	update.PresentVersion = "1.0.0"
	update.NewestVersion = "2.0.0"
	update.NewReleaseKey = 202
	require.NoError(t, am.CreateOrUpdateApp(update))

	// ...then tries to act, and is dropped by the held lock.
	require.NoError(t, am.RequestAppState(update), "the dropped request is reported as success")

	app.StateLock.Lock()
	version := app.Version
	app.StateLock.Unlock()
	assert.Equal(t, "1.0.0", version, "the update did not run — it was dropped")

	// The intent survives in the row: this is the queue manifested in the DB.
	row, err := st.GetRequestedState(42, common.PROD)
	require.NoError(t, err)
	assert.True(t, row.RequestUpdate, "the pending update flag persists despite the drop")
	assert.Equal(t, "2.0.0", row.NewestVersion, "the pending target version persists")

	// The transition finishes: the app reaches RUNNING and the lock is released.
	app.UnlockTransition()
	app.StateLock.Lock()
	app.CurrentState = common.RUNNING
	app.StateLock.Unlock()

	// Stub the container calls the recovered update makes, and fail the pull so the
	// re-driven update stops at a terminal state (FAILED) rather than running to
	// completion or looping. Reaching UPDATING/FAILED proves the update was
	// re-driven; a stranded update would leave the app sitting in RUNNING.
	mc.EXPECT().GetContainer(mock.Anything, mock.Anything).
		Return(dockercontainer.Summary{}, notFoundErr()).Maybe()
	mc.EXPECT().HandleRegistryLogins(mock.Anything).Return(nil).Maybe()
	mc.EXPECT().Pull(mock.Anything, mock.Anything, mock.Anything).
		Return(nil, assert.AnError).Maybe()
	fwdAllowLogs(mc)

	// VerifyState runs at the end of every transition. It must recover the pending
	// update even though the state already matches its target.
	require.NoError(t, am.VerifyState(app))

	// The recovery re-drives the update, moving the app off RUNNING. Currently RED:
	// VerifyState ignores request_update on a state match, so the app stays RUNNING.
	var recovered bool
	for i := 0; i < 200; i++ {
		app.StateLock.Lock()
		s := app.CurrentState
		app.StateLock.Unlock()
		if s == common.UPDATING || s == common.FAILED {
			recovered = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	assert.True(t, recovered,
		"after the transition completes, the pending update must be recovered (the app leaves RUNNING to re-drive the update); "+
			"instead it was left stranded because VerifyState ignores request_update on a state match")
}

// =============================================================================
// The local requested-state row
//
// VerifyState and the state observer both re-drive an app from this row rather
// than from the original cloud payload. If it does not carry the update fields,
// a re-driven update silently degrades into a plain restart.
// =============================================================================

func TestRequestedStateRoundTripPreservesUpdateFields(t *testing.T) {
	_, _, _, st, _, _ := amHarness(t)

	amSeed(t, st, 31, "roundtrip", common.PRESENT, common.PROD)

	payload := amPayload(31, "roundtrip", common.RUNNING, common.PROD)
	payload.RequestUpdate = true
	payload.PresentVersion = "1.0.0"
	payload.NewestVersion = "2.0.0"
	payload.NewReleaseKey = 202
	payload.NewDockerCompose = map[string]interface{}{
		"services": map[string]interface{}{
			"app": map[string]interface{}{"image": "registry.test/prod/roundtrip:2.0.0"},
		},
	}

	require.NoError(t, st.UpdateLocalRequestedState(payload))

	stored, err := st.GetRequestedState(31, common.PROD)
	require.NoError(t, err)

	assert.True(t, stored.RequestUpdate, "request_update must survive; without it the re-drive is a plain restart")
	assert.Equal(t, "2.0.0", stored.NewestVersion)
	assert.Equal(t, "1.0.0", stored.PresentVersion)
	assert.Equal(t, uint64(202), stored.NewReleaseKey)
	assert.NotNil(t, stored.NewDockerCompose,
		"the pending compose must survive, or the update would deploy the old definition")
}

// =============================================================================
// updateApp — the bookkeeping a completed update leaves behind
// =============================================================================

func TestUpdateAppRecordsUpdateBookkeeping(t *testing.T) {
	sm, mc, st, msg, _ := wiredRunBuildSM(t)

	app := updSeedAt(t, sm, "bookkeeping", common.PRESENT, "1.0.0")
	payload := updRequest("bookkeeping", common.RUNNING, "1.0.0", "2.0.0")

	updExpectPull(mc, payload, "1.0.0")

	require.NoError(t, sm.updateApp(payload, app))

	app.StateLock.Lock()
	version := app.Version
	releaseKey := app.ReleaseKey
	updateStatus := app.UpdateStatus
	app.StateLock.Unlock()

	assert.Equal(t, "2.0.0", version)
	assert.Equal(t, uint64(202), releaseKey, "the new release key must be adopted")

	// The pending flag only exists between the pull and the state report that
	// carries it; by the time updateApp returns it has been acknowledged. What
	// matters is that the backend was actually told.
	assert.Contains(t, updSentUpdateStatuses(msg), common.PENDING_REMOTE_CONFIRMATION,
		"the update must be confirmed to the backend, or it keeps re-requesting it")
	assert.Equal(t, common.COMPLETED, updateStatus,
		"a delivered confirmation is marked completed locally")

	// The write-back collapses both versions onto the newly installed one, so a
	// re-drive of this row does not attempt the same update again.
	stored, err := st.GetRequestedState(1, common.PROD)
	require.NoError(t, err)
	assert.Equal(t, "2.0.0", stored.PresentVersion)
	assert.Equal(t, "2.0.0", stored.NewestVersion)
}

// =============================================================================
// Production repro: an app updated while its target changes ends up stuck
//
// Observed in production against a compose app whose services were silently
// crash-looping while still reported RUNNING:
//
//	1. "Update" is requested; the marker persists across refreshes but nothing
//	   downloads (the update request is dropped — see
//	   TestRequestAppStateDropsUpdateWhileTransitioning for that mechanism).
//	2. The user switches the manual requested state to PRESENT; that finally
//	   lets the update transition run and the image downloads.
//	3. While it downloads, the user switches the manual requested state to
//	   RUNNING again.
//	4. The update finishes, but the app stays PRESENT — it never returns to
//	   RUNNING.
//
// The device's target lives in the requested-state row's manually_requested_state
// column. updateApp's closing UpdateLocalRequestedState upserts that column from
// the payload that TRIGGERED the update (ON CONFLICT ... SET
// manually_requested_state = excluded, statements.go), and that payload's
// RequestedState is the PRESENT the user set in step 2. So the write clobbers the
// RUNNING that arrived in step 3, and VerifyState then reconciles the device
// toward the stale PRESENT.
//
// The compose and non-compose update handlers share this closing write verbatim;
// the non-compose path is used here because it is unit-testable without the
// docker compose CLI.
// =============================================================================

func TestUpdateDiscardsRequestedStateChangeDuringDownload(t *testing.T) {
	sm, mc, st, _, _ := wiredRunBuildSM(t)

	// The app is RUNNING v1.0.0 (in production, with silently-failing services).
	app := updSeedAt(t, sm, "retarget", common.RUNNING, "1.0.0")

	// Step 3: mid-download the user re-targets the app to RUNNING. On the device
	// this is CreateOrUpdateApp, which updates BOTH the in-memory target and the
	// persisted requested-state row (its RequestAppState is then dropped because
	// the update transition holds the lock). Mirror both here.
	app.StateLock.Lock()
	app.RequestedState = common.RUNNING
	app.StateLock.Unlock()
	running := updRequest("retarget", common.RUNNING, "1.0.0", "2.0.0")
	require.NoError(t, st.UpdateLocalRequestedState(running))

	pre, err := st.GetRequestedState(1, common.PROD)
	require.NoError(t, err)
	require.Equal(t, common.RUNNING, pre.RequestedState,
		"precondition: RUNNING is the device's latest target before the update finishes")

	// Step 4: the update — triggered in step 2 with a PRESENT target — completes.
	trigger := updRequest("retarget", common.PRESENT, "1.0.0", "2.0.0")
	updExpectPull(mc, trigger, "1.0.0")
	require.NoError(t, sm.updateApp(trigger, app))

	// The device's target should still be RUNNING — the last thing the user
	// asked for. This is the bug: the update overwrote it with its own trigger
	// state, so the app reconciles to PRESENT and never returns to RUNNING.
	post, err := st.GetRequestedState(1, common.PROD)
	require.NoError(t, err)
	assert.Equal(t, common.RUNNING, post.RequestedState,
		"the update discarded the requested-state change that arrived during the download")
}

// A user may toggle the requested-state button several times while the image
// downloads. Each toggle is a CreateOrUpdateApp that updates the in-memory
// target and the requested-state row; its follow-up RequestAppState is dropped
// because the update holds the transition lock, so no intermediate transition
// runs mid-download. The update's closing write persists whatever the LATEST
// target is, so the device reconciles to the last toggle — not the trigger, and
// not any intermediate toggle.
func TestUpdateHonorsTheLastRequestedStateToggleDuringDownload(t *testing.T) {
	sm, mc, st, _, _ := wiredRunBuildSM(t)

	app := updSeedAt(t, sm, "toggle", common.RUNNING, "1.0.0")

	// The update was triggered by switching to PRESENT.
	trigger := updRequest("toggle", common.PRESENT, "1.0.0", "2.0.0")

	// While it downloads, the user toggles RUNNING -> PRESENT -> RUNNING. Mirror
	// what CreateOrUpdateApp does for each toggle: set the in-memory target and
	// upsert the requested-state row.
	for _, target := range []common.AppState{common.RUNNING, common.PRESENT, common.RUNNING} {
		app.StateLock.Lock()
		app.RequestedState = target
		app.StateLock.Unlock()
		toggle := updRequest("toggle", target, "1.0.0", "2.0.0")
		require.NoError(t, st.UpdateLocalRequestedState(toggle))
	}

	// The update completes.
	updExpectPull(mc, trigger, "1.0.0")
	require.NoError(t, sm.updateApp(trigger, app))

	// The device reconciles to the LAST toggle (RUNNING).
	post, err := st.GetRequestedState(1, common.PROD)
	require.NoError(t, err)
	assert.Equal(t, common.RUNNING, post.RequestedState, "the last toggle during the download wins")

	// And the versions are collapsed so the reconcile does not re-trigger the update.
	assert.Equal(t, "2.0.0", post.PresentVersion)
	assert.Equal(t, "2.0.0", post.NewestVersion)
}

// =============================================================================
// Update-in-progress guard
//
// While an update is genuinely running (current_state == UPDATING, holding the
// transition lock), RequestAppState drops every incoming transition except
// PRESENT. Dropping breaks the VerifyState update-recovery loop; PRESENT is the
// escape hatch and cancels the in-flight update.
// =============================================================================

func updComposeMap() map[string]interface{} {
	return map[string]interface{}{
		"services": map[string]interface{}{"app": map[string]interface{}{"image": "x:1"}},
	}
}

// "Keep going" states (RUNNING, BUILT, ...) requested while an app is UPDATING
// are dropped: no work happens and the update keeps running. This is what breaks
// the VerifyState update-recovery loop — a re-driven update whose app is still
// UPDATING is refused instead of re-running.
func TestRequestAppStateDropsKeepGoingTransitionsWhileUpdating(t *testing.T) {
	for _, requested := range []common.AppState{common.RUNNING, common.BUILT} {
		t.Run(string(requested), func(t *testing.T) {
			am, _, _, st, _, _ := amHarness(t)

			app := amSeed(t, st, 61, "updating", common.UPDATING, common.PROD)

			// No cancel func registered and no container expectations: the strict
			// mock proves nothing ran.
			payload := amPayload(61, "updating", requested, common.PROD)
			payload.DockerCompose = updComposeMap()
			require.NoError(t, am.RequestAppState(payload), "dropped, reported as success")

			app.StateLock.Lock()
			state := app.CurrentState
			app.StateLock.Unlock()
			assert.Equal(t, common.UPDATING, state, "the app is untouched — the update keeps running")
		})
	}
}

// The settle state (PRESENT) and the teardown states (REMOVED, UNINSTALLED)
// requested while UPDATING are the escape hatches: each cancels the in-flight
// update first (killing a hung compose CLI so it unwinds and releases the lock),
// then runs its terminal transition.
func TestRequestAppStateCancelsUpdateForTeardownStates(t *testing.T) {
	for _, requested := range []common.AppState{common.PRESENT, common.REMOVED, common.UNINSTALLED} {
		t.Run(string(requested), func(t *testing.T) {
			am, mc, mockTunnel, st, _, _ := amHarness(t)

			// The UNINSTALLED teardown now removes the app's tunnels; not
			// tunnel-capable keeps this a unit test.
			mockTunnel.EXPECT().TunnelCapable().Return(false).Maybe()

			// The teardown transitions call removeApp; an unsupported compose
			// short-circuits it (no docker compose CLI) so the test stays a unit.
			mc.EXPECT().Compose().Return(&containerpkg.Compose{}).Maybe()
			// SetupComposeFiles now recovers published ports via the Docker API
			// (not `docker compose ps`); the teardown path reaches it.
			mc.EXPECT().GetComposePublishedPorts(mock.Anything, mock.Anything).Return(map[string]uint64{}, nil).Maybe()
			// interruptActiveTransition fires both cancel mechanisms; the
			// docker-API side has no active stream here and reports so.
			mc.EXPECT().CancelStream(mock.Anything).Return(nil).Maybe()

			amSeed(t, st, 62, "cancelme", common.UPDATING, common.PROD)

			// A compose update is in flight, with its cancel func registered as
			// updateComposeApp would.
			canceled := make(chan struct{}, 1)
			am.StateMachine.registerComposeTransitionCancel(common.PROD, 62, func() { canceled <- struct{}{} })

			payload := amPayload(62, "cancelme", requested, common.PROD)
			payload.DockerCompose = updComposeMap()
			require.NoError(t, am.RequestAppState(payload))

			// The cancel runs asynchronously via CancelTransition.
			select {
			case <-canceled:
			case <-time.After(2 * time.Second):
				t.Fatalf("%s requested during an update must cancel the in-flight compose update", requested)
			}
		})
	}
}

// interruptActiveTransition is the shared cancellation used by every UPDATING
// teardown transition and by teardown requests that hit a held transition
// lock. It fires BOTH cancel mechanisms — the compose transition's context
// (killing the CLI) and the docker-API pull stream — because payload
// .DockerCompose describes the INSTALLED release while an update that migrates
// the app between the flows runs the other flow's cancelable work. Each is a
// no-op when its side has nothing in flight.
func TestInterruptActiveTransitionCancelsComposeAndStream(t *testing.T) {
	t.Run("compose app cancels the update context", func(t *testing.T) {
		am, mc, _, _, _, _ := amHarness(t)

		canceled := false
		am.StateMachine.registerComposeTransitionCancel(common.PROD, 64, func() { canceled = true })

		mc.EXPECT().CancelStream(mock.Anything).Return(nil).Once()

		payload := amPayload(64, "compose", common.PRESENT, common.PROD)
		payload.DockerCompose = updComposeMap()

		am.StateMachine.interruptActiveTransition(payload)
		assert.True(t, canceled, "a compose update is canceled via its context")
	})

	t.Run("a legacy install updating to a compose release cancels the compose CLI", func(t *testing.T) {
		am, mc, _, _, _, _ := amHarness(t)

		// The marker says legacy (that IS the installed release), but the work
		// in flight is `docker compose pull` for the target. Picking the
		// mechanism by the marker lost this cancel entirely.
		canceled := false
		am.StateMachine.registerComposeTransitionCancel(common.PROD, 68, func() { canceled = true })

		mc.EXPECT().CancelStream(mock.Anything).Return(nil).Once()

		payload := amPayload(68, "tocompose", common.PRESENT, common.PROD)
		payload.DockerCompose = nil
		payload.NewDockerCompose = updComposeMap()

		am.StateMachine.interruptActiveTransition(payload)
		assert.True(t, canceled, "the in-flight compose pull must be cancelable")
	})

	t.Run("plain app cancels the docker-API pull stream", func(t *testing.T) {
		am, mc, _, _, _, _ := amHarness(t)

		mc.EXPECT().CancelStream(mock.Anything).Return(nil).Once()

		payload := amPayload(65, "plain", common.PRESENT, common.PROD)
		payload.DockerCompose = nil // non-compose

		am.StateMachine.interruptActiveTransition(payload)
		// mc's strict expectation verifies CancelStream was called exactly once.
	})

	t.Run("plain DEV app additionally cancels the build stream", func(t *testing.T) {
		am, mc, _, _, _, _ := amHarness(t)

		mc.EXPECT().CancelStream(mock.Anything).Return(nil).Twice()
		// CancelBuild only consults the compose process registry; a zero-value
		// Compose simply reports no active build.
		mc.EXPECT().Compose().Return(&containerpkg.Compose{}).Once()

		payload := amPayload(66, "plaindev", common.PRESENT, common.DEV)
		payload.DockerCompose = nil // non-compose

		am.StateMachine.interruptActiveTransition(payload)
		// pull stream + build stream: exactly two cancels.
	})

	t.Run("compose DEV app additionally cancels the compose build", func(t *testing.T) {
		am, mc, _, _, _, _ := amHarness(t)

		canceled := false
		am.StateMachine.registerComposeTransitionCancel(common.DEV, 67, func() { canceled = true })

		// CancelBuild only consults the compose process registry; a zero-value
		// Compose simply reports no active build, which interrupt ignores.
		mc.EXPECT().Compose().Return(&containerpkg.Compose{}).Once()
		// The docker-API side (pull + build stream) has nothing in flight.
		mc.EXPECT().CancelStream(mock.Anything).Return(nil).Twice()

		payload := amPayload(67, "composedev", common.PRESENT, common.DEV)
		payload.DockerCompose = updComposeMap()

		am.StateMachine.interruptActiveTransition(payload)
		assert.True(t, canceled, "the compose transition context must be canceled for DEV too")
	})
}

// =============================================================================
// updateComposeApp — guards
//
// container.Compose is a concrete struct that shells out to the docker compose
// CLI, so only the guards ahead of the first CLI call are unit-testable. The
// teardown/pull lifecycle belongs in compose_integration_test.go.
// =============================================================================

func TestUpdateComposeAppGuards(t *testing.T) {
	// The stage and version guards live on updateApp, the single entry point
	// both flows are routed from, so they are exercised through it.
	t.Run("rejects dev apps", func(t *testing.T) {
		sm, _, _, _, _ := wiredRunBuildSM(t)

		app := updSeedAt(t, sm, "compose-dev", common.PRESENT, "1.0.0")
		payload := updRequest("compose-dev", common.PRESENT, "1.0.0", "2.0.0")
		payload.Stage = common.DEV
		payload.NewDockerCompose = updComposeMap()

		err := sm.updateApp(payload, app)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot update dev app")
	})

	t.Run("rejects an update to the already-installed version", func(t *testing.T) {
		sm, _, _, _, _ := wiredRunBuildSM(t)

		app := updSeedAt(t, sm, "compose-same", common.PRESENT, "2.0.0")
		payload := updRequest("compose-same", common.PRESENT, "2.0.0", "2.0.0")
		payload.NewDockerCompose = updComposeMap()

		err := sm.updateApp(payload, app)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already equal to the newest version")
	})

	t.Run("rejects an update with no target release", func(t *testing.T) {
		sm, _, _, _, _ := wiredRunBuildSM(t)

		app := updSeedAt(t, sm, "compose-no-target", common.PRESENT, "1.0.0")
		payload := updRequest("compose-no-target", common.PRESENT, "1.0.0", "")
		payload.DockerCompose = updComposeMap()

		// The strict mock proves nothing was pulled or torn down.
		err := sm.updateApp(payload, app)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no release available to update to")
	})

	t.Run("reports compose being unsupported instead of downloading", func(t *testing.T) {
		sm, mc, _, _, _ := wiredRunBuildSM(t)

		app := updSeedAt(t, sm, "compose-unsupported", common.PRESENT, "1.0.0")
		payload := updRequest("compose-unsupported", common.PRESENT, "1.0.0", "2.0.0")

		mc.EXPECT().Compose().Return(&containerpkg.Compose{}).Once()

		err := sm.updateComposeApp(payload, app)
		require.Error(t, err)
		assert.True(t, errdefs.IsDockerComposeNotSupported(err),
			"expected a compose-unsupported error, got %v", err)
	})
}

// =============================================================================
// updateApp — flow routing
//
// A release decides which flow its app runs under: a docker-compose.yml makes
// it a compose project, a bare Dockerfile makes it a single container. An
// update can therefore change the flow, and it is the TARGET release
// (NewDockerCompose) that says which one to install — not the installed one
// (DockerCompose), which describes the version being replaced.
// =============================================================================

func TestUpdateAppRoutesOnTheTargetRelease(t *testing.T) {
	t.Run("compose install updating to a single-container release pulls the image", func(t *testing.T) {
		sm, mc, _, _, _ := wiredRunBuildSM(t)

		app := updSeedAt(t, sm, "to-legacy", common.PRESENT, "1.0.0")
		payload := updRequest("to-legacy", common.PRESENT, "1.0.0", "2.0.0")
		payload.DockerCompose = updComposeMap() // installed: compose
		payload.NewDockerCompose = nil          // target: single container

		mc.EXPECT().HandleRegistryLogins(mock.Anything).Return(nil).Once()
		mc.EXPECT().
			Pull(mock.Anything, mock.Anything, mock.Anything).
			Return(fwdDockerStream(), nil).
			Once()
		// The predecessor is a compose project: torn down through the compose
		// CLI, not through GetContainer/RemoveContainerByID. A zero-value
		// Compose reports itself unsupported, which short-circuits the teardown
		// without shelling out.
		mc.EXPECT().Compose().Return(&containerpkg.Compose{}).Once()
		fwdAllowLogs(mc)

		require.NoError(t, sm.updateApp(payload, app))

		app.StateLock.Lock()
		version := app.Version
		app.StateLock.Unlock()
		assert.Equal(t, "2.0.0", version, "the new version must be recorded as installed")
	})

	t.Run("single-container install updating to a compose release routes compose", func(t *testing.T) {
		sm, mc, _, _, _ := wiredRunBuildSM(t)

		app := updSeedAt(t, sm, "to-compose", common.PRESENT, "1.0.0")
		payload := updRequest("to-compose", common.PRESENT, "1.0.0", "2.0.0")
		payload.DockerCompose = nil                // installed: single container
		payload.NewDockerCompose = updComposeMap() // target: compose

		// Reaching the compose-unsupported guard IS the assertion: the compose
		// path was taken. The strict mock proves no image was pulled through
		// the single-container path, which is what used to 404 here.
		mc.EXPECT().Compose().Return(&containerpkg.Compose{}).Once()

		err := sm.updateApp(payload, app)
		require.Error(t, err)
		assert.True(t, errdefs.IsDockerComposeNotSupported(err),
			"expected the compose path to be taken, got %v", err)
	})
}

// A completed update promotes the target definition to the active one in the
// persisted requested state — including promoting it to nil. That row is what
// every later transition (and the offline re-drive in
// EnsureLocalRequestedStates) routes on, so a migrated app that kept the old
// marker would go on managing a flow it no longer runs under.
func TestUpdateAppPromotesTheInstalledFlowMarker(t *testing.T) {
	sm, mc, st, _, _ := wiredRunBuildSM(t)

	app := updSeedAt(t, sm, "promote", common.PRESENT, "1.0.0")
	payload := updRequest("promote", common.PRESENT, "1.0.0", "2.0.0")
	payload.DockerCompose = updComposeMap()
	payload.NewDockerCompose = nil

	mc.EXPECT().HandleRegistryLogins(mock.Anything).Return(nil).Once()
	mc.EXPECT().
		Pull(mock.Anything, mock.Anything, mock.Anything).
		Return(fwdDockerStream(), nil).
		Once()
	mc.EXPECT().Compose().Return(&containerpkg.Compose{}).Once()
	fwdAllowLogs(mc)

	require.NoError(t, sm.updateApp(payload, app))

	persisted, err := st.GetRequestedState(payload.AppKey, payload.Stage)
	require.NoError(t, err)
	assert.Nil(t, persisted.DockerCompose,
		"the app now runs as a single container; the compose marker must be gone")
}
