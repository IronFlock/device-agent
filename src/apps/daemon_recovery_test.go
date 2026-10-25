package apps

import (
	"context"
	"errors"
	"testing"
	"time"

	dockercontainer "github.com/docker/docker/api/types/container"

	"reagent/common"
	containerpkg "reagent/container"
	"reagent/messenger/topics"

	"github.com/docker/docker/api/types/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// daemon_recovery.go + the observers' transient-error resilience
//
// A Docker daemon outage used to (a) kill every plain-container observer on
// its first failed poll and (b) kill the container-event spawner permanently,
// so nothing reconciled app states when the daemon returned. These tests pin
// the recovery behavior: observers keep polling through an outage, and the
// spawner supervisor reopens the event stream and reconciles once the daemon
// answers again.
// =============================================================================

func TestObserveAppStateSurvivesDaemonOutage(t *testing.T) {
	so, mc, st, msg := newObserverHarness(t)
	so.pollingRate = 5 * time.Millisecond

	app := observerSeedApp(t, st, "outage-app", common.PRESENT, common.RUNNING, common.DEV)
	containerName := common.BuildContainerName(common.DEV, 1, "outage-app")

	// The daemon is unreachable for the first three polls — the old code tore
	// the observer down on the first one and never looked at the app again.
	mc.EXPECT().GetContainerState(mock.Anything, containerName).
		Return(containerpkg.ContainerState{}, errors.New("Cannot connect to the Docker daemon")).
		Times(3)
	// Daemon back: the container is running while the recorded state is still
	// PRESENT — exactly the divergence an outage leaves behind.
	mc.EXPECT().GetContainerState(mock.Anything, containerName).
		Return(containerpkg.ContainerState{Status: "running"}, nil)

	observerCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	so.observeAppState(observerCtx, common.DEV, 1, "outage-app")

	require.Eventually(t, func() bool {
		app.StateLock.Lock()
		current := app.CurrentState
		app.StateLock.Unlock()
		return current == common.RUNNING
	}, 3*time.Second, 10*time.Millisecond,
		"the observer must survive the outage and correct the state once the daemon returns")

	require.Eventually(t, func() bool {
		for _, call := range msg.GetCallCalls() {
			if call.Topic != topics.SetActualAppOnDeviceState || len(call.Args) == 0 {
				continue
			}
			if dict, ok := call.Args[0].(common.Dict); ok && dict["state"] == common.RUNNING {
				return true
			}
		}
		return false
	}, 3*time.Second, 10*time.Millisecond,
		"the corrected state must also be pushed to the backend")
}

func TestSpawnerSupervisorRecoversFromDaemonOutage(t *testing.T) {
	oldBackoff := spawnerRestartBackoff
	spawnerRestartBackoff = time.Millisecond
	t.Cleanup(func() { spawnerRestartBackoff = oldBackoff })

	// No docker CLI: the reconcile re-probes compose support (`docker
	// compose`), which then fails at once instead of running the runner's
	// real CLI — that has taken longer than the waits below on the Windows
	// runner.
	t.Setenv("PATH", t.TempDir())

	am, mc, _, _, _, _ := amHarness(t)
	so := am.StateObserver

	msgC1 := make(chan events.Message)
	errC1 := make(chan error, 1)
	msgC2 := make(chan events.Message)
	errC2 := make(chan error, 1)
	_ = errC2 // never fired: the second stream stays healthy for the test

	secondListen := make(chan struct{})
	reconcileRan := make(chan struct{})

	// The supervisor must poke an immediate device-status push on both
	// transitions: stream lost (badge on) and daemon back (badge off).
	transitionFires := make(chan struct{}, 4)
	so.SetDaemonTransitionFunc(func() { transitionFires <- struct{}{} })

	mc.EXPECT().ListenForContainerEvents(mock.Anything).Return(msgC1, errC1).Once()
	mc.EXPECT().ListenForContainerEvents(mock.Anything).
		Run(func(ctx context.Context) { close(secondListen) }).
		Return(msgC2, errC2).
		Once()

	// The daemon answers the recovery probe immediately.
	mc.EXPECT().Ping(mock.Anything).Return(containerpkg.Ping{}, nil).Once()

	// Reconcile steps over an empty app store. GetImages proves
	// CorrectAppStates ran; GetContainers proves the reconcile reached the
	// orphaned-container sweep.
	mc.EXPECT().Compose().Return(&containerpkg.Compose{}).Maybe()
	mc.EXPECT().GetImages(mock.Anything, "").Return(nil, nil).Once()
	mc.EXPECT().GetContainers(mock.Anything).
		Run(func(ctx context.Context) { close(reconcileRan) }).
		Return([]dockercontainer.Summary{}, nil).
		Once()

	// Zero seeded apps: this just latches the supervisor and starts spawner #1.
	require.NoError(t, so.ObserveAppStates())

	// Daemon outage: the docker client delivers exactly one error per stream.
	errC1 <- errors.New("unexpected EOF")

	// Generous waits: under -race on a CI runner the reconcile (SQLite with a
	// 2.5 s busy timeout) has taken longer than 3 s.
	select {
	case <-secondListen:
	case <-time.After(10 * time.Second):
		t.Fatal("the event spawner was not restarted after the stream died")
	}

	select {
	case <-reconcileRan:
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon-recovery reconcile did not run after the spawner restart")
	}

	// One push for the loss, one for the recovery.
	for i := 0; i < 2; i++ {
		select {
		case <-transitionFires:
		case <-time.After(3 * time.Second):
			t.Fatalf("expected 2 daemon-transition status pushes, got %d", i)
		}
	}
}

func TestNextSpawnerDelay(t *testing.T) {
	t.Run("resets to the base delay after a stable stream", func(t *testing.T) {
		got := nextSpawnerDelay(spawnerRestartBackoffMax, spawnerStableReset+time.Second)
		assert.Equal(t, spawnerRestartBackoff, got)
	})

	t.Run("doubles across rapid outage cycles", func(t *testing.T) {
		got := nextSpawnerDelay(spawnerRestartBackoff, time.Second)
		assert.Equal(t, spawnerRestartBackoff*2, got)
	})

	t.Run("caps the escalation", func(t *testing.T) {
		got := nextSpawnerDelay(spawnerRestartBackoffMax, time.Second)
		assert.Equal(t, spawnerRestartBackoffMax, got)
	})
}

func TestObserveAppStatesStartsSupervisorOnlyOnce(t *testing.T) {
	am, mc, _, _, _, _ := amHarness(t)
	so := am.StateObserver

	msgC := make(chan events.Message)
	errC := make(chan error, 1)

	// A second ListenForContainerEvents call would fail the strict mock.
	mc.EXPECT().ListenForContainerEvents(mock.Anything).Return(msgC, errC).Once()

	require.NoError(t, so.ObserveAppStates())
	require.NoError(t, so.ObserveAppStates())

	// Give a hypothetical duplicate supervisor a moment to call the mock.
	time.Sleep(50 * time.Millisecond)
}
