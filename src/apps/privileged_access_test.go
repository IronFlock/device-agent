package apps

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"reagent/common"
	containerpkg "reagent/container"
	"reagent/errdefs"
	"reagent/system"
	"reagent/trust"

	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/go-connections/nat"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func boolPtr(value bool) *bool { return &value }

// =============================================================================
// Effective value and compose label
// =============================================================================

func TestGrantsPrivileged(t *testing.T) {
	assert.True(t, grantsPrivileged(nil), "an old backend sends nothing: the app keeps its privileges")
	assert.True(t, grantsPrivileged(boolPtr(true)))
	assert.False(t, grantsPrivileged(boolPtr(false)))
}

func TestSetComposePrivilegedLabel(t *testing.T) {
	t.Run("no labels", func(t *testing.T) {
		service := map[string]interface{}{}
		setComposePrivilegedLabel(service, false)
		assert.Equal(t, map[string]interface{}{privilegedLabel: "false"}, service["labels"])
	})

	t.Run("map labels keep the authored ones, the agent's value wins", func(t *testing.T) {
		service := map[string]interface{}{"labels": map[string]interface{}{"a": "1", privilegedLabel: "true"}}
		setComposePrivilegedLabel(service, false)
		assert.Equal(t, map[string]interface{}{"a": "1", privilegedLabel: "false"}, service["labels"])
	})

	t.Run("list labels keep the authored ones, the agent's value wins", func(t *testing.T) {
		service := map[string]interface{}{"labels": []interface{}{"a=1", privilegedLabel + "=false", privilegedLabel}}
		setComposePrivilegedLabel(service, true)
		assert.Equal(t, []interface{}{"a=1", privilegedLabel + "=true"}, service["labels"])
	})
}

// =============================================================================
// App-log notice
// =============================================================================

func TestUnprivilegedStartNotice(t *testing.T) {
	compose := map[string]interface{}{
		"services": map[string]interface{}{
			"plc": map[string]interface{}{"image": "x", "privileged": true, "devices": []interface{}{"/dev/ttyUSB0"}},
			"web": map[string]interface{}{"image": "y"},
			"bus": map[string]interface{}{"image": "z", "network_mode": "host"},
		},
		"secrets": map[string]interface{}{"shadow": map[string]interface{}{"file": "/etc/shadow"}},
	}

	t.Run("granted or old backend: nothing", func(t *testing.T) {
		assert.Empty(t, unprivilegedStartNotice(common.TransitionPayload{Stage: common.PROD, DockerCompose: compose}))
		assert.Empty(t, unprivilegedStartNotice(common.TransitionPayload{Stage: common.PROD, DockerCompose: compose, Privileged: boolPtr(true)}))
	})

	t.Run("compose: one line per affected service, top-level last", func(t *testing.T) {
		lines := unprivilegedStartNotice(common.TransitionPayload{Stage: common.PROD, DockerCompose: compose, Privileged: boolPtr(false)})
		assert.Equal(t, []string{
			"started without privileged access (the release does not request it, or the project has not granted it): removed network_mode host from service bus",
			"started without privileged access (the release does not request it, or the project has not granted it): removed privileged, devices /dev/ttyUSB0 from service plc",
			"started without privileged access (the release does not request it, or the project has not granted it): removed secret_file shadow from the top-level settings",
		}, lines)
	})

	t.Run("compose without privileged settings: nothing to report", func(t *testing.T) {
		plain := map[string]interface{}{"services": map[string]interface{}{"web": map[string]interface{}{"image": "y"}}}
		assert.Empty(t, unprivilegedStartNotice(common.TransitionPayload{Stage: common.PROD, DockerCompose: plain, Privileged: boolPtr(false)}))
	})

	t.Run("DEV points the developer at the declaration", func(t *testing.T) {
		lines := unprivilegedStartNotice(common.TransitionPayload{Stage: common.DEV, DockerCompose: compose, Privileged: boolPtr(false)})
		require.Len(t, lines, 3)
		for _, line := range lines {
			assert.Contains(t, line, "the development build does not declare `privileged` with a `reason` in .ironflock/data-template.yml")
			assert.NotContains(t, line, "project")
		}
	})

	t.Run("single container: one line", func(t *testing.T) {
		lines := unprivilegedStartNotice(common.TransitionPayload{Stage: common.PROD, Privileged: boolPtr(false)})
		assert.Equal(t, []string{"started without privileged access (the release does not request it, or the project has not granted it): no host devices, no extra capabilities and no host D-Bus"}, lines)
	})
}

// =============================================================================
// Compose: SetupComposeFiles strips before its own injections
// =============================================================================

func privilegedTestCompose() map[string]interface{} {
	return map[string]interface{}{
		"services": map[string]interface{}{
			"plc": map[string]interface{}{
				"image":      "registry.test/plc:1",
				"privileged": true,
				"devices":    []interface{}{"/dev/ttyUSB0:/dev/ttyUSB0"},
				"volumes":    []interface{}{"./data:/data", "/dev:/dev"},
			},
		},
	}
}

func renderPrivilegedCompose(t *testing.T, payload common.TransitionPayload, updatingApp bool) map[string]interface{} {
	t.Helper()

	am, mockContainer, _, _, _, _ := amHarness(t)
	mockContainer.EXPECT().GetComposePublishedPorts(mock.Anything, mock.Anything).
		Return(map[string]uint64{}, nil).Maybe()

	app := &common.App{AppKey: payload.AppKey, AppName: payload.AppName, Stage: payload.Stage}
	composePath, err := am.StateMachine.SetupComposeFiles(payload, app, updatingApp)
	require.NoError(t, err)

	rendered, err := os.ReadFile(composePath)
	require.NoError(t, err)

	var compose map[string]interface{}
	require.NoError(t, json.Unmarshal(rendered, &compose))
	return compose["services"].(map[string]interface{})["plc"].(map[string]interface{})
}

func TestSetupComposeFilesPrivilegedAccess(t *testing.T) {
	base := common.TransitionPayload{Stage: common.PROD, AppKey: 7, AppName: "plcapp"}

	t.Run("not granted: privileged settings stripped, the agent's own binds kept", func(t *testing.T) {
		payload := base
		payload.DockerCompose = privilegedTestCompose()
		payload.Privileged = boolPtr(false)

		service := renderPrivilegedCompose(t, payload, false)

		assert.NotContains(t, service, "privileged")
		assert.NotContains(t, service, "devices")
		volumes := service["volumes"].([]interface{})
		assert.Contains(t, volumes, "./data:/data")
		assert.NotContains(t, volumes, "/dev:/dev")
		hasEnvMount := false
		for _, volume := range volumes {
			if strings.HasSuffix(volume.(string), ":/data/env:ro") {
				hasEnvMount = true
			}
		}
		assert.True(t, hasEnvMount, "the env-files bind is the agent's, not the app's: it must survive")
		assert.Equal(t, map[string]interface{}{privilegedLabel: "false"}, service["labels"])
		assert.Equal(t, true, payload.DockerCompose["services"].(map[string]interface{})["plc"].(map[string]interface{})["privileged"],
			"the payload keeps the authored definition")
	})

	t.Run("old backend (nil): passed through as authored", func(t *testing.T) {
		payload := base
		payload.DockerCompose = privilegedTestCompose()

		service := renderPrivilegedCompose(t, payload, false)

		assert.Equal(t, true, service["privileged"])
		assert.Equal(t, []interface{}{"/dev/ttyUSB0:/dev/ttyUSB0"}, service["devices"])
		assert.Contains(t, service["volumes"], "/dev:/dev")
		assert.Equal(t, map[string]interface{}{privilegedLabel: "true"}, service["labels"])
	})

	t.Run("update renders the newest release with its own grant", func(t *testing.T) {
		payload := base
		payload.DockerCompose = map[string]interface{}{"services": map[string]interface{}{"plc": map[string]interface{}{"image": "registry.test/plc:0"}}}
		payload.Privileged = boolPtr(true)
		payload.NewDockerCompose = privilegedTestCompose()
		payload.NewPrivileged = boolPtr(false)

		service := renderPrivilegedCompose(t, payload, true)

		assert.NotContains(t, service, "privileged")
		assert.Equal(t, map[string]interface{}{privilegedLabel: "false"}, service["labels"])
	})

	t.Run("a start renders the present release with its own grant", func(t *testing.T) {
		payload := base
		payload.DockerCompose = privilegedTestCompose()
		payload.Privileged = boolPtr(true)
		payload.NewDockerCompose = privilegedTestCompose()
		payload.NewPrivileged = boolPtr(false)

		service := renderPrivilegedCompose(t, payload, false)

		assert.Equal(t, true, service["privileged"])
		assert.Equal(t, map[string]interface{}{privilegedLabel: "true"}, service["labels"])
	})
}

// =============================================================================
// Single container: host config and mounts
// =============================================================================

// todaysHostConfig is the host config every single-container app got before
// privileged access could be withheld; nil and true must still produce it.
func todaysHostConfig(mounts []mount.Mount) dockercontainer.HostConfig {
	hostConfig := dockercontainer.HostConfig{
		RestartPolicy: dockercontainer.RestartPolicy{
			Name: "no",
		},
		Mounts: mounts,
		Resources: dockercontainer.Resources{
			Devices: []dockercontainer.DeviceMapping{
				{
					PathOnHost:      "/dev",
					PathInContainer: "/dev",
				},
			},
		},
		Privileged:   true,
		NetworkMode:  "bridge",
		PortBindings: nat.PortMap{},
		ExtraHosts:   []string{hostGatewayEntry},
		CapAdd:       []string{"ALL"},
	}
	if system.HasNvidiaGPU() {
		hostConfig.Runtime = "nvidia"
	}
	return hostConfig
}

// fakeHostAccessDirs points the host-access binds at existing temp dirs so
// they are bound wherever the test runs.
func fakeHostAccessDirs(t *testing.T) (w1Dir string, dbusDir string) {
	t.Helper()
	originalW1, originalDBus := hostW1DevicesDir, hostDBusDir
	t.Cleanup(func() { hostW1DevicesDir, hostDBusDir = originalW1, originalDBus })
	hostW1DevicesDir, hostDBusDir = t.TempDir(), t.TempDir()
	return hostW1DevicesDir, hostDBusDir
}

func cudaMounts() []mount.Mount {
	if _, err := os.Stat("/usr/local/cuda"); os.IsNotExist(err) {
		return nil
	}
	return []mount.Mount{{Type: mount.TypeBind, Source: "/usr/local/cuda", Target: "/usr/local/cuda"}}
}

func TestComputeContainerConfigsPrivilegedAccess(t *testing.T) {
	w1Dir, dbusDir := fakeHostAccessDirs(t)

	compute := func(t *testing.T, privileged *bool) (*dockercontainer.HostConfig, []mount.Mount, []mount.Mount) {
		t.Helper()
		sm, mc, st, _, cfg := wiredRunBuildSM(t)

		app := seedApp(t, st, "hostcfg", common.PRESENT, common.PROD)
		payload := execPayload("hostcfg", common.RUNNING, common.PROD)
		payload.Privileged = privileged

		mc.EXPECT().GetImage(mock.Anything, payload.RegistryImageName.Prod, mock.Anything).
			Return(containerpkg.ImageResult{}, nil).Once()

		_, hostConfig, err := sm.computeContainerConfigs(payload, app)
		require.NoError(t, err)

		baseMounts := []mount.Mount{
			{Type: mount.TypeBind, Source: cfg.CommandLineArguments.AppsSharedDir, Target: "/shared"},
			{Type: mount.TypeBind, Source: cfg.CommandLineArguments.AppsDirectory + "/prod/hostcfg", Target: "/data"},
		}
		todaysMounts := append(append([]mount.Mount{}, baseMounts...),
			mount.Mount{Type: mount.TypeBind, Source: w1Dir, Target: "/sys/bus/w1/devices"},
			mount.Mount{Type: mount.TypeBind, Source: dbusDir, Target: "/run/dbus"},
		)
		todaysMounts = append(todaysMounts, cudaMounts()...)
		unprivilegedMounts := append(baseMounts, cudaMounts()...)

		return hostConfig, todaysMounts, unprivilegedMounts
	}

	t.Run("nil (old backend) is exactly today's host config", func(t *testing.T) {
		hostConfig, todaysMounts, _ := compute(t, nil)
		assert.Equal(t, todaysHostConfig(todaysMounts), *hostConfig)
	})

	t.Run("granted is exactly today's host config", func(t *testing.T) {
		hostConfig, todaysMounts, _ := compute(t, boolPtr(true))
		assert.Equal(t, todaysHostConfig(todaysMounts), *hostConfig)
	})

	t.Run("not granted: Docker defaults, no host access binds", func(t *testing.T) {
		hostConfig, _, unprivilegedMounts := compute(t, boolPtr(false))

		expected := todaysHostConfig(unprivilegedMounts)
		expected.Privileged = false
		expected.CapAdd = nil
		expected.Resources = dockercontainer.Resources{}
		assert.Equal(t, expected, *hostConfig)
	})
}

func TestComputeMountsWithoutPrivilegedAccess(t *testing.T) {
	fakeHostAccessDirs(t)

	cfg := caTestConfig(t, "tls-sf015.corp.trumpf.com", true)
	cfg.CommandLineArguments.AppsSharedDir = t.TempDir()

	targets := func(mounts []mount.Mount) []string {
		var result []string
		for _, m := range mounts {
			result = append(result, m.Target)
		}
		return result
	}

	privileged, err := computeMounts(common.PROD, "myapp", cfg, true)
	require.NoError(t, err)
	assert.Subset(t, targets(privileged), []string{"/shared", "/data", "/sys/bus/w1/devices", trust.ContainerDir, "/run/dbus"})

	unprivileged, err := computeMounts(common.PROD, "myapp", cfg, false)
	require.NoError(t, err)
	got := targets(unprivileged)
	assert.Subset(t, got, []string{"/shared", "/data", trust.ContainerDir}, "the app's own dirs and the CA bundle stay")
	assert.NotContains(t, got, "/sys/bus/w1/devices")
	assert.NotContains(t, got, "/run/dbus")
	assert.Len(t, unprivileged, len(privileged)-2, "only the two host-access binds go")
}

func TestContainerNetworkConfigOutdatedComparesPrivileged(t *testing.T) {
	sm, mc, _, _ := wiredStateMachine(t)
	const name = "prod_1_privcmp"
	existing := dockercontainer.Summary{ID: "cid"}
	existing.HostConfig.NetworkMode = "bridge"

	mc.EXPECT().GetContainerPrivileged(mock.Anything, name).Return(true, nil).Once()
	assert.True(t, sm.containerNetworkConfigOutdated(existing, &dockercontainer.HostConfig{NetworkMode: "bridge"}, name),
		"a privileged container must not be reused for an app that lost the grant")

	mc.EXPECT().GetContainerPrivileged(mock.Anything, name).Return(false, nil).Once()
	assert.True(t, sm.containerNetworkConfigOutdated(existing, &dockercontainer.HostConfig{NetworkMode: "bridge", Privileged: true}, name))

	mc.EXPECT().GetContainerPrivileged(mock.Anything, name).Return(true, nil).Once()
	assert.False(t, sm.containerNetworkConfigOutdated(existing, &dockercontainer.HostConfig{NetworkMode: "bridge", Privileged: true}, name))
}

// =============================================================================
// Restart on a change of the grant
// =============================================================================

func privilegedComposeContainer(service string, state string, labels map[string]string) containerpkg.ContainerResult {
	all := map[string]string{containerpkg.ComposeServiceLabel: service}
	for key, value := range labels {
		all[key] = value
	}
	return containerpkg.ContainerResult{ID: service + "-id", State: state, Labels: all}
}

func TestPrivilegedAccessChangedSingleContainer(t *testing.T) {
	cases := []struct {
		name       string
		granted    *bool
		running    bool
		inspectErr error
		changed    bool
	}{
		{name: "old backend, privileged container (agent upgrade)", granted: nil, running: true, changed: false},
		{name: "granted, privileged container", granted: boolPtr(true), running: true, changed: false},
		{name: "not granted, unprivileged container", granted: boolPtr(false), running: false, changed: false},
		{name: "revoked", granted: boolPtr(false), running: true, changed: true},
		{name: "granted again", granted: boolPtr(true), running: false, changed: true},
		{name: "old backend, unprivileged container", granted: nil, running: false, changed: true},
		{name: "no container: undecidable, no restart", granted: boolPtr(false), inspectErr: errdefs.ContainerNotFound(errors.New("gone")), changed: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sm, mc, _, _ := wiredStateMachine(t)
			app := createTestApp("single", common.RUNNING, common.PROD)
			payload := amPayload(1, "single", common.RUNNING, common.PROD)
			payload.Privileged = tc.granted

			mc.EXPECT().GetContainerPrivileged(mock.Anything, payload.ContainerName.Prod).Return(tc.running, tc.inspectErr).Once()

			assert.Equal(t, tc.changed, sm.privilegedAccessChanged(payload, app))
		})
	}
}

func TestPrivilegedAccessChangedCompose(t *testing.T) {
	cases := []struct {
		name       string
		granted    *bool
		containers []containerpkg.ContainerResult
		listErr    error
		changed    bool
	}{
		{name: "old backend, containers from an old agent", granted: nil,
			containers: []containerpkg.ContainerResult{privilegedComposeContainer("app", "running", nil)}, changed: false},
		{name: "granted, labeled true", granted: boolPtr(true),
			containers: []containerpkg.ContainerResult{privilegedComposeContainer("app", "running", map[string]string{privilegedLabel: "true"})}, changed: false},
		{name: "not granted, labeled false", granted: boolPtr(false),
			containers: []containerpkg.ContainerResult{privilegedComposeContainer("app", "running", map[string]string{privilegedLabel: "false"})}, changed: false},
		{name: "revoked, containers from an old agent", granted: boolPtr(false),
			containers: []containerpkg.ContainerResult{privilegedComposeContainer("app", "running", nil)}, changed: true},
		{name: "granted again", granted: boolPtr(true),
			containers: []containerpkg.ContainerResult{privilegedComposeContainer("app", "running", map[string]string{privilegedLabel: "false"})}, changed: true},
		{name: "an orphan of a dropped service does not count", granted: boolPtr(false),
			containers: []containerpkg.ContainerResult{
				privilegedComposeContainer("app", "running", map[string]string{privilegedLabel: "false"}),
				privilegedComposeContainer("dropped", "running", nil),
			}, changed: false},
		{name: "a stopped container does not count", granted: boolPtr(false),
			containers: []containerpkg.ContainerResult{
				privilegedComposeContainer("app", "running", map[string]string{privilegedLabel: "false"}),
				privilegedComposeContainer("init", "exited", nil),
			}, changed: false},
		{name: "list error: undecidable, no restart", granted: boolPtr(false), listErr: errors.New("daemon busy"), changed: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sm, mc, _, _ := wiredStateMachine(t)
			app := createTestApp("composed", common.RUNNING, common.PROD)
			payload := createTestPayload("composed", common.RUNNING, common.PROD)
			payload.DockerCompose = map[string]interface{}{"services": map[string]interface{}{
				"app":  map[string]interface{}{"image": "x"},
				"init": map[string]interface{}{"image": "y"},
			}}
			payload.Privileged = tc.granted

			mc.EXPECT().ListContainers(mock.Anything, mock.Anything).Return(tc.containers, tc.listErr).Once()

			assert.Equal(t, tc.changed, sm.privilegedAccessChanged(payload, app))
		})
	}
}

func TestRunningToRunningWithoutChangeDoesNothing(t *testing.T) {
	sm, mc, st, _ := wiredStateMachine(t)

	app := seedApp(t, st, "steady", common.RUNNING, common.PROD)
	payload := execPayload("steady", common.RUNNING, common.PROD)
	payload.Privileged = boolPtr(false)

	// Strict mock: one inspect, nothing stopped or started.
	mc.EXPECT().GetContainerPrivileged(mock.Anything, payload.ContainerName.Prod).Return(false, nil).Once()

	errC := sm.InitTransition(app, payload)
	require.NotNil(t, errC)
	err := <-errC
	assert.True(t, errdefs.IsNoActionTransition(err), "expected a no-action transition, got %v", err)
}

func TestRestartOnPrivilegedAccessChangeRestartsSingleContainer(t *testing.T) {
	sm, mc, st, msg, _ := wiredRunBuildSM(t)
	fakeHostAccessDirs(t)

	app := seedApp(t, st, "revoked", common.RUNNING, common.PROD)
	payload := execPayload("revoked", common.RUNNING, common.PROD)
	payload.Privileged = boolPtr(false)

	const oldID, newID = "old-cid", "new-cid"

	// The running container is still privileged.
	mc.EXPECT().GetContainerPrivileged(mock.Anything, payload.ContainerName.Prod).Return(true, nil).Once()

	// stopProdApp
	mc.EXPECT().GetContainer(mock.Anything, payload.ContainerName.Prod).Return(dockercontainer.Summary{ID: oldID}, nil).Once()
	mc.EXPECT().StopContainerByID(mock.Anything, oldID, mock.Anything).Return(nil).Once()
	mc.EXPECT().WaitForContainerByID(mock.Anything, oldID, mock.Anything).Return(int64(0), nil).Once()
	mc.EXPECT().RemoveContainerByID(mock.Anything, oldID, mock.Anything).Return(nil).Once()
	removed := make(chan error, 1)
	removed <- notFoundErr()
	close(removed)
	mc.EXPECT().PollContainerState(mock.Anything, oldID, mock.Anything).Return(make(chan containerpkg.ContainerState), removed).Once()

	// runProdApp: a new, unprivileged container.
	mc.EXPECT().GetImage(mock.Anything, payload.RegistryImageName.Prod, mock.Anything).Return(containerpkg.ImageResult{}, nil).Once()
	mc.EXPECT().GetContainer(mock.Anything, payload.ContainerName.Prod).Return(dockercontainer.Summary{}, notFoundErr()).Once()
	mc.EXPECT().CreateContainer(mock.Anything, mock.Anything, mock.MatchedBy(func(h dockercontainer.HostConfig) bool {
		return !h.Privileged && len(h.CapAdd) == 0 && len(h.Resources.Devices) == 0
	}), mock.Anything, payload.ContainerName.Prod).Return(newID, nil).Once()
	mc.EXPECT().StartContainer(mock.Anything, newID).Return(nil).Once()
	running, errC := fwdRunningSignal()
	mc.EXPECT().WaitForRunning(mock.Anything, newID, mock.Anything).Return(running, errC).Once()
	fwdAllowLogs(mc)

	require.NoError(t, sm.restartOnPrivilegedAccessChange(payload, app))

	app.StateLock.Lock()
	final := app.CurrentState
	app.StateLock.Unlock()
	assert.Equal(t, common.RUNNING, final)

	states := remoteStatesFor(msg)
	assert.Contains(t, states, common.STOPPING)
	assert.Contains(t, states, common.STARTING)
}

func TestPersistPostUpdatePromotesPrivileged(t *testing.T) {
	sm, _, st, _, _ := wiredRunBuildSM(t)

	app := seedApp(t, st, "promoted", common.PRESENT, common.PROD)
	payload := execPayload("promoted", common.RUNNING, common.PROD)
	payload.Privileged = boolPtr(true)
	payload.NewPrivileged = boolPtr(false)

	require.NoError(t, sm.persistPostUpdateRequestedState(payload, app))

	persisted, err := st.GetRequestedState(payload.AppKey, payload.Stage)
	require.NoError(t, err)
	require.NotNil(t, persisted.Privileged)
	assert.False(t, *persisted.Privileged, "the installed release's grant is now the present one")
}
