package tunnel

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reagent/common"
	"reagent/config"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// A cloud device gets the tunnel server's 443 websocket ingress as a second
// route and starts on 7000. Appliances and dev stacks keep the one route they
// always had.
func TestCloudDevicesGetTheWssFallbackRoute(t *testing.T) {
	cloudRoutes := []tunnelRoute{{Port: 7000}, {Port: 443, Protocol: "wss"}}

	tests := []struct {
		name       string
		reswarm    *config.ReswarmConfig
		wantRoutes []tunnelRoute
	}{
		{
			name: "cloud device",
			reswarm: &config.ReswarmConfig{
				DeviceEndpointURL: "wss://cbw.ironflock.com/ws-re-dev",
				Environment:       string(common.PRODUCTION),
			},
			wantRoutes: cloudRoutes,
		},
		{
			name:       "cloud device on the environment default",
			reswarm:    &config.ReswarmConfig{Environment: string(common.PRODUCTION)},
			wantRoutes: cloudRoutes,
		},
		{
			name: "domain-mode appliance",
			reswarm: &config.ReswarmConfig{
				ApplianceDomain:   "tls-sf015.corp.example.com",
				DeviceEndpointURL: "wss://ws.tls-sf015.corp.example.com/ws-re-dev",
				Environment:       string(common.PRODUCTION),
			},
			wantRoutes: []tunnelRoute{{Port: 443, Protocol: "wss"}},
		},
		{
			name: "plain-mode appliance",
			reswarm: &config.ReswarmConfig{
				ApplianceDomain:   "192.168.0.21.nip.io",
				DeviceEndpointURL: "ws://192.168.0.21:18080/ws-re-dev",
				Environment:       string(common.PRODUCTION),
			},
			wantRoutes: []tunnelRoute{{Port: 7000}},
		},
		{
			name:       "local dev stack",
			reswarm:    &config.ReswarmConfig{DeviceEndpointURL: "ws://localhost:8080/ws-re-dev", Environment: string(common.LOCAL)},
			wantRoutes: []tunnelRoute{{Port: 7400}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearProxyEnv(t)
			builder := NewTunnelConfigBuilder(builderConfig(t, tt.reswarm))
			assert.Equal(t, tt.wantRoutes, builder.routes)
			assert.Equal(t, tt.wantRoutes[0], builder.route(), "a device starts on its preferred route")
		})
	}
}

// The wss route rides 443, so HTTPS_PROXY governs it — which is what gets a
// device behind a 443-only corporate proxy a tunnel at all — while the tcp
// route keeps frpc's own lowercase http_proxy rule.
func TestWssFallbackRouteResolvesHTTPSProxy(t *testing.T) {
	cloud := &config.ReswarmConfig{
		DeviceEndpointURL: "wss://cbw.ironflock.com/ws-re-dev",
		Environment:       string(common.PRODUCTION),
	}

	t.Run("HTTPS_PROXY is the wss route's proxy", func(t *testing.T) {
		clearProxyEnv(t)
		t.Setenv("HTTPS_PROXY", "http://user:secret@proxy.corp.example.com:3128")
		builder := NewTunnelConfigBuilder(builderConfig(t, cloud))
		require.Len(t, builder.routes, 2)
		assert.Empty(t, builder.routes[0].ProxyURL)
		assert.Equal(t, "http://user:secret@proxy.corp.example.com:3128", builder.routes[1].ProxyURL)
		assert.Equal(t, "wss/443 via proxy", builder.routes[1].String(), "logs name the proxy, never its credentials")
	})

	t.Run("NO_PROXY exempts it", func(t *testing.T) {
		clearProxyEnv(t)
		t.Setenv("HTTPS_PROXY", "http://proxy.corp.example.com:3128")
		t.Setenv("NO_PROXY", "app.ironflock.com")
		builder := NewTunnelConfigBuilder(builderConfig(t, cloud))
		require.Len(t, builder.routes, 2)
		assert.Empty(t, builder.routes[1].ProxyURL)
	})
}

// Start probes the tcp route first; when it is out of reach and the wss
// route works, frpc is started on wss, and the device reports it can tunnel.
func TestStartFallsBackToWssWhenTcpRouteIsUnreachable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake frpc is a shell script")
	}

	m, cert := routeTestManager(t)
	wssPort, upgrades := wssIngress(t, cert, true)
	tcp := tunnelRoute{Port: closedPort(t)}
	wss := tunnelRoute{Port: wssPort, Protocol: "wss"}
	useRoutes(m, tcp, wss)
	silentFrpc(t, m)

	require.NoError(t, m.Start())

	assert.Equal(t, wss, m.configBuilder.route())
	written := readFrpcYAML(t, m)
	assert.Equal(t, wssPort, written.ServerPort, "frpc must read the route it is meant to use")
	assert.Equal(t, "wss", written.Transport.Protocol)
	assert.Equal(t, "app.ironflock.com", written.Transport.TLS.ServerName, "the wss route verifies the same name")

	capability, lastErr := m.Capability()
	assert.Equal(t, CapabilityAvailable, capability, lastErr)
	assert.GreaterOrEqual(t, upgrades.Load(), int32(1), "the wss route must have been proven by a websocket upgrade")
}

// The configured route is kept while it works. A device on 7000 never dials
// 443, and a device that fell back to 443 does not flip back on a restart.
func TestSelectRouteKeepsAWorkingRoute(t *testing.T) {
	t.Run("tcp route works, wss is never dialed", func(t *testing.T) {
		m, cert := routeTestManager(t)
		tcpListener, _ := tlsHandshakeRecorder(t, cert)
		wssPort, upgrades := wssIngress(t, cert, true)
		tcp := tunnelRoute{Port: tcpListener.Addr().(*net.TCPAddr).Port}
		useRoutes(m, tcp, tunnelRoute{Port: wssPort, Protocol: "wss"})

		m.selectRoute()

		assert.Equal(t, tcp, m.configBuilder.route())
		assert.Zero(t, upgrades.Load(), "a working tcp route must not cost a dial to 443")
	})

	t.Run("a device on the wss route stays there while it works", func(t *testing.T) {
		m, cert := routeTestManager(t)
		tcpListener, _ := tlsHandshakeRecorder(t, cert)
		wssPort, _ := wssIngress(t, cert, true)
		wss := tunnelRoute{Port: wssPort, Protocol: "wss"}
		useRoutes(m, tunnelRoute{Port: tcpListener.Addr().(*net.TCPAddr).Port}, wss)
		m.configBuilder.useRoute(wss)

		m.selectRoute()

		assert.Equal(t, wss, m.configBuilder.route())
	})
}

// With no route reaching frps the config stays where it is, and the device
// reports why it cannot tunnel.
func TestNoWorkingRouteLeavesTheConfigAlone(t *testing.T) {
	m, _ := routeTestManager(t)
	tcp := tunnelRoute{Port: closedPort(t)}
	useRoutes(m, tcp, tunnelRoute{Port: closedPort(t), Protocol: "wss"})

	m.selectRoute()
	assert.Equal(t, tcp, m.configBuilder.route())

	m.clientAlive.Store(true)
	m.probeCapability()
	capability, lastErr := m.Capability()
	assert.Equal(t, CapabilityUnavailable, capability)
	assert.Contains(t, lastErr, "frps unreachable")
}

// A 443 that completes TLS but does not relay frp's websocket path to frps
// (FRPS_WS_INGRESS=false, or an ingress older than the relay) is not a route:
// moving a device there would trade a dead port for a dead path.
func TestWssRouteNeedsTheWebsocketPath(t *testing.T) {
	m, cert := routeTestManager(t)
	wssPort, _ := wssIngress(t, cert, false)
	tcp := tunnelRoute{Port: closedPort(t)}
	wss := tunnelRoute{Port: wssPort, Protocol: "wss"}
	useRoutes(m, tcp, wss)

	err := m.routeReachable(wss, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "instead of opening frp's websocket")

	m.selectRoute()
	assert.Equal(t, tcp, m.configBuilder.route())
}

// The capability probe runs every minute and must not open frp's websocket:
// frps logs each one that closes unused as a warning. TLS is its proof that a
// wss route still reaches the tunnel server; the websocket is asked for only
// when a client is moved onto the route or started on it.
func TestCapabilityProbeOnWssRouteStopsAtTLS(t *testing.T) {
	m, cert := routeTestManager(t)
	wssPort, upgrades := wssIngress(t, cert, true)
	wss := tunnelRoute{Port: wssPort, Protocol: "wss"}
	useRoutes(m, tunnelRoute{Port: closedPort(t)}, wss)
	m.configBuilder.useRoute(wss)
	m.clientAlive.Store(true)

	m.probeCapability()

	capability, lastErr := m.Capability()
	assert.Equal(t, CapabilityAvailable, capability, lastErr)
	assert.Zero(t, upgrades.Load())
}

// The case the fallback exists for: a corporate proxy that allows CONNECT to
// 443 only. The tcp route's port is open to a direct dial, which is exactly
// why the probe must go through the proxy — dialing direct, it would call the
// tcp route reachable while frpc, sent through the proxy, gets a 403.
func TestRoutesAreProbedThroughTheirProxy(t *testing.T) {
	m, cert := routeTestManager(t)
	tcpListener, tcpHandshakes := tlsHandshakeRecorder(t, cert)
	wssPort, upgrades := wssIngress(t, cert, true)
	proxy := newConnectProxy(t, wssPort)

	tcpPort := tcpListener.Addr().(*net.TCPAddr).Port
	tcp := tunnelRoute{Port: tcpPort, ProxyURL: proxy.url}
	wss := tunnelRoute{Port: wssPort, Protocol: "wss", ProxyURL: proxy.url}
	useRoutes(m, tcp, wss)

	m.selectRoute()

	assert.Equal(t, wss, m.configBuilder.route())
	assert.Equal(t, []string{"127.0.0.1:" + strconv.Itoa(tcpPort), "127.0.0.1:" + strconv.Itoa(wssPort)}, proxy.connects(),
		"both routes must be asked of the proxy, with its credentials")
	assert.Equal(t, int32(1), upgrades.Load())
	select {
	case <-tcpHandshakes:
		t.Fatal("the probe went around the proxy to the tcp route")
	default:
	}
}

// A running client whose route stops reaching frps while another route
// works is restarted onto that route, not reported unable to tunnel.
func TestProbeCapabilityMovesARunningClientToAWorkingRoute(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake frpc is a shell script")
	}

	m, cert := routeTestManager(t)
	wssPort, _ := wssIngress(t, cert, true)
	wss := tunnelRoute{Port: wssPort, Protocol: "wss"}
	useRoutes(m, tunnelRoute{Port: closedPort(t)}, wss)
	silentFrpc(t, m)
	m.clientAlive.Store(true) // a client started on the tcp route while it still worked

	m.probeCapability()

	assert.Eventually(t, func() bool {
		capability, _ := m.Capability()
		return m.configBuilder.route() == wss && capability == CapabilityAvailable
	}, 5*time.Second, 20*time.Millisecond, "the client must be restarted on the wss route")
}

// A proxy scheme frpc speaks but the probe does not (ntlm) proves nothing
// either way, so the route counts as reachable, as every route did before the
// probe went through proxies.
func TestUnprobeableProxyCountsAsReachable(t *testing.T) {
	_, err := dialTunnelServer("app.ironflock.com:7000", "ntlm://proxy.corp.example.com:8080")
	require.ErrorIs(t, err, errUnprobeableProxy)

	m, _ := routeTestManager(t)
	assert.NoError(t, m.routeReachable(tunnelRoute{Port: 7000, ProxyURL: "ntlm://proxy.corp.example.com:8080"}, true))
}

// routeTestRoot is the CA routeTestManager's devices trust, as DER. It joins
// the host store through tunnelHostRoots, which is wrapped once at init and
// never reassigned: Start rebuilds the bundle from the host store, so a CA
// appended to the file would not survive it, and the monitors earlier tests
// leave running read tunnelHostRoots whenever they rebuild a bundle.
var routeTestRoot atomic.Pointer[[]byte]

func init() {
	hostRoots := tunnelHostRoots
	tunnelHostRoots = func() ([][]byte, error) {
		roots, err := hostRoots()
		if root := routeTestRoot.Load(); root != nil {
			return append(roots, *root), nil
		}
		return roots, err
	}
}

// routeTestManager is a cloud device whose tunnel server is on loopback. The
// returned certificate is issued for app.ironflock.com by a test CA the
// device's tunnel CA bundle holds, for every listener the test serves. The
// caller sets the routes (useRoutes).
func routeTestManager(t *testing.T) (*FrpTunnelManager, tls.Certificate) {
	t.Helper()
	ca, caKey := testTunnelCA(t, "Route Test Root")
	root := ca.Raw
	routeTestRoot.Store(&root)
	t.Cleanup(func() { routeTestRoot.Store(nil) })

	m := newCapabilityTestManager(t)
	require.Equal(t, "app.ironflock.com", m.configBuilder.yamlConfig.Transport.TLS.ServerName)
	require.NotEmpty(t, m.configBuilder.yamlConfig.Transport.TLS.TrustedCaFile)
	m.configBuilder.yamlConfig.ServerAddr = "127.0.0.1"
	m.loginDeadline = 200 * time.Millisecond
	fakeAdminAPI(t, m)

	return m, testLeaf(t, ca, caKey, "app.ironflock.com")
}

// useRoutes gives the device routes, most preferred first, and puts its
// config on the first.
func useRoutes(m *FrpTunnelManager, routes ...tunnelRoute) {
	m.configBuilder.routes = routes
	m.configBuilder.useRoute(routes[0])
}

// silentFrpc installs an frpc that runs and says nothing on stdout, like the
// real one logging to its file. exec, so that killing it closes its stdout
// at once instead of when an orphaned sleep exits. Registered after
// routeTestManager's fakeAdminAPI, its Stop runs before that cleanup.
func silentFrpc(t *testing.T, m *FrpTunnelManager) {
	t.Helper()
	frpcPath := filepath.Join(m.config.CommandLineArguments.AgentDir, "frpc")
	require.NoError(t, os.WriteFile(frpcPath, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755))
	t.Cleanup(func() { _ = m.Stop() })
}

func readFrpcYAML(t *testing.T, m *FrpTunnelManager) FrpcYamlConfig {
	t.Helper()
	raw, err := os.ReadFile(m.configBuilder.ConfigPath)
	require.NoError(t, err)
	var written FrpcYamlConfig
	require.NoError(t, yaml.Unmarshal(raw, &written))
	require.NotNil(t, written.Transport)
	require.NotNil(t, written.Transport.TLS)
	return written
}

// closedPort returns a loopback port nothing listens on.
func closedPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())
	return port
}

// wssIngress serves cert on loopback the way the tunnel server's 443 listener
// and the frps behind it do for frpc: frp's websocket path upgrades, and only
// for a request with an Origin (frps's golang.org/x/net/websocket handler
// refuses the rest with 403). relay=false stands in for a 443 that does not
// relay the path to frps. The counter is the number of upgrades granted.
func wssIngress(t *testing.T, cert tls.Certificate, relay bool) (int, *atomic.Int32) {
	t.Helper()
	upgrades := &atomic.Int32{}

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The raw request target, not the decoded r.URL.Path: frps matches the
		// bytes "GET /~!frp" and drops a percent-encoded /~%21frp, which the
		// tunnel server's ingress — matching on the decoded path — would
		// still pass along.
		if !relay || r.RequestURI != frpWebsocketPath {
			http.NotFound(w, r)
			return
		}
		// The User-Agent check pins the probe to frpc's request, which has
		// none: an inspecting proxy can filter on it.
		if r.Header.Get("Origin") == "" || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || r.UserAgent() != "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}

		conn, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		upgrades.Add(1)
		_, _ = buffered.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = buffered.Flush()
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	server.StartTLS()
	t.Cleanup(server.Close)

	return server.Listener.Addr().(*net.TCPAddr).Port, upgrades
}

// connectProxy is an HTTP CONNECT proxy on loopback with the ACL of a
// corporate proxy: it tunnels to one port only and answers 403 for the rest.
// It requires the basic credentials in its url, and the user agent frpc's
// dialer (golib) sends — proxies filter on it.
type connectProxy struct {
	url string

	mu      sync.Mutex
	targets []string
}

func newConnectProxy(t *testing.T, allowedPort int) *connectProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	proxy := &connectProxy{url: "http://user:secret@" + listener.Addr().String()}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go proxy.serve(conn, strconv.Itoa(allowedPort))
		}
	}()
	return proxy
}

func (proxy *connectProxy) serve(conn net.Conn, allowedPort string) {
	defer conn.Close()

	reader := bufio.NewReader(conn)
	request, err := http.ReadRequest(reader)
	if err != nil || request.Method != http.MethodConnect {
		return
	}

	if request.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("user:secret")) {
		_, _ = io.WriteString(conn, "HTTP/1.1 407 Proxy Authentication Required\r\n\r\n")
		return
	}
	if request.UserAgent() != "Mozilla/5.0" {
		_, _ = io.WriteString(conn, "HTTP/1.1 403 Forbidden\r\n\r\n")
		return
	}
	proxy.mu.Lock()
	proxy.targets = append(proxy.targets, request.Host)
	proxy.mu.Unlock()

	if _, port, _ := net.SplitHostPort(request.Host); port != allowedPort {
		_, _ = io.WriteString(conn, "HTTP/1.1 403 Forbidden\r\n\r\n")
		return
	}

	upstream, err := net.Dial("tcp", request.Host)
	if err != nil {
		_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer upstream.Close()

	_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n")
	go func() { _, _ = io.Copy(upstream, reader) }()
	_, _ = io.Copy(conn, upstream)
}

// connects lists the CONNECT targets the proxy was asked for with valid
// credentials, in order.
func (proxy *connectProxy) connects() []string {
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	return append([]string(nil), proxy.targets...)
}
