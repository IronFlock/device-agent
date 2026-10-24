//go:build windows || linux

package system

import (
	"reagent/config"
	"reagent/lifecycle"
	"reagent/selfupdate"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The activation interlock with the layout migration: while the migration
// is in progress (its health check waits for THIS version to stay up), a
// downloaded update is not activated and no restart is requested; checked
// again right before the swap.
func TestActivationWaitsForTheLayoutMigration(t *testing.T) {
	restarts := lifecycle.Subscribe()
	previousDelay, previousHold, previousActivate := activationDelay, migrationInProgress, activateUpdate
	t.Cleanup(func() {
		activationDelay, migrationInProgress, activateUpdate = previousDelay, previousHold, previousActivate
	})
	activationDelay = 0

	run := func(t *testing.T, inProgress []bool) (activated bool, restarted bool) {
		calls := 0
		migrationInProgress = func() bool {
			answer := inProgress[min(calls, len(inProgress)-1)]
			calls++
			return answer
		}
		done := make(chan struct{})
		activateUpdate = func(*selfupdate.Manager, string, string) error { activated = true; close(done); return nil }
		sys := System{config: &config.Config{CommandLineArguments: &config.CommandLineArguments{AgentDir: t.TempDir(), BinaryName: "device-agent"}}}
		sys.maybeActivateAgentUpdate(UpdateResult{DidAgentUpdate: true, DidUpdate: true, LatestVersion: "9.9.9", CurrentVersion: "1.4.0"})
		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
		}
		select {
		case <-restarts:
			restarted = true
		case <-time.After(200 * time.Millisecond):
		}
		return activated, restarted
	}

	activated, restarted := run(t, []bool{true})
	assert.False(t, activated, "held before the delay")
	assert.False(t, restarted)

	activated, restarted = run(t, []bool{false, true})
	assert.False(t, activated, "held at the second check: the migration started during the delay")
	assert.False(t, restarted)

	activated, restarted = run(t, []bool{false})
	require.True(t, activated, "no migration: activated")
	assert.True(t, restarted, "and a restart requested")
}
