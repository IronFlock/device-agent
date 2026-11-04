package logging

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"reagent/common"
	containerpkg "reagent/container"
	"reagent/testutil/builders"
	"reagent/testutil/mocks"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// fakeComposeCLI puts a `docker` on PATH that answers `compose ls` with one
// running project and turns `compose … logs -f` into a tail that prints one
// line unique to it and then runs until it is stopped. It returns the file
// that records every invocation.
func fakeComposeCLI(t *testing.T, project, configFile string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake docker CLI is a shell script")
	}

	binDir := t.TempDir()
	script := `#!/bin/sh
echo "$*" >> "$FAKE_DOCKER_CALLS"
case "$*" in
  "compose ls -a --format json")
    printf '[{"Name":"%s","Status":"running(1)","ConfigFiles":"%s"}]\n' "$FAKE_PROJECT" "$FAKE_CONFIG" ;;
  *" logs -f")
    echo "tailing $$"
    exec sleep 60 ;;
esac
`
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "docker"), []byte(script), 0o700)) //nolint:gosec // G306: test fake must be executable

	calls := filepath.Join(t.TempDir(), "calls")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_DOCKER_CALLS", calls)
	t.Setenv("FAKE_PROJECT", project)
	t.Setenv("FAKE_CONFIG", configFile)
	return calls
}

func callsEndingWith(t *testing.T, calls, suffix string) int {
	t.Helper()
	raw, err := os.ReadFile(calls)
	if os.IsNotExist(err) {
		return 0
	}
	require.NoError(t, err)

	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.HasSuffix(line, suffix) {
			n++
		}
	}
	return n
}

func activeEntry(lm *LogManager, containerName string) *LogProccess {
	lm.activeLogsMutex.Lock()
	defer lm.activeLogsMutex.Unlock()
	return lm.activeLogs[containerName]
}

func liveEntry(lm *LogManager, containerName string) bool {
	entry := activeEntry(lm, containerName)
	if entry == nil {
		return false
	}
	entry.subscriptionStateMutex.Lock()
	defer entry.subscriptionStateMutex.Unlock()
	return entry.Active
}

// tailLine waits for the app's live tail to have printed its line and returns
// it. Stopping a tail before that would kill the shell before the exec, and
// the test would not know which tail a persisted line came from.
func tailLine(t *testing.T, lm *LogManager, containerName string) string {
	t.Helper()
	var line string
	require.Eventually(t, func() bool {
		entry := activeEntry(lm, containerName)
		if entry == nil {
			return false
		}
		entry.logEntriesMutex.Lock()
		defer entry.logEntriesMutex.Unlock()
		if len(entry.logHistory) == 0 {
			return false
		}
		line = entry.logHistory[0].entry
		return true
	}, 5*time.Second, 10*time.Millisecond, "the tail must have printed its line")
	return line
}

// awaitTailCleanup waits until the ended tail's line is persisted. The end of
// a tail runs its cleanup in a goroutine of its own (emitChannelStream): the
// history goes to the database, then a read through the Docker API when no
// container line is stored. A test must not return before that, or the
// goroutine meets a closed database and a temp dir being removed.
func awaitTailCleanup(t *testing.T, lm *LogManager, containerName, line string) {
	t.Helper()
	require.Eventually(t, func() bool {
		history, err := lm.getPersistedLogHistory(containerName)
		if err != nil {
			return false
		}
		for _, entry := range history {
			if entry.entry == line {
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond, "the ended tail's history must be persisted")
}

// allowTailCleanupRead keeps the strict mock quiet should a cleanup still
// read through the Docker API (a tail stopped before it printed).
func allowTailCleanupRead(cont *mocks.Container) {
	cont.EXPECT().ListContainers(mock.Anything, mock.Anything).Return(nil, nil).Maybe()
}

// stopAllTails ends the fake tails a test started, so none outlives it.
func stopAllTails(lm *LogManager) {
	lm.activeLogsMutex.Lock()
	entries := make([]*LogProccess, 0, len(lm.activeLogs))
	for _, entry := range lm.activeLogs {
		entries = append(entries, entry)
	}
	lm.activeLogsMutex.Unlock()
	for _, entry := range entries {
		if entry.stop != nil {
			entry.stop()
		}
	}
}

func TestReviveDeadLogsLeavesALiveComposeTailAlone(t *testing.T) {
	lm, cont, _, db := newTestManager(t)
	addApp(t, db, 7, "composeapp", common.PROD)
	project := common.BuildComposeContainerName(common.PROD, 7, "composeapp")
	calls := fakeComposeCLI(t, project, "/apps/compose/composeapp/docker-compose.yml")

	compose := containerpkg.NewCompose(builders.DefaultTestConfig())
	compose.SetSupported(true)
	cont.EXPECT().Compose().Return(compose)
	allowTailCleanupRead(cont)
	t.Cleanup(func() { stopAllTails(lm) })

	containerName := common.BuildContainerName(common.PROD, 7, "composeapp")

	// The first connect starts the app's tail.
	require.NoError(t, lm.ReviveDeadLogs())
	require.Eventually(t, func() bool { return liveEntry(lm, containerName) }, 5*time.Second, 10*time.Millisecond,
		"the first sweep must start a tail for the running compose app")
	line := tailLine(t, lm, containerName)
	require.Equal(t, 1, callsEndingWith(t, calls, " logs -f"))

	// A reconnect runs the sweep again with the tail still running.
	require.NoError(t, lm.ReviveDeadLogs())
	require.Eventually(t, func() bool { return callsEndingWith(t, calls, "compose ls -a --format json") == 2 },
		5*time.Second, 10*time.Millisecond, "the second sweep must have listed the projects")
	time.Sleep(200 * time.Millisecond) // time for a wrongly started second tail to show up

	require.Equal(t, 1, callsEndingWith(t, calls, " logs -f"),
		"a reconnect must not start a second tail for an app whose tail is running")
	require.True(t, liveEntry(lm, containerName))

	stopAllTails(lm)
	awaitTailCleanup(t, lm, containerName, line)
}

func TestStreamComposeTailStopsTheEarlierTail(t *testing.T) {
	lm, cont, _, db := newTestManager(t)
	addApp(t, db, 7, "composeapp", common.PROD)
	fakeComposeCLI(t, "unused", "unused")
	allowTailCleanupRead(cont)

	compose := containerpkg.NewCompose(builders.DefaultTestConfig())
	compose.SetSupported(true)
	t.Cleanup(func() { stopAllTails(lm) })

	containerName := common.BuildContainerName(common.PROD, 7, "composeapp")
	composeFile := "/apps/compose/composeapp/docker-compose.yml"

	first, err := compose.LogStream(composeFile)
	require.NoError(t, err)
	_, err = lm.StreamComposeTail(first, containerName)
	require.NoError(t, err)
	firstLine := tailLine(t, lm, containerName)

	second, err := compose.LogStream(composeFile)
	require.NoError(t, err)
	_, err = lm.StreamComposeTail(second, containerName)
	require.NoError(t, err)

	// The earlier tail's process is gone, so its lines close.
	require.Eventually(t, func() bool {
		select {
		case _, open := <-first.Lines:
			return !open
		default:
			return false
		}
	}, 5*time.Second, 10*time.Millisecond, "the earlier tail must be stopped when a new one starts for the same app")
	awaitTailCleanup(t, lm, containerName, firstLine)

	// The new tail is the app's live entry.
	require.Eventually(t, func() bool {
		entry := activeEntry(lm, containerName)
		if entry == nil || entry.ChannelStream != second.Lines {
			return false
		}
		entry.subscriptionStateMutex.Lock()
		defer entry.subscriptionStateMutex.Unlock()
		return entry.Active
	}, 5*time.Second, 10*time.Millisecond)
	secondLine := tailLine(t, lm, containerName)
	require.NotEqual(t, firstLine, secondLine)

	second.Stop()
	awaitTailCleanup(t, lm, containerName, secondLine)
}
