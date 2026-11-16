// Package agentstate keeps the agent's state file for local tools
// (`ironflock status`): /run/ironflock-device-agent/state.json on Linux,
// under the unit ironflock-device-agent.service. It says which agent runs
// (version, process ID), whether it is connected to IronFlock and since
// when, and the update in progress. docs/AGENT-STATE.md is the contract with
// the readers.
//
// The file is readable by everyone (`ironflock status` runs without root),
// is replaced atomically and holds no secret. The agent writes it at its
// start, at each change it sees (it looks every Poll), and every Refresh
// when nothing changed, so that updated_at shows a reader that the agent is
// alive.
package agentstate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog/log"
)

// Path is where the agent keeps the file: the unit's RuntimeDirectory,
// which systemd creates empty at each start and removes at each stop.
const Path = "/run/ironflock-device-agent/state.json"

// Schema is the version of the file's format. It changes only when a field
// changes its meaning or goes away; a new field leaves it as it is, and
// readers ignore the fields they do not know.
const Schema = 1

// The values of State.Connection.
const (
	Connected    = "connected"
	Disconnected = "disconnected"
)

const (
	// Poll is how often the agent looks at its connection.
	Poll = 5 * time.Second
	// Refresh is how often the agent writes the file when nothing changed.
	// /run is in memory: the writes cost no SD card.
	Refresh = time.Minute
)

// State is the file's content.
type State struct {
	Schema  int    `json:"schema"`
	Version string `json:"version"`
	PID     int    `json:"pid"`
	// Connection is connected while the agent's session to IronFlock is up,
	// else disconnected.
	Connection string `json:"connection"`
	// Since is when Connection began to hold, in this process: the start of
	// the process for the first disconnected.
	Since string `json:"since"`
	// UpdatedAt is when the agent last wrote the file.
	UpdatedAt string `json:"updated_at"`
	// Update is the self-update in progress (update-state.json in the
	// agent's folder); nil when there is none.
	Update *Update `json:"update,omitempty"`
}

// Update is the self-update in progress, in the words of update-state.json.
type Update struct {
	Phase      string `json:"phase"`
	NewVersion string `json:"new_version,omitempty"`
	OldVersion string `json:"old_version,omitempty"`
	Reason     string `json:"reason,omitempty"`
	UpdatedAt  string `json:"updated_at,omitempty"`
}

// Config is what a Keeper observes of the running agent.
type Config struct {
	Path    string
	Version string
	PID     int
	// Connected reports whether the session to IronFlock is up.
	Connected func() bool
	// Update returns the update in progress (nil: none). Optional.
	Update func() *Update
	// Refresh: zero is the package's Refresh.
	Refresh time.Duration
}

// Keeper keeps the file current. It is not safe for concurrent use: one
// goroutine runs it (Run).
type Keeper struct {
	cfg     Config
	state   State
	written time.Time
	started bool
	// failing: the last write failed; logged once until a write works again.
	failing bool
}

// New returns the Keeper of cfg.
func New(cfg Config) *Keeper {
	if cfg.Path == "" {
		cfg.Path = Path
	}
	if cfg.Refresh == 0 {
		cfg.Refresh = Refresh
	}
	return &Keeper{cfg: cfg}
}

// Step looks once at the agent and writes the file when the connection or
// the update changed, at the first step, and when Refresh has passed.
func (k *Keeper) Step(now time.Time) {
	connection := Disconnected
	if k.cfg.Connected != nil && k.cfg.Connected() {
		connection = Connected
	}
	var update *Update
	if k.cfg.Update != nil {
		update = k.cfg.Update()
	}

	changed := !k.started || connection != k.state.Connection || !sameUpdate(update, k.state.Update)
	// A clock that went back counts as Refresh passed.
	if age := now.Sub(k.written); !changed && age >= 0 && age < k.cfg.Refresh {
		return
	}
	if !k.started || connection != k.state.Connection {
		k.state.Since = stamp(now)
	}
	k.started = true
	k.state.Schema = Schema
	k.state.Version = k.cfg.Version
	k.state.PID = k.cfg.PID
	k.state.Connection = connection
	k.state.Update = update
	k.state.UpdatedAt = stamp(now)

	err := Write(k.cfg.Path, k.state)
	switch {
	case err != nil && !k.failing:
		log.Warn().Err(err).Msgf("could not write %s; local tools cannot see the agent's connection", k.cfg.Path)
	case err == nil && k.failing:
		log.Info().Msgf("%s is written again", k.cfg.Path)
	}
	k.failing = err != nil
	// A failed write waits for the next Refresh too: no retry every Poll.
	k.written = now
}

// Run steps every Poll until ctx is done.
func (k *Keeper) Run(ctx context.Context) {
	ticker := time.NewTicker(Poll)
	defer ticker.Stop()
	for {
		k.Step(time.Now())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Write replaces the file at path with s atomically: a temporary file in
// the same folder, renamed over it. A reader sees the old file or the new
// one, never a part. The folder is made when it is not there.
func Write(path string, s State) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // gone after the rename
	_, err = tmp.Write(append(data, '\n'))
	if err == nil {
		// CreateTemp makes 0600; every user may read the state.
		err = tmp.Chmod(0o644) //nolint:gosec // G302: the agent's state for local tools, no secret
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func stamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func sameUpdate(a, b *Update) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
