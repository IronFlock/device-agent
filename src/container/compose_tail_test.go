//go:build !windows

package container

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The fake CLI stands in for `docker compose logs -f` with the shape that
// matters: a child (the compose plugin) that inherits the output pipe and
// outlives a signal to the CLI alone.
func startFakeTail(t *testing.T) (*ComposeLog, int) {
	t.Helper()

	pidFile := filepath.Join(t.TempDir(), "plugin.pid")
	c := newFakeComposeCompose(t, `sleep 300 &
echo $! > "`+pidFile+`"
echo first line
wait`)

	tail, err := c.LogStream("/apps/compose/x/docker-compose.yml")
	require.NoError(t, err)
	t.Cleanup(tail.Stop)
	require.Equal(t, "first line", <-tail.Lines)

	raw, err := os.ReadFile(pidFile)
	require.NoError(t, err)
	pluginPID, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, err)
	return tail, pluginPID
}

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func TestLogStreamStopEndsTheCLIAndItsPlugin(t *testing.T) {
	tail, pluginPID := startFakeTail(t)
	require.True(t, processAlive(pluginPID), "the stand-in plugin must be running before the stop")

	tail.Stop()

	_, open := <-tail.Lines
	require.False(t, open, "Lines must be closed once the tail is stopped")
	require.Eventually(t, func() bool { return !processAlive(pluginPID) }, 5*time.Second, 20*time.Millisecond,
		"the plugin the CLI started must end with it; a signal to the CLI alone leaves it running")
	select {
	case <-tail.done:
	default:
		t.Fatal("Stop must return only once the process is reaped")
	}
	require.NotNil(t, tail.cmd.ProcessState, "the tail must be reaped (Wait called)")

	tail.Stop() // a second stop is a no-op
}

func TestLogStreamReapsATailThatEndsByItself(t *testing.T) {
	c := newFakeComposeCompose(t, `echo one
echo two`)

	tail, err := c.LogStream("/apps/compose/x/docker-compose.yml")
	require.NoError(t, err)

	var lines []string
	for line := range tail.Lines {
		lines = append(lines, line)
	}
	require.Equal(t, []string{"one", "two"}, lines)

	select {
	case <-tail.done:
	case <-time.After(5 * time.Second):
		t.Fatal("an ended tail must be reaped")
	}
	// Without the Wait the process stays a zombie and ProcessState is never set.
	require.NotNil(t, tail.cmd.ProcessState)

	tail.Stop() // stopping an ended tail returns at once
}
