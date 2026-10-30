//go:build integration

// Additional end-to-end tunnel tests that drive a real frpc process against a
// live frps server. They are excluded from `just test` and run only under
// `just test-integration` (-tags integration). See src/TESTING.md.
//
// Unlike the package's existing setupTunnel()/TestAddTunnel (which call
// log.Fatal on any setup error), every test here first probes whether the frps
// server is actually reachable from this environment and t.Skip()s cleanly when
// it is not — so the integration suite stays green on machines (CI, laptops)
// that have no route to frps and no writable frpc log path.
package tunnel

import (
	"net"
	"os"
	"path/filepath"
	"reagent/common"
	"reagent/config"
	"reagent/errdefs"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// frpsServerPort is the frps listen port for production tunnels (see
// initialize() in frp.go, which sets ServerPort: 7000 for non-local addresses).
const frpsServerPort = 7000

// frpcLogPath mirrors the hard-coded log destination initialize() bakes into the
// generated frpc.yaml. If frpc cannot open it for writing, Start() (and thus
// setupTunnel()) would fatal, so we treat an unwritable path as "resource absent".
const frpcLogPath = "/var/log/frpc.log"

// resolveFrpsAddr returns the frps server hostname the production config builder
// would target, without starting any frpc process. Honours FRPS_SERVER_ADDR for
// environments that point at a non-default tunnel server.
func resolveFrpsAddr(t *testing.T) string {
	t.Helper()

	if override := os.Getenv("FRPS_SERVER_ADDR"); override != "" {
		return override
	}

	cfg := &config.Config{
		CommandLineArguments: &config.CommandLineArguments{AgentDir: t.TempDir()},
		ReswarmConfig:        &config.ReswarmConfig{Environment: string(common.PRODUCTION)},
	}
	// NewTunnelConfigBuilder only writes a frpc.yaml into AgentDir and resolves
	// the server address — it does not spawn frpc or touch the network.
	builder := NewTunnelConfigBuilder(cfg)
	return builder.BaseTunnelURL
}

// frpsServerPortNum returns the frps port, honouring FRPS_SERVER_PORT.
func frpsServerPortNum(t *testing.T) int {
	t.Helper()
	if override := os.Getenv("FRPS_SERVER_PORT"); override != "" {
		port, err := strconv.Atoi(override)
		require.NoErrorf(t, err, "invalid FRPS_SERVER_PORT %q", override)
		return port
	}
	return frpsServerPort
}

// requireFrpsReachable skips the test unless the prerequisites for a real frpc
// run are all present in this environment:
//   - the frpc binary exists in the resolved agent dir, and
//   - the frpc log path is writable (otherwise Start() fatals), and
//   - the frps server accepts a TCP connection within a short timeout.
//
// On success it returns nothing; the caller may then safely use setupTunnel().
func requireFrpsReachable(t *testing.T) {
	t.Helper()

	// frpc binary must be available where setupTunnel() expects it.
	frpcBinary := filepath.Join(getTestAgentDir(), "frpc")
	if _, err := os.Stat(frpcBinary); err != nil {
		t.Skipf("skipping: frpc binary not found at %s (run `just build-frpc`): %v", frpcBinary, err)
	}

	// The generated config logs to a fixed path; if we can't open it for append,
	// frpc Start() would fail and setupTunnel() would call log.Fatal. Probe it
	// here and skip instead of crashing the suite.
	f, err := os.OpenFile(frpcLogPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		t.Skipf("skipping: frpc log path %s is not writable (need root or a pre-created file): %v", frpcLogPath, err)
	}
	_ = f.Close()

	addr := resolveFrpsAddr(t)
	if addr == "" || addr == "localhost" || addr == "127.0.0.1" {
		t.Skipf("skipping: no production frps server configured (resolved address %q)", addr)
	}

	hostPort := net.JoinHostPort(addr, strconv.Itoa(frpsServerPortNum(t)))
	conn, err := net.DialTimeout("tcp", hostPort, 3*time.Second)
	if err != nil {
		t.Skipf("skipping: frps server %s not reachable from this environment: %v", hostPort, err)
	}
	_ = conn.Close()

	t.Logf("frps server %s reachable; running live tunnel integration test", hostPort)
}

// waitForRunning polls Status until the tunnel reports "running" or the deadline
// elapses. Returns the last observed status. Fails the test on a status error or
// on a frps-reported tunnel error.
func waitForRunning(t *testing.T, tm *FrpTunnelManager, tunnelID string, timeout time.Duration) TunnelStatus {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var last TunnelStatus
	for time.Now().Before(deadline) {
		status, err := tm.Status(tunnelID)
		if err != nil {
			// ErrNotFound just means frpc hasn't surfaced the proxy yet; keep polling.
			time.Sleep(200 * time.Millisecond)
			continue
		}
		last = status
		require.Emptyf(t, status.Error, "tunnel %s reported an error: %s", tunnelID, status.Error)
		if status.Status == "running" {
			return status
		}
		time.Sleep(200 * time.Millisecond)
	}
	return last
}

// TestTunnelLifecycleAgainstLiveServer exercises Add -> Get -> Status -> Remove
// of an HTTP tunnel against the real frps server. HTTP is used deliberately:
// AddTunnel short-circuits the WAMP reserveRemotePort() call for HTTP/HTTPS, so
// no real backend session is required (the test uses an Offline messenger, like
// the existing TestAddTunnel).
func TestTunnelLifecycleAgainstLiveServer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tunnel integration test - requires connection to frps server")
	}
	requireFrpsReachable(t)

	tm := setupTunnel()
	t.Cleanup(func() { _ = tm.Stop() })

	require.NoError(t, tm.Reset(), "reset should clear any pre-existing tunnel config")

	subdomain := CreateSubdomain(HTTP, 1, "LifecycleTest", 18080, common.PROD)
	want := TunnelConfig{Protocol: HTTP, Subdomain: subdomain, LocalPort: 18080, RemotePort: 0}
	tunnelID := CreateTunnelID(subdomain, string(HTTP))

	// --- Add ---
	got, err := tm.AddTunnel(want)
	require.NoError(t, err, "AddTunnel against live frps should succeed")
	assert.Equal(t, want.Subdomain, got.Subdomain)
	assert.Equal(t, want.Protocol, got.Protocol)
	assert.Equal(t, want.LocalPort, got.LocalPort)

	// Ensure the tunnel is torn down even if a later assertion fails.
	t.Cleanup(func() { _ = tm.RemoveTunnel(got) })

	// --- Get (in-memory active tunnel registry) ---
	active := tm.Get(tunnelID)
	require.NotNil(t, active, "Get must return the just-added tunnel")
	assert.Equal(t, want.Subdomain, active.Config.Subdomain)
	assert.Equal(t, want.Protocol, active.Config.Protocol)
	assert.Empty(t, active.Error, "freshly added tunnel should carry no error")

	// --- Status (from the live frpc admin API) ---
	status := waitForRunning(t, tm, tunnelID, 20*time.Second)
	require.Equal(t, "running", status.Status, "tunnel should reach running state on the live server")
	assert.Equal(t, tunnelID, status.Name)
	assert.Equal(t, Protocol(HTTP), status.Protocol)

	// --- GetTunnelConfig round-trips the persisted proxy config ---
	configs, err := tm.GetTunnelConfig()
	require.NoError(t, err)
	require.Len(t, configs, 1, "exactly one tunnel should be configured")
	assert.Equal(t, want.Subdomain, configs[0].Subdomain)
	assert.Equal(t, want.Protocol, configs[0].Protocol)
	assert.Equal(t, want.LocalPort, configs[0].LocalPort)

	// --- GetStateById reflects the running tunnel ---
	state, err := tm.GetStateById(tunnelID)
	require.NoError(t, err, "GetStateById should find the running tunnel")
	assert.True(t, state.Active, "running tunnel should be reported active")
	assert.False(t, state.Error, "running tunnel should not be in error")
	assert.NotEmpty(t, state.URL, "running tunnel should expose a URL")

	// --- Remove ---
	require.NoError(t, tm.RemoveTunnel(got), "RemoveTunnel should succeed")
	assert.Nil(t, tm.Get(tunnelID), "Get must return nil after removal")

	// After removal the config no longer lists the proxy.
	configsAfter, err := tm.GetTunnelConfig()
	require.NoError(t, err)
	assert.Empty(t, configsAfter, "no tunnels should remain after removal")
}

// TestStatusUnknownTunnelAgainstLiveServer asserts that querying a tunnel that
// was never added returns ErrNotFound from the live frpc admin API (real code
// path: AllStatus -> filter by name).
func TestStatusUnknownTunnelAgainstLiveServer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping tunnel integration test - requires connection to frps server")
	}
	requireFrpsReachable(t)

	tm := setupTunnel()
	t.Cleanup(func() { _ = tm.Stop() })

	require.NoError(t, tm.Reset())

	unknownID := CreateTunnelID(CreateSubdomain(HTTP, 999, "no-such-app", 65000, common.PROD), string(HTTP))

	_, err := tm.Status(unknownID)
	assert.ErrorIs(t, err, errdefs.ErrNotFound, "Status of an unknown tunnel should be not-found")

	assert.Nil(t, tm.Get(unknownID), "Get of an unknown tunnel must be nil")
}
