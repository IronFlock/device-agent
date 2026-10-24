package apps

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	dockercontainer "github.com/docker/docker/api/types/container"

	"reagent/common"
	"reagent/config"
	containerpkg "reagent/container"
	"reagent/store"
	"reagent/testutil/builders"
	"reagent/testutil/mocks"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Update ordering: pull BEFORE teardown
//
// An update must not destroy the running version until the new release is fully
// downloaded. The 2026-08-27 field incident is the motivating failure: the old
// flow tore the app down first, then hit an unreachable registry (a wedged
// proxy on the edge PC), and a production QDS deployment sat dead for hours of
// login-timeout retries. With pull-first, every failure up to and including the
// download leaves the old version running and the update merely refused.
//
// The non-compose flow is asserted against the strict mocks.Container (call
// order recorded via Run hooks). The compose flow shells out through
// container.Compose, so it is driven end-to-end against a FAKE docker CLI
// (container.NewComposeWithBinary) that records its invocations to a file —
// covering the exact `pull` / `down` sequencing without a daemon.
//
// Helpers here are prefixed updOrd*; everything else (updRequest, updSeedAt,
// updSentUpdateStatuses, amHarness, wiredRunBuildSM, fwdDockerStream,
// fwdAllowLogs, notFoundErr) comes from the other files in this package.
// =============================================================================

func TestUpdateAppPullsBeforeTeardown(t *testing.T) {
	sm, mc, _, _, _ := wiredRunBuildSM(t)

	app := updSeedAt(t, sm, "pullfirst", common.RUNNING, "1.0.0")
	payload := updRequest("pullfirst", common.RUNNING, "1.0.0", "2.0.0")

	// The transition executes its container calls sequentially in one
	// goroutine, so plain slice appends are race-free.
	var seq []string

	mc.EXPECT().HandleRegistryLogins(mock.Anything).
		Run(func(map[string]common.DockerCredential) { seq = append(seq, "login") }).
		Return(nil).Once()
	mc.EXPECT().Pull(mock.Anything, mock.Anything, mock.Anything).
		Run(func(context.Context, string, containerpkg.PullOptions) { seq = append(seq, "pull") }).
		Return(fwdDockerStream(), nil).Once()

	// The OLD container exists and must only be removed after the pull.
	mc.EXPECT().GetContainer(mock.Anything, payload.ContainerName.Prod).
		Run(func(context.Context, string) { seq = append(seq, "get-container") }).
		Return(dockercontainer.Summary{ID: "old-ctr"}, nil).Once()
	mc.EXPECT().RemoveContainerByID(mock.Anything, "old-ctr", mock.Anything).
		Run(func(context.Context, string, map[string]interface{}) { seq = append(seq, "remove-container") }).
		Return(nil).Once()

	pollErrC := make(chan error, 1)
	pollErrC <- notFoundErr()
	close(pollErrC)
	stateC := make(chan containerpkg.ContainerState)
	mc.EXPECT().PollContainerState(mock.Anything, "old-ctr", mock.Anything).
		Return(stateC, pollErrC).Once()

	mc.EXPECT().RemoveImageByName(mock.Anything, payload.RegistryImageName.Prod, "1.0.0", mock.Anything).
		Run(func(context.Context, string, string, map[string]interface{}) { seq = append(seq, "remove-old-image") }).
		Return(nil).Once()

	fwdAllowLogs(mc)

	require.NoError(t, sm.updateApp(payload, app))

	assert.Equal(t,
		[]string{"login", "pull", "get-container", "remove-container", "remove-old-image"},
		seq,
		"the old container must outlive the registry login and the full download")
}

// A registry that cannot be reached (login fails or the pull errors) must
// refuse the update WITHOUT touching the old container. The strict mock proves
// the point structurally: no GetContainer / RemoveContainerByID expectation is
// registered, so any teardown attempt fails the test.
func TestUpdateAppRegistryFailureLeavesOldContainerUntouched(t *testing.T) {
	t.Run("login failure", func(t *testing.T) {
		sm, mc, _, _, _ := wiredRunBuildSM(t)

		app := updSeedAt(t, sm, "loginfail", common.RUNNING, "1.0.0")
		payload := updRequest("loginfail", common.RUNNING, "1.0.0", "2.0.0")

		mc.EXPECT().HandleRegistryLogins(mock.Anything).
			Return(fmt.Errorf("registry login to registry.test timed out after 10s")).Once()
		fwdAllowLogs(mc)

		err := sm.updateApp(payload, app)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "timed out")

		app.StateLock.Lock()
		version := app.Version
		app.StateLock.Unlock()
		assert.Equal(t, "1.0.0", version, "a refused update must not adopt the new version")
	})

	t.Run("pull failure", func(t *testing.T) {
		sm, mc, _, _, _ := wiredRunBuildSM(t)

		app := updSeedAt(t, sm, "pullfail", common.RUNNING, "1.0.0")
		payload := updRequest("pullfail", common.RUNNING, "1.0.0", "2.0.0")

		mc.EXPECT().HandleRegistryLogins(mock.Anything).Return(nil).Once()
		mc.EXPECT().Pull(mock.Anything, mock.Anything, mock.Anything).
			Return(nil, fmt.Errorf("pull access denied")).Once()
		fwdAllowLogs(mc)

		err := sm.updateApp(payload, app)
		require.Error(t, err)

		app.StateLock.Lock()
		version := app.Version
		app.StateLock.Unlock()
		assert.Equal(t, "1.0.0", version)
	})
}

// =============================================================================
// Compose update lifecycle against a fake docker CLI
// =============================================================================

// updOrdHasLoop reports whether a crashloop task is registered for the app.
func updOrdHasLoop(am *AppManager, appKey uint64, stage common.Stage) bool {
	am.crashLoopLock.Lock()
	defer am.crashLoopLock.Unlock()
	for task := range am.crashLoops {
		if task.Payload.Stage == stage && task.Payload.AppKey == appKey {
			return true
		}
	}
	return false
}

// updOrdComposeHarness builds the fake CLI + an amHarness-backed StateMachine
// ready to run updateComposeApp. pullExit is the exit code the fake CLI uses
// for `pull` invocations (0 = success). The fake matches the `pull` subcommand
// by EXACT argument equality (never substring — a temp path containing "pull"
// must not trigger it) and records one line of argv per invocation.
func updOrdComposeHarness(t *testing.T, pullExit int) (*StateMachine, *mocks.Container, *store.AppStore, *config.Config, string) {
	if runtime.GOOS == "windows" {
		t.Skip("the compose CLI stub is a POSIX shell script")
	}
	t.Helper()

	am, mc, mockTunnel, st, _, cfg := amHarness(t)
	sm := am.StateMachine

	mockTunnel.EXPECT().TunnelCapable().Return(false).Maybe()

	dir := t.TempDir()
	callsFile := filepath.Join(dir, "calls.log")
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %q
for a in "$@"; do
  if [ "$a" = "pull" ] && [ %d -ne 0 ]; then echo "pull failed"; exit %d; fi
done
exit 0
`, callsFile, pullExit, pullExit)
	binPath := filepath.Join(dir, "fake-docker")
	require.NoError(t, os.WriteFile(binPath, []byte(script), 0o755))

	fake := containerpkg.NewComposeWithBinary(cfg, binPath)
	mc.EXPECT().Compose().Return(fake).Maybe()
	// SetupComposeFiles recovers already-published host ports via the API.
	mc.EXPECT().GetComposePublishedPorts(mock.Anything, mock.Anything).Return(map[string]uint64{}, nil).Maybe()
	// A compose log stream that ends reads the project's history, which looks
	// the project up via the API.
	mc.EXPECT().ListContainers(mock.Anything, mock.Anything).Return(nil, nil).Maybe()
	fwdAllowLogs(mc)

	return sm, mc, st, cfg, callsFile
}

// updOrdSubcommand extracts the compose subcommand from a recorded argv line by
// exact field match, so path components never alias a subcommand.
func updOrdSubcommand(line string) string {
	fields := strings.Fields(line)
	for i, f := range fields {
		// Skip the flags that take a value; the first bare word after them is
		// the subcommand ("compose -f <file> --env-file <file> <subcommand> ...").
		if f == "compose" || f == "--remove-orphans" || f == "-d" {
			continue
		}
		if f == "-f" || f == "--env-file" {
			continue
		}
		if i > 0 && (fields[i-1] == "-f" || fields[i-1] == "--env-file") {
			continue
		}
		return f
	}
	return ""
}

// updOrdEnvMirrorDir is where SetupComposeFiles' env-file mirror lands — the
// dir the RUNNING containers bind-mount at /data/env.
func updOrdEnvMirrorDir(cfg *config.Config, appName string) string {
	return filepath.Join(cfg.CommandLineArguments.AppsDirectory, "prod", strings.ToLower(appName), "env")
}

// updOrdComposeApp seeds a compose app + returns the update payload moving it
// from 1.0.0 to 2.0.0 with a changed service definition.
func updOrdComposeApp(t *testing.T, st *store.AppStore, name string) (*common.App, common.TransitionPayload) {
	t.Helper()

	oldCompose := map[string]interface{}{
		"services": map[string]interface{}{"app": map[string]interface{}{"image": "registry.test/x:1.0.0"}},
	}
	newCompose := map[string]interface{}{
		"services": map[string]interface{}{"app": map[string]interface{}{"image": "registry.test/x:2.0.0"}},
	}

	seed := builders.BuildTransitionPayload(name, common.RUNNING, common.PROD)
	seed.AppKey = 1
	seed.CurrentState = common.RUNNING
	seed.DockerCompose = oldCompose
	app, err := st.AddApp(seed)
	require.NoError(t, err)
	require.NotNil(t, app)
	app.StateLock.Lock()
	app.Version = "1.0.0"
	app.StateLock.Unlock()

	payload := updRequest(name, common.RUNNING, "1.0.0", "2.0.0")
	payload.DockerCompose = oldCompose
	payload.NewDockerCompose = newCompose
	return app, payload
}

// updOrdCalls reads the fake CLI's invocation log; a missing file means the CLI
// was never invoked at all.
func updOrdCalls(t *testing.T, callsFile string) []string {
	t.Helper()

	raw, err := os.ReadFile(callsFile)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func TestUpdateComposeAppPullsBeforeTeardown(t *testing.T) {
	sm, _, st, cfg, callsFile := updOrdComposeHarness(t, 0)

	mcc := sm.Container.(*mocks.Container)
	mcc.EXPECT().HandleRegistryLogins(mock.Anything).Return(nil).Once()

	app, payload := updOrdComposeApp(t, st, "cpullfirst")

	require.NoError(t, sm.updateComposeApp(payload, app))

	calls := updOrdCalls(t, callsFile)
	require.Len(t, calls, 2, "exactly one pull and one down expected, got: %v", calls)
	assert.Equal(t, "pull", updOrdSubcommand(calls[0]), "the download must run first")
	assert.Equal(t, "down", updOrdSubcommand(calls[1]), "teardown only after every image is local")
	assert.Contains(t, strings.Fields(calls[1]), "--remove-orphans",
		"the teardown must remove containers of services dropped in the new compose")

	// The env-file mirror the running containers bind-mount must NOT have been
	// written by the update (it would inject the new release's values into the
	// old containers mid-download); the next start renders it instead.
	assert.NoDirExists(t, updOrdEnvMirrorDir(cfg, "cpullfirst"),
		"the update must not touch the /data/env mirror of the running app")

	// The successful update's bookkeeping (mirrors the non-compose test).
	app.StateLock.Lock()
	version := app.Version
	app.StateLock.Unlock()
	assert.Equal(t, "2.0.0", version)

	stored, err := st.GetRequestedState(1, common.PROD)
	require.NoError(t, err)
	assert.Equal(t, "2.0.0", stored.PresentVersion)
	assert.Equal(t, "2.0.0", stored.NewestVersion)
}

func TestUpdateComposeAppRegistryFailureLeavesProjectUntouched(t *testing.T) {
	t.Run("login failure: the CLI is never invoked", func(t *testing.T) {
		sm, _, st, cfg, callsFile := updOrdComposeHarness(t, 0)

		mcc := sm.Container.(*mocks.Container)
		mcc.EXPECT().HandleRegistryLogins(mock.Anything).
			Return(fmt.Errorf("registry login to registry.test timed out after 10s")).Once()

		app, payload := updOrdComposeApp(t, st, "cloginfail")

		err := sm.updateComposeApp(payload, app)
		require.Error(t, err)

		assert.Empty(t, updOrdCalls(t, callsFile),
			"an unreachable registry must abort the update before any compose command — the old project keeps running")
		assert.NoDirExists(t, updOrdEnvMirrorDir(cfg, "cloginfail"),
			"a refused update must not have injected the new release's env values into the running app's /data/env mirror")

		app.StateLock.Lock()
		version := app.Version
		app.StateLock.Unlock()
		assert.Equal(t, "1.0.0", version)
	})

	t.Run("pull failure: no teardown happens", func(t *testing.T) {
		sm, _, st, cfg, callsFile := updOrdComposeHarness(t, 1)

		mcc := sm.Container.(*mocks.Container)
		mcc.EXPECT().HandleRegistryLogins(mock.Anything).Return(nil).Once()

		app, payload := updOrdComposeApp(t, st, "cpullfail")

		err := sm.updateComposeApp(payload, app)
		require.Error(t, err)

		calls := updOrdCalls(t, callsFile)
		require.NotEmpty(t, calls, "the pull must have been attempted")
		for _, call := range calls {
			assert.NotEqual(t, "down", updOrdSubcommand(call),
				"a failed download must never tear the running project down")
		}
		assert.NoDirExists(t, updOrdEnvMirrorDir(cfg, "cpullfail"),
			"a refused update must not have injected the new release's env values into the running app's /data/env mirror")

		app.StateLock.Lock()
		version := app.Version
		app.StateLock.Unlock()
		assert.Equal(t, "1.0.0", version)
	})
}

// =============================================================================
// Crashloop convergence with pull-first updates
//
// A failed pull-first update leaves the old version RUNNING; the observer
// corrects the FAILED blip back to RUNNING — which is exactly the state the
// update was requested from. crashLoopWake's "reached the target state" exit
// must therefore consult the row's pending update, or the retry loop ends with
// the update silently dropped.
// =============================================================================

func TestCrashLoopWakeKeepsRetryingAPendingUpdateAtItsTargetState(t *testing.T) {
	am, _, mockTunnel, st, _, _ := amHarness(t)

	// RequestAppState reaches syncPortState — and with it this probe — BEFORE
	// the transition-lock check, so counting it observes that the wake actually
	// DISPATCHED a retry (a kept-but-inert loop would count zero).
	dispatched := 0
	mockTunnel.EXPECT().TunnelCapable().
		RunAndReturn(func() bool { dispatched++; return false }).Maybe()

	app := amSeed(t, st, 71, "pendingupd", common.RUNNING, common.PROD)

	// The requested-state row still carries the un-collapsed update.
	row := amPayload(71, "pendingupd", common.RUNNING, common.PROD)
	row.RequestUpdate = true
	row.PresentVersion = "1.0.0"
	row.NewestVersion = "2.0.0"
	require.NoError(t, st.UpdateLocalRequestedState(row))

	captured := row
	captured.Retrying = true
	task := &CrashLoop{Payload: captured}
	am.crashLoopLock.Lock()
	am.crashLoops[task] = struct{}{}
	am.crashLoopLock.Unlock()

	// Pre-lock the app so the wake's RequestAppState is dropped at the
	// transition lock: the test observes the wake's DECISION (loop kept,
	// retry dispatched) without running a whole update transition.
	require.False(t, app.SecureTransition())

	am.crashLoopWake(task)

	assert.True(t, updOrdHasLoop(am, 71, common.PROD),
		"reaching the target STATE must not end the loop while the row still carries the update")
	assert.GreaterOrEqual(t, dispatched, 1,
		"the wake must actually dispatch the retry, not merely keep the loop registered")
}

func TestCrashLoopWakeClearsALoopAtItsTargetStateWithNoPendingUpdate(t *testing.T) {
	am, _, mockTunnel, st, _, _ := amHarness(t)

	dispatched := 0
	mockTunnel.EXPECT().TunnelCapable().
		RunAndReturn(func() bool { dispatched++; return false }).Maybe()

	amSeed(t, st, 72, "settled", common.RUNNING, common.PROD)

	row := amPayload(72, "settled", common.RUNNING, common.PROD)
	require.NoError(t, st.UpdateLocalRequestedState(row))

	captured := row
	captured.Retrying = true
	task := &CrashLoop{Payload: captured}
	am.crashLoopLock.Lock()
	am.crashLoops[task] = struct{}{}
	am.crashLoopLock.Unlock()

	am.crashLoopWake(task)

	assert.False(t, updOrdHasLoop(am, 72, common.PROD),
		"at the target state with nothing pending the loop must end (existing behavior)")
	assert.Zero(t, dispatched, "a converged loop must not dispatch another transition")
}

// The row-read-failure branch must make the same distinction, using the
// CAPTURED payload (the row is unreadable there): at the target state with a
// pending captured update it must fall through to the captured-payload retry
// instead of clearing the loop.
func TestCrashLoopWakeRowReadFailureKeepsAPendingUpdate(t *testing.T) {
	am, _, mockTunnel, st, _, _ := amHarness(t)

	dispatched := 0
	mockTunnel.EXPECT().TunnelCapable().
		RunAndReturn(func() bool { dispatched++; return false }).Maybe()

	app := amSeed(t, st, 73, "rowless", common.RUNNING, common.PROD)
	// No requested-state row is written for app 73: GetRequestedState fails,
	// exercising the rowErr branch. (amSeed's AddApp writes the app row only.)
	// Align the in-memory target with the captured payload — amSeed defaults it
	// to PRESENT, which would take the branch's "target changed" exit instead.
	app.StateLock.Lock()
	app.RequestedState = common.RUNNING
	app.StateLock.Unlock()

	captured := amPayload(73, "rowless", common.RUNNING, common.PROD)
	captured.RequestUpdate = true
	captured.PresentVersion = "1.0.0"
	captured.NewestVersion = "2.0.0"
	captured.Retrying = true
	task := &CrashLoop{Payload: captured}
	am.crashLoopLock.Lock()
	am.crashLoops[task] = struct{}{}
	am.crashLoopLock.Unlock()

	require.False(t, app.SecureTransition())

	am.crashLoopWake(task)

	assert.True(t, updOrdHasLoop(am, 73, common.PROD),
		"an unreadable row must not let a pending captured update be dropped at its target state")
	assert.GreaterOrEqual(t, dispatched, 1, "the captured-payload retry must be dispatched")
}

// =============================================================================
// The helpers the observer guard is built from
// =============================================================================

func TestHasPendingUpdate(t *testing.T) {
	base := amPayload(80, "helper", common.RUNNING, common.PROD)

	pending := base
	pending.RequestUpdate = true
	pending.PresentVersion = "1.0.0"
	pending.NewestVersion = "2.0.0"
	assert.True(t, hasPendingUpdate(pending))

	collapsed := pending
	collapsed.PresentVersion = "2.0.0"
	assert.False(t, hasPendingUpdate(collapsed), "collapsed versions mean the update completed")

	notRequested := pending
	notRequested.RequestUpdate = false
	assert.False(t, hasPendingUpdate(notRequested))

	uninstalling := pending
	uninstalling.RequestedState = common.UNINSTALLED
	assert.False(t, hasPendingUpdate(uninstalling), "an uninstall wins over a pending update")
}
