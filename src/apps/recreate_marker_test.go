package apps

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reagent/common"
	"reagent/errdefs"
	"reagent/testutil/mocks"
	"reagent/winservice"
	"testing"

	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// recreate_marker.go: the one-shot recreation of the app containers after
// the agent folder moved (the Windows layout migration), with their volumes.
// =============================================================================

// markerHarness is an AppManager whose config has an agent dir with the
// migration's (empty) marker in it.
func markerHarness(t *testing.T) (*AppManager, *mocks.Container, string) {
	t.Helper()
	am, mc, _, _, _, cfg := amHarness(t)
	agentDir := t.TempDir()
	cfg.CommandLineArguments.AgentDir = agentDir
	require.NoError(t, os.WriteFile(filepath.Join(agentDir, winservice.RecreateMarkerName), nil, 0o644))
	return am, mc, agentDir
}

func readMarker(t *testing.T, agentDir string) recreateMarker {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(agentDir, winservice.RecreateMarkerName))
	require.NoError(t, err)
	var m recreateMarker
	require.NoError(t, json.Unmarshal(data, &m))
	return m
}

var (
	appContainer = dockercontainer.Summary{
		ID:    "c1",
		Names: []string{"/" + common.BuildContainerName(common.PROD, 1, "app1")},
		Mounts: []dockercontainer.MountPoint{
			{Type: mount.TypeBind, Source: `C:\ProgramData\IronFlock\Reagent\apps\prod\app1`, Destination: "/data"},
			{Type: mount.TypeVolume, Name: "0123abcd", Destination: "/var/lib/postgresql/data"},
			{Type: mount.TypeVolume, Name: "named-cache", Destination: "/cache"},
		},
	}
	composeContainer = dockercontainer.Summary{
		ID:     "c2",
		Names:  []string{"/prod_2_app2-db-1"},
		Labels: map[string]string{composeProjectLabel: "prod_2_app2"},
		Mounts: []dockercontainer.MountPoint{{Type: mount.TypeVolume, Name: "compose-vol", Destination: "/x"}},
	}
	otherContainer = dockercontainer.Summary{ID: "c3", Names: []string{"/portainer"}}
)

// The marker consumed: single-container app containers are removed, their
// volumes recorded; compose and foreign containers are left alone; the
// listing runs once.
func TestRecreateContainersAfterMoveRemovesAppContainersOnce(t *testing.T) {
	am, mc, agentDir := markerHarness(t)
	mc.EXPECT().GetContainers(mock.Anything).Return([]dockercontainer.Summary{appContainer, composeContainer, otherContainer}, nil).Once()
	mc.EXPECT().RemoveContainerByID(mock.Anything, "c1", map[string]interface{}{"force": true}).Return(nil).Once()
	mc.EXPECT().WaitForContainerByID(mock.Anything, "c1", dockercontainer.WaitConditionRemoved).Return(0, nil).Once()

	require.NoError(t, am.RecreateContainersAfterMove(agentDir))

	m := readMarker(t, agentDir)
	assert.True(t, m.Listed)
	require.Contains(t, m.Containers, "prod_1_app1")
	assert.Equal(t, []recordedVolume{{Name: "0123abcd", Target: "/var/lib/postgresql/data"}, {Name: "named-cache", Target: "/cache"}}, m.Containers["prod_1_app1"].Volumes)
	assert.False(t, m.Containers["prod_1_app1"].Recreated)
	assert.Len(t, m.Containers, 1, "compose and foreign containers are not the marker's business")

	// the next start lists nothing again (the mock allows one listing) and
	// keeps the marker for the pending entry
	require.NoError(t, am.RecreateContainersAfterMove(agentDir))
	assert.FileExists(t, filepath.Join(agentDir, winservice.RecreateMarkerName))
}

// Docker unreachable (or failing): nothing is marked listed, the marker
// stays for the next start.
func TestRecreateContainersAfterMoveKeepsTheMarkerWhenDockerFails(t *testing.T) {
	previous := recreateRetryDelay
	recreateRetryDelay = 0
	defer func() { recreateRetryDelay = previous }()
	am, mc, agentDir := markerHarness(t)
	mc.EXPECT().GetContainers(mock.Anything).Return(nil, errors.New("Cannot connect to the Docker daemon")).Times(recreateRetries)
	require.Error(t, am.RecreateContainersAfterMove(agentDir))
	data, err := os.ReadFile(filepath.Join(agentDir, winservice.RecreateMarkerName))
	require.NoError(t, err)
	assert.Empty(t, data, "untouched")

	// a removal that fails (every time) leaves the entry recorded (the
	// volumes are safe) and the marker unlisted, so the next start removes
	// the container
	mc.EXPECT().GetContainers(mock.Anything).Return([]dockercontainer.Summary{appContainer}, nil).Times(recreateRetries)
	mc.EXPECT().RemoveContainerByID(mock.Anything, "c1", mock.Anything).Return(errors.New("device or resource busy")).Times(recreateRetries)
	require.Error(t, am.RecreateContainersAfterMove(agentDir))
	m := readMarker(t, agentDir)
	assert.False(t, m.Listed)
	assert.Contains(t, m.Containers, "prod_1_app1")
}

// No marker: nothing happens, Docker is not even asked.
func TestRecreateContainersAfterMoveWithoutAMarker(t *testing.T) {
	am, _, agentDir := markerHarness(t)
	require.NoError(t, os.Remove(filepath.Join(agentDir, winservice.RecreateMarkerName)))
	require.NoError(t, am.RecreateContainersAfterMove(agentDir))
}

// The recreated container carries the recorded volumes into its host
// config (an image's VOLUME keeps its data); a target the host config
// mounts already is left alone. Once every entry is recreated the marker
// goes.
func TestCreateContainerCarriesTheRecordedVolumes(t *testing.T) {
	am, mc, agentDir := markerHarness(t)
	name := common.BuildContainerName(common.PROD, 1, "app1")
	require.NoError(t, writeRecreateMarker(recreateMarkerPath(agentDir), &recreateMarker{
		Listed: true,
		Containers: map[string]*recreateEntry{name: {Volumes: []recordedVolume{
			{Name: "0123abcd", Target: "/var/lib/postgresql/data"},
			{Name: "stale", Target: "/data"},
		}}},
	}))
	mc.EXPECT().GetContainer(mock.Anything, name).Return(dockercontainer.Summary{}, errdefs.ContainerNotFound(errors.New("no such container"))).Once()
	mc.EXPECT().CreateContainer(mock.Anything, mock.Anything, mock.MatchedBy(func(h dockercontainer.HostConfig) bool {
		var volumes []mount.Mount
		for _, m := range h.Mounts {
			if m.Type == mount.TypeVolume {
				volumes = append(volumes, m)
			}
		}
		return len(volumes) == 1 && volumes[0].Source == "0123abcd" && volumes[0].Target == "/var/lib/postgresql/data" && len(h.Mounts) == 2
	}), mock.Anything, name).Return("new-id", nil).Once()

	sm := am.StateMachine
	app := createTestApp("app1", common.PRESENT, common.PROD)
	payload := createTestPayload("app1", common.RUNNING, common.PROD)
	payload.ContainerName.Prod = name
	hConfig := dockercontainer.HostConfig{Mounts: []mount.Mount{{Type: mount.TypeBind, Source: filepath.Join(agentDir, "apps", "prod", "app1"), Target: "/data"}}}
	id, err := sm.createContainer(payload, app, &dockercontainer.Config{}, &hConfig)
	require.NoError(t, err)
	assert.Equal(t, "new-id", id)
	assert.NoFileExists(t, filepath.Join(agentDir, winservice.RecreateMarkerName), "every entry recreated: the marker has served")
}

// A container the marker does not know (made after the move) is created as
// it is; a pending entry of another container keeps the marker.
func TestCreateContainerLeavesUnknownContainersAlone(t *testing.T) {
	am, mc, agentDir := markerHarness(t)
	other := common.BuildContainerName(common.PROD, 2, "app2")
	require.NoError(t, writeRecreateMarker(recreateMarkerPath(agentDir), &recreateMarker{
		Listed:     true,
		Containers: map[string]*recreateEntry{other: {Volumes: []recordedVolume{{Name: "v", Target: "/v"}}}},
	}))
	name := common.BuildContainerName(common.PROD, 1, "app1")
	mc.EXPECT().GetContainer(mock.Anything, name).Return(dockercontainer.Summary{}, errdefs.ContainerNotFound(errors.New("no such container"))).Once()
	mc.EXPECT().CreateContainer(mock.Anything, mock.Anything, mock.MatchedBy(func(h dockercontainer.HostConfig) bool { return len(h.Mounts) == 0 }), mock.Anything, name).Return("id", nil).Once()

	app := createTestApp("app1", common.PRESENT, common.PROD)
	payload := createTestPayload("app1", common.RUNNING, common.PROD)
	payload.ContainerName.Prod = name
	_, err := am.StateMachine.createContainer(payload, app, &dockercontainer.Config{}, &dockercontainer.HostConfig{})
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(agentDir, winservice.RecreateMarkerName), "app2's entry is still pending")
}
