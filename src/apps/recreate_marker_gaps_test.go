package apps

import (
	"errors"
	"os"
	"path/filepath"
	"reagent/common"
	"reagent/errdefs"
	"reagent/winservice"
	"sync"
	"testing"
	"time"

	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// The marker's boundaries: a cut right after a recorded removal, a
// transient Docker error, the reuse of a container an earlier start made,
// apps starting in parallel.

// The marker is flushed before it is renamed into place: an entry recorded
// there is the only record of a removed container's volumes.
func TestWriteRecreateMarkerSyncsBeforeTheRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, winservice.RecreateMarkerName)
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o644))
	synced := 0
	previous := syncMarkerFile
	syncMarkerFile = func(f *os.File) error {
		synced++
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, "old", string(data), "synced before the rename: the old content is still in place")
		return f.Sync()
	}
	defer func() { syncMarkerFile = previous }()
	require.NoError(t, writeRecreateMarker(path, &recreateMarker{Listed: true, Containers: map[string]*recreateEntry{}}))
	assert.Equal(t, 1, synced)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"listed": true`)
}

// A transient Docker error right after the daemon answered: the listing is
// tried again before the apps start on their stale containers.
func TestRecreateContainersAfterMoveRetriesATransientDockerError(t *testing.T) {
	previous := recreateRetryDelay
	recreateRetryDelay = 0
	defer func() { recreateRetryDelay = previous }()
	am, mc, agentDir := markerHarness(t)
	mc.EXPECT().GetContainers(mock.Anything).Return(nil, errors.New("error during connect: the pipe has been ended")).Once()
	mc.EXPECT().GetContainers(mock.Anything).Return([]dockercontainer.Summary{appContainer}, nil).Once()
	mc.EXPECT().RemoveContainerByID(mock.Anything, "c1", map[string]interface{}{"force": true}).Return(nil).Once()
	mc.EXPECT().WaitForContainerByID(mock.Anything, "c1", dockercontainer.WaitConditionRemoved).Return(0, nil).Once()
	require.NoError(t, am.RecreateContainersAfterMove(agentDir))
	m := readMarker(t, agentDir)
	assert.True(t, m.Listed)
	assert.Contains(t, m.Containers, "prod_1_app1")
}

// An error that stays gives up after the retries and keeps the marker.
func TestRecreateContainersAfterMoveGivesUpAfterTheRetries(t *testing.T) {
	previous := recreateRetryDelay
	recreateRetryDelay = 0
	defer func() { recreateRetryDelay = previous }()
	am, mc, agentDir := markerHarness(t)
	mc.EXPECT().GetContainers(mock.Anything).Return(nil, errors.New("Cannot connect to the Docker daemon")).Times(recreateRetries)
	require.Error(t, am.RecreateContainersAfterMove(agentDir))
	data, err := os.ReadFile(filepath.Join(agentDir, winservice.RecreateMarkerName))
	require.NoError(t, err)
	assert.Empty(t, data, "untouched")
}

// A crash between the recorded removal and the listing's end leaves an
// unlisted marker with an entry; the next start lists again (the container
// is gone), keeps the entry, and the app's start carries its volumes.
func TestRecreateContainersAfterMoveResumesAfterACrash(t *testing.T) {
	am, mc, agentDir := markerHarness(t)
	name := common.BuildContainerName(common.PROD, 1, "app1")
	require.NoError(t, writeRecreateMarker(recreateMarkerPath(agentDir), &recreateMarker{
		Listed:     false,
		Containers: map[string]*recreateEntry{name: {Volumes: []recordedVolume{{Name: "0123abcd", Target: "/var/lib/postgresql/data"}}}},
	}))
	mc.EXPECT().GetContainers(mock.Anything).Return([]dockercontainer.Summary{}, nil).Once()
	require.NoError(t, am.RecreateContainersAfterMove(agentDir))
	m := readMarker(t, agentDir)
	assert.True(t, m.Listed)
	require.Contains(t, m.Containers, name, "the entry of the removed container is kept")
	assert.False(t, m.Containers[name].Recreated)

	mc.EXPECT().GetContainer(mock.Anything, name).Return(dockercontainer.Summary{}, errdefs.ContainerNotFound(errors.New("no such container"))).Once()
	mc.EXPECT().CreateContainer(mock.Anything, mock.Anything, mock.MatchedBy(func(h dockercontainer.HostConfig) bool {
		return len(h.Mounts) == 1 && h.Mounts[0].Type == mount.TypeVolume && h.Mounts[0].Source == "0123abcd"
	}), mock.Anything, name).Return("new-id", nil).Once()
	app := createTestApp("app1", common.PRESENT, common.PROD)
	payload := createTestPayload("app1", common.RUNNING, common.PROD)
	payload.ContainerName.Prod = name
	id, err := am.StateMachine.createContainer(payload, app, &dockercontainer.Config{}, &dockercontainer.HostConfig{})
	require.NoError(t, err)
	assert.Equal(t, "new-id", id)
	assert.NoFileExists(t, filepath.Join(agentDir, winservice.RecreateMarkerName))
}

// A container that exists again (an earlier start created it and was cut
// before it recorded so) has served the marker too: the reuse marks the
// entry recreated, so the marker goes and later recreates are plain.
func TestCreateContainerMarksAReusedContainerRecreated(t *testing.T) {
	am, mc, agentDir := markerHarness(t)
	name := common.BuildContainerName(common.PROD, 1, "app1")
	require.NoError(t, writeRecreateMarker(recreateMarkerPath(agentDir), &recreateMarker{
		Listed:     true,
		Containers: map[string]*recreateEntry{name: {Volumes: []recordedVolume{{Name: "v", Target: "/v"}}}},
	}))
	mc.EXPECT().GetContainer(mock.Anything, name).Return(dockercontainer.Summary{ID: "existing", Names: []string{"/" + name}}, nil).Once()
	app := createTestApp("app1", common.PRESENT, common.PROD)
	payload := createTestPayload("app1", common.RUNNING, common.PROD)
	payload.ContainerName.Prod = name
	id, err := am.StateMachine.createContainer(payload, app, &dockercontainer.Config{}, &dockercontainer.HostConfig{})
	require.NoError(t, err)
	assert.Equal(t, "existing", id)
	assert.NoFileExists(t, filepath.Join(agentDir, winservice.RecreateMarkerName), "served")
}

// Apps start in parallel: two containers marking their entries at once must
// not lose each other's update (a read-modify-write of one file).
func TestMarkRecreatedIsSerialised(t *testing.T) {
	am, _, agentDir := markerHarness(t)
	a := common.BuildContainerName(common.PROD, 1, "app1")
	b := common.BuildContainerName(common.PROD, 2, "app2")
	require.NoError(t, writeRecreateMarker(recreateMarkerPath(agentDir), &recreateMarker{
		Listed:     true,
		Containers: map[string]*recreateEntry{a: {}, b: {}},
	}))
	sm := am.StateMachine
	// A reads the marker and pauses before it writes; B marks its entry
	// meanwhile. Serialised, B waits for A; otherwise A's write loses B's.
	aRead := make(chan struct{})
	var once sync.Once
	markRecreatedAfterRead = func(name string) {
		if name == a {
			once.Do(func() { close(aRead) })
			time.Sleep(200 * time.Millisecond)
		}
	}
	defer func() { markRecreatedAfterRead = nil }()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); sm.markRecreated(a) }()
	go func() { defer wg.Done(); <-aRead; sm.markRecreated(b) }()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the markers did not finish")
	}
	assert.NoFileExists(t, filepath.Join(agentDir, winservice.RecreateMarkerName), "both entries recreated: the marker has served")
}
