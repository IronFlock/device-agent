package container

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDaemon is a daemon that answers pings from a point in time on, with a
// clock that moves only when the wait sleeps.
type fakeDaemon struct {
	at     time.Time
	upFrom time.Time // zero: never
	pings  int
	sleeps []time.Duration
}

func (d *fakeDaemon) waiter() *daemonWait {
	return &daemonWait{
		now: func() time.Time { return d.at },
		sleep: func(s time.Duration) {
			d.sleeps = append(d.sleeps, s)
			d.at = d.at.Add(s)
		},
	}
}

func (d *fakeDaemon) ping() error {
	d.pings++
	if !d.upFrom.IsZero() && !d.at.Before(d.upFrom) {
		return nil
	}
	return errors.New("open //./pipe/docker_engine: The system cannot find the file specified.")
}

// captureLog sends the global logger to a buffer at level info, the level
// the lines must show at, until the test ends.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevLogger, prevLevel := log.Logger, zerolog.GlobalLevel()
	log.Logger = zerolog.New(&buf)
	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	t.Cleanup(func() {
		log.Logger = prevLogger
		zerolog.SetGlobalLevel(prevLevel)
	})
	return &buf
}

func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if raw == "" {
			continue
		}
		var line map[string]any
		require.NoError(t, json.Unmarshal([]byte(raw), &line), raw)
		lines = append(lines, line)
	}
	return lines
}

func TestWaitForDaemonAnswersAtOnceWithoutALine(t *testing.T) {
	buf := captureLog(t)
	d := &fakeDaemon{at: time.Unix(0, 0), upFrom: time.Unix(0, 0)}

	require.NoError(t, d.waiter().wait(d.ping, 5*time.Second))
	assert.Equal(t, 1, d.pings)
	assert.Empty(t, d.sleeps)
	assert.Empty(t, buf.String())
}

func TestWaitForDaemonBacksOffToTheCap(t *testing.T) {
	captureLog(t)
	d := &fakeDaemon{at: time.Unix(0, 0)}

	err := d.waiter().wait(d.ping, 30*time.Second)
	require.Error(t, err)

	want := []time.Duration{100, 200, 400, 800, 1600}
	for i, ms := range want {
		assert.Equal(t, ms*time.Millisecond, d.sleeps[i], "pause %d", i)
	}
	for i, s := range d.sleeps[len(want) : len(d.sleeps)-1] {
		assert.Equal(t, daemonRetryMax, s, "pause %d", len(want)+i)
	}
	// Not one ping per 100 ms: 30 s with Docker away take 20 pings, the last
	// one at the deadline.
	assert.Equal(t, 20, d.pings)
	assert.Equal(t, time.Unix(30, 0), d.at)
}

func TestWaitForDaemonPingsAtTheDeadline(t *testing.T) {
	captureLog(t)
	// The daemon comes up during the last pause before the 5 s deadline.
	d := &fakeDaemon{at: time.Unix(0, 0), upFrom: time.Unix(0, int64(4500*time.Millisecond))}

	require.NoError(t, d.waiter().wait(d.ping, 5*time.Second))
	assert.Equal(t, time.Unix(5, 0), d.at)
}

func TestWaitForDaemonLogsAnOutageNotEachPing(t *testing.T) {
	buf := captureLog(t)
	d := &fakeDaemon{at: time.Unix(0, 0), upFrom: time.Unix(0, 0).Add(12 * time.Minute)}
	w := d.waiter()

	// The agent waits 30 s at a time while Docker is away (agent.go).
	for w.wait(d.ping, 30*time.Second) != nil {
	}

	lines := logLines(t, buf)
	require.Len(t, lines, 4, buf.String())
	assert.Equal(t, "warn", lines[0]["level"])
	assert.Equal(t, "Docker daemon is not reachable, retrying", lines[0]["message"])
	assert.Contains(t, lines[0]["error"], "docker_engine")
	assert.Equal(t, "warn", lines[1]["level"])
	assert.Equal(t, "Docker daemon has not been reachable for 5m0s, retrying", lines[1]["message"])
	assert.Equal(t, "Docker daemon has not been reachable for 10m0s, retrying", lines[2]["message"])
	assert.Equal(t, "info", lines[3]["level"])
	assert.Equal(t, "Docker daemon is reachable again after 12m0s", lines[3]["message"])

	// The backoff went on across the 30 s waits: about one ping per 2 s.
	assert.Less(t, d.pings, 12*60/2+30)
}

func TestWaitForDaemonStartsOverAfterTheDaemonCameBack(t *testing.T) {
	buf := captureLog(t)
	d := &fakeDaemon{at: time.Unix(0, 0), upFrom: time.Unix(1, 0)}
	w := d.waiter()

	require.NoError(t, w.wait(d.ping, 5*time.Second))
	// The daemon goes away again: a new outage, logged and paced from the
	// start.
	d.upFrom = d.at.Add(time.Second)
	d.sleeps = nil
	require.NoError(t, w.wait(d.ping, 5*time.Second))

	assert.Equal(t, 100*time.Millisecond, d.sleeps[0])
	var messages []string
	for _, line := range logLines(t, buf) {
		messages = append(messages, line["message"].(string))
	}
	assert.Equal(t, []string{
		"Docker daemon is not reachable, retrying",
		"Docker daemon is reachable again after 2s",
		"Docker daemon is not reachable, retrying",
		"Docker daemon is reachable again after 2s",
	}, messages)
}
