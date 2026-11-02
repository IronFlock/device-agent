//go:build integration

package apps

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"reagent/common"
	"reagent/config"
	"reagent/container"
	"reagent/logging"
	"reagent/store"
	"reagent/testutil/builders"
	"reagent/testutil/fakes"
	"reagent/testutil/mocks"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Compose lifecycle — integration
//
// Unit tests in this package drive the *non-compose* lifecycle handlers against
// a strict mocks.Container, but they deliberately skip every path that flows
// through Container.Compose(), because Compose() shells out to the real
// `docker compose` CLI (and `docker compose ps` further pipes through `jq`).
//
// This file fills that gap. It wires a StateMachine + StateObserver against the
// REAL container.NewDocker(...) client (not a mock) so Compose() is real, brings
// a tiny 2-service compose project up via the real CLI, and asserts the observer
// corrects the persisted app state to RUNNING off the real `compose ls` /
// `compose ps` output. The data/DB helpers (newExecTestDB) are reused from
// transition_execution_test.go; only NEW, uniquely-named helpers (prefixed
// composeIT*) are added here so nothing collides with the existing harness.
//
// Resource gate: every test skips cleanly when docker, `docker compose`, or `jq`
// is unavailable, so `just test-integration` stays green on bare hosts.
//
// Robustness: the compose project name is unique per run (incorporates t.Name()
// and os.Getpid()), and t.Cleanup force-tears-down the project (`compose down`)
// plus the temp compose files, ignoring "not found"/already-gone errors so
// back-to-back runs against one shared docker daemon never collide.
// =============================================================================

// composeITLongImage is a tiny public image used for the two long-running
// services. `sleep` keeps the containers in the "running" state long enough for
// the observer's real `compose ps` poll to report RUNNING; compose `up -d`
// auto-pulls it if absent.
const composeITLongImage = "alpine:latest"

// composeITDocker builds the REAL docker client and gates the whole test on the
// resources the compose code paths actually need:
//   - a reachable docker daemon (NewDocker + WaitForDaemon),
//   - a working `docker compose` CLI (Compose().Supported, set from
//     IsComposeSupported at construction),
//   - `jq` on PATH (Compose().Status pipes `compose ps` through jq).
//
// Any of these missing -> t.Skip, never a failure.
func composeITDocker(t *testing.T) *container.Docker {
	t.Helper()

	docker, err := container.NewDocker(builders.DefaultTestConfig())
	if err != nil {
		t.Skipf("skipping: cannot construct docker client: %v", err)
	}

	if err := docker.WaitForDaemon(2 * time.Second); err != nil {
		t.Skipf("skipping: docker daemon not available: %v", err)
	}

	if !docker.Compose().Supported() {
		t.Skip("skipping: `docker compose` is not available on this host")
	}

	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("skipping: `jq` is not available (required by Compose().Status)")
	}

	return docker
}

// composeITSanitize reduces an arbitrary string to the lowercase
// [a-z0-9] charset that is always safe both as a docker compose project name
// fragment and inside common.BuildComposeContainerName (which only lowercases).
func composeITSanitize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// composeITAppName returns a per-run-unique app name. The resulting compose
// project name (BuildComposeContainerName) therefore never collides with a
// concurrent run or a leftover project from a crashed previous run.
func composeITAppName(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("itc%d%s", os.Getpid(), composeITSanitize(t.Name()))
}

// composeITStateMachine wires a real StateMachine + StateObserver + AppStore
// against the REAL docker client and a temp-dir config (so SetupComposeFiles'
// filesystem writes land somewhere isolated and writable). It mirrors the
// structure of wiredRunBuildSM but swaps the mock for the live client.
func composeITStateMachine(t *testing.T, docker *container.Docker) (*StateMachine, *StateObserver, *store.AppStore, *config.Config) {
	t.Helper()

	// Point every Apps* dir at a fresh temp dir. SetupComposeFiles writes the
	// docker-compose.json + .env-compose under AppsBuildDir (DEV) / AppsComposeDir
	// (PROD); the registry URL is irrelevant here because we never log in.
	cfg := docker.GetConfig()
	base := t.TempDir()
	cfg.CommandLineArguments.AppsDirectory = base + "/apps"
	cfg.CommandLineArguments.AppsBuildDir = base + "/build"
	cfg.CommandLineArguments.AppsComposeDir = base + "/compose"
	cfg.CommandLineArguments.AppsSharedDir = base + "/shared"
	cfg.CommandLineArguments.CompressedBuildExtension = "tgz"

	db := newExecTestDB(t)
	msg := fakes.NewMessenger()

	appStore := store.NewAppStore(db, msg)
	observer := NewObserver(docker, &appStore, nil)
	logManager := logging.NewLogManager(docker, msg, db, appStore)
	sm := NewStateMachine(docker, &logManager, &observer, nil)
	// SetupComposeFiles resolves host ports through the AppManager the observer
	// points back to (rewriteComposeHostPorts), exactly as in production. The
	// tunnel manager is a strict mock: nothing on the compose path may reach it.
	NewAppManager(&sm, &appStore, &observer, mocks.NewTunnelManager(t))

	// Give any async log-history drain goroutines a moment before the DB closes
	// (newExecTestDB registers the close; LIFO ordering runs this sleep first).
	t.Cleanup(func() { time.Sleep(150 * time.Millisecond) })

	return &sm, &observer, &appStore, cfg
}

// composeITTwoServiceCompose returns a minimal valid 2-service compose document
// whose services are long-running so the project sits in "running" while the
// observer polls it. SetupComposeFiles overwrites the "name" key and injects
// env_file per service, so we leave those out here.
func composeITTwoServiceCompose() map[string]any {
	longRun := func() map[string]any {
		return map[string]any{
			"image":   composeITLongImage,
			"command": []any{"sleep", "120"},
		}
	}
	return map[string]any{
		"version": "3",
		"services": map[string]any{
			"svc_a": longRun(),
			"svc_b": longRun(),
		},
	}
}

// composeITOneShotCompose returns a compose document with a one-shot "migrate"
// service that exits 0 and a long-running "web" service that only starts once
// migrate has completed successfully — so `up -d` returns with migrate already
// exited, the common init-job pattern.
func composeITOneShotCompose() map[string]any {
	return map[string]any{
		"services": map[string]any{
			"migrate": map[string]any{
				"image":   composeITLongImage,
				"command": []any{"true"},
			},
			"web": map[string]any{
				"image":   composeITLongImage,
				"command": []any{"sleep", "120"},
				"depends_on": map[string]any{
					"migrate": map[string]any{"condition": "service_completed_successfully"},
				},
			},
		},
	}
}

// composeITRequireStarted runs the run handlers' post-`up` start check against
// the live project and fails the test unless it reports the project started.
func composeITRequireStarted(t *testing.T, compose *container.Compose, composePath string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	runningC, errC := compose.WaitForRunning(ctx, composePath, 200*time.Millisecond)
	select {
	case err := <-errC:
		require.NoError(t, err, "WaitForRunning should report the project started")
	case <-runningC:
	}
}

// composeITDrainUp drains a `compose` command's output channel and waits for the
// process to finish, mirroring what LogManager.StreamLogsChannel + cmd.Wait do
// in the production runProdComposeApp / runDevComposeApp handlers.
func composeITDrainUp(outputChan chan string, cmd *container.ComposeCmd) error {
	for range outputChan {
		// drain; the goroutine in composeCommandContext closes this on EOF
	}
	return cmd.Wait()
}

// composeITForceDown best-effort tears the project down via the real CLI. It is
// safe to call multiple times and on an already-removed project: errors (and the
// "no such project" case) are intentionally ignored.
func composeITForceDown(composePath string) {
	if composePath == "" {
		return
	}
	// `down -v` removes containers, the default network and named volumes for the
	// project, matching the cleanup the production cleanup paths perform.
	_ = exec.Command("docker", "compose", "-f", composePath, "down", "-v", "--remove-orphans").Run()
}

// =============================================================================
// Test 1: real compose up -> observer corrects persisted state to RUNNING
// =============================================================================

func TestIntegrationComposeUpAndObserverCorrection(t *testing.T) {
	docker := composeITDocker(t)
	sm, observer, st, _ := composeITStateMachine(t, docker)

	appName := composeITAppName(t)
	const appKey = uint64(1)
	const stage = common.DEV // DEV keeps generateDotEnvContents trivial (no tunnel lookups)

	// Build a compose transition payload and seed the app row so the observer can
	// upsert against an existing record and GetApp succeeds.
	payload := builders.BuildTransitionPayload(appName, common.RUNNING, stage)
	payload.AppKey = appKey
	payload.CurrentState = common.STARTING
	payload.DockerCompose = composeITTwoServiceCompose()
	payload.ContainerName = common.StageBasedResult{
		Dev:  common.BuildContainerName(common.DEV, appKey, appName),
		Prod: common.BuildContainerName(common.PROD, appKey, appName),
	}

	app, err := st.AddApp(payload)
	require.NoError(t, err)
	require.NotNil(t, app)

	composeName := common.BuildComposeContainerName(stage, appKey, appName)

	// SetupComposeFiles is the REAL production method: it writes the
	// docker-compose.json (stamping in the project "name" = composeName and the
	// per-service env_file) plus the .env-compose file under the temp build dir.
	composePath, err := sm.SetupComposeFiles(payload, app, false)
	require.NoError(t, err, "SetupComposeFiles should write the compose + env files")
	require.FileExists(t, composePath)

	// Force teardown registered up-front so even an assertion failure or a panic
	// between here and the explicit Down still removes the project.
	t.Cleanup(func() { composeITForceDown(composePath) })

	compose := docker.Compose()

	// --- Bring the project up via the REAL compose CLI (the exact call
	// runProdComposeApp / runDevComposeApp make). ---
	outputChan, upCmd, err := compose.Up(composePath)
	require.NoError(t, err, "compose up should start")
	require.NoError(t, composeITDrainUp(outputChan, upCmd), "compose up should exit 0")

	// --- Assert the containers are actually up, straight from the real client. ---
	// WaitForRunning is the start check the run handlers make after `up -d`.
	composeITRequireStarted(t, compose, composePath)

	statuses, err := compose.Status(composePath)
	require.NoError(t, err, "Status (real `compose ps` piped through jq) should not error")
	require.Len(t, statuses, 2, "the 2-service project should report 2 containers")
	for _, s := range statuses {
		assert.Equal(t, "running", s.State, "service %s should be running", s.Service)
	}

	// The project must show up in the real `compose ls` listing under composeName.
	listEntries, err := compose.List()
	require.NoError(t, err, "compose ls should not error")
	var found bool
	for _, e := range listEntries {
		if e.Name == composeName {
			found = true
			break
		}
	}
	require.True(t, found, "compose ls should list project %q", composeName)

	// --- Drive the observer's compose-correction path against the live project. ---
	// updateRemote=false -> NotifyLocal only (no WAMP call needed). Because every
	// container is running, aggregateStatuses returns RUNNING and the observer
	// corrects + persists the app to RUNNING.
	err = observer.CorrectComposeAppState(payload, nil, listEntries, false)
	require.NoError(t, err, "CorrectComposeAppState should succeed against the live project")

	app.StateLock.Lock()
	corrected := app.CurrentState
	app.StateLock.Unlock()
	assert.Equal(t, common.RUNNING, corrected,
		"observer should have corrected the live compose app to RUNNING")

	// Persisted, too: the local app-state row should now read RUNNING.
	fromDB, err := st.GetApp(appKey, stage)
	require.NoError(t, err)
	require.NotNil(t, fromDB)
	assert.Equal(t, common.RUNNING, fromDB.CurrentState,
		"the corrected RUNNING state should be persisted to the store")

	// --- Explicit teardown via the real CLI (the t.Cleanup above is the safety
	// net). After down, the project must disappear from `compose ls`. ---
	composeITForceDown(composePath)

	listAfter, err := compose.List()
	require.NoError(t, err)
	for _, e := range listAfter {
		assert.NotEqual(t, composeName, e.Name,
			"project %q should be gone after compose down", composeName)
	}
}

// =============================================================================
// Test 2: a one-shot service that exits 0 does not fail the start
// =============================================================================

func TestIntegrationComposeOneShotServiceStarts(t *testing.T) {
	docker := composeITDocker(t)
	sm, _, st, _ := composeITStateMachine(t, docker)

	appName := composeITAppName(t)
	const appKey = uint64(2)
	const stage = common.DEV

	payload := builders.BuildTransitionPayload(appName, common.RUNNING, stage)
	payload.AppKey = appKey
	payload.CurrentState = common.STARTING
	payload.DockerCompose = composeITOneShotCompose()
	payload.ContainerName = common.StageBasedResult{
		Dev:  common.BuildContainerName(common.DEV, appKey, appName),
		Prod: common.BuildContainerName(common.PROD, appKey, appName),
	}

	app, err := st.AddApp(payload)
	require.NoError(t, err)

	composePath, err := sm.SetupComposeFiles(payload, app, false)
	require.NoError(t, err)
	t.Cleanup(func() { composeITForceDown(composePath) })

	compose := docker.Compose()
	outputChan, upCmd, err := compose.Up(composePath)
	require.NoError(t, err)
	require.NoError(t, composeITDrainUp(outputChan, upCmd), "compose up should exit 0")

	composeITRequireStarted(t, compose, composePath)

	statuses, err := compose.Status(composePath)
	require.NoError(t, err)
	states := map[string]string{}
	for _, s := range statuses {
		states[s.Service] = s.State
	}
	assert.Equal(t, map[string]string{"migrate": "exited", "web": "running"}, states,
		"the one-shot must have finished while web runs — the case the start used to fail on")
}

// =============================================================================
// Test 3: the privileged label reaches the containers, and only a changed
// grant reads as a change
// =============================================================================

func TestIntegrationComposePrivilegedLabel(t *testing.T) {
	docker := composeITDocker(t)
	sm, _, st, _ := composeITStateMachine(t, docker)

	appName := composeITAppName(t)
	const appKey = uint64(3)
	const stage = common.DEV

	revoked := false
	payload := builders.BuildTransitionPayload(appName, common.RUNNING, stage)
	payload.AppKey = appKey
	payload.CurrentState = common.STARTING
	payload.DockerCompose = composeITTwoServiceCompose()
	payload.DockerCompose["services"].(map[string]any)["svc_a"].(map[string]any)["cap_add"] = []any{"NET_ADMIN"}
	payload.Privileged = &revoked
	payload.ContainerName = common.StageBasedResult{
		Dev:  common.BuildContainerName(common.DEV, appKey, appName),
		Prod: common.BuildContainerName(common.PROD, appKey, appName),
	}

	app, err := st.AddApp(payload)
	require.NoError(t, err)

	composePath, err := sm.SetupComposeFiles(payload, app, false)
	require.NoError(t, err)
	t.Cleanup(func() { composeITForceDown(composePath) })

	compose := docker.Compose()
	outputChan, upCmd, err := compose.Up(composePath)
	require.NoError(t, err)
	require.NoError(t, composeITDrainUp(outputChan, upCmd), "compose up should exit 0")
	composeITRequireStarted(t, compose, composePath)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	containers, err := container.ListComposeProjectContainers(ctx, docker, common.BuildComposeContainerName(stage, appKey, appName))
	require.NoError(t, err)
	require.Len(t, containers, 2)
	for _, cont := range containers {
		assert.Equal(t, "false", cont.Labels[privilegedLabel], "container %s", cont.Names)
	}

	assert.False(t, sm.privilegedAccessChanged(payload, app), "the grant the app started with is no change")

	granted := true
	payload.Privileged = &granted
	assert.True(t, sm.privilegedAccessChanged(payload, app), "granting it afterwards is")
}
