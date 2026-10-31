package apps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"reagent/common"
	"reagent/config"
	"reagent/tunnel"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// Development runs get a tunnel for each port of their port template while the
// container runs: a private one for an http(s) port, a remote port for a
// tcp/udp one. The label carries a -dev marker, so the PROD install of the
// same app on the same device keeps its own tunnel.

func devTunnelID(t *testing.T, deviceKey int, app string, port uint64, protocol string) (string, string) {
	t.Helper()
	sub := tunnel.CreateSubdomain(tunnel.Protocol(protocol), uint64(deviceKey), app, port, common.DEV)
	return sub, tunnel.CreateTunnelID(sub, protocol)
}

// readBackConfig is a tunnel as GetTunnelConfig reads it back from frpc.yaml.
func readBackConfig(t *testing.T, deviceKey int, app string, port uint64, protocol string, stage common.Stage) tunnel.TunnelConfig {
	t.Helper()
	sub := tunnel.CreateSubdomain(tunnel.Protocol(protocol), uint64(deviceKey), app, port, stage)
	name := tunnel.CreateTunnelID(sub, protocol)
	gotApp, declared, gotStage, ok := tunnel.ParseSubdomain(sub)
	require.True(t, ok)
	return tunnel.TunnelConfig{Name: name, Subdomain: sub, AppName: gotApp, DeclaredPort: declared, Stage: gotStage, Protocol: tunnel.Protocol(protocol), LocalPort: 41000 + port}
}

// devEnvFile reads /data/env/<name>.txt of a development run as its container
// sees it.
func devEnvFile(t *testing.T, cfg *config.Config, app, name string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(appEnvFilesHostDir(cfg, common.DEV, app), name+".txt"))
	if err != nil {
		require.True(t, os.IsNotExist(err))
		return "", false
	}
	return string(data), true
}

// A running development run gets a tunnel for every port of its template. Its
// tcp/udp ports have no switch: a rule flagged inactive is tunneled all the
// same. Their remote ports reach the container through its env files, and
// stay on the rules so the next run asks for the same ports again.
func TestSyncPortStateDevTunnelsAllPorts(t *testing.T) {
	am, _, mockTunnel, appStore, _, cfg := amHarness(t)
	mockTunnel.EXPECT().TunnelCapable().Return(true).Maybe()

	app := amSeed(t, appStore, 60, "devweb", common.RUNNING, common.DEV)
	app.RequestedState = common.RUNNING

	payload := amPayload(60, "devweb", common.RUNNING, common.DEV)
	payload.Ports, _ = tunnel.PortForwardRuleToInterface([]common.PortForwardRule{
		{RuleName: "ui", Port: 8080, Protocol: "http", Active: true},
		{RuleName: "mqtt", Port: 1883, Protocol: "tcp", Active: true, RemotePortEnvironment: "MQTT_PORT"},
		{RuleName: "media", Port: 10000, Protocol: "udp", Active: false},
	})

	// The launch path published all three on host ports.
	_, err := am.hostPorts.RecoverOrReserve(hostPortKey{Stage: common.DEV, AppKey: 60, Protocol: "tcp", Port: 8080}, 41800)
	require.NoError(t, err)
	_, err = am.hostPorts.RecoverOrReserve(hostPortKey{Stage: common.DEV, AppKey: 60, Protocol: "tcp", Port: 1883}, 41801)
	require.NoError(t, err)
	_, err = am.hostPorts.RecoverOrReserve(hostPortKey{Stage: common.DEV, AppKey: 60, Protocol: "udp", Port: 10000}, 41802)
	require.NoError(t, err)

	sub, id := devTunnelID(t, cfg.ReswarmConfig.DeviceKey, "devweb", 8080, "http")
	assert.Equal(t, "1-devweb-8080-dev", sub)
	tcpSub, tcpID := devTunnelID(t, cfg.ReswarmConfig.DeviceKey, "devweb", 1883, "tcp")
	assert.Equal(t, "1-devweb-1883-dev-tcp", tcpID)
	udpSub, udpID := devTunnelID(t, cfg.ReswarmConfig.DeviceKey, "devweb", 10000, "udp")

	mockTunnel.EXPECT().Get(id).Return(nil).Once()
	mockTunnel.EXPECT().Get(tcpID).Return(nil).Once()
	mockTunnel.EXPECT().Get(udpID).Return(nil).Once()
	added := map[string]tunnel.TunnelConfig{}
	mockTunnel.EXPECT().AddTunnel(mock.Anything).RunAndReturn(func(conf tunnel.TunnelConfig) (tunnel.TunnelConfig, error) {
		assert.Equal(t, common.DEV, conf.Stage)
		assert.Zero(t, conf.RemotePort, "a first run asks for any free port")
		added[conf.Subdomain] = conf
		switch conf.Protocol {
		case tunnel.TCP:
			conf.RemotePort = 30123
		case tunnel.UDP:
			conf.RemotePort = 30124
		}
		return conf, nil
	}).Times(3)

	var saved []interface{}
	mockTunnel.EXPECT().SaveRemotePorts(mock.Anything).RunAndReturn(func(p common.TransitionPayload) error {
		assert.Equal(t, common.DEV, p.Stage)
		saved = p.Ports
		return nil
	}).Once()
	mockTunnel.EXPECT().GetState().Return([]tunnel.TunnelState{}, nil).Once()

	require.NoError(t, am.syncPortState(payload, app))

	require.Contains(t, added, sub)
	assert.Equal(t, uint64(41800), added[sub].LocalPort, "frpc dials the DEV container's host port")
	require.Contains(t, added, tcpSub)
	assert.Equal(t, uint64(41801), added[tcpSub].LocalPort)
	require.Contains(t, added, udpSub, "a DEV udp port has no switch: an inactive flag does not hold it off")

	rules := spsRules(t, saved)
	require.Len(t, rules, 3)
	assert.Equal(t, uint64(41800), rules[0].HostPort, "the DEV row learns its host ports")
	assert.Zero(t, rules[0].RemotePort)
	assert.Equal(t, uint64(30123), rules[1].RemotePort, "the DEV row keeps its tunnel ports for the next run")
	assert.Equal(t, uint64(30124), rules[2].RemotePort)

	for name, want := range map[string]string{"REMOTE_PORT_FOR_1883": "30123", "MQTT_PORT": "30123", "REMOTE_PORT_FOR_10000": "30124"} {
		got, ok := devEnvFile(t, cfg, "devweb", name)
		assert.True(t, ok, name)
		assert.Equal(t, want, got, name)
	}
	_, ok := devEnvFile(t, cfg, "devweb", "REMOTE_PORT_FOR_8080")
	assert.False(t, ok, "an http tunnel has no remote port")
}

// Once a development run stops, its tcp/udp tunnel goes and so does the port
// announced to the container. The rule keeps the port, and the next run asks
// frps for it again.
func TestSyncPortStateDevTcpTunnelFollowsRun(t *testing.T) {
	am, _, mockTunnel, appStore, _, cfg := amHarness(t)
	mockTunnel.EXPECT().TunnelCapable().Return(true).Maybe()

	app := amSeed(t, appStore, 65, "devmedia", common.PRESENT, common.DEV)
	app.RequestedState = common.PRESENT

	payload := amPayload(65, "devmedia", common.PRESENT, common.DEV)
	payload.Ports = spsPorts(t, common.PortForwardRule{RuleName: "media", Port: 10000, Protocol: "udp", Active: true, HostPort: 41850, RemotePort: 30200})
	_, err := am.hostPorts.RecoverOrReserve(hostPortKey{Stage: common.DEV, AppKey: 65, Protocol: "udp", Port: 10000}, 41850)
	require.NoError(t, err)

	// A file from the last run, whose tunnel is gone.
	envDir := appEnvFilesHostDir(cfg, common.DEV, "devmedia")
	require.NoError(t, ensureEnvDir(envDir))
	require.NoError(t, writeEnvValue(envDir, "REMOTE_PORT_FOR_10000", "30200"))

	sub, id := devTunnelID(t, cfg.ReswarmConfig.DeviceKey, "devmedia", 10000, "udp")

	// Stopped: the tunnel is removed, and so is the announced port. Nothing
	// changed on the rule, so nothing is saved.
	mockTunnel.EXPECT().Get(id).Return(&tunnel.Tunnel{Config: tunnel.TunnelConfig{Subdomain: sub, RemotePort: 30200}}).Once()
	mockTunnel.EXPECT().RemoveTunnel(mock.MatchedBy(func(c tunnel.TunnelConfig) bool {
		return c.Subdomain == sub && c.Protocol == tunnel.UDP && c.Stage == common.DEV
	})).Return(nil).Once()
	mockTunnel.EXPECT().GetState().Return([]tunnel.TunnelState{}, nil).Once()
	require.NoError(t, am.syncPortState(payload, app))
	_, ok := devEnvFile(t, cfg, "devmedia", "REMOTE_PORT_FOR_10000")
	assert.False(t, ok, "no tunnel, no announced port")

	// Running again: the same port is asked for, and announced once granted.
	app.CurrentState = common.RUNNING
	app.RequestedState = common.RUNNING
	mockTunnel.EXPECT().Get(id).Return(nil).Once()
	mockTunnel.EXPECT().AddTunnel(mock.Anything).RunAndReturn(func(conf tunnel.TunnelConfig) (tunnel.TunnelConfig, error) {
		assert.Equal(t, uint64(30200), conf.RemotePort, "the next run asks for the port of the last")
		assert.False(t, conf.Reserved, "a fallback port is fine for a development run")
		return conf, nil
	}).Once()
	mockTunnel.EXPECT().GetState().Return([]tunnel.TunnelState{}, nil).Once()
	require.NoError(t, am.syncPortState(payload, app))
	got, ok := devEnvFile(t, cfg, "devmedia", "REMOTE_PORT_FOR_10000")
	assert.True(t, ok)
	assert.Equal(t, "30200", got)
}

// Only an http(s) tunnel is reached by its label: a tcp/udp port of an app
// whose DEV label exceeds a DNS label still gets its tunnel.
func TestSyncPortStateDevLongNameTcpTunnel(t *testing.T) {
	am, _, mockTunnel, appStore, _, cfg := amHarness(t)
	mockTunnel.EXPECT().TunnelCapable().Return(true).Maybe()

	name := strings.Repeat("b", 53)
	app := amSeed(t, appStore, 66, name, common.RUNNING, common.DEV)
	app.RequestedState = common.RUNNING

	payload := amPayload(66, name, common.RUNNING, common.DEV)
	payload.Ports = spsPorts(t, common.PortForwardRule{RuleName: "media", Port: 10001, Protocol: "tcp", Active: true, HostPort: 41860})
	_, err := am.hostPorts.RecoverOrReserve(hostPortKey{Stage: common.DEV, AppKey: 66, Protocol: "tcp", Port: 10001}, 41860)
	require.NoError(t, err)

	sub, id := devTunnelID(t, cfg.ReswarmConfig.DeviceKey, name, 10001, "tcp")
	require.Greater(t, len(sub), tunnel.MaxLabelLength)

	mockTunnel.EXPECT().Get(id).Return(nil).Once()
	mockTunnel.EXPECT().AddTunnel(mock.Anything).RunAndReturn(func(conf tunnel.TunnelConfig) (tunnel.TunnelConfig, error) {
		conf.RemotePort = 30300
		return conf, nil
	}).Once()
	mockTunnel.EXPECT().SaveRemotePorts(mock.Anything).Return(nil).Once()
	mockTunnel.EXPECT().GetState().Return([]tunnel.TunnelState{}, nil).Once()
	require.NoError(t, am.syncPortState(payload, app))
}

// A development run is not restarted: once it stopped or crashed, its tunnel
// goes, and the request to run it does not bring the tunnel back.
func TestSyncPortStateDevNotRunningRemovesTunnel(t *testing.T) {
	for _, current := range []common.AppState{common.FAILED, common.PRESENT} {
		t.Run(string(current), func(t *testing.T) {
			am, _, mockTunnel, appStore, _, cfg := amHarness(t)
			mockTunnel.EXPECT().TunnelCapable().Return(true).Maybe()

			app := amSeed(t, appStore, 61, "devcrash", current, common.DEV)
			app.RequestedState = common.RUNNING

			payload := amPayload(61, "devcrash", common.RUNNING, common.DEV)
			payload.Ports = spsPorts(t, common.PortForwardRule{RuleName: "ui", Port: 8080, Protocol: "http", Active: true, HostPort: 41810})
			_, err := am.hostPorts.RecoverOrReserve(hostPortKey{Stage: common.DEV, AppKey: 61, Protocol: "tcp", Port: 8080}, 41810)
			require.NoError(t, err)

			sub, id := devTunnelID(t, cfg.ReswarmConfig.DeviceKey, "devcrash", 8080, "http")
			mockTunnel.EXPECT().Get(id).Return(&tunnel.Tunnel{Config: tunnel.TunnelConfig{Subdomain: sub}}).Once()
			mockTunnel.EXPECT().RemoveTunnel(mock.MatchedBy(func(c tunnel.TunnelConfig) bool {
				return c.Subdomain == sub && c.Protocol == tunnel.HTTP && c.Stage == common.DEV
			})).Return(nil).Once()
			mockTunnel.EXPECT().GetState().Return([]tunnel.TunnelState{}, nil).Once()
			// AddTunnel is not expected: the strict mock fails the test on it.

			require.NoError(t, am.syncPortState(payload, app))
		})
	}
}

// Stopping or removing a development run removes its tunnels and leaves the
// PROD install's of the same app; uninstalling the PROD install leaves the
// development run's.
func TestRemoveAppTunnelsIsPerStage(t *testing.T) {
	am, _, mockTunnel, _, _, cfg := amHarness(t)
	dk := cfg.ReswarmConfig.DeviceKey

	prod := readBackConfig(t, dk, "shared", 8080, "http", common.PROD)
	dev := readBackConfig(t, dk, "shared", 8080, "http", common.DEV)
	devSecure := readBackConfig(t, dk, "shared", 8443, "https", common.DEV)
	otherDev := readBackConfig(t, dk, "other", 8080, "http", common.DEV)
	all := []tunnel.TunnelConfig{prod, dev, devSecure, otherDev}

	mockTunnel.EXPECT().TunnelCapable().Return(true).Times(3)
	mockTunnel.EXPECT().GetTunnelConfig().Return(all, nil).Times(3)

	// A DEV stop (the StateMachine hook every DEV stop and removal runs).
	mockTunnel.EXPECT().RemoveTunnel(dev).Return(nil).Once()
	mockTunnel.EXPECT().RemoveTunnel(devSecure).Return(nil).Once()
	am.StateMachine.dropDevTunnels(amPayload(70, "shared", common.PRESENT, common.DEV))

	// A PROD payload never touches tunnels through that hook.
	am.StateMachine.dropDevTunnels(amPayload(70, "shared", common.PRESENT, common.PROD))

	// A PROD uninstall.
	mockTunnel.EXPECT().RemoveTunnel(prod).Return(nil).Once()
	am.RemoveAppTunnels("shared", common.PROD)

	// The state observer seeing a development run stop on its own.
	mockTunnel.EXPECT().RemoveTunnel(otherDev).Return(nil).Once()
	am.StateObserver.followDevRun(71, "other", common.FAILED)
}

// A DEV label is four characters longer than the PROD one. One that exceeds a
// DNS label could never be reached: it gets no tunnel, and the developer reads
// why in the app log, once per run of the container.
func TestSyncPortStateDevLabelTooLong(t *testing.T) {
	am, _, mockTunnel, appStore, msg, cfg := amHarness(t)
	mockTunnel.EXPECT().TunnelCapable().Return(true).Maybe()

	name := strings.Repeat("a", 53)
	app := amSeed(t, appStore, 62, name, common.RUNNING, common.DEV)
	app.RequestedState = common.RUNNING

	payload := amPayload(62, name, common.RUNNING, common.DEV)
	payload.Ports = spsPorts(t, common.PortForwardRule{RuleName: "ui", Port: 8080, Protocol: "http", Active: true, HostPort: 41820})
	_, err := am.hostPorts.RecoverOrReserve(hostPortKey{Stage: common.DEV, AppKey: 62, Protocol: "tcp", Port: 8080}, 41820)
	require.NoError(t, err)

	sub, _ := devTunnelID(t, cfg.ReswarmConfig.DeviceKey, name, 8080, "http")
	require.Equal(t, 64, len(sub), "one octet over the limit")
	prodSub := tunnel.CreateSubdomain(tunnel.HTTP, uint64(cfg.ReswarmConfig.DeviceKey), name, 8080, common.PROD)
	require.Equal(t, 60, len(prodSub))

	// No Get, no AddTunnel (strict mock); nothing changed, so nothing saved.
	mockTunnel.EXPECT().GetState().Return([]tunnel.TunnelState{}, nil).Times(3)

	lines := func() int {
		n := 0
		for _, call := range msg.GetPublishCalls() {
			if len(call.Args) == 0 {
				continue
			}
			if dict, ok := call.Args[0].(common.Dict); ok {
				if chunk, ok := dict["chunk"].(string); ok && strings.Contains(chunk, "gets no tunnel") {
					n++
				}
			}
		}
		return n
	}

	require.NoError(t, am.syncPortState(payload, app))
	require.NoError(t, am.syncPortState(payload, app))
	assert.Equal(t, 1, lines(), "said once per run, not once per sync pass")

	// The run ends; the next run says it again.
	app.CurrentState = common.PRESENT
	app.RequestedState = common.PRESENT
	require.NoError(t, am.syncPortState(payload, app))
	app.CurrentState = common.RUNNING
	app.RequestedState = common.RUNNING
	mockTunnel.EXPECT().GetState().Return([]tunnel.TunnelState{}, nil).Once()
	require.NoError(t, am.syncPortState(payload, app))
	assert.Equal(t, 2, lines())
}

// A developer's Active switch made while the device was offline never reaches
// it as a push. The reconnect bulk sync adopts the DEV row's port rules (and
// nothing else of it), and the run's next sync pass applies them.
func TestReconnectAdoptsDevPortRules(t *testing.T) {
	am, _, mockTunnel, appStore, _, cfg := amHarness(t)
	mockTunnel.EXPECT().TunnelCapable().Return(true).Maybe()

	amSeed(t, appStore, 63, "devoff", common.RUNNING, common.DEV)
	_, err := am.hostPorts.RecoverOrReserve(hostPortKey{Stage: common.DEV, AppKey: 63, Protocol: "tcp", Port: 8080}, 41830)
	require.NoError(t, err)

	stored := amPayload(63, "devoff", common.RUNNING, common.DEV)
	stored.Ports = spsPorts(t, common.PortForwardRule{RuleName: "ui", Port: 8080, Protocol: "http", Active: true, HostPort: 41830})
	require.NoError(t, appStore.UpdateLocalRequestedState(stored))

	// The cloud: the developer switched the port off; the requested state is
	// not a stop, so it is not applied.
	remote := amPayload(63, "devoff", common.BUILT, common.DEV)
	remote.Ports = spsPorts(t, common.PortForwardRule{RuleName: "ui", Port: 8080, Protocol: "http", Active: false, HostPort: 41830})
	require.NoError(t, am.UpdateLocalRequestedAppStatesWithRemote([]common.TransitionPayload{remote}))

	got, err := appStore.GetRequestedState(63, common.DEV)
	require.NoError(t, err)
	assert.Equal(t, common.RUNNING, got.RequestedState, "the run request stays the developer's")
	rules := spsRules(t, got.Ports)
	require.Len(t, rules, 1)
	assert.False(t, rules[0].Active, "the switch arrived")

	// The reconnect pass's sync removes the tunnel.
	sub, id := devTunnelID(t, cfg.ReswarmConfig.DeviceKey, "devoff", 8080, "http")
	mockTunnel.EXPECT().Get(id).Return(&tunnel.Tunnel{Config: tunnel.TunnelConfig{Subdomain: sub}}).Once()
	mockTunnel.EXPECT().RemoveTunnel(mock.MatchedBy(func(c tunnel.TunnelConfig) bool { return c.Subdomain == sub })).Return(nil).Once()
	mockTunnel.EXPECT().GetState().Return([]tunnel.TunnelState{}, nil).Once()
	am.retryPortSync(63, common.DEV)

	// Same rules again: nothing to adopt, nothing written.
	require.NoError(t, am.UpdateLocalRequestedAppStatesWithRemote([]common.TransitionPayload{remote}))
}

// frpc.yaml starts empty on every agent start: the all-apps reconcile brings a
// still-running development run's tunnel back.
func TestSyncAllPortStatesRebuildsRunningDevTunnel(t *testing.T) {
	am, _, mockTunnel, appStore, _, cfg := amHarness(t)
	mockTunnel.EXPECT().TunnelCapable().Return(true).Maybe()

	amSeed(t, appStore, 64, "devboot", common.RUNNING, common.DEV)
	_, err := am.hostPorts.RecoverOrReserve(hostPortKey{Stage: common.DEV, AppKey: 64, Protocol: "tcp", Port: 8080}, 41840)
	require.NoError(t, err)

	stored := amPayload(64, "devboot", common.RUNNING, common.DEV)
	stored.Ports = spsPorts(t, common.PortForwardRule{RuleName: "ui", Port: 8080, Protocol: "http", Active: true, HostPort: 41840})
	require.NoError(t, appStore.UpdateLocalRequestedState(stored))

	sub, id := devTunnelID(t, cfg.ReswarmConfig.DeviceKey, "devboot", 8080, "http")
	mockTunnel.EXPECT().Get(id).Return(nil).Once()
	mockTunnel.EXPECT().AddTunnel(mock.Anything).RunAndReturn(func(conf tunnel.TunnelConfig) (tunnel.TunnelConfig, error) {
		assert.Equal(t, sub, conf.Subdomain)
		assert.Equal(t, uint64(41840), conf.LocalPort)
		return conf, nil
	}).Once()
	mockTunnel.EXPECT().GetState().Return([]tunnel.TunnelState{}, nil).Once()

	am.SyncAllPortStates()
}

// A development run's container gets no remote-port variable, even while a
// tunnel of its own stands (it belongs to the run before): a variable could
// not be withdrawn once that tunnel went. Its ports reach it through the env
// files, which follow the live tunnel (TestSyncPortStateDevTunnelsAllPorts).
func TestRemotePortEnvVarsNoneForDev(t *testing.T) {
	am, _, mockTunnel, _, _, cfg := amHarness(t)
	rules := []common.PortForwardRule{{Port: 1883, Protocol: "tcp", Active: true}}
	dk := uint64(cfg.ReswarmConfig.DeviceKey)

	// No Get for DEV (strict mock).
	assert.Nil(t, am.StateMachine.remotePortEnvVars(cfg, "envapp", common.DEV, rules))

	prodID := tunnel.CreateTunnelID(tunnel.CreateSubdomain(tunnel.TCP, dk, "envapp", 1883, common.PROD), "tcp")
	mockTunnel.EXPECT().Get(prodID).Return(&tunnel.Tunnel{Config: tunnel.TunnelConfig{RemotePort: 30111}}).Once()
	assert.Equal(t, []string{"REMOTE_PORT_FOR_1883=30111"}, am.StateMachine.remotePortEnvVars(cfg, "envapp", common.PROD, rules))
}
