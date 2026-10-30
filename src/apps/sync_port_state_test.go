package apps

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"reagent/common"
	"reagent/errdefs"
	"reagent/tunnel"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// spsPorts builds the payload.Ports value for a single active rule.
func spsPorts(t *testing.T, rule common.PortForwardRule) []interface{} {
	t.Helper()
	ports, err := tunnel.PortForwardRuleToInterface([]common.PortForwardRule{rule})
	require.NoError(t, err)
	return ports
}

func spsRules(t *testing.T, ports []interface{}) []common.PortForwardRule {
	t.Helper()
	rules, err := tunnel.InterfaceToPortForwardRule(ports)
	require.NoError(t, err)
	return rules
}

// TestSyncPortStateUsesManagedHostPort: the tunnel dials the agent-managed
// host port while the subdomain keeps the declared port, and the assignment
// is persisted as host_port on the saved rules.
func TestSyncPortStateUsesManagedHostPort(t *testing.T) {
	am, _, mockTunnel, appStore, _, cfg := amHarness(t)

	mockTunnel.EXPECT().TunnelCapable().Return(true).Maybe()

	app := amSeed(t, appStore, 7, "portapp", common.RUNNING, common.PROD)
	app.RequestedState = common.RUNNING

	payload := amPayload(7, "portapp", common.RUNNING, common.PROD)
	payload.Ports = spsPorts(t, common.PortForwardRule{RuleName: "web", Port: 8080, Protocol: "http", Active: true})

	// The launch path allocated a managed host port for the declared port.
	hostPort, err := am.hostPorts.RecoverOrReserve(hostPortKey{Stage: common.PROD, AppKey: 7, Protocol: "tcp", Port: 8080}, 41234)
	require.NoError(t, err)
	require.Equal(t, uint64(41234), hostPort)

	expectedSubdomain := tunnel.CreateSubdomain(tunnel.Protocol("http"), uint64(cfg.ReswarmConfig.DeviceKey), "portapp", 8080, common.PROD)
	tunnelID := tunnel.CreateTunnelID(expectedSubdomain, "http")

	mockTunnel.EXPECT().Get(tunnelID).Return(nil).Once()
	mockTunnel.EXPECT().AddTunnel(mock.Anything).RunAndReturn(func(conf tunnel.TunnelConfig) (tunnel.TunnelConfig, error) {
		assert.Equal(t, expectedSubdomain, conf.Subdomain, "subdomain must keep the declared port")
		assert.Equal(t, uint64(41234), conf.LocalPort, "frpc must dial the managed host port")
		assert.Equal(t, "", conf.LocalIP)
		conf.RemotePort = 30123
		return conf, nil
	}).Once()

	var savedPorts []interface{}
	mockTunnel.EXPECT().SaveRemotePorts(mock.Anything).RunAndReturn(func(p common.TransitionPayload) error {
		savedPorts = p.Ports
		return nil
	}).Once()
	mockTunnel.EXPECT().GetState().Return([]tunnel.TunnelState{}, nil).Once()

	require.NoError(t, am.syncPortState(payload, app))

	savedRules := spsRules(t, savedPorts)
	require.Len(t, savedRules, 1)
	assert.Equal(t, uint64(8080), savedRules[0].Port, "declared port stays the identity")
	assert.Equal(t, uint64(41234), savedRules[0].HostPort, "host port is persisted upstream")
	assert.Equal(t, uint64(30123), savedRules[0].RemotePort)
}

// TestSyncPortStateDefersFreshApp: nothing allocated, no container, nothing
// persisted — tunnel creation waits for the post-transition sync.
func TestSyncPortStateDefersFreshApp(t *testing.T) {
	am, mockContainer, mockTunnel, appStore, _, _ := amHarness(t)

	mockTunnel.EXPECT().TunnelCapable().Return(true).Maybe()

	app := amSeed(t, appStore, 8, "freshapp", common.RUNNING, common.PROD)
	app.RequestedState = common.RUNNING

	payload := amPayload(8, "freshapp", common.RUNNING, common.PROD)
	payload.Ports = spsPorts(t, common.PortForwardRule{RuleName: "web", Port: 8080, Protocol: "http", Active: true})

	mockContainer.EXPECT().GetContainerPortBindings(mock.Anything, payload.ContainerName.Prod).
		Return(nil, errdefs.ContainerNotFound(errors.New("not found"))).Once()

	// SaveRemotePorts is deliberately not expected: nothing changed against the
	// rules the cloud just sent, so there is nothing to persist upstream (a
	// strict mock fails the test on any call).
	mockTunnel.EXPECT().GetState().Return([]tunnel.TunnelState{}, nil).Once()

	require.NoError(t, am.syncPortState(payload, app))
}

// TestSyncPortStateLegacyHostNetworking: a pre-migration container running
// with host networking keeps its declared-port tunnel.
func TestSyncPortStateLegacyHostNetworking(t *testing.T) {
	am, mockContainer, mockTunnel, appStore, _, _ := amHarness(t)

	mockTunnel.EXPECT().TunnelCapable().Return(true).Maybe()

	app := amSeed(t, appStore, 9, "legacyapp", common.RUNNING, common.PROD)
	app.RequestedState = common.RUNNING

	payload := amPayload(9, "legacyapp", common.RUNNING, common.PROD)
	payload.Ports = spsPorts(t, common.PortForwardRule{RuleName: "web", Port: 8080, Protocol: "http", Active: true})

	mockContainer.EXPECT().GetContainerPortBindings(mock.Anything, payload.ContainerName.Prod).
		Return(map[string]uint64{}, nil).Once()
	mockContainer.EXPECT().GetContainerNetworkMode(mock.Anything, payload.ContainerName.Prod).
		Return("host", nil).Once()

	mockTunnel.EXPECT().Get(mock.Anything).Return(nil).Once()
	mockTunnel.EXPECT().AddTunnel(mock.Anything).RunAndReturn(func(conf tunnel.TunnelConfig) (tunnel.TunnelConfig, error) {
		assert.Equal(t, uint64(8080), conf.LocalPort, "legacy host networking dials the declared port")
		return conf, nil
	}).Once()
	mockTunnel.EXPECT().SaveRemotePorts(mock.Anything).Return(nil).Once()
	mockTunnel.EXPECT().GetState().Return([]tunnel.TunnelState{}, nil).Once()

	require.NoError(t, am.syncPortState(payload, app))

	// The legacy port is not a managed assignment.
	_, assigned := am.hostPorts.GetByPort(common.PROD, 9, "tcp", 8080)
	assert.False(t, assigned)
}

// TestSyncPortStateReplacesStaleTunnel: an in-memory tunnel dialing an
// outdated host port is removed and re-added with the current one.
func TestSyncPortStateReplacesStaleTunnel(t *testing.T) {
	am, _, mockTunnel, appStore, _, cfg := amHarness(t)

	mockTunnel.EXPECT().TunnelCapable().Return(true).Maybe()

	app := amSeed(t, appStore, 10, "staleapp", common.RUNNING, common.PROD)
	app.RequestedState = common.RUNNING

	payload := amPayload(10, "staleapp", common.RUNNING, common.PROD)
	payload.Ports = spsPorts(t, common.PortForwardRule{RuleName: "web", Port: 8080, Protocol: "http", Active: true})

	_, err := am.hostPorts.RecoverOrReserve(hostPortKey{Stage: common.PROD, AppKey: 10, Protocol: "tcp", Port: 8080}, 41300)
	require.NoError(t, err)

	subdomain := tunnel.CreateSubdomain(tunnel.Protocol("http"), uint64(cfg.ReswarmConfig.DeviceKey), "staleapp", 8080, common.PROD)
	staleConfig := tunnel.TunnelConfig{Subdomain: subdomain, Protocol: tunnel.Protocol("http"), LocalPort: 40000}

	mockTunnel.EXPECT().Get(mock.Anything).Return(&tunnel.Tunnel{Config: staleConfig}).Once()
	mockTunnel.EXPECT().RemoveTunnel(staleConfig).Return(nil).Once()
	mockTunnel.EXPECT().AddTunnel(mock.Anything).RunAndReturn(func(conf tunnel.TunnelConfig) (tunnel.TunnelConfig, error) {
		assert.Equal(t, uint64(41300), conf.LocalPort)
		return conf, nil
	}).Once()
	mockTunnel.EXPECT().SaveRemotePorts(mock.Anything).Return(nil).Once()
	mockTunnel.EXPECT().GetState().Return([]tunnel.TunnelState{}, nil).Once()

	require.NoError(t, am.syncPortState(payload, app))
}

// TestSyncPortStateKeepsReservedRemotePort: when the tunnel already dials the
// right host port the add is skipped, but the remote port frps reserved must
// still reach the persisted rule. The cloud does not know it (it sends 0), so
// dropping it here publishes remote_port 0 upstream and makes the next agent
// start reserve a different port.
func TestSyncPortStateKeepsReservedRemotePort(t *testing.T) {
	am, _, mockTunnel, appStore, _, cfg := amHarness(t)

	mockTunnel.EXPECT().TunnelCapable().Return(true).Maybe()

	app := amSeed(t, appStore, 12, "mqttapp", common.RUNNING, common.PROD)
	app.RequestedState = common.RUNNING

	payload := amPayload(12, "mqttapp", common.RUNNING, common.PROD)
	payload.Ports = spsPorts(t, common.PortForwardRule{RuleName: "mqtt", Port: 1883, Protocol: "tcp", Active: true})

	_, err := am.hostPorts.RecoverOrReserve(hostPortKey{Stage: common.PROD, AppKey: 12, Protocol: "tcp", Port: 1883}, 40001)
	require.NoError(t, err)

	subdomain := tunnel.CreateSubdomain(tunnel.Protocol("tcp"), uint64(cfg.ReswarmConfig.DeviceKey), "mqttapp", 1883, common.PROD)
	existing := tunnel.TunnelConfig{
		Subdomain:  subdomain,
		Protocol:   tunnel.Protocol("tcp"),
		LocalPort:  40001,
		RemotePort: 30001, // granted by frps when the tunnel was first added
	}

	// Already dialing the current host port -> AddTunnel must not be called.
	mockTunnel.EXPECT().Get(tunnel.CreateTunnelID(subdomain, "tcp")).Return(&tunnel.Tunnel{Config: existing}).Once()
	// Skip-add now requires a LIVE frpc proxy, not just bookkeeping.
	mockTunnel.EXPECT().Status(tunnel.CreateTunnelID(subdomain, "tcp")).Return(tunnel.TunnelStatus{Status: "running"}, nil).Once()

	var savedPorts []interface{}
	mockTunnel.EXPECT().SaveRemotePorts(mock.Anything).RunAndReturn(func(p common.TransitionPayload) error {
		savedPorts = p.Ports
		return nil
	}).Once()
	mockTunnel.EXPECT().GetState().Return([]tunnel.TunnelState{}, nil).Once()

	require.NoError(t, am.syncPortState(payload, app))

	savedRules := spsRules(t, savedPorts)
	require.Len(t, savedRules, 1)
	assert.Equal(t, uint64(30001), savedRules[0].RemotePort, "the reserved remote port must survive a re-sync")
	assert.Equal(t, uint64(40001), savedRules[0].HostPort)
}

// TestSyncPortStatePersistsHostPortWithoutTunnels: a device that cannot tunnel
// (frpc missing/quarantined, or frps unreachable) still publishes its apps on
// host ports, and the LAN address is then the only way in — so the assignment
// must reach the cloud. Only the frpc calls are skipped.
func TestSyncPortStatePersistsHostPortWithoutTunnels(t *testing.T) {
	am, _, mockTunnel, appStore, _, _ := amHarness(t)

	mockTunnel.EXPECT().TunnelCapable().Return(false).Maybe()

	app := amSeed(t, appStore, 13, "notunnelapp", common.RUNNING, common.PROD)
	app.RequestedState = common.RUNNING

	payload := amPayload(13, "notunnelapp", common.RUNNING, common.PROD)
	payload.Ports = spsPorts(t, common.PortForwardRule{RuleName: "web", Port: 8080, Protocol: "http", Active: true})

	// The launch path allocated a managed host port; it does not depend on
	// tunnels being available.
	_, err := am.hostPorts.RecoverOrReserve(hostPortKey{Stage: common.PROD, AppKey: 13, Protocol: "tcp", Port: 8080}, 41999)
	require.NoError(t, err)

	// Get/AddTunnel/RemoveTunnel/GetState are deliberately not expected: any
	// call would reach an frpc that cannot serve it.
	var savedPorts []interface{}
	mockTunnel.EXPECT().SaveRemotePorts(mock.Anything).RunAndReturn(func(p common.TransitionPayload) error {
		savedPorts = p.Ports
		return nil
	}).Once()

	require.NoError(t, am.syncPortState(payload, app))

	savedRules := spsRules(t, savedPorts)
	require.Len(t, savedRules, 1)
	assert.Equal(t, uint64(41999), savedRules[0].HostPort, "host port must be persisted without tunnels")
	assert.Zero(t, savedRules[0].RemotePort, "no tunnel means no remote port")
}

// TestSyncPortStateNoRulesIsANoop: an app that exposes nothing must not cost a
// round trip on every state request.
func TestSyncPortStateNoRulesIsANoop(t *testing.T) {
	am, _, mockTunnel, appStore, _, _ := amHarness(t)

	mockTunnel.EXPECT().TunnelCapable().Return(true).Maybe()

	app := amSeed(t, appStore, 14, "portlessapp", common.RUNNING, common.PROD)
	app.RequestedState = common.RUNNING

	payload := amPayload(14, "portlessapp", common.RUNNING, common.PROD)
	payload.Ports = nil

	// No SaveRemotePorts/GetState expectations: nothing may be called.
	require.NoError(t, am.syncPortState(payload, app))
}

// TestGenerateDotEnvContentsCloudRemotePort: an instance-patched cloud port
// reaches the compose dotenv as {RemotePortEnvironment}_CLOUD even when no
// local tunnel object exists (the value is payload-borne).
func TestGenerateDotEnvContentsCloudRemotePort(t *testing.T) {
	am, _, mockTunnel, appStore, _, cfg := amHarness(t)

	mockTunnel.EXPECT().Get(mock.Anything).Return(nil).Maybe()

	app := amSeed(t, appStore, 11, "vpnapp", common.PRESENT, common.PROD)
	payload := amPayload(11, "vpnapp", common.RUNNING, common.PROD)
	payload.Ports = spsPorts(t, common.PortForwardRule{
		RuleName:              "vpn",
		Port:                  51820,
		Protocol:              "udp",
		Active:                true,
		RemotePortEnvironment: "WG_PORT",
		CloudRemotePort:       31099,
	})

	contents, skipped, err := am.StateMachine.generateDotEnvContents(cfg, payload, app, payload.DockerCompose)
	require.NoError(t, err)
	assert.Empty(t, skipped)
	assert.Contains(t, contents, "WG_PORT_CLOUD=31099")
	// The canonical name is emitted alongside the custom one.
	assert.Contains(t, contents, "REMOTE_PORT_FOR_51820_CLOUD=31099")
	// No local tunnel object -> the base WG_PORT env is absent; the cloud
	// port must not depend on it.
	assert.NotContains(t, contents, "\nWG_PORT=")
	assert.NotContains(t, contents, "REMOTE_PORT_FOR_51820=")
}

// The remove-and-reinstall deadlock: bookkeeping says the tunnel exists (same
// tunnel id, same host port), but frpc has no live proxy — it was dropped with
// the previous installation. The old code trusted the bookkeeping, skipped the
// add on every restart, and nothing ever re-established the tunnel. Skip-add
// must demand a LIVE proxy; a dead one gets replaced.
func TestSyncPortStateRebuildsDeadTunnel(t *testing.T) {
	am, _, mockTunnel, appStore, _, cfg := amHarness(t)

	mockTunnel.EXPECT().TunnelCapable().Return(true).Maybe()

	app := amSeed(t, appStore, 21, "deadtunnel", common.RUNNING, common.PROD)
	app.RequestedState = common.RUNNING

	payload := amPayload(21, "deadtunnel", common.RUNNING, common.PROD)
	payload.Ports = spsPorts(t, common.PortForwardRule{RuleName: "ui", Port: 9090, Protocol: "http", Active: true})

	_, err := am.hostPorts.RecoverOrReserve(hostPortKey{Stage: common.PROD, AppKey: 21, Protocol: "tcp", Port: 9090}, 41500)
	require.NoError(t, err)

	subdomain := tunnel.CreateSubdomain(tunnel.Protocol("http"), uint64(cfg.ReswarmConfig.DeviceKey), "deadtunnel", 9090, common.PROD)
	tunnelID := tunnel.CreateTunnelID(subdomain, "http")
	stale := tunnel.TunnelConfig{Subdomain: subdomain, AppName: "deadtunnel", Protocol: tunnel.Protocol("http"), LocalPort: 41500}

	// Bookkeeping claims the tunnel is up on the SAME host port…
	mockTunnel.EXPECT().Get(tunnelID).Return(&tunnel.Tunnel{Config: stale}).Once()
	// …but frpc has no such proxy.
	mockTunnel.EXPECT().Status(tunnelID).Return(tunnel.TunnelStatus{}, errdefs.ErrNotFound).Once()

	// The dead entry must be replaced, not trusted.
	mockTunnel.EXPECT().RemoveTunnel(stale).Return(nil).Once()
	mockTunnel.EXPECT().AddTunnel(mock.Anything).RunAndReturn(func(conf tunnel.TunnelConfig) (tunnel.TunnelConfig, error) {
		assert.Equal(t, uint64(41500), conf.LocalPort)
		return conf, nil
	}).Once()
	mockTunnel.EXPECT().SaveRemotePorts(mock.Anything).Return(nil).Once()
	mockTunnel.EXPECT().GetState().Return([]tunnel.TunnelState{}, nil).Once()

	require.NoError(t, am.syncPortState(payload, app))
}

// Uninstall must tear down every tunnel the app owns — leaving them standing
// is what made the later reinstall skip recreating them.
func TestUninstallRemovesAppTunnels(t *testing.T) {
	am, _, mockTunnel, _, _, cfg := amHarness(t)
	_ = cfg

	mockTunnel.EXPECT().TunnelCapable().Return(true).Once()

	mine := tunnel.TunnelConfig{Subdomain: "sub-a", AppName: "victim", Protocol: tunnel.Protocol("http"), LocalPort: 41000}
	other := tunnel.TunnelConfig{Subdomain: "sub-b", AppName: "bystander", Protocol: tunnel.Protocol("tcp"), LocalPort: 41001}
	mockTunnel.EXPECT().GetTunnelConfig().Return([]tunnel.TunnelConfig{mine, other}, nil).Once()

	// Only the uninstalled app's tunnel is removed; the bystander stays.
	mockTunnel.EXPECT().RemoveTunnel(mine).Return(nil).Once()

	am.RemoveAppTunnels("victim", common.PROD)
}

// A tunnel frps keeps refusing must be retried, but PACED. The refusal is
// invisible to AddTunnel (it returns success once frpc reloads its config), so
// without a backoff every sync rebuilds the same dead proxy — rewriting
// frpc.yaml and re-driving NewProxy several times a second, hardest exactly
// when the appliance-side cause ("authorization check failed") is least able to
// clear. Observed on tls-sf012, 2026-08-28.
func TestSyncPortStatePacesRepeatedDeadTunnelRebuilds(t *testing.T) {
	am, _, mockTunnel, appStore, _, cfg := amHarness(t)

	mockTunnel.EXPECT().TunnelCapable().Return(true).Maybe()

	app := amSeed(t, appStore, 22, "flappy", common.RUNNING, common.PROD)
	app.RequestedState = common.RUNNING

	payload := amPayload(22, "flappy", common.RUNNING, common.PROD)
	payload.Ports = spsPorts(t, common.PortForwardRule{RuleName: "ui", Port: 9091, Protocol: "http", Active: true})

	_, err := am.hostPorts.RecoverOrReserve(hostPortKey{Stage: common.PROD, AppKey: 22, Protocol: "tcp", Port: 9091}, 41501)
	require.NoError(t, err)

	subdomain := tunnel.CreateSubdomain(tunnel.Protocol("http"), uint64(cfg.ReswarmConfig.DeviceKey), "flappy", 9091, common.PROD)
	tunnelID := tunnel.CreateTunnelID(subdomain, "http")
	stale := tunnel.TunnelConfig{Subdomain: subdomain, AppName: "flappy", Protocol: tunnel.Protocol("http"), LocalPort: 41501, RemotePort: 30222}

	mockTunnel.EXPECT().Get(tunnelID).Return(&tunnel.Tunnel{Config: stale})
	// frps refused this proxy; frpc records the reason on the status. The
	// closure lets the test flip the proxy to healthy later on.
	proxyUp := false
	mockTunnel.EXPECT().Status(tunnelID).RunAndReturn(func(string) (tunnel.TunnelStatus, error) {
		if proxyUp {
			return tunnel.TunnelStatus{Name: tunnelID, Status: "running"}, nil
		}
		return tunnel.TunnelStatus{Name: tunnelID, Status: "start error", Error: "authorization check failed"}, nil
	})
	mockTunnel.EXPECT().GetState().Return([]tunnel.TunnelState{}, nil)

	// Every pass persists; keep the last rules so the deferred passes can be
	// checked for the reserved remote port.
	var saved []interface{}
	mockTunnel.EXPECT().SaveRemotePorts(mock.Anything).RunAndReturn(func(p common.TransitionPayload) error {
		saved = p.Ports
		return nil
	})

	// Exactly ONE rebuild across three back-to-back syncs.
	mockTunnel.EXPECT().RemoveTunnel(stale).Return(nil).Once()
	mockTunnel.EXPECT().AddTunnel(mock.Anything).RunAndReturn(func(conf tunnel.TunnelConfig) (tunnel.TunnelConfig, error) {
		return conf, nil
	}).Once()

	for i := 0; i < 3; i++ {
		require.NoError(t, am.syncPortState(payload, app))
	}
	mockTunnel.AssertExpectations(t)

	// Backing off must not clobber the reserved remote port with a zero — that
	// both hides the address upstream and makes the next start reserve another.
	rules := spsRules(t, saved)
	require.Len(t, rules, 1)
	assert.Equal(t, uint64(30222), rules[0].RemotePort, "a deferred rebuild must keep the reserved remote port")

	// Once the proxy comes up the backoff is cleared, so the NEXT failure
	// retries immediately rather than inheriting a stale delay.
	proxyUp = true
	require.NoError(t, am.syncPortState(payload, app))

	ok, _ := am.mayRebuildTunnel(tunnelID)
	assert.True(t, ok, "a healthy proxy must reset the rebuild backoff")
}

// =============================================================================
// User-reserved remote ports
// =============================================================================

// The reservation drives the tunnel config (strict mode) and a stale
// reservation_error is cleared on a matching grant, while reserved_* keys
// round-trip untouched.
func TestSyncPortStateReservedRemoteDrivesConfig(t *testing.T) {
	am, mockContainer, mockTunnel, appStore, _, cfg := amHarness(t)

	mockTunnel.EXPECT().TunnelCapable().Return(true).Maybe()

	app := amSeed(t, appStore, 40, "reservedapp", common.RUNNING, common.PROD)
	app.RequestedState = common.RUNNING

	payload := amPayload(40, "reservedapp", common.RUNNING, common.PROD)
	payload.Ports = spsPorts(t, common.PortForwardRule{
		RuleName: "mqtt", Port: 1883, Protocol: "tcp", Active: true,
		ReservedHostPort: 15000, ReservedRemotePort: 30500,
		ReservationError: "stale failure from a previous pass",
	})

	// The container already publishes the reserved port (recreated since the
	// reservation), so the reservation is active, not pending.
	mockContainer.EXPECT().GetContainerPortBindings(mock.Anything, payload.ContainerName.Prod).
		Return(map[string]uint64{"1883/tcp": 15000}, nil).Once()

	subdomain := tunnel.CreateSubdomain(tunnel.Protocol("tcp"), uint64(cfg.ReswarmConfig.DeviceKey), "reservedapp", 1883, common.PROD)
	tunnelID := tunnel.CreateTunnelID(subdomain, "tcp")

	mockTunnel.EXPECT().Get(tunnelID).Return(nil).Once()
	mockTunnel.EXPECT().AddTunnel(mock.Anything).RunAndReturn(func(conf tunnel.TunnelConfig) (tunnel.TunnelConfig, error) {
		assert.Equal(t, uint64(15000), conf.LocalPort, "frpc must dial the reserved host port")
		assert.Equal(t, uint64(30500), conf.RemotePort, "the reservation drives the requested remote port")
		assert.True(t, conf.Reserved, "a reserved rule must request strict validation")
		return conf, nil
	}).Once()

	var savedPorts []interface{}
	mockTunnel.EXPECT().SaveRemotePorts(mock.Anything).RunAndReturn(func(p common.TransitionPayload) error {
		savedPorts = p.Ports
		return nil
	}).Once()
	mockTunnel.EXPECT().GetState().Return([]tunnel.TunnelState{}, nil).Once()

	require.NoError(t, am.syncPortState(payload, app))

	savedRules := spsRules(t, savedPorts)
	require.Len(t, savedRules, 1)
	assert.Equal(t, uint64(30500), savedRules[0].RemotePort)
	assert.Equal(t, uint64(15000), savedRules[0].HostPort)
	assert.Equal(t, uint64(15000), savedRules[0].ReservedHostPort, "reserved_* round-trips untouched")
	assert.Equal(t, uint64(30500), savedRules[0].ReservedRemotePort)
	assert.Empty(t, savedRules[0].ReservationError, "a matching grant clears the stale error")
}

// A LIVE tunnel sitting on the wrong remote port is stale once a reservation
// applies: applying/changing a reservation must move it (Remove+Add) so the
// old frps bind is dropped promptly.
func TestSyncPortStateMovesLiveTunnelToReservedRemote(t *testing.T) {
	am, mockContainer, mockTunnel, appStore, _, cfg := amHarness(t)

	mockTunnel.EXPECT().TunnelCapable().Return(true).Maybe()

	app := amSeed(t, appStore, 41, "moveapp", common.RUNNING, common.PROD)
	app.RequestedState = common.RUNNING

	payload := amPayload(41, "moveapp", common.RUNNING, common.PROD)
	payload.Ports = spsPorts(t, common.PortForwardRule{
		RuleName: "mqtt", Port: 1883, Protocol: "tcp", Active: true, RemotePort: 30001,
		ReservedHostPort: 15000, ReservedRemotePort: 30500,
	})

	mockContainer.EXPECT().GetContainerPortBindings(mock.Anything, payload.ContainerName.Prod).
		Return(map[string]uint64{"1883/tcp": 15000}, nil).Once()

	subdomain := tunnel.CreateSubdomain(tunnel.Protocol("tcp"), uint64(cfg.ReswarmConfig.DeviceKey), "moveapp", 1883, common.PROD)
	tunnelID := tunnel.CreateTunnelID(subdomain, "tcp")
	existing := tunnel.TunnelConfig{Subdomain: subdomain, AppName: "moveapp", Protocol: tunnel.Protocol("tcp"), LocalPort: 15000, RemotePort: 30001}

	// Same local port, wrong remote port: no liveness probe, straight replace.
	mockTunnel.EXPECT().Get(tunnelID).Return(&tunnel.Tunnel{Config: existing}).Once()
	mockTunnel.EXPECT().RemoveTunnel(existing).Return(nil).Once()
	mockTunnel.EXPECT().AddTunnel(mock.Anything).RunAndReturn(func(conf tunnel.TunnelConfig) (tunnel.TunnelConfig, error) {
		assert.Equal(t, uint64(30500), conf.RemotePort)
		assert.True(t, conf.Reserved)
		return conf, nil
	}).Once()

	var savedPorts []interface{}
	mockTunnel.EXPECT().SaveRemotePorts(mock.Anything).RunAndReturn(func(p common.TransitionPayload) error {
		savedPorts = p.Ports
		return nil
	}).Once()
	mockTunnel.EXPECT().GetState().Return([]tunnel.TunnelState{}, nil).Once()

	require.NoError(t, am.syncPortState(payload, app))

	savedRules := spsRules(t, savedPorts)
	require.Len(t, savedRules, 1)
	assert.Equal(t, uint64(30500), savedRules[0].RemotePort)
}

// A grant contradicting the reservation must never be persisted: the reserved
// value stays the desired state and the mismatch surfaces as reservation_error.
func TestSyncPortStateWriteBackGuardKeepsReservedRemote(t *testing.T) {
	am, mockContainer, mockTunnel, appStore, _, cfg := amHarness(t)

	mockTunnel.EXPECT().TunnelCapable().Return(true).Maybe()

	app := amSeed(t, appStore, 42, "guardapp", common.RUNNING, common.PROD)
	app.RequestedState = common.RUNNING

	payload := amPayload(42, "guardapp", common.RUNNING, common.PROD)
	payload.Ports = spsPorts(t, common.PortForwardRule{
		RuleName: "mqtt", Port: 1883, Protocol: "tcp", Active: true,
		ReservedHostPort: 15000, ReservedRemotePort: 30500,
	})

	mockContainer.EXPECT().GetContainerPortBindings(mock.Anything, payload.ContainerName.Prod).
		Return(map[string]uint64{"1883/tcp": 15000}, nil).Once()

	subdomain := tunnel.CreateSubdomain(tunnel.Protocol("tcp"), uint64(cfg.ReswarmConfig.DeviceKey), "guardapp", 1883, common.PROD)
	tunnelID := tunnel.CreateTunnelID(subdomain, "tcp")

	mockTunnel.EXPECT().Get(tunnelID).Return(nil).Once()
	mockTunnel.EXPECT().AddTunnel(mock.Anything).RunAndReturn(func(conf tunnel.TunnelConfig) (tunnel.TunnelConfig, error) {
		conf.RemotePort = 31000 // frps handed out something else
		return conf, nil
	}).Once()

	var savedPorts []interface{}
	mockTunnel.EXPECT().SaveRemotePorts(mock.Anything).RunAndReturn(func(p common.TransitionPayload) error {
		savedPorts = p.Ports
		return nil
	}).Once()
	mockTunnel.EXPECT().GetState().Return([]tunnel.TunnelState{}, nil).Once()

	require.NoError(t, am.syncPortState(payload, app))

	savedRules := spsRules(t, savedPorts)
	require.Len(t, savedRules, 1)
	assert.Equal(t, uint64(30500), savedRules[0].RemotePort, "the granted 31000 must not clobber the reservation")
	assert.Contains(t, savedRules[0].ReservationError, "granted port 31000 instead of reserved 30500")
}

// A failed AddTunnel on a reserved rule must surface on the rule instead of
// being log-only (the UI row would spin forever).
func TestSyncPortStateReservedAddFailureSetsReservationError(t *testing.T) {
	am, mockContainer, mockTunnel, appStore, msg, cfg := amHarness(t)

	mockTunnel.EXPECT().TunnelCapable().Return(true).Maybe()

	app := amSeed(t, appStore, 43, "failapp", common.RUNNING, common.PROD)
	app.RequestedState = common.RUNNING

	payload := amPayload(43, "failapp", common.RUNNING, common.PROD)
	payload.Ports = spsPorts(t, common.PortForwardRule{
		RuleName: "mqtt", Port: 1883, Protocol: "tcp", Active: true,
		ReservedHostPort: 15000, ReservedRemotePort: 30500,
	})

	mockContainer.EXPECT().GetContainerPortBindings(mock.Anything, payload.ContainerName.Prod).
		Return(map[string]uint64{"1883/tcp": 15000}, nil).Times(2)

	subdomain := tunnel.CreateSubdomain(tunnel.Protocol("tcp"), uint64(cfg.ReswarmConfig.DeviceKey), "failapp", 1883, common.PROD)
	tunnelID := tunnel.CreateTunnelID(subdomain, "tcp")

	mockTunnel.EXPECT().Get(tunnelID).Return(nil).Times(2)
	mockTunnel.EXPECT().AddTunnel(mock.Anything).Return(tunnel.TunnelConfig{}, errors.New("port 30500 already in use")).Times(2)

	var savedPorts []interface{}
	mockTunnel.EXPECT().SaveRemotePorts(mock.Anything).RunAndReturn(func(p common.TransitionPayload) error {
		savedPorts = p.Ports
		return nil
	}).Once()
	mockTunnel.EXPECT().GetState().Return([]tunnel.TunnelState{}, nil).Times(2)

	require.NoError(t, am.syncPortState(payload, app))

	savedRules := spsRules(t, savedPorts)
	require.Len(t, savedRules, 1)
	assert.Contains(t, savedRules[0].ReservationError, "already in use")
	assert.Zero(t, savedRules[0].RemotePort, "no grant may be invented on failure")

	countFailureLines := func() int {
		count := 0
		for _, call := range msg.GetPublishCalls() {
			if len(call.Args) == 0 {
				continue
			}
			if dict, ok := call.Args[0].(common.Dict); ok {
				if chunk, ok := dict["chunk"].(string); ok && strings.Contains(chunk, "could not be established") {
					count++
				}
			}
		}
		return count
	}
	assert.Equal(t, 1, countFailureLines(), "the failure is written to the app log")

	// The next sync pass carries the persisted reservation_error back in; the
	// identical failure must not write the app-log line again (a persistent
	// frps refusal would otherwise spam the user-visible log once per sync).
	// SaveRemotePorts is not expected again: nothing changed.
	payload2 := amPayload(43, "failapp", common.RUNNING, common.PROD)
	payload2.Ports = spsPorts(t, common.PortForwardRule{
		RuleName: "mqtt", Port: 1883, Protocol: "tcp", Active: true,
		HostPort:         15000,
		ReservedHostPort: 15000, ReservedRemotePort: 30500,
		ReservationError: savedRules[0].ReservationError,
	})
	require.NoError(t, am.syncPortState(payload2, app))
	assert.Equal(t, 1, countFailureLines(), "an identical persisting failure logs once")
}

// A fully satisfied reservation round-trips as a no-op: nothing changed, so
// nothing is persisted upstream.
func TestSyncPortStateReservedNoopRoundTrip(t *testing.T) {
	am, mockContainer, mockTunnel, appStore, _, cfg := amHarness(t)

	mockTunnel.EXPECT().TunnelCapable().Return(true).Maybe()

	app := amSeed(t, appStore, 44, "noopapp", common.RUNNING, common.PROD)
	app.RequestedState = common.RUNNING

	payload := amPayload(44, "noopapp", common.RUNNING, common.PROD)
	payload.Ports = spsPorts(t, common.PortForwardRule{
		RuleName: "mqtt", Port: 1883, Protocol: "tcp", Active: true,
		HostPort: 15000, RemotePort: 30500,
		ReservedHostPort: 15000, ReservedRemotePort: 30500,
	})

	mockContainer.EXPECT().GetContainerPortBindings(mock.Anything, payload.ContainerName.Prod).
		Return(map[string]uint64{"1883/tcp": 15000}, nil).Once()

	subdomain := tunnel.CreateSubdomain(tunnel.Protocol("tcp"), uint64(cfg.ReswarmConfig.DeviceKey), "noopapp", 1883, common.PROD)
	tunnelID := tunnel.CreateTunnelID(subdomain, "tcp")
	existing := tunnel.TunnelConfig{Subdomain: subdomain, AppName: "noopapp", Protocol: tunnel.Protocol("tcp"), LocalPort: 15000, RemotePort: 30500}

	mockTunnel.EXPECT().Get(tunnelID).Return(&tunnel.Tunnel{Config: existing}).Once()
	mockTunnel.EXPECT().Status(tunnelID).Return(tunnel.TunnelStatus{Status: "running"}, nil).Once()

	// SaveRemotePorts deliberately not expected: the strict mock fails on any
	// unexpected call.
	mockTunnel.EXPECT().GetState().Return([]tunnel.TunnelState{}, nil).Once()

	require.NoError(t, am.syncPortState(payload, app))
}

// http/https rules and their reserved_* keys are inert: no reserved remote is
// requested, and the host port stays pool-managed.
func TestSyncPortStateHttpRuleIgnoresReservations(t *testing.T) {
	am, _, mockTunnel, appStore, _, cfg := amHarness(t)

	mockTunnel.EXPECT().TunnelCapable().Return(true).Maybe()

	app := amSeed(t, appStore, 45, "webapp", common.RUNNING, common.PROD)
	app.RequestedState = common.RUNNING

	payload := amPayload(45, "webapp", common.RUNNING, common.PROD)
	payload.Ports = spsPorts(t, common.PortForwardRule{
		RuleName: "web", Port: 8080, Protocol: "http", Active: true,
		ReservedHostPort: 15000, ReservedRemotePort: 30500,
	})

	// The launch path allocated a pool port; the "reservation" must not move it.
	_, err := am.hostPorts.RecoverOrReserve(hostPortKey{Stage: common.PROD, AppKey: 45, Protocol: "tcp", Port: 8080}, 41234)
	require.NoError(t, err)

	subdomain := tunnel.CreateSubdomain(tunnel.Protocol("http"), uint64(cfg.ReswarmConfig.DeviceKey), "webapp", 8080, common.PROD)
	tunnelID := tunnel.CreateTunnelID(subdomain, "http")

	mockTunnel.EXPECT().Get(tunnelID).Return(nil).Once()
	mockTunnel.EXPECT().AddTunnel(mock.Anything).RunAndReturn(func(conf tunnel.TunnelConfig) (tunnel.TunnelConfig, error) {
		assert.Equal(t, uint64(41234), conf.LocalPort, "the pool port stays; reserved_* is inert on http")
		assert.Zero(t, conf.RemotePort)
		assert.False(t, conf.Reserved)
		return conf, nil
	}).Once()

	mockTunnel.EXPECT().SaveRemotePorts(mock.Anything).Return(nil).Once()
	mockTunnel.EXPECT().GetState().Return([]tunnel.TunnelState{}, nil).Once()

	require.NoError(t, am.syncPortState(payload, app))
}

// A host-port reservation saved while the app is RUNNING arrives via a sync
// that never recreates the container, so nothing binds the reserved port yet.
// The working tunnel must keep dialing the port the container actually
// publishes (host_port write-back stays truthful, the UI shows the
// reservation as pending), while the reserved port is pinned so the pool
// cannot hand it to another app meanwhile. The friendly "takes effect on next
// restart" app-log note is written once, not on every sync pass.
func TestSyncPortStateReservationOnRunningAppKeepsLiveDialPort(t *testing.T) {
	am, mockContainer, mockTunnel, appStore, msg, cfg := amHarness(t)

	mockTunnel.EXPECT().TunnelCapable().Return(true).Maybe()

	app := amSeed(t, appStore, 46, "pendingapp", common.RUNNING, common.PROD)
	app.RequestedState = common.RUNNING

	rule := common.PortForwardRule{
		RuleName: "mqtt", Port: 1883, Protocol: "tcp", Active: true,
		HostPort: 41234, RemotePort: 30001,
		ReservedHostPort: 15000,
	}
	payload := amPayload(46, "pendingapp", common.RUNNING, common.PROD)
	payload.Ports = spsPorts(t, rule)

	// The running container still publishes its pre-reservation pool port.
	mockContainer.EXPECT().GetContainerPortBindings(mock.Anything, payload.ContainerName.Prod).
		Return(map[string]uint64{"1883/tcp": 41234}, nil).Times(3)

	subdomain := tunnel.CreateSubdomain(tunnel.Protocol("tcp"), uint64(cfg.ReswarmConfig.DeviceKey), "pendingapp", 1883, common.PROD)
	tunnelID := tunnel.CreateTunnelID(subdomain, "tcp")
	existing := tunnel.TunnelConfig{Subdomain: subdomain, AppName: "pendingapp", Protocol: tunnel.Protocol("tcp"), LocalPort: 41234, RemotePort: 30001}

	// The live tunnel keeps dialing the published port: no Remove/Add, and
	// nothing changed, so nothing is persisted (SaveRemotePorts not expected).
	mockTunnel.EXPECT().Get(tunnelID).Return(&tunnel.Tunnel{Config: existing}).Times(3)
	mockTunnel.EXPECT().Status(tunnelID).Return(tunnel.TunnelStatus{Status: "running"}, nil).Times(3)
	mockTunnel.EXPECT().GetState().Return([]tunnel.TunnelState{}, nil).Times(3)

	require.NoError(t, am.syncPortState(payload, app))

	// The reserved port stays pinned for the app so the pool cannot hand it
	// out while the reservation waits for the next container recreate.
	key := hostPortKeyForRule(common.PROD, 46, rule)
	assert.True(t, am.hostPorts.IsPinned(key))
	pinnedPort, ok := am.hostPorts.Get(key)
	require.True(t, ok)
	assert.Equal(t, uint64(15000), pinnedPort)

	logTopic := fmt.Sprintf("reswarm.logs.%s.%s", cfg.ReswarmConfig.SerialNumber, payload.ContainerName.Prod)
	countNotes := func() int {
		count := 0
		for _, call := range msg.GetPublishCalls() {
			if string(call.Topic) != logTopic || len(call.Args) == 0 {
				continue
			}
			if dict, ok := call.Args[0].(common.Dict); ok {
				if chunk, ok := dict["chunk"].(string); ok && strings.Contains(chunk, "takes effect on the next restart") {
					count++
				}
			}
		}
		return count
	}
	assert.Equal(t, 1, countNotes(), "the pending note is written on the first pass")

	// An identical second sync pass must not repeat the note (no app-log spam
	// while the reservation stays pending) and must keep the dial port.
	payload2 := amPayload(46, "pendingapp", common.RUNNING, common.PROD)
	payload2.Ports = spsPorts(t, rule)
	require.NoError(t, am.syncPortState(payload2, app))
	assert.Equal(t, 1, countNotes(), "a persisting pending reservation logs once")

	// Removing the reservation while it was still pending must drop the pin
	// and keep dialing the live port: recovering the never-bound 15000 here
	// would break the working tunnel exactly like dialing it directly would.
	ruleCleared := rule
	ruleCleared.ReservedHostPort = 0
	payload3 := amPayload(46, "pendingapp", common.RUNNING, common.PROD)
	payload3.Ports = spsPorts(t, ruleCleared)
	require.NoError(t, am.syncPortState(payload3, app))

	assert.False(t, am.hostPorts.IsPinned(key), "the stale pin is dropped with the reservation")
	recovered, ok := am.hostPorts.Get(key)
	require.True(t, ok)
	assert.Equal(t, uint64(41234), recovered, "the assignment reverts to the live pool port")
}

// =============================================================================
// Agent-driven tunnel retries
// =============================================================================

// spsPacing returns a copy of the tunnel's retry pacing state, if it has one.
func spsPacing(am *AppManager, tunnelID string) (tunnelRebuildState, bool) {
	am.tunnelRebuildLock.Lock()
	defer am.tunnelRebuildLock.Unlock()

	st := am.tunnelRebuilds[tunnelID]
	if st == nil {
		return tunnelRebuildState{}, false
	}
	return *st, true
}

// spsAgePacing moves the tunnel's last attempt back by d: the backoff elapsing,
// without sleeping through it.
func spsAgePacing(t *testing.T, am *AppManager, tunnelID string, d time.Duration) {
	t.Helper()

	am.tunnelRebuildLock.Lock()
	defer am.tunnelRebuildLock.Unlock()

	st := am.tunnelRebuilds[tunnelID]
	require.NotNil(t, st, "the tunnel has no pacing state to age")
	st.last = st.last.Add(-d)
}

// spsPendingRetry returns the app's pending re-sync (nil when none) and how
// many re-syncs are pending across all apps.
func spsPendingRetry(am *AppManager, appKey uint64, stage common.Stage) (*portSyncRetry, int) {
	am.tunnelRebuildLock.Lock()
	defer am.tunnelRebuildLock.Unlock()

	return am.portSyncRetries[appStageKey{AppKey: appKey, Stage: stage}], len(am.portSyncRetries)
}

// The 2026-10-02 outage: after an agent update REtunnel refused
// re.tunnel.expose_port for the tunnels' previous remote ports ("not
// authorized"). The refusal cleared within minutes, but no state push,
// reconnect or transition re-ran the sync, so the TCP/UDP tunnels on devices
// 4749 and 4806 stayed down until their apps were restarted by hand. The agent
// must retry a failed add by itself, paced, from the stored requested state,
// until frpc is seen serving the tunnel.
func TestSyncPortStateRetriesFailedAddWithoutTrigger(t *testing.T) {
	am, _, mockTunnel, appStore, _, cfg := amHarness(t)
	retries := amCapturePortSyncRetries(t)

	mockTunnel.EXPECT().TunnelCapable().Return(true).Maybe()

	app := amSeed(t, appStore, 50, "relayapp", common.RUNNING, common.PROD)
	app.RequestedState = common.RUNNING

	rule := common.PortForwardRule{RuleName: "mqtt", Port: 1883, Protocol: "tcp", Active: true, HostPort: 41600, RemotePort: 30777}
	payload := amPayload(50, "relayapp", common.RUNNING, common.PROD)
	payload.Ports = spsPorts(t, rule)
	require.NoError(t, appStore.UpdateLocalRequestedState(payload))

	_, err := am.hostPorts.RecoverOrReserve(hostPortKey{Stage: common.PROD, AppKey: 50, Protocol: "tcp", Port: 1883}, 41600)
	require.NoError(t, err)

	subdomain := tunnel.CreateSubdomain(tunnel.Protocol("tcp"), uint64(cfg.ReswarmConfig.DeviceKey), "relayapp", 1883, common.PROD)
	tunnelID := tunnel.CreateTunnelID(subdomain, "tcp")
	up := tunnel.TunnelConfig{Subdomain: subdomain, AppName: "relayapp", Protocol: tunnel.Protocol("tcp"), LocalPort: 41600, RemotePort: 30778}

	// A failed add releases its claim, so every pass up to the successful add
	// finds no tunnel; the last one finds the tunnel that came up.
	mockTunnel.EXPECT().Get(tunnelID).Return(nil).Times(3)
	mockTunnel.EXPECT().Get(tunnelID).Return(&tunnel.Tunnel{Config: up}).Once()

	var requestedRemotes []uint64
	refusals := 2
	mockTunnel.EXPECT().AddTunnel(mock.Anything).RunAndReturn(func(conf tunnel.TunnelConfig) (tunnel.TunnelConfig, error) {
		requestedRemotes = append(requestedRemotes, conf.RemotePort)
		if refusals > 0 {
			refusals--
			return tunnel.TunnelConfig{}, errors.New("not authorized")
		}
		return conf, nil
	}).Times(3)
	mockTunnel.EXPECT().Status(tunnelID).Return(tunnel.TunnelStatus{Name: tunnelID, Status: "running"}, nil).Once()
	mockTunnel.EXPECT().GetState().Return([]tunnel.TunnelState{}, nil).Times(4)

	// The only external trigger in this test.
	require.NoError(t, am.syncPortState(payload, app))

	scheduled := retries.all()
	require.Len(t, scheduled, 1, "a failed add schedules exactly one re-sync")
	assert.Equal(t, rebuildBackoffBase, scheduled[0].after)

	// The retry works from the stored requested state, not from the payload
	// the failed pass ran with: a push stored meanwhile wins.
	rule.RemotePort = 30778
	payload.Ports = spsPorts(t, rule)
	require.NoError(t, appStore.UpdateLocalRequestedState(payload))

	scheduled[0].fire()
	scheduled = retries.all()
	require.Len(t, scheduled, 2, "a retry that fails again schedules the next one")
	assert.Equal(t, 2*rebuildBackoffBase, scheduled[1].after, "backing off")

	scheduled[1].fire()
	scheduled = retries.all()
	require.Len(t, scheduled, 3, "a tunnel that failed before is looked at once more after its add succeeded")
	assert.Equal(t, 4*rebuildBackoffBase, scheduled[2].after)

	// frpc serves the proxy: the chain ends and the pacing is cleared.
	scheduled[2].fire()
	assert.Len(t, retries.all(), 3, "a serving tunnel needs no further re-sync")
	_, paced := spsPacing(am, tunnelID)
	assert.False(t, paced, "a serving tunnel clears its pacing")
	_, pending := spsPendingRetry(am, 50, common.PROD)
	assert.Zero(t, pending)

	assert.Equal(t, []uint64{30777, 30778, 30778}, requestedRemotes, "each retry re-reads the stored requested state")
}

// A dead proxy rebuilt once is held off by the backoff on the next pass. The
// deferred rebuild then waited for some unrelated sync to come along after the
// backoff expired; the agent must come back for it by itself.
func TestSyncPortStateRetriesDeferredRebuild(t *testing.T) {
	am, _, mockTunnel, appStore, _, cfg := amHarness(t)
	retries := amCapturePortSyncRetries(t)

	mockTunnel.EXPECT().TunnelCapable().Return(true).Maybe()

	app := amSeed(t, appStore, 51, "refusedapp", common.RUNNING, common.PROD)
	app.RequestedState = common.RUNNING

	payload := amPayload(51, "refusedapp", common.RUNNING, common.PROD)
	payload.Ports = spsPorts(t, common.PortForwardRule{RuleName: "vpn", Port: 51820, Protocol: "udp", Active: true, HostPort: 41700, RemotePort: 30801})
	require.NoError(t, appStore.UpdateLocalRequestedState(payload))

	_, err := am.hostPorts.RecoverOrReserve(hostPortKey{Stage: common.PROD, AppKey: 51, Protocol: "udp", Port: 51820}, 41700)
	require.NoError(t, err)

	subdomain := tunnel.CreateSubdomain(tunnel.Protocol("udp"), uint64(cfg.ReswarmConfig.DeviceKey), "refusedapp", 51820, common.PROD)
	tunnelID := tunnel.CreateTunnelID(subdomain, "udp")
	stale := tunnel.TunnelConfig{Subdomain: subdomain, AppName: "refusedapp", Protocol: tunnel.Protocol("udp"), LocalPort: 41700, RemotePort: 30801}

	// Bookkeeping keeps the tunnel while frps keeps refusing its proxy.
	mockTunnel.EXPECT().Get(tunnelID).Return(&tunnel.Tunnel{Config: stale})
	mockTunnel.EXPECT().Status(tunnelID).Return(tunnel.TunnelStatus{Name: tunnelID, Status: "start error", Error: "authorization check failed"}, nil)
	mockTunnel.EXPECT().GetState().Return([]tunnel.TunnelState{}, nil)

	// Two rebuilds: the first pass's, and the one once the backoff elapsed.
	mockTunnel.EXPECT().RemoveTunnel(stale).Return(nil).Times(2)
	mockTunnel.EXPECT().AddTunnel(mock.Anything).RunAndReturn(func(conf tunnel.TunnelConfig) (tunnel.TunnelConfig, error) {
		return conf, nil
	}).Times(2)

	require.NoError(t, am.syncPortState(payload, app))
	scheduled := retries.all()
	require.Len(t, scheduled, 1, "a rebuild is looked at again once the backoff would allow the next one")
	assert.Equal(t, rebuildBackoffBase, scheduled[0].after)

	// The re-sync lands with 4s of the backoff gone: the rebuild is deferred,
	// and the agent must come back for the remaining 6s by itself.
	spsAgePacing(t, am, tunnelID, 4*time.Second)
	scheduled[0].fire()
	scheduled = retries.all()
	require.Len(t, scheduled, 2, "a deferred rebuild schedules its own re-sync")
	assert.LessOrEqual(t, scheduled[1].after, rebuildBackoffBase-4*time.Second, "at the remaining backoff")
	assert.Greater(t, scheduled[1].after, rebuildBackoffBase-5*time.Second, "at the remaining backoff")

	// Fired once the backoff has elapsed, it rebuilds and paces the next look.
	spsAgePacing(t, am, tunnelID, rebuildBackoffBase)
	scheduled[1].fire()

	st, paced := spsPacing(am, tunnelID)
	require.True(t, paced)
	assert.Equal(t, 2, st.attempts, "the re-sync rebuilt the proxy")
	scheduled = retries.all()
	require.Len(t, scheduled, 3)
	assert.Equal(t, 2*rebuildBackoffBase, scheduled[2].after)
}

// The normal boot path: a tunnel added successfully for the first time costs
// no follow-up re-sync.
func TestSyncPortStateFirstAddSchedulesNoRetry(t *testing.T) {
	am, _, mockTunnel, appStore, _, cfg := amHarness(t)
	retries := amCapturePortSyncRetries(t)

	mockTunnel.EXPECT().TunnelCapable().Return(true).Maybe()

	app := amSeed(t, appStore, 52, "freshtunnel", common.RUNNING, common.PROD)
	app.RequestedState = common.RUNNING

	payload := amPayload(52, "freshtunnel", common.RUNNING, common.PROD)
	payload.Ports = spsPorts(t, common.PortForwardRule{RuleName: "mqtt", Port: 1883, Protocol: "tcp", Active: true, HostPort: 41800, RemotePort: 30900})

	_, err := am.hostPorts.RecoverOrReserve(hostPortKey{Stage: common.PROD, AppKey: 52, Protocol: "tcp", Port: 1883}, 41800)
	require.NoError(t, err)

	subdomain := tunnel.CreateSubdomain(tunnel.Protocol("tcp"), uint64(cfg.ReswarmConfig.DeviceKey), "freshtunnel", 1883, common.PROD)
	tunnelID := tunnel.CreateTunnelID(subdomain, "tcp")

	mockTunnel.EXPECT().Get(tunnelID).Return(nil).Once()
	mockTunnel.EXPECT().AddTunnel(mock.Anything).RunAndReturn(func(conf tunnel.TunnelConfig) (tunnel.TunnelConfig, error) {
		return conf, nil
	}).Once()
	mockTunnel.EXPECT().GetState().Return([]tunnel.TunnelState{}, nil).Once()

	require.NoError(t, am.syncPortState(payload, app))

	assert.Empty(t, retries.all(), "a first add that succeeds schedules no re-sync")
	_, paced := spsPacing(am, tunnelID)
	assert.False(t, paced, "and leaves no pacing behind")
}

// A retry that fires after the app was uninstalled has nothing left to do and
// must not bring the app's tunnels back.
func TestPortSyncRetryStopsForRemovedApp(t *testing.T) {
	am, _, mockTunnel, appStore, _, cfg := amHarness(t)
	retries := amCapturePortSyncRetries(t)

	app := amSeed(t, appStore, 53, "goneapp", common.RUNNING, common.PROD)
	app.RequestedState = common.RUNNING

	payload := amPayload(53, "goneapp", common.RUNNING, common.PROD)
	payload.Ports = spsPorts(t, common.PortForwardRule{RuleName: "mqtt", Port: 1883, Protocol: "tcp", Active: true, HostPort: 41900, RemotePort: 30950})
	require.NoError(t, appStore.UpdateLocalRequestedState(payload))

	_, err := am.hostPorts.RecoverOrReserve(hostPortKey{Stage: common.PROD, AppKey: 53, Protocol: "tcp", Port: 1883}, 41900)
	require.NoError(t, err)

	subdomain := tunnel.CreateSubdomain(tunnel.Protocol("tcp"), uint64(cfg.ReswarmConfig.DeviceKey), "goneapp", 1883, common.PROD)
	tunnelID := tunnel.CreateTunnelID(subdomain, "tcp")

	// Exactly the first pass's calls: the strict mock fails the test on any
	// tunnel call a retry makes.
	mockTunnel.EXPECT().TunnelCapable().Return(true).Once()
	mockTunnel.EXPECT().Get(tunnelID).Return(nil).Once()
	mockTunnel.EXPECT().AddTunnel(mock.Anything).Return(tunnel.TunnelConfig{}, errors.New("not authorized")).Once()
	mockTunnel.EXPECT().GetState().Return([]tunnel.TunnelState{}, nil).Once()

	require.NoError(t, am.syncPortState(payload, app))
	scheduled := retries.all()
	require.Len(t, scheduled, 1)

	// A completed teardown deletes both rows; the in-memory app entry stays.
	require.NoError(t, appStore.DeleteAppState(53, common.PROD))
	require.NoError(t, appStore.DeleteRequestedState(53, common.PROD))

	scheduled[0].fire()
	assert.Len(t, retries.all(), 1, "a retry for a removed app schedules nothing")

	// Likewise for an app the agent does not know at all.
	am.schedulePortSyncRetry(999, common.PROD, rebuildBackoffBase)
	scheduled = retries.all()
	require.Len(t, scheduled, 2)
	scheduled[1].fire()
	assert.Len(t, retries.all(), 2)
}

// A disk emergency stops every app container and refuses to start one. The
// agent's own tunnel re-syncs must stand down meanwhile: re-adding tunnels for
// apps that are not running only rewrites frpc.yaml and reloads frpc on a
// critically full disk. Nothing skipped is lost: leaving the emergency
// reconciles every app's tunnels.
func TestPortSyncRetryStandsDownInDiskEmergency(t *testing.T) {
	am, _, mockTunnel, appStore, _, cfg := amHarness(t)
	retries := amCapturePortSyncRetries(t)

	app := amSeed(t, appStore, 55, "fullapp", common.RUNNING, common.PROD)
	app.RequestedState = common.RUNNING

	payload := amPayload(55, "fullapp", common.RUNNING, common.PROD)
	payload.Ports = spsPorts(t, common.PortForwardRule{RuleName: "mqtt", Port: 1883, Protocol: "tcp", Active: true, HostPort: 42000, RemotePort: 31000})
	require.NoError(t, appStore.UpdateLocalRequestedState(payload))

	_, err := am.hostPorts.RecoverOrReserve(hostPortKey{Stage: common.PROD, AppKey: 55, Protocol: "tcp", Port: 1883}, 42000)
	require.NoError(t, err)

	subdomain := tunnel.CreateSubdomain(tunnel.Protocol("tcp"), uint64(cfg.ReswarmConfig.DeviceKey), "fullapp", 1883, common.PROD)
	tunnelID := tunnel.CreateTunnelID(subdomain, "tcp")

	// The first pass's add fails and schedules a retry.
	mockTunnel.EXPECT().TunnelCapable().Return(true).Once()
	mockTunnel.EXPECT().Get(tunnelID).Return(nil).Once()
	mockTunnel.EXPECT().AddTunnel(mock.Anything).Return(tunnel.TunnelConfig{}, errors.New("not authorized")).Once()
	mockTunnel.EXPECT().GetState().Return([]tunnel.TunnelState{}, nil).Once()

	require.NoError(t, am.syncPortState(payload, app))
	scheduled := retries.all()
	require.Len(t, scheduled, 1)

	emergency := true
	restore := diskEmergency
	diskEmergency = func() bool { return emergency }
	t.Cleanup(func() { diskEmergency = restore })

	// In the emergency neither the retry nor a reconcile on the capability
	// coming back touches the tunnels: the strict mock fails on any call.
	scheduled[0].fire()
	am.SyncAllPortStates()
	mockTunnel.AssertExpectations(t)
	assert.Len(t, retries.all(), 1, "a re-sync skipped in the emergency schedules nothing")

	// Leaving it, diskguard's OnRecover reconciles the tunnel the emergency
	// held back.
	emergency = false
	mockTunnel.EXPECT().TunnelCapable().Return(true).Once()
	mockTunnel.EXPECT().Get(tunnelID).Return(nil).Once()
	mockTunnel.EXPECT().AddTunnel(mock.Anything).RunAndReturn(func(conf tunnel.TunnelConfig) (tunnel.TunnelConfig, error) {
		return conf, nil
	}).Once()
	mockTunnel.EXPECT().SaveRemotePorts(mock.Anything).Return(nil).Maybe()
	mockTunnel.EXPECT().GetState().Return([]tunnel.TunnelState{}, nil).Once()

	am.SyncAllPortStates()
	scheduled = retries.all()
	require.Len(t, scheduled, 2, "the tunnel failed before, so its successful add gets a follow-up")
	assert.Equal(t, 2*rebuildBackoffBase, scheduled[1].after)
}

// Retries re-read the stored requested state, which never carries back the
// reservation_error the agent persisted upstream. Unless the agent remembers
// what it already wrote, every retry of a refused reserved port writes the same
// user-visible app-log line again.
func TestSyncPortStateRetryLogsReservationFailureOnce(t *testing.T) {
	am, mockContainer, mockTunnel, appStore, msg, cfg := amHarness(t)
	retries := amCapturePortSyncRetries(t)

	mockTunnel.EXPECT().TunnelCapable().Return(true).Maybe()

	app := amSeed(t, appStore, 54, "pinnedapp", common.RUNNING, common.PROD)
	app.RequestedState = common.RUNNING

	payload := amPayload(54, "pinnedapp", common.RUNNING, common.PROD)
	payload.Ports = spsPorts(t, common.PortForwardRule{
		RuleName: "mqtt", Port: 1883, Protocol: "tcp", Active: true,
		HostPort: 15000, ReservedHostPort: 15000, ReservedRemotePort: 30500,
	})
	require.NoError(t, appStore.UpdateLocalRequestedState(payload))

	mockContainer.EXPECT().GetContainerPortBindings(mock.Anything, mock.Anything).
		Return(map[string]uint64{"1883/tcp": 15000}, nil).Times(3)

	subdomain := tunnel.CreateSubdomain(tunnel.Protocol("tcp"), uint64(cfg.ReswarmConfig.DeviceKey), "pinnedapp", 1883, common.PROD)
	tunnelID := tunnel.CreateTunnelID(subdomain, "tcp")

	mockTunnel.EXPECT().Get(tunnelID).Return(nil).Times(3)
	mockTunnel.EXPECT().AddTunnel(mock.Anything).Return(tunnel.TunnelConfig{}, errors.New("port 30500 is not authorized")).Times(3)
	// Each pass surfaces the failure on a rule that, as stored, never carries
	// it, so each one persists it upstream.
	mockTunnel.EXPECT().SaveRemotePorts(mock.Anything).Return(nil).Times(3)
	mockTunnel.EXPECT().GetState().Return([]tunnel.TunnelState{}, nil).Times(3)

	countFailureLines := func() int {
		count := 0
		for _, call := range msg.GetPublishCalls() {
			if len(call.Args) == 0 {
				continue
			}
			if dict, ok := call.Args[0].(common.Dict); ok {
				if chunk, ok := dict["chunk"].(string); ok && strings.Contains(chunk, "could not be established") {
					count++
				}
			}
		}
		return count
	}

	require.NoError(t, am.syncPortState(payload, app))
	require.Equal(t, 1, countFailureLines(), "the failure is written to the app log")

	for retry := 0; retry < 2; retry++ {
		scheduled := retries.all()
		scheduled[len(scheduled)-1].fire()
	}
	require.Len(t, retries.all(), 3, "both retries ran and failed again")
	assert.Equal(t, 1, countFailureLines(), "retries hitting the same failure do not write it again")
}

// One pending re-sync per app, at the earliest time any of its tunnels asked
// for: a later request must not postpone it, an earlier one replaces it.
func TestPortSyncRetryKeepsEarliest(t *testing.T) {
	am, _, _, _, _, _ := amHarness(t)
	retries := amCapturePortSyncRetries(t)

	start := time.Now()
	am.schedulePortSyncRetry(55, common.PROD, 2*rebuildBackoffBase)
	am.schedulePortSyncRetry(55, common.PROD, rebuildBackoffBase)
	am.schedulePortSyncRetry(55, common.PROD, 4*rebuildBackoffBase)
	end := time.Now()

	scheduled := retries.all()
	require.Len(t, scheduled, 2, "only the earlier request arms a new timer")
	assert.Equal(t, rebuildBackoffBase, scheduled[1].after)

	pending, count := spsPendingRetry(am, 55, common.PROD)
	require.Equal(t, 1, count, "one pending re-sync per app")
	require.NotNil(t, pending)
	assert.False(t, pending.due.Before(start.Add(rebuildBackoffBase)), "at the earlier due time")
	assert.False(t, pending.due.After(end.Add(rebuildBackoffBase)), "at the earlier due time")

	// The replaced timer firing anyway (it was already running when it was
	// stopped) must not take the pending re-sync with it. The app is unknown
	// here, so the re-sync itself does nothing.
	scheduled[0].fire()
	_, count = spsPendingRetry(am, 55, common.PROD)
	assert.Equal(t, 1, count, "a replaced timer leaves its successor pending")

	scheduled[1].fire()
	_, count = spsPendingRetry(am, 55, common.PROD)
	assert.Zero(t, count, "a fired re-sync is no longer pending")
}
