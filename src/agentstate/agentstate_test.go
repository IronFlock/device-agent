package agentstate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// device is a running agent as a Keeper sees it.
type device struct {
	t         *testing.T
	path      string
	now       time.Time
	connected bool
	update    *Update
	k         *Keeper
}

func newDevice(t *testing.T) *device {
	d := &device{t: t, path: filepath.Join(t.TempDir(), "run", "state.json"), now: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	d.k = New(Config{
		Path:      d.path,
		Version:   "1.6.17",
		PID:       4242,
		Connected: func() bool { return d.connected },
		Update:    func() *Update { return d.update },
	})
	return d
}

// step advances the clock by after and lets the Keeper look.
func (d *device) step(after time.Duration) {
	d.now = d.now.Add(after)
	d.k.Step(d.now)
}

func (d *device) read() State {
	d.t.Helper()
	data, err := os.ReadFile(d.path)
	require.NoError(d.t, err)
	var s State
	require.NoError(d.t, json.Unmarshal(data, &s))
	return s
}

func at(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func TestTheFirstStepWritesDisconnectedSinceTheStart(t *testing.T) {
	d := newDevice(t)
	start := d.now
	d.step(0)

	assert.Equal(t, State{Schema: 1, Version: "1.6.17", PID: 4242, Connection: Disconnected, Since: at(start), UpdatedAt: at(start)}, d.read())
}

func TestConnectAndDisconnectChangeSince(t *testing.T) {
	d := newDevice(t)
	d.step(0)

	d.connected = true
	d.step(Poll)
	connectedAt := d.now
	assert.Equal(t, Connected, d.read().Connection)
	assert.Equal(t, at(connectedAt), d.read().Since)

	// Unchanged: the file is not written again before Refresh.
	d.step(Poll)
	assert.Equal(t, at(connectedAt), d.read().UpdatedAt)

	// The cable is pulled: the next look says so.
	d.connected = false
	d.step(Poll)
	s := d.read()
	assert.Equal(t, Disconnected, s.Connection)
	assert.Equal(t, at(d.now), s.Since)

	d.connected = true
	d.step(Poll)
	assert.Equal(t, Connected, d.read().Connection)
	assert.Equal(t, at(d.now), d.read().Since)
}

func TestRefreshKeepsSinceAndMovesUpdatedAt(t *testing.T) {
	d := newDevice(t)
	d.connected = true
	d.step(0)
	since := d.now

	for i := 0; i < 11; i++ {
		d.step(Poll)
	}
	assert.Equal(t, at(since), d.read().UpdatedAt, "55 s: not refreshed yet")

	d.step(Poll)
	s := d.read()
	assert.Equal(t, at(since.Add(Refresh)), s.UpdatedAt, "refreshed after a minute")
	assert.Equal(t, at(since), s.Since, "the connection holds since the first look")
}

func TestAClockThatWentBackRefreshes(t *testing.T) {
	d := newDevice(t)
	d.step(0)
	d.step(-time.Hour)
	assert.Equal(t, at(d.now), d.read().UpdatedAt)
}

func TestTheUpdateInProgressIsWrittenWhenItChanges(t *testing.T) {
	d := newDevice(t)
	d.connected = true
	d.step(0)
	assert.Nil(t, d.read().Update)

	d.update = &Update{Phase: "probation", NewVersion: "1.6.17", OldVersion: "1.6.16", UpdatedAt: "2026-11-16T11:59:00Z"}
	d.step(Poll)
	assert.Equal(t, d.update, d.read().Update)
	assert.Equal(t, at(d.now), d.read().UpdatedAt)

	// The same update, as a new value: no write.
	written := d.now
	d.update = &Update{Phase: "probation", NewVersion: "1.6.17", OldVersion: "1.6.16", UpdatedAt: "2026-11-16T11:59:00Z"}
	d.step(Poll)
	assert.Equal(t, at(written), d.read().UpdatedAt)

	// Probation passed: the marker is gone.
	d.update = nil
	d.step(Poll)
	assert.Nil(t, d.read().Update)
	assert.Equal(t, at(d.now), d.read().UpdatedAt)
}

func TestTheFileIsReadableByEveryoneAndHasNoLeftovers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix modes")
	}
	d := newDevice(t)
	d.step(0)
	d.connected = true
	d.step(Poll)

	info, err := os.Stat(d.path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())

	entries, err := os.ReadDir(filepath.Dir(d.path))
	require.NoError(t, err)
	require.Len(t, entries, 1, "no temporary file is left")
	assert.Equal(t, "state.json", entries[0].Name())
}

func TestTheFileHasTheNamesOfTheContract(t *testing.T) {
	d := newDevice(t)
	d.update = &Update{Phase: "rolledback", NewVersion: "1.7.0", OldVersion: "1.6.17", Reason: "r", UpdatedAt: "u"}
	d.step(0)

	data, err := os.ReadFile(d.path)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(data, &fields))
	assert.ElementsMatch(t, []string{"schema", "version", "pid", "connection", "since", "updated_at", "update"}, keys(fields))
	assert.ElementsMatch(t, []string{"phase", "new_version", "old_version", "reason", "updated_at"}, keys(fields["update"].(map[string]any)))
	assert.Equal(t, float64(1), fields["schema"])
}

// A write that fails costs the file, never the agent: the next one works.
func TestAFailedWriteIsTriedAgainAtTheNextRefresh(t *testing.T) {
	d := newDevice(t)
	// A file where the folder should be.
	require.NoError(t, os.WriteFile(filepath.Dir(d.path), nil, 0o600))
	d.step(0)
	assert.True(t, d.k.failing)

	require.NoError(t, os.Remove(filepath.Dir(d.path)))
	d.step(Poll)
	assert.NoFileExists(t, d.path, "no retry at every look")

	d.step(Refresh)
	assert.False(t, d.k.failing)
	assert.Equal(t, Disconnected, d.read().Connection)
}

func keys(m map[string]any) []string {
	var k []string
	for key := range m {
		k = append(k, key)
	}
	return k
}
