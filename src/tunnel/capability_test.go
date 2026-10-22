package tunnel

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reagent/common"
	"reagent/config"
	"reagent/filesystem"
	"reagent/messenger"
	"runtime"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func newCapabilityTestManager(t *testing.T) *FrpTunnelManager {
	t.Helper()
	cfg := &config.Config{
		CommandLineArguments: &config.CommandLineArguments{AgentDir: t.TempDir()},
		ReswarmConfig:        &config.ReswarmConfig{Environment: string(common.PRODUCTION)},
	}
	m, err := NewFrpTunnelManager(messenger.NewOffline(cfg), cfg)
	require.NoError(t, err)
	// One route: these tests repoint the server at loopback listeners and judge
	// that one frps. Left with the cloud's second route, a dead listener would
	// send the probe on to 127.0.0.1:443 and whatever happens to listen there.
	// Falling back between routes has its own tests (route_test.go).
	m.configBuilder.routes = nil
	return m
}

// A freshly constructed manager is "capable" (not yet definitively
// unavailable), so the Linux boot path — where an app can reconcile before
// frpc finishes logging in — is not skipped.
func TestCapabilityDefaultsToCapable(t *testing.T) {
	m := newCapabilityTestManager(t)
	assert.True(t, m.TunnelCapable(), "unknown capability must count as capable")

	cap, _ := m.Capability()
	assert.Equal(t, CapabilityUnknown, cap)
}

func TestTunnelCapableOnlyFalseWhenUnavailable(t *testing.T) {
	m := newCapabilityTestManager(t)

	m.setCapability(CapabilityStarting, nil)
	assert.True(t, m.TunnelCapable(), "starting is still capable")

	m.setCapability(CapabilityAvailable, nil)
	assert.True(t, m.TunnelCapable())

	m.setCapability(CapabilityUnavailable, errors.New("frpc gone"))
	assert.False(t, m.TunnelCapable(), "only unavailable is incapable")

	cap, lastErr := m.Capability()
	assert.Equal(t, CapabilityUnavailable, cap)
	assert.Equal(t, "frpc gone", lastErr)
}

func TestMarkUnavailable(t *testing.T) {
	m := newCapabilityTestManager(t)
	m.MarkUnavailable("tunnels are not yet supported on Windows")

	assert.False(t, m.TunnelCapable())
	_, lastErr := m.Capability()
	assert.Equal(t, "tunnels are not yet supported on Windows", lastErr)
}

// Becoming able to tunnel is the moment the agent reconciles every app's
// tunnels: those skipped while it could not, and adds that failed while frpc
// was starting, have nothing else to bring them up. Only a transition INTO
// Available may run it — the capability probe re-confirms Available every
// minute, and each confirmation must not re-sync every app.
func TestSetCapabilityRunsOnAvailableOnTransition(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newCapabilityTestManager(t)
		// Keep the capability publish off frpc's admin API: nothing listens
		// there, and the refused connection would re-supervise frpc, which
		// flips the capability behind the test's back.
		m.configBuilder.yamlConfig.WebServer = nil

		var calls atomic.Int32
		m.SetOnAvailable(func() { calls.Add(1) })

		// synctest.Wait returns once every goroutine setCapability spawned
		// has finished or is durably blocked.
		m.setCapability(CapabilityStarting, nil)
		synctest.Wait()
		assert.Zero(t, calls.Load(), "starting is not available yet")

		m.setCapability(CapabilityAvailable, nil)
		synctest.Wait()
		assert.Equal(t, int32(1), calls.Load(), "becoming available runs the callback once")

		m.setCapability(CapabilityAvailable, nil)
		synctest.Wait()
		assert.Equal(t, int32(1), calls.Load(), "staying available is not a transition")

		m.setCapability(CapabilityUnavailable, errors.New("frps unreachable"))
		synctest.Wait()
		assert.Equal(t, int32(1), calls.Load(), "becoming unavailable must not run it")

		m.setCapability(CapabilityAvailable, nil)
		synctest.Wait()
		assert.Equal(t, int32(2), calls.Load(), "coming back runs it again")
	})
}

func TestBecomingAvailableClearsLastErr(t *testing.T) {
	m := newCapabilityTestManager(t)
	m.setCapability(CapabilityUnavailable, errors.New("boom"))
	m.setCapability(CapabilityAvailable, nil)

	_, lastErr := m.Capability()
	assert.Empty(t, lastErr)
}

// ensureFrpcBinary re-acquires a missing binary via the injected callback, so
// a runtime quarantine (antivirus deleting frpc.exe) is recoverable instead of
// a permanent crash-loop.
func TestEnsureFrpcBinaryReacquiresWhenMissing(t *testing.T) {
	m := newCapabilityTestManager(t)
	frpcPath := filesystem.GetTunnelBinaryPath(m.config, "frpc") // frpc.exe on Windows

	called := false
	m.SetReacquireFrpc(func() error {
		called = true
		return os.WriteFile(frpcPath, []byte("binary"), 0755)
	})

	require.NoError(t, m.ensureFrpcBinary())
	assert.True(t, called, "re-acquire must run when the binary is missing")
	assert.FileExists(t, frpcPath)
}

func TestEnsureFrpcBinaryNoopWhenPresent(t *testing.T) {
	m := newCapabilityTestManager(t)
	frpcPath := filesystem.GetTunnelBinaryPath(m.config, "frpc") // frpc.exe on Windows
	require.NoError(t, os.WriteFile(frpcPath, []byte("binary"), 0755))

	m.SetReacquireFrpc(func() error {
		t.Fatal("re-acquire must not run when the binary is present")
		return nil
	})

	require.NoError(t, m.ensureFrpcBinary())
}

func TestEnsureFrpcBinaryFailsWhenReacquireFails(t *testing.T) {
	m := newCapabilityTestManager(t)
	m.SetReacquireFrpc(func() error { return errors.New("download blocked") })

	err := m.ensureFrpcBinary()
	require.ErrorContains(t, err, "re-acquire failed")
}

func TestEnsureFrpcBinaryFailsWithoutReacquire(t *testing.T) {
	m := newCapabilityTestManager(t)
	// no SetReacquireFrpc

	err := m.ensureFrpcBinary()
	require.ErrorContains(t, err, "no re-acquire available")
}

// Only one supervisor loop may run at a time: competing loops spawned frpc
// against frpc, and the loser's "bind: address already in use" exit marked
// tunnels unavailable even while a healthy client was up.
func TestSuperviseStartSingleFlight(t *testing.T) {
	m := newCapabilityTestManager(t)
	require.True(t, m.supervising.CompareAndSwap(false, true), "latching the supervisor flag must succeed on a fresh manager")
	defer m.supervising.Store(false)

	done := make(chan struct{})
	go func() {
		m.SuperviseStart()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SuperviseStart must return immediately while another supervisor is running")
	}

	cap, _ := m.Capability()
	assert.Equal(t, CapabilityUnknown, cap, "a skipped supervisor must not touch capability")
}

// Stop before any client was ever spawned must be a no-op, not a nil deref.
func TestStopWithoutClientIsSafe(t *testing.T) {
	m := newCapabilityTestManager(t)
	assert.NoError(t, m.Stop())
}

// reachableFrps points the manager at a live local listener, standing in for a
// running frps control port.
func reachableFrps(t *testing.T, m *FrpTunnelManager) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	m.configBuilder.yamlConfig.ServerAddr = "127.0.0.1"
	m.configBuilder.yamlConfig.ServerPort = ln.Addr().(*net.TCPAddr).Port
	plainProbe(m)
	fakeAdminAPI(t, m)
}

// fakeAdminAPI gives the manager an frpc admin API on loopback that reports no
// proxies. A test that turns the device available needs one: the state publish
// that follows asks the admin API and, finding it refused, takes frpc for dead
// and restarts it — a loop that outlives the test and writes into its TempDir
// while that is being removed ("TempDir RemoveAll cleanup: directory not
// empty"). Cleanup parks the manager unavailable, so the monitors a Start left
// running stop asking.
//
// The API is deliberately never closed. The publish starts in the background
// the moment the device turns available, and may already be past its
// "available?" check when the test returns; closing the API under it is what
// still triggered the restart, about once in a thousand runs.
func fakeAdminAPI(t *testing.T, m *FrpTunnelManager) {
	t.Helper()
	adminAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "{}")
	}))
	m.configBuilder.yamlConfig.WebServer.Port = adminAPI.Listener.Addr().(*net.TCPAddr).Port
	t.Cleanup(func() { m.MarkUnavailable("test finished") })
}

// plainProbe drops certificate verification from the manager's config, so the
// probe checks reachability alone against these plain listeners. The verified
// probe has its own tests (TestProbeCapabilityReportsUntrustedTunnelServer).
func plainProbe(m *FrpTunnelManager) {
	m.configBuilder.yamlConfig.Transport.TLS.TrustedCaFile = ""
	m.configBuilder.yamlConfig.Transport.TLS.ServerName = ""
}

// unreachableFrps points the manager at a port nothing is listening on, standing
// in for the appliance tunnel profile being switched off.
func unreachableFrps(t *testing.T, m *FrpTunnelManager) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close()) // hand the port back before anyone can use it

	m.configBuilder.yamlConfig.ServerAddr = "127.0.0.1"
	m.configBuilder.yamlConfig.ServerPort = port
	plainProbe(m)
	fakeAdminAPI(t, m) // TestUnavailableRecoversOnceFrpsComesUp turns it available
}

// The capability verdict comes from dialling frps, because frpc logs to a file
// and never tells us on stdout that it logged in.
func TestProbeCapabilityFollowsFrpsReachability(t *testing.T) {
	t.Run("reachable frps is capable", func(t *testing.T) {
		m := newCapabilityTestManager(t)
		reachableFrps(t, m)
		m.clientAlive.Store(true)

		m.probeCapability()

		cap, _ := m.Capability()
		assert.Equal(t, CapabilityAvailable, cap)
		assert.True(t, m.TunnelCapable())
	})

	t.Run("unreachable frps is not capable", func(t *testing.T) {
		m := newCapabilityTestManager(t)
		unreachableFrps(t, m)
		m.clientAlive.Store(true)

		m.probeCapability()

		cap, lastErr := m.Capability()
		assert.Equal(t, CapabilityUnavailable, cap)
		assert.False(t, m.TunnelCapable())
		assert.Contains(t, lastErr, "frps unreachable")
	})
}

// A start failure (missing/quarantined frpc) owns the capability. A frps that
// merely happens to be reachable must not paper over it.
func TestProbeCapabilityIgnoredWithoutRunningClient(t *testing.T) {
	m := newCapabilityTestManager(t)
	reachableFrps(t, m)
	m.MarkUnavailable("frpc binary quarantined")

	m.probeCapability() // clientAlive is false

	cap, lastErr := m.Capability()
	assert.Equal(t, CapabilityUnavailable, cap, "a reachable frps must not revive a device whose frpc cannot run")
	assert.Equal(t, "frpc binary quarantined", lastErr)
}

// Regression (found in production): frpc logs to /var/log/frpc.log, so
// "login to server success" never reaches the stdout the agent scans and the
// login ack never arrives. Treating that silence as failure marked every healthy
// device "tunnel disabled". A working tunnel must stay capable.
func TestStartStaysCapableWhenLoginAckNeverArrivesButFrpsIsUp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake frpc is a shell script")
	}

	m := newCapabilityTestManager(t)
	m.loginDeadline = 200 * time.Millisecond
	reachableFrps(t, m)

	// frpc that runs happily but says nothing on stdout — the production case.
	fake := "#!/bin/sh\nsleep 2\n"
	frpcPath := filesystem.GetTunnelBinaryPath(m.config, "frpc") // frpc.exe on Windows
	require.NoError(t, os.WriteFile(frpcPath, []byte(fake), 0o755))
	t.Cleanup(func() { _ = m.Stop() })

	require.NoError(t, m.Start())

	cap, _ := m.Capability()
	assert.Equal(t, CapabilityAvailable, cap, "a silent frpc against a reachable frps must NOT be reported unavailable")
	assert.True(t, m.TunnelCapable())
}

// The appliance case the badge exists for: frpc is up and retrying (loginFailExit
// = false keeps it alive), but the tunnel service is off, so nothing accepts a
// connection and the device must report itself unavailable.
func TestStartReportsUnavailableWhenFrpsIsDown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake frpc is a shell script")
	}

	m := newCapabilityTestManager(t)
	m.loginDeadline = 200 * time.Millisecond
	unreachableFrps(t, m)

	fake := "#!/bin/sh\necho '[E] connect to server error: connection refused'\nsleep 2\n"
	frpcPath := filesystem.GetTunnelBinaryPath(m.config, "frpc") // frpc.exe on Windows
	require.NoError(t, os.WriteFile(frpcPath, []byte(fake), 0o755))
	t.Cleanup(func() { _ = m.Stop() })

	require.NoError(t, m.Start(), "Start must succeed and leave frpc retrying, not error out")

	cap, _ := m.Capability()
	assert.Equal(t, CapabilityUnavailable, cap)
	assert.False(t, m.TunnelCapable())
}

// The verdict is never permanent: once frps comes up, the next probe clears it.
// This is what saves an appliance where the agent wins the boot race against the
// frps container.
func TestUnavailableRecoversOnceFrpsComesUp(t *testing.T) {
	m := newCapabilityTestManager(t)
	unreachableFrps(t, m)
	m.clientAlive.Store(true)

	m.probeCapability()
	cap, _ := m.Capability()
	require.Equal(t, CapabilityUnavailable, cap)

	// frps starts listening on the very port that was refused a moment ago.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", m.configBuilder.yamlConfig.ServerPort))
	require.NoError(t, err)
	defer ln.Close()

	m.probeCapability()

	cap, lastErr := m.Capability()
	assert.Equal(t, CapabilityAvailable, cap, "a recovered frps must clear the badge without an agent restart")
	assert.Empty(t, lastErr)
}

// End-to-end reproduction of the production failure with a real spawned
// process: the admin port is taken by another socket (in the field: any
// outbound localhost connection, a stale frpc, a Docker publish), and frpc —
// like frp 0.69 when its webServer cannot bind — prints a bare
// "bind: address already in use" and exits before logging in. The supervisor
// must then persist a *different* admin port before the next attempt;
// retrying the dead port forever was what kept appliance tunnels down until a
// manual agent restart.
func TestSuperviseStartRepicksAdminPortAfterPreLoginExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake frpc is a shell script")
	}

	m := newCapabilityTestManager(t)

	fake := "#!/bin/sh\necho 'listen tcp 127.0.0.1:42567: bind: address already in use'\nexit 1\n"
	frpcPath := filesystem.GetTunnelBinaryPath(m.config, "frpc") // frpc.exe on Windows
	require.NoError(t, os.WriteFile(frpcPath, []byte(fake), 0o755))

	initialPort, err := m.configBuilder.GetAdminPort()
	require.NoError(t, err)

	// Occupy the picked admin port, exactly like the socket that breaks frpc
	// in the field — the re-pick must not hand it out again.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", initialPort))
	require.NoError(t, err)
	defer ln.Close()

	go m.SuperviseStart()

	require.Eventually(t, func() bool {
		raw, readErr := os.ReadFile(m.configBuilder.ConfigPath)
		if readErr != nil {
			return false
		}
		var onDisk FrpcYamlConfig
		if yaml.Unmarshal(raw, &onDisk) != nil || onDisk.WebServer == nil {
			return false
		}
		return onDisk.WebServer.Port != initialPort && onDisk.WebServer.Port >= adminPortScanStart
	}, 10*time.Second, 100*time.Millisecond,
		"supervisor must persist a re-picked admin port after a pre-login exit")
}
