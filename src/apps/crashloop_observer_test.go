package apps

import (
	"errors"
	"testing"

	"github.com/docker/docker/api/types/container"

	"reagent/common"
	"reagent/messenger/topics"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Observer re-drive vs. an active crashloop
//
// Field incident (2026-09-17, tls-sf002, app datarelay): `docker compose stop`
// crashed on every run. The stop transition failed, marked the app FAILED and
// started a ~5s backoff (attempt 1). One second later the compose observer
// saw the containers still running, corrected FAILED back to RUNNING and
// re-drove the stop as a FRESH request — which cleared the crashloop, ran the
// crashing stop again and started a new loop at attempt 1. Net effect: a
// retry every 1-2s with "attempt: 1, sleeping for ~5s" logged each time, the
// backoff never honoured and the counter never advancing, for as long as the
// CLI kept crashing. The user's own stop requests went through the same cycle.
//
// Helpers (amHarness, amSeed, amPayload, crashSeedTeardownRow, notFoundErr,
// fwdAllowLogs) come from the other files in this package.
// =============================================================================

// activeLoop registers a crashloop for the payload's app, as incrementCrashLoop
// does after a failed transition, and returns its task.
func activeLoop(am *AppManager, payload common.TransitionPayload, retries uint) *CrashLoop {
	payload.Retrying = true
	task := &CrashLoop{Payload: payload, Retries: retries}

	am.crashLoopLock.Lock()
	am.crashLoops[task] = struct{}{}
	am.crashLoopLock.Unlock()

	return task
}

func loopCount(am *AppManager) int {
	am.crashLoopLock.Lock()
	defer am.crashLoopLock.Unlock()
	return len(am.crashLoops)
}

func loopRetries(am *AppManager, task *CrashLoop) (uint, bool) {
	am.crashLoopLock.Lock()
	defer am.crashLoopLock.Unlock()
	_, alive := am.crashLoops[task]
	return task.Retries, alive
}

func TestObserverDefersReDriveToAnActiveCrashLoop(t *testing.T) {
	am, _, _, st, _, _ := amHarness(t)

	// The stop failed, the observer has just corrected the FAILED blip back to
	// RUNNING (the containers are still up), and the loop's goroutine is
	// sleeping out its backoff.
	app := amSeed(t, st, 81, "relay", common.RUNNING, common.PROD)
	stop := amPayload(81, "relay", common.PRESENT, common.PROD)
	crashSeedTeardownRow(t, am, app, stop)
	task := activeLoop(am, stop, 3)

	// Strict mocks without expectations: a re-driven stop would call into the
	// container mock and fail the test.
	stopObserving := am.StateObserver.driveCorrectedProdApp(app, common.RUNNING, stop.ContainerName.Prod)

	assert.False(t, stopObserving, "the observer keeps watching the containers")
	retries, alive := loopRetries(am, task)
	assert.True(t, alive, "the loop survives the correction")
	assert.Equal(t, uint(3), retries, "the backoff counter is untouched")
}

// Negative control: with no loop owning the app the observer re-drives as it
// always did. Here the re-driven stop fails, so a loop is started by the
// transition failure — the state the guard above protects.
func TestObserverReDrivesWhenNoCrashLoopOwnsTheApp(t *testing.T) {
	am, mc, mt, st, _, _ := amHarness(t)

	app := amSeed(t, st, 82, "relay", common.RUNNING, common.PROD)
	stop := amPayload(82, "relay", common.PRESENT, common.PROD)
	crashSeedTeardownRow(t, am, app, stop)
	require.False(t, am.hasActiveCrashLoop(82, common.PROD))

	mt.EXPECT().TunnelCapable().Return(false).Maybe()
	mt.EXPECT().GetState().Return(nil, nil).Maybe()
	fwdAllowLogs(mc)
	// The re-driven stop reaches the container; a daemon error (not "not
	// found") ends the transition in FAILED without further calls.
	mc.EXPECT().
		GetContainer(mock.Anything, stop.ContainerName.Prod).
		Return(container.Summary{}, errors.New("daemon busy")).
		Once()

	stopObserving := am.StateObserver.driveCorrectedProdApp(app, common.RUNNING, stop.ContainerName.Prod)

	assert.False(t, stopObserving)
	app.StateLock.Lock()
	state := app.CurrentState
	app.StateLock.Unlock()
	assert.Equal(t, common.FAILED, state, "the re-driven stop ran and failed")
	assert.True(t, am.hasActiveCrashLoop(82, common.PROD), "the failed transition started a crashloop")
}

// A retry that completes leaves no loop behind. Before, the entry lingered
// until the next fresh push, so a later failure resumed at the old, already
// long backoff — and with the observers now deferring to an active loop, a
// lingering entry would have silenced them for an app nothing was driving.
func TestCompletedRetryClearsItsCrashLoop(t *testing.T) {
	am, mc, mt, st, _, _ := amHarness(t)

	app := amSeed(t, st, 83, "settle", common.FAILED, common.PROD)
	uninstall := amPayload(83, "settle", common.UNINSTALLED, common.PROD)
	crashSeedTeardownRow(t, am, app, uninstall)
	activeLoop(am, uninstall, 4)

	mt.EXPECT().TunnelCapable().Return(false).Maybe()
	mt.EXPECT().GetState().Return(nil, nil).Maybe()
	mc.EXPECT().
		GetContainer(mock.Anything, uninstall.ContainerName.Prod).
		Return(container.Summary{}, notFoundErr()).
		Once()
	mc.EXPECT().
		RemoveImagesByName(mock.Anything, mock.Anything, mock.Anything).
		Return(nil).
		Once()
	fwdAllowLogs(mc)

	retry := uninstall
	retry.Retrying = true
	require.NoError(t, am.RequestAppState(retry))

	app.StateLock.Lock()
	state := app.CurrentState
	app.StateLock.Unlock()
	assert.Equal(t, common.UNINSTALLED, state)
	assert.Equal(t, 0, loopCount(am), "a completed retry leaves no loop behind")
}

// A retry that dies before its transition starts (here: no registry token)
// never reaches the failure path that extends the loop. The wake must count
// it as a failed attempt and schedule the next one, or the loop keeps its
// entry — which the observers defer to — with no goroutine behind it.
func TestCrashLoopWakeReArmsWhenTheRetryCannotStart(t *testing.T) {
	am, _, mt, st, msg, _ := amHarness(t)

	app := amSeed(t, st, 84, "token", common.FAILED, common.PROD)
	run := amPayload(84, "token", common.RUNNING, common.PROD)
	crashSeedTeardownRow(t, am, app, run)
	task := activeLoop(am, run, 2)

	mt.EXPECT().TunnelCapable().Return(false).Maybe()
	msg.SetCallError(string(topics.GetRegistryToken), errors.New("offline"))

	// No container expectations: the attempt must not get as far as a pull.
	am.crashLoopWake(task)

	retries, alive := loopRetries(am, task)
	assert.True(t, alive, "the loop keeps owning the app")
	assert.Equal(t, uint(3), retries, "the aborted attempt counts and the next wake is scheduled")

	app.StateLock.Lock()
	state := app.CurrentState
	app.StateLock.Unlock()
	assert.Equal(t, common.FAILED, state)
}

// A FRESH request that cannot start for want of a registry token — the agent
// re-drives its apps right after a router restart, before RESWARM has
// re-registered — has no loop behind it. An app that crashed in that window
// stayed FAILED until the next push; now a crashloop owns the retry.
func TestAFreshRequestThatCannotStartIsHandedToACrashLoop(t *testing.T) {
	am, _, mt, st, msg, _ := amHarness(t)

	app := amSeed(t, st, 85, "reconnect", common.FAILED, common.PROD)
	run := amPayload(85, "reconnect", common.RUNNING, common.PROD)
	crashSeedTeardownRow(t, am, app, run)

	mt.EXPECT().TunnelCapable().Return(false).Maybe()
	msg.SetCallError(string(topics.GetRegistryToken), errors.New("wamp.error.no_such_procedure"))

	require.Error(t, am.RequestAppState(run))
	defer am.clearCrashLoop(85, common.PROD) // the backoff goroutine exits at its wake

	assert.True(t, am.hasActiveCrashLoop(85, common.PROD), "a crashloop owns the retry")
	assert.Equal(t, 1, loopCount(am))
}

// The crashloop's own retry re-arms through driveRetry; arming here as well
// would run two backoff goroutines for one loop.
func TestARetryThatCannotStartDoesNotArmASecondTime(t *testing.T) {
	am, _, mt, st, msg, _ := amHarness(t)

	app := amSeed(t, st, 86, "rearm", common.FAILED, common.PROD)
	run := amPayload(86, "rearm", common.RUNNING, common.PROD)
	crashSeedTeardownRow(t, am, app, run)
	task := activeLoop(am, run, 2)

	mt.EXPECT().TunnelCapable().Return(false).Maybe()
	msg.SetCallError(string(topics.GetRegistryToken), errors.New("offline"))

	retry := run
	retry.Retrying = true
	require.Error(t, am.RequestAppState(retry))

	retries, alive := loopRetries(am, task)
	assert.True(t, alive)
	assert.Equal(t, uint(2), retries, "left to driveRetry")
	assert.Equal(t, 1, loopCount(am))
}

// DEV apps are driven by their developer, never by a crashloop.
func TestADevRequestThatCannotStartIsNotRetried(t *testing.T) {
	am, _, mt, st, msg, _ := amHarness(t)

	app := amSeed(t, st, 87, "devtoken", common.FAILED, common.DEV)
	run := amPayload(87, "devtoken", common.RUNNING, common.DEV)
	crashSeedTeardownRow(t, am, app, run)

	mt.EXPECT().TunnelCapable().Return(false).Maybe()
	msg.SetCallError(string(topics.GetRegistryToken), errors.New("offline"))

	_ = am.RequestAppState(run)

	assert.Equal(t, 0, loopCount(am))
}
