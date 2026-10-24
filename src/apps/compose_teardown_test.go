package apps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"reagent/common"
	containerpkg "reagent/container"
	"reagent/errdefs"
	"reagent/store"
	"reagent/testutil/builders"
	"reagent/testutil/fakes"
	"reagent/testutil/mocks"

	"github.com/docker/docker/api/types/filters"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// teardownComposeProject — observed state first, Docker API when the CLI fails
//
// 2026-09-17, tls-sf002: `docker compose stop` aborted with a Go runtime crash
// on every run. Every stop of the app failed while its containers kept
// running, and nothing the user did from the UI could stop it. The teardown
// now asks the daemon before spawning the CLI and finishes the job through
// the Docker API when the CLI fails.
//
// The CLI is a fake script (container.NewComposeWithBinary) that records its
// argv per invocation; the daemon is the strict mocks.Container. Helpers
// (wiredStateMachine, seedApp, execPayload, fwdAllowLogs) come from the other
// files in this package.
// =============================================================================

// teardownHarness wires a StateMachine whose compose CLI is a fake script that
// crashes on `stop` with exit code stopExit (0 = every subcommand succeeds),
// and returns the file the fake appends one argv line per invocation to.
func teardownHarness(t *testing.T, stopExit int) (*StateMachine, *mocks.Container, *store.AppStore, *fakes.Messenger, string) {
	if runtime.GOOS == "windows" {
		t.Skip("the compose CLI stub is a POSIX shell script")
	}
	t.Helper()

	sm, mc, st, msg := wiredStateMachine(t)

	dir := t.TempDir()
	callsFile := filepath.Join(dir, "calls.log")
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %q
for a in "$@"; do
  if [ "$a" = "stop" ] && [ %d -ne 0 ]; then
    echo "panic: runtime error: invalid memory address" >&2
    echo "goroutine 1 [running]:" >&2
    echo "rax 0x0; rdx 0x6; rip 0x7f32f088195c" >&2
    exit %d
  fi
done
exit 0
`, callsFile, stopExit, stopExit)
	binPath := filepath.Join(dir, "fake-docker")
	require.NoError(t, os.WriteFile(binPath, []byte(script), 0o755))

	fake := containerpkg.NewComposeWithBinary(builders.DefaultTestConfig(), binPath)
	mc.EXPECT().Compose().Return(fake).Maybe()
	fwdAllowLogs(mc)

	return sm, mc, st, msg, callsFile
}

// composeCalls returns one argv line per compose invocation the fake CLI saw.
func composeCalls(t *testing.T, callsFile string) []string {
	t.Helper()

	data, err := os.ReadFile(callsFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	require.NoError(t, err)

	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func runningRelayContainer() []containerpkg.ContainerResult {
	return []containerpkg.ContainerResult{{ID: "relay-c1", State: "running"}}
}

// publishedAppLogContaining reports whether an app-log chunk carrying text was
// published (LogManager.Write publishes each line as {"chunk": text}).
func publishedAppLogContaining(msg *fakes.Messenger, text string) bool {
	for _, call := range msg.PublishCalls {
		if len(call.Args) == 0 {
			continue
		}
		dict, ok := call.Args[0].(common.Dict)
		if !ok {
			continue
		}
		chunk, _ := dict["chunk"].(string)
		if strings.Contains(chunk, text) {
			return true
		}
	}
	return false
}

func TestTeardownComposeProjectSkipsTheCLIWhenTheProjectHasNoContainers(t *testing.T) {
	sm, mc, st, _, callsFile := teardownHarness(t, 0)
	app := seedApp(t, st, "relay", common.RUNNING, common.PROD)
	payload := execPayload("relay", common.PRESENT, common.PROD)

	var seenFilters filters.Args
	mc.EXPECT().
		ListContainers(mock.Anything, mock.Anything).
		Run(func(_ context.Context, options common.Dict) {
			seenFilters, _ = options["filters"].(filters.Args)
		}).
		Return(nil, nil).
		Once()

	require.NoError(t, sm.teardownComposeProject(payload, app, filepath.Join(t.TempDir(), "docker-compose.json")))

	assert.Empty(t, composeCalls(t, callsFile), "no compose process is spawned for a project that is already down")
	assert.Equal(t, []string{"com.docker.compose.project=prod_1_relay_compose"}, seenFilters.Get("label"),
		"containers are found the way compose finds them: by project label")
}

// Negative control for the fallback: with a working CLI the API path stays out
// of it entirely (the strict mock rejects any StopContainerByID/Remove call).
func TestTeardownComposeProjectRunsStopThenRemove(t *testing.T) {
	sm, mc, st, _, callsFile := teardownHarness(t, 0)
	app := seedApp(t, st, "relay", common.RUNNING, common.PROD)
	payload := execPayload("relay", common.PRESENT, common.PROD)

	mc.EXPECT().ListContainers(mock.Anything, mock.Anything).Return(runningRelayContainer(), nil).Once()

	require.NoError(t, sm.teardownComposeProject(payload, app, filepath.Join(t.TempDir(), "docker-compose.json")))

	calls := composeCalls(t, callsFile)
	require.Len(t, calls, 2)
	assert.True(t, strings.HasSuffix(calls[0], " stop"), "first: %s", calls[0])
	assert.True(t, strings.HasSuffix(calls[1], " rm -f"), "then: %s", calls[1])
}

func TestTeardownComposeProjectFallsBackToTheAPIWhenTheCLICrashes(t *testing.T) {
	sm, mc, st, msg, callsFile := teardownHarness(t, 2)
	app := seedApp(t, st, "relay", common.RUNNING, common.PROD)
	payload := execPayload("relay", common.PRESENT, common.PROD)

	// Once before the CLI, once fresh for the fallback.
	mc.EXPECT().ListContainers(mock.Anything, mock.Anything).Return(runningRelayContainer(), nil).Times(2)
	mc.EXPECT().StopContainerByID(mock.Anything, "relay-c1", composeFallbackStopTimeout).Return(nil).Once()
	mc.EXPECT().RemoveContainerByID(mock.Anything, "relay-c1", map[string]interface{}{"force": true}).Return(nil).Once()

	require.NoError(t, sm.teardownComposeProject(payload, app, filepath.Join(t.TempDir(), "docker-compose.json")),
		"the transition's goal is reached: no container of the project is left")

	calls := composeCalls(t, callsFile)
	require.Len(t, calls, 1, "rm is not attempted after a failed stop")
	assert.True(t, strings.HasSuffix(calls[0], " stop"), "%s", calls[0])

	// The user learns from the app log what happened and why.
	assert.True(t, publishedAppLogContaining(msg, "stopped and removed directly"))
	assert.True(t, publishedAppLogContaining(msg, "panic: runtime error: invalid memory address"), "the CLI's own reason is quoted")
}

func TestTeardownComposeProjectReturnsTheCLIErrorWhenTheFallbackCannotFinish(t *testing.T) {
	sm, mc, st, _, _ := teardownHarness(t, 2)
	app := seedApp(t, st, "relay", common.RUNNING, common.PROD)
	payload := execPayload("relay", common.PRESENT, common.PROD)

	mc.EXPECT().ListContainers(mock.Anything, mock.Anything).Return(runningRelayContainer(), nil).Times(2)
	mc.EXPECT().StopContainerByID(mock.Anything, "relay-c1", mock.Anything).Return(errors.New("daemon busy")).Once()

	err := sm.teardownComposeProject(payload, app, filepath.Join(t.TempDir(), "docker-compose.json"))

	var composeErr *containerpkg.ComposeError
	require.ErrorAs(t, err, &composeErr, "the CLI failure is what the transition reports")
	assert.Equal(t, "stop", composeErr.Subcommand)
	assert.Contains(t, err.Error(), "panic: runtime error: invalid memory address")
}

func TestTeardownComposeProjectStillTriesTheCLIWhenTheDaemonCannotBeAsked(t *testing.T) {
	sm, mc, st, _, callsFile := teardownHarness(t, 2)
	app := seedApp(t, st, "relay", common.RUNNING, common.PROD)
	payload := execPayload("relay", common.PRESENT, common.PROD)

	mc.EXPECT().ListContainers(mock.Anything, mock.Anything).Return(nil, errors.New("daemon down")).Times(2)

	err := sm.teardownComposeProject(payload, app, filepath.Join(t.TempDir(), "docker-compose.json"))

	var composeErr *containerpkg.ComposeError
	require.ErrorAs(t, err, &composeErr)
	assert.Len(t, composeCalls(t, callsFile), 1, "a failed listing proves nothing about the project, so the CLI is still run")
}

func TestTeardownComposeProjectToleratesContainersVanishingDuringTheFallback(t *testing.T) {
	sm, mc, st, _, _ := teardownHarness(t, 2)
	app := seedApp(t, st, "relay", common.RUNNING, common.PROD)
	payload := execPayload("relay", common.PRESENT, common.PROD)

	gone := errdefs.ContainerNotFound(errors.New("No such container: relay-c1"))
	mc.EXPECT().ListContainers(mock.Anything, mock.Anything).Return(runningRelayContainer(), nil).Times(2)
	mc.EXPECT().StopContainerByID(mock.Anything, "relay-c1", mock.Anything).Return(gone).Once()
	mc.EXPECT().RemoveContainerByID(mock.Anything, "relay-c1", mock.Anything).Return(gone).Once()

	require.NoError(t, sm.teardownComposeProject(payload, app, filepath.Join(t.TempDir(), "docker-compose.json")),
		"a container that disappeared underneath is already gone, not a failure")
}
