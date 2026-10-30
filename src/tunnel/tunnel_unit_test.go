// Unit tests for the frps-free pure helpers in the tunnel package. These do not
// start frpc and need no live frps server, so they run under `just test` (no
// build tag). The frps-dependent end-to-end test lives in tunnel_test.go
// behind //go:build integration.
package tunnel

import (
	"errors"
	"fmt"
	"net"
	"os"
	"reagent/common"
	"reagent/config"
	"reagent/messenger"
	"reagent/messenger/topics"
	"reagent/testutil/fakes"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// frp defaults loginFailExit to true, so the intended false has to actually be
// written: frpc must retry a frps that is temporarily unreachable rather than
// exiting on the first failed login and taking tunnels down with it.
func TestConfigBuilderPersistsLoginFailExit(t *testing.T) {
	cfg := builderConfig(t, &config.ReswarmConfig{Environment: string(common.PRODUCTION)})
	builder := NewTunnelConfigBuilder(cfg)

	raw, err := os.ReadFile(builder.ConfigPath)
	require.NoError(t, err)

	assert.Contains(t, string(raw), "loginFailExit: false")
}

// builderConfig returns a config whose AgentDir points at a writable temp dir
// so that initialize()/SaveConfig() can persist frpc.yaml without touching a
// fixed path. Only the fields the config builder reads are populated.
func builderConfig(t *testing.T, reswarm *config.ReswarmConfig) *config.Config {
	t.Helper()
	return &config.Config{
		CommandLineArguments: &config.CommandLineArguments{AgentDir: t.TempDir()},
		ReswarmConfig:        reswarm,
	}
}

func TestCreateTunnelID(t *testing.T) {
	tests := []struct {
		name      string
		subdomain string
		protocol  string
		expected  string
	}{
		{name: "http", subdomain: "9-myapp-8080", protocol: "http", expected: "9-myapp-8080-http"},
		{name: "tcp", subdomain: "9-myapp-22", protocol: "tcp", expected: "9-myapp-22-tcp"},
		{name: "empty subdomain", subdomain: "", protocol: "udp", expected: "-udp"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, CreateTunnelID(tt.subdomain, tt.protocol))
		})
	}
}

func TestCreateSubdomain(t *testing.T) {
	tests := []struct {
		name      string
		protocol  Protocol
		deviceKey uint64
		appName   string
		localPort uint64
		expected  string
	}{
		{
			name:      "http lowercases app name",
			protocol:  HTTP,
			deviceKey: 9,
			appName:   "MyApp",
			localPort: 8080,
			expected:  "9-myapp-8080",
		},
		{
			name:      "tcp keeps base form",
			protocol:  TCP,
			deviceKey: 42,
			appName:   "svc",
			localPort: 22,
			expected:  "42-svc-22",
		},
		{
			name:      "https gets secure prefix",
			protocol:  HTTPS,
			deviceKey: 7,
			appName:   "Secure-App",
			localPort: 443,
			expected:  "secure-7-secure-app-443",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CreateSubdomain(tt.protocol, tt.deviceKey, tt.appName, tt.localPort, common.PROD)
			assert.Equal(t, tt.expected, got)
		})
	}
}

// CreateSubdomain output fed into CreateTunnelID must round-trip the app name
// back out via the subdomainRegex used by GetTunnelConfig.
func TestCreateSubdomainParsableByGetTunnelConfig(t *testing.T) {
	cfg := builderConfig(t, &config.ReswarmConfig{Environment: string(common.PRODUCTION)})
	builder := NewTunnelConfigBuilder(cfg)

	subdomain := CreateSubdomain(HTTP, 9, "MyApp", 8080, common.PROD)
	builder.AddTunnelConfig(TunnelConfig{
		Subdomain: subdomain,
		Protocol:  HTTP,
		LocalPort: 8080,
	})

	configs, err := builder.GetTunnelConfig()
	require.NoError(t, err)
	require.Len(t, configs, 1)
	assert.Equal(t, "myapp", configs[0].AppName)
}

func TestProtocolConstants(t *testing.T) {
	assert.Equal(t, Protocol("tcp"), TCP)
	assert.Equal(t, Protocol("udp"), UDP)
	assert.Equal(t, Protocol("http"), HTTP)
	assert.Equal(t, Protocol("https"), HTTPS)
}

func TestBuildURL(t *testing.T) {
	// buildURL only reads configBuilder.BaseTunnelURL, so we can construct the
	// manager directly with a hand-built builder — no frpc process involved.
	frpTm := &FrpTunnelManager{
		configBuilder: TunnelConfigBuilder{BaseTunnelURL: "app.ironflock.com"},
	}

	tests := []struct {
		name       string
		protocol   Protocol
		subdomain  string
		remotePort uint64
		expected   string
	}{
		{
			name:      "http always upgrades to https without port",
			protocol:  HTTP,
			subdomain: "9-myapp-8080",
			expected:  "https://9-myapp-8080.app.ironflock.com",
		},
		{
			name:      "https keeps scheme without port",
			protocol:  HTTPS,
			subdomain: "secure-9-myapp-443",
			expected:  "https://secure-9-myapp-443.app.ironflock.com",
		},
		{
			name:       "http ignores remote port",
			protocol:   HTTP,
			subdomain:  "9-myapp-8080",
			remotePort: 12345,
			expected:   "https://9-myapp-8080.app.ironflock.com",
		},
		{
			name:       "tcp includes remote port",
			protocol:   TCP,
			subdomain:  "9-svc-22",
			remotePort: 30022,
			expected:   "tcp://9-svc-22.app.ironflock.com:30022",
		},
		{
			name:      "tcp without remote port omits port",
			protocol:  TCP,
			subdomain: "9-svc-22",
			expected:  "tcp://9-svc-22.app.ironflock.com",
		},
		{
			name:       "udp includes remote port",
			protocol:   UDP,
			subdomain:  "9-svc-53",
			remotePort: 30053,
			expected:   "udp://9-svc-53.app.ironflock.com:30053",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := frpTm.buildURL(tt.protocol, tt.subdomain, tt.remotePort)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestInterfaceToPortForwardRule(t *testing.T) {
	t.Run("defaults empty protocol to http", func(t *testing.T) {
		dat := []interface{}{
			map[string]interface{}{
				"name": "web",
				"port": float64(8080),
			},
		}

		rules, err := InterfaceToPortForwardRule(dat)
		require.NoError(t, err)
		require.Len(t, rules, 1)
		assert.Equal(t, "web", rules[0].RuleName)
		assert.Equal(t, uint64(8080), rules[0].Port)
		assert.Equal(t, "http", rules[0].Protocol)
	})

	t.Run("preserves explicit protocol and flags", func(t *testing.T) {
		dat := []interface{}{
			map[string]interface{}{
				"name":        "ssh",
				"port":        float64(22),
				"protocol":    "tcp",
				"main":        true,
				"public":      true,
				"remote_port": float64(30022),
			},
		}

		rules, err := InterfaceToPortForwardRule(dat)
		require.NoError(t, err)
		require.Len(t, rules, 1)
		assert.Equal(t, "tcp", rules[0].Protocol)
		assert.Equal(t, uint64(30022), rules[0].RemotePort)
		assert.True(t, rules[0].Main)
		assert.True(t, rules[0].Public)
	})

	t.Run("empty input yields empty slice", func(t *testing.T) {
		rules, err := InterfaceToPortForwardRule([]interface{}{})
		require.NoError(t, err)
		assert.Empty(t, rules)
	})
}

func TestPortForwardRuleToInterfaceRoundTrip(t *testing.T) {
	original := []common.PortForwardRule{
		{RuleName: "web", Port: 8080, Protocol: "http", Public: true},
		{RuleName: "ssh", Port: 22, Protocol: "tcp", RemotePort: 30022, Main: true},
	}

	ifaces, err := PortForwardRuleToInterface(original)
	require.NoError(t, err)
	require.Len(t, ifaces, 2)

	roundTripped, err := InterfaceToPortForwardRule(ifaces)
	require.NoError(t, err)
	require.Len(t, roundTripped, 2)
	assert.Equal(t, original, roundTripped)
}

func TestParseProxyStatus(t *testing.T) {
	t.Run("parses status and extracts remote port", func(t *testing.T) {
		text := `{"tcp":[{"name":"9-svc-22-tcp","type":"tcp","status":"running","err":"","local_addr":"127.0.0.1:22","plugin":"","remote_addr":"app.ironflock.com:30022"}]}`

		statuses, err := parseProxyStatus(text)
		require.NoError(t, err)
		require.Len(t, statuses, 1)

		s := statuses[0]
		assert.Equal(t, "9-svc-22-tcp", s.Name)
		assert.Equal(t, "running", s.Status)
		assert.Equal(t, Protocol("tcp"), s.Protocol)
		assert.Equal(t, "127.0.0.1:22", s.LocalAddr)
		assert.Equal(t, uint64(30022), s.RemotePort)
		assert.Empty(t, s.Error)
	})

	t.Run("carries error message through", func(t *testing.T) {
		text := `{"http":[{"name":"9-web-8080-http","type":"http","status":"error","err":"port already used","local_addr":"127.0.0.1:8080","remote_addr":""}]}`

		statuses, err := parseProxyStatus(text)
		require.NoError(t, err)
		require.Len(t, statuses, 1)

		s := statuses[0]
		assert.Equal(t, "port already used", s.Error)
		assert.Equal(t, uint64(0), s.RemotePort)
	})

	t.Run("empty map yields empty slice", func(t *testing.T) {
		statuses, err := parseProxyStatus(`{}`)
		require.NoError(t, err)
		assert.Empty(t, statuses)
	})

	t.Run("malformed remote port is an error", func(t *testing.T) {
		text := `{"tcp":[{"name":"x","type":"tcp","status":"running","remote_addr":"host:notaport"}]}`

		_, err := parseProxyStatus(text)
		assert.Error(t, err)
	})

	t.Run("invalid json is an error", func(t *testing.T) {
		_, err := parseProxyStatus(`not-json`)
		assert.Error(t, err)
	})
}

// initialize() resolves the frps server address (and thus BaseTunnelURL) from
// the ReswarmConfig with a fixed precedence: appliance_domain >
// device_endpoint_url (subdomain rewritten to "app") > environment default.
func TestServerAddressResolution(t *testing.T) {
	tests := []struct {
		name     string
		reswarm  *config.ReswarmConfig
		expected string
	}{
		{
			name:     "production default",
			reswarm:  &config.ReswarmConfig{Environment: string(common.PRODUCTION)},
			expected: PROD_SERVER_ADDR,
		},
		{
			name:     "test default",
			reswarm:  &config.ReswarmConfig{Environment: string(common.TEST)},
			expected: TEST_SERVER_ADDR,
		},
		{
			name:     "local default",
			reswarm:  &config.ReswarmConfig{Environment: string(common.LOCAL)},
			expected: "localhost",
		},
		{
			name:     "appliance_domain wins over everything",
			reswarm:  &config.ReswarmConfig{ApplianceDomain: "tunnel.appliance.example.com", DeviceEndpointURL: "https://api.ironflock.com", Environment: string(common.PRODUCTION)},
			expected: "tunnel.appliance.example.com",
		},
		{
			name:     "device_endpoint_url rewrites leading subdomain to app",
			reswarm:  &config.ReswarmConfig{DeviceEndpointURL: "https://api.ironflock.com", Environment: string(common.PRODUCTION)},
			expected: "app.ironflock.com",
		},
		{
			name:     "device_endpoint_url IP literal kept as-is",
			reswarm:  &config.ReswarmConfig{DeviceEndpointURL: "https://192.168.0.21:8080", Environment: string(common.PRODUCTION)},
			expected: "192.168.0.21",
		},
		{
			name:     "device_endpoint_url localhost kept as-is",
			reswarm:  &config.ReswarmConfig{DeviceEndpointURL: "http://localhost:3000", Environment: string(common.LOCAL)},
			expected: "localhost",
		},
		{
			// Containerized dev agent (compose `agent` service): the name must
			// not be subdomain-rewritten into the nonexistent app.docker.internal.
			name:     "device_endpoint_url host.docker.internal kept as-is",
			reswarm:  &config.ReswarmConfig{DeviceEndpointURL: "ws://host.docker.internal:8080/ws-re-dev", Environment: string(common.LOCAL)},
			expected: "host.docker.internal",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := NewTunnelConfigBuilder(builderConfig(t, tt.reswarm))
			assert.Equal(t, tt.expected, builder.BaseTunnelURL)
		})
	}
}

// Local dev servers (native agent via localhost, containerized agent via
// host.docker.internal) expose frps on 7400; everything else uses 7000.
func TestServerPortResolution(t *testing.T) {
	local := NewTunnelConfigBuilder(builderConfig(t, &config.ReswarmConfig{DeviceEndpointURL: "ws://host.docker.internal:8080/ws-re-dev", Environment: string(common.LOCAL)}))
	assert.Equal(t, 7400, local.yamlConfig.ServerPort)

	prod := NewTunnelConfigBuilder(builderConfig(t, &config.ReswarmConfig{DeviceEndpointURL: "https://api.ironflock.com", Environment: string(common.PRODUCTION)}))
	assert.Equal(t, 7000, prod.yamlConfig.ServerPort)
}

// Domain-mode appliances (corporate-cert TLS: appliance_domain set AND a wss://
// device endpoint) carry the tunnel control connection over the appliance's 443
// ingress with frp's wss transport. Every other combination — plain-mode
// appliance (ws:// endpoint), cloud device (wss:// but no appliance_domain) —
// keeps the classic tcp transport on 7000.
func TestWSSTunnelTransportGating(t *testing.T) {
	tests := []struct {
		name         string
		reswarm      *config.ReswarmConfig
		wantPort     int
		wantProtocol string
	}{
		{
			name: "domain-mode appliance rides 443 with wss",
			reswarm: &config.ReswarmConfig{
				ApplianceDomain:   "tls-sf015.corp.example.com",
				DeviceEndpointURL: "wss://ws.tls-sf015.corp.example.com/ws-re-dev",
				Environment:       string(common.PRODUCTION),
			},
			wantPort:     443,
			wantProtocol: "wss",
		},
		{
			name: "plain-mode appliance stays on tcp 7000",
			reswarm: &config.ReswarmConfig{
				ApplianceDomain:   "appliance.example.com",
				DeviceEndpointURL: "ws://192.168.0.21:18080/ws-re-dev",
				Environment:       string(common.PRODUCTION),
			},
			wantPort:     7000,
			wantProtocol: "",
		},
		{
			name: "cloud device (wss but no appliance_domain) stays on tcp 7000",
			reswarm: &config.ReswarmConfig{
				DeviceEndpointURL: "wss://cbw.ironflock.com/ws-re-dev",
				Environment:       string(common.PRODUCTION),
			},
			wantPort:     7000,
			wantProtocol: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := NewTunnelConfigBuilder(builderConfig(t, tt.reswarm))
			assert.Equal(t, tt.wantPort, builder.yamlConfig.ServerPort)
			assert.Equal(t, tt.wantProtocol, builder.yamlConfig.Transport.Protocol)
		})
	}
}

// clearProxyEnv makes a test hermetic against the developer's or CI runner's own
// proxy configuration. httpproxy.FromEnvironment falls through to the lowercase
// spelling when the uppercase one is empty, so both have to be cleared.
func clearProxyEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"HTTP_PROXY", "http_proxy",
		"HTTPS_PROXY", "https_proxy",
		"ALL_PROXY", "all_proxy",
		"NO_PROXY", "no_proxy",
	} {
		t.Setenv(key, "")
	}
}

// In wss mode the control connection rides the appliance's 443 ingress, so it is
// HTTPS_PROXY that governs it. The agent resolves it (frpc applies NO_PROXY to
// nothing) and passes the result to frpc as transport.proxyURL.
func TestWSSTunnelProxyResolution(t *testing.T) {
	domainMode := &config.ReswarmConfig{
		ApplianceDomain:   "tls-sf015.corp.example.com",
		DeviceEndpointURL: "wss://ws.tls-sf015.corp.example.com/ws-re-dev",
		Environment:       string(common.PRODUCTION),
	}

	t.Run("HTTPS_PROXY is passed through in wss mode", func(t *testing.T) {
		clearProxyEnv(t)
		t.Setenv("HTTPS_PROXY", "http://user:pass@proxy.corp.example.com:3128")
		builder := NewTunnelConfigBuilder(builderConfig(t, domainMode))
		assert.Equal(t, "http://user:pass@proxy.corp.example.com:3128", builder.yamlConfig.Transport.ProxyURL)
	})

	t.Run("NO_PROXY exemption suppresses the proxy", func(t *testing.T) {
		clearProxyEnv(t)
		t.Setenv("HTTPS_PROXY", "http://proxy.corp.example.com:3128")
		t.Setenv("NO_PROXY", "tls-sf015.corp.example.com")
		builder := NewTunnelConfigBuilder(builderConfig(t, domainMode))
		assert.Empty(t, builder.yamlConfig.Transport.ProxyURL)
	})

	// Scheme selection, not mode, is what picks the variable: a raw tcp control
	// connection is not an HTTPS_PROXY matter.
	t.Run("HTTPS_PROXY does not govern a plain-mode connection", func(t *testing.T) {
		clearProxyEnv(t)
		t.Setenv("HTTPS_PROXY", "http://proxy.corp.example.com:3128")
		plain := &config.ReswarmConfig{
			ApplianceDomain:   "appliance.example.com",
			DeviceEndpointURL: "ws://192.168.0.21:18080/ws-re-dev",
			Environment:       string(common.PRODUCTION),
		}
		builder := NewTunnelConfigBuilder(builderConfig(t, plain))
		assert.Empty(t, builder.yamlConfig.Transport.ProxyURL)
	})
}

// A plain-mode control connection dials raw tcp 7000, so HTTP_PROXY governs it —
// the same variable frpc would have picked up on its own, except that frpc
// applies NO_PROXY to nothing. Resolving it here is what lets a NO_PROXY-exempted
// appliance be dialed directly; without it a corporate proxy answers the CONNECT
// with 407 and no tunnel ever registers, while every other agent connection to
// that same appliance keeps working.
func TestPlainModeTunnelProxyResolution(t *testing.T) {
	plain := &config.ReswarmConfig{
		ApplianceDomain:   "appliance.corp.example.com",
		DeviceEndpointURL: "ws://192.168.0.21/ws-re-dev",
		Environment:       string(common.PRODUCTION),
	}

	t.Run("the lowercase http_proxy frpc reads is passed through", func(t *testing.T) {
		clearProxyEnv(t)
		t.Setenv("http_proxy", "http://proxy.corp.example.com:3128")
		builder := NewTunnelConfigBuilder(builderConfig(t, plain))
		assert.Equal(t, "http://proxy.corp.example.com:3128", builder.yamlConfig.Transport.ProxyURL)
	})

	// The invariant that keeps this change safe to roll out: frp reads exactly
	// one variable, os.Getenv("http_proxy"), which is case sensitive on Linux —
	// so a Linux agent's frpc never saw the uppercase spelling both installers
	// write, and dialed direct. Resolving the uppercase value here would newly
	// proxy every such device and break tunnels that work today. Skip on
	// Windows, where os.Getenv is case insensitive and the two spellings are
	// the same variable (which is why the field failure was Windows-only).
	t.Run("an uppercase-only HTTP_PROXY is ignored, exactly as frpc ignores it", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("os.Getenv is case-insensitive on Windows; frpc sees the uppercase spelling too")
		}
		clearProxyEnv(t)
		t.Setenv("HTTP_PROXY", "http://proxy.corp.example.com:3128")
		builder := NewTunnelConfigBuilder(builderConfig(t, plain))
		assert.Empty(t, builder.yamlConfig.Transport.ProxyURL)
	})

	// The point of dialing the control connection at the appliance itself: the
	// single NO_PROXY entry an operator writes for the appliance — the host the
	// WAMP session already uses — now exempts the tunnel too. While frps was
	// dialed by APPLIANCE_DOMAIN, that entry left the tunnel proxied and only
	// the tunnel, which is how a site lost remote access with nothing else on
	// the device looking wrong.
	t.Run("one NO_PROXY entry covers the WAMP session and the tunnel", func(t *testing.T) {
		clearProxyEnv(t)
		t.Setenv("http_proxy", "http://proxy.corp.example.com:3128")
		t.Setenv("NO_PROXY", "192.168.0.21")
		builder := NewTunnelConfigBuilder(builderConfig(t, plain))
		assert.Empty(t, builder.yamlConfig.Transport.ProxyURL)
	})

	// Measured against frpc 0.70.0: an explicit `proxyURL: ""` does NOT stop it
	// falling back to http_proxy — frpcEnv() is what enforces that. What this
	// buys is the record: the decision is in the file a support engineer reads
	// instead of being inferred from an absent key.
	t.Run("a direct-dial decision is still written to the config", func(t *testing.T) {
		clearProxyEnv(t)
		builder := NewTunnelConfigBuilder(builderConfig(t, plain))

		raw, err := os.ReadFile(builder.ConfigPath)
		require.NoError(t, err)

		assert.Contains(t, string(raw), `proxyURL: ""`)
	})
}

// frps' 7000 does no SNI and no vhost matching, so a plain-mode appliance is
// dialed at its own address rather than at APPLIANCE_DOMAIN — which keeps
// tunnels off public DNS (the installer's default domain is <ip>.nip.io) and
// makes the tunnel share the appliance's proxy exemption. The wildcard domain
// is still what every user-facing tunnel URL is built under, so BaseTunnelURL
// must NOT follow the control address.
func TestApplianceControlAddress(t *testing.T) {
	tests := []struct {
		name              string
		reswarm           *config.ReswarmConfig
		wantControlAddr   string
		wantBaseTunnelURL string
		wantPort          int
	}{
		{
			name: "plain-mode appliance dials its own IP, URLs keep the domain",
			reswarm: &config.ReswarmConfig{
				ApplianceDomain:   "192.168.0.21.nip.io",
				DeviceEndpointURL: "ws://192.168.0.21/ws-re-dev",
				Environment:       string(common.PRODUCTION),
			},
			wantControlAddr:   "192.168.0.21",
			wantBaseTunnelURL: "192.168.0.21.nip.io",
			wantPort:          7000,
		},
		{
			// SNI and the wildcard certificate are issued for the domain, and
			// the TLS ingress routes on it.
			name: "wss-mode appliance keeps dialing the domain",
			reswarm: &config.ReswarmConfig{
				ApplianceDomain:   "appliance.corp.example.com",
				DeviceEndpointURL: "wss://appliance.corp.example.com/ws-re-dev",
				Environment:       string(common.PRODUCTION),
			},
			wantControlAddr:   "appliance.corp.example.com",
			wantBaseTunnelURL: "appliance.corp.example.com",
			wantPort:          443,
		},
		{
			// A short name need not resolve as widely as the domain does.
			name: "a hostname endpoint keeps the domain",
			reswarm: &config.ReswarmConfig{
				ApplianceDomain:   "appliance.corp.example.com",
				DeviceEndpointURL: "ws://tls-sf015/ws-re-dev",
				Environment:       string(common.PRODUCTION),
			},
			wantControlAddr:   "appliance.corp.example.com",
			wantBaseTunnelURL: "appliance.corp.example.com",
			wantPort:          7000,
		},
		{
			// Loopback stays on the domain so the local dev appliance stack is
			// not diverted onto the 7400 dev-port rewrite; its frps is on 7000.
			name: "loopback endpoint keeps the domain and port 7000",
			reswarm: &config.ReswarmConfig{
				ApplianceDomain:   "localhost.nip.io",
				DeviceEndpointURL: "ws://127.0.0.1:18080/ws-re-dev",
				Environment:       string(common.LOCAL),
			},
			wantControlAddr:   "localhost.nip.io",
			wantBaseTunnelURL: "localhost.nip.io",
			wantPort:          7000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearProxyEnv(t)
			builder := NewTunnelConfigBuilder(builderConfig(t, tt.reswarm))

			assert.Equal(t, tt.wantControlAddr, builder.yamlConfig.ServerAddr)
			assert.Equal(t, tt.wantBaseTunnelURL, builder.BaseTunnelURL)
			assert.Equal(t, tt.wantPort, builder.yamlConfig.ServerPort)
		})
	}
}

// The half that actually enforces "no proxy". A config carrying `proxyURL: ""`
// still dials http_proxy from the environment (measured against frpc 0.70.0), so
// the variables have to be gone. Deleting this in favour of the config field
// reintroduces the 407 loop.
func TestFrpcEnvStripsProxyVariables(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://proxy.corp.example.com:3128")
	t.Setenv("http_proxy", "http://proxy.corp.example.com:3128")
	t.Setenv("HTTPS_PROXY", "http://proxy.corp.example.com:3128")
	t.Setenv("ALL_PROXY", "socks5://proxy.corp.example.com:1080")
	t.Setenv("NO_PROXY", "appliance.corp.example.com")
	t.Setenv("REAGENT_TEST_SENTINEL", "kept")

	stripped := map[string]bool{
		"http_proxy":  true,
		"https_proxy": true,
		"all_proxy":   true,
		"no_proxy":    true,
	}

	sentinelKept := false
	for _, entry := range frpcEnv() {
		key, value, _ := strings.Cut(entry, "=")
		assert.False(t, stripped[strings.ToLower(key)], "proxy variable %s must not reach frpc", key)
		if key == "REAGENT_TEST_SENTINEL" {
			sentinelKept = true
			assert.Equal(t, "kept", value)
		}
	}

	assert.True(t, sentinelKept, "frpcEnv must pass the rest of the environment through")
}

// The agent derives a tunnel_proof from its existing .flock secret + device_key
// and sends it as frp client metadata (transport.metadatas.tunnel_proof) — no
// minted token, nothing to refresh. With no secret/device_key no metadata is
// emitted (the plugin then allows all — enforcement is opt-in server-side).
func TestTunnelProofMetadataEmission(t *testing.T) {
	withSecret := &config.ReswarmConfig{
		DeviceEndpointURL: "wss://cbw.ironflock.com/ws-re-dev",
		Environment:       string(common.PRODUCTION),
		Secret:            "device-cra-secret",
		DeviceKey:         2,
	}
	builder := NewTunnelConfigBuilder(builderConfig(t, withSecret))
	if assert.NotNil(t, builder.yamlConfig.Metadatas) {
		got := builder.yamlConfig.Metadatas["tunnel_proof"]
		assert.Equal(t, tunnelAuthProof("device-cra-secret", 2), got)
		assert.NotEmpty(t, got)
	}

	// Regression guard for the prod outage: `metadatas` is a TOP-LEVEL frp
	// client field, not a transport field. frpc parses with
	// DisallowUnknownFields and refuses to start ("json: unknown field
	// \"metadatas\"") if it is nested under transport. Assert the serialized
	// YAML has metadatas at column 0 and never indented under transport.
	out, err := yaml.Marshal(builder.yamlConfig)
	assert.NoError(t, err)
	yamlStr := string(out)
	assert.Contains(t, yamlStr, "\nmetadatas:", "metadatas must be a top-level key")
	assert.NotContains(t, yamlStr, "  metadatas:", "metadatas must not be nested (would break frpc)")

	// no secret/device_key => no proof metadata
	without := &config.ReswarmConfig{
		DeviceEndpointURL: "wss://cbw.ironflock.com/ws-re-dev",
		Environment:       string(common.PRODUCTION),
	}
	builder = NewTunnelConfigBuilder(builderConfig(t, without))
	assert.Nil(t, builder.yamlConfig.Metadatas)
}

func TestConfigBuilderAddAndGetTunnelConfig(t *testing.T) {
	cfg := builderConfig(t, &config.ReswarmConfig{Environment: string(common.PRODUCTION)})
	builder := NewTunnelConfigBuilder(cfg)

	httpConf := TunnelConfig{
		Subdomain: CreateSubdomain(HTTP, 9, "web", 8080, common.PROD),
		Protocol:  HTTP,
		LocalPort: 8080,
		LocalIP:   "127.0.0.1",
	}
	tcpConf := TunnelConfig{
		Subdomain:  CreateSubdomain(TCP, 9, "ssh", 22, common.PROD),
		Protocol:   TCP,
		LocalPort:  22,
		RemotePort: 30022,
	}

	builder.AddTunnelConfig(httpConf)
	builder.AddTunnelConfig(tcpConf)

	configs, err := builder.GetTunnelConfig()
	require.NoError(t, err)
	require.Len(t, configs, 2)

	byProto := map[Protocol]TunnelConfig{}
	for _, c := range configs {
		byProto[c.Protocol] = c
	}

	// HTTP proxy keeps subdomain, no remote port.
	gotHTTP := byProto[HTTP]
	assert.Equal(t, httpConf.Subdomain, gotHTTP.Subdomain)
	assert.Equal(t, uint64(8080), gotHTTP.LocalPort)
	assert.Equal(t, "127.0.0.1", gotHTTP.LocalIP)
	assert.Equal(t, "web", gotHTTP.AppName)
	assert.Equal(t, uint64(0), gotHTTP.RemotePort)

	// TCP proxy carries a remote port. It stores no subdomain key, so the
	// subdomain is recovered from the proxy name — callers rebuild the tunnel
	// id from it to match the config against its frpc status.
	gotTCP := byProto[TCP]
	assert.Equal(t, uint64(22), gotTCP.LocalPort)
	assert.Equal(t, uint64(30022), gotTCP.RemotePort)
	assert.Equal(t, "ssh", gotTCP.AppName)
	assert.Equal(t, tcpConf.Subdomain, gotTCP.Subdomain)
	assert.Empty(t, builder.yamlConfig.Proxies[1].SubDomain, "no subdomain key is stored for tcp")
}

func TestConfigBuilderAddTunnelConfigDeduplicates(t *testing.T) {
	cfg := builderConfig(t, &config.ReswarmConfig{Environment: string(common.PRODUCTION)})
	builder := NewTunnelConfigBuilder(cfg)

	conf := TunnelConfig{
		Subdomain: CreateSubdomain(HTTP, 9, "web", 8080, common.PROD),
		Protocol:  HTTP,
		LocalPort: 8080,
	}

	builder.AddTunnelConfig(conf)
	builder.AddTunnelConfig(conf) // same Name -> should be skipped

	configs, err := builder.GetTunnelConfig()
	require.NoError(t, err)
	assert.Len(t, configs, 1)
}

func TestConfigBuilderRemoveTunnelConfig(t *testing.T) {
	cfg := builderConfig(t, &config.ReswarmConfig{Environment: string(common.PRODUCTION)})
	builder := NewTunnelConfigBuilder(cfg)

	conf := TunnelConfig{
		Subdomain: CreateSubdomain(HTTP, 9, "web", 8080, common.PROD),
		Protocol:  HTTP,
		LocalPort: 8080,
	}

	builder.AddTunnelConfig(conf)
	require.Len(t, mustConfigs(t, &builder), 1)

	builder.RemoveTunnelConfig(conf)
	assert.Empty(t, mustConfigs(t, &builder))

	// Removing a non-existent tunnel is a no-op (does not panic / error).
	builder.RemoveTunnelConfig(TunnelConfig{Subdomain: "does-not-exist", Protocol: TCP})
	assert.Empty(t, mustConfigs(t, &builder))
}

func TestConfigBuilderSetTunnelVariable(t *testing.T) {
	cfg := builderConfig(t, &config.ReswarmConfig{Environment: string(common.PRODUCTION)})
	builder := NewTunnelConfigBuilder(cfg)

	conf := TunnelConfig{
		Subdomain:  CreateSubdomain(TCP, 9, "ssh", 22, common.PROD),
		Protocol:   TCP,
		LocalPort:  22,
		RemotePort: 30022,
	}
	builder.AddTunnelConfig(conf)

	tunnelID := CreateTunnelID(conf.Subdomain, string(conf.Protocol))
	builder.SetTunnelVariable(tunnelID, LOCAL_PORT, "2200")
	builder.SetTunnelVariable(tunnelID, REMOTE_PORT, "30099")
	builder.SetTunnelVariable(tunnelID, LOCAL_IP, "10.0.0.5")

	configs := mustConfigs(t, &builder)
	require.Len(t, configs, 1)
	assert.Equal(t, uint64(2200), configs[0].LocalPort)
	assert.Equal(t, uint64(30099), configs[0].RemotePort)
	assert.Equal(t, "10.0.0.5", configs[0].LocalIP)
}

func TestConfigBuilderSetCommonVariableAndAdminPort(t *testing.T) {
	cfg := builderConfig(t, &config.ReswarmConfig{Environment: string(common.PRODUCTION)})
	builder := NewTunnelConfigBuilder(cfg)

	builder.SetCommonVariable(SERVER_ADDRESS, "custom.example.com")
	builder.SetCommonVariable(SERVER_PORT, "7777")
	builder.SetCommonVariable(ADMIN_PORT, "48000")
	builder.SetCommonVariable(ENALBE_TLS, "false")

	port, err := builder.GetAdminPort()
	require.NoError(t, err)
	assert.Equal(t, 48000, port)
}

func TestGetAdminPortInitialised(t *testing.T) {
	cfg := builderConfig(t, &config.ReswarmConfig{Environment: string(common.PRODUCTION)})
	builder := NewTunnelConfigBuilder(cfg)

	// initialize() always wires a WebServer, so a port is available.
	port, err := builder.GetAdminPort()
	require.NoError(t, err)
	assert.Greater(t, port, 0)
}

// frpc reads frpc.yaml, not the builder's memory: SetAdminPort must write the
// re-picked port to disk, or the recovery path respawns frpc on the very port
// that just failed with "bind: address already in use" — forever.
func TestSetAdminPortPersistsToDisk(t *testing.T) {
	cfg := builderConfig(t, &config.ReswarmConfig{Environment: string(common.PRODUCTION)})
	builder := NewTunnelConfigBuilder(cfg)

	// Divert the in-memory port without saving, so persistence (not a stale
	// file from initialize()) is what the assertion proves.
	builder.SetCommonVariable(ADMIN_PORT, "48000")

	builder.SetAdminPort()

	memPort, err := builder.GetAdminPort()
	require.NoError(t, err)
	assert.NotEqual(t, 48000, memPort, "SetAdminPort must re-pick the port")

	raw, err := os.ReadFile(builder.ConfigPath)
	require.NoError(t, err)

	var onDisk FrpcYamlConfig
	require.NoError(t, yaml.Unmarshal(raw, &onDisk))
	require.NotNil(t, onDisk.WebServer)
	assert.Equal(t, memPort, onDisk.WebServer.Port, "the re-picked admin port must be persisted for frpc to see")
}

// A port that is bound — by a stale frpc, an outbound connection, anything —
// must never be picked for the admin webserver again.
func TestPickAdminPortSkipsBoundPorts(t *testing.T) {
	first := pickAdminPort()
	require.GreaterOrEqual(t, first, adminPortScanStart)

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", first))
	require.NoError(t, err)
	defer ln.Close()

	second := pickAdminPort()
	assert.NotEqual(t, first, second, "a bound port must not be re-picked")
	assert.GreaterOrEqual(t, second, adminPortScanStart)
}

// GetState matches a stored config to its frpc status by tunnel id. Only
// HTTP/HTTPS proxies persist a subdomain, so every protocol must still round-
// trip an id equal to the proxy name frpc reports — otherwise the tunnel is
// silently missing from the reported state and the UI shows it as never
// coming up.
func TestGetTunnelConfigRoundTripsTunnelIdentity(t *testing.T) {
	cfg := builderConfig(t, &config.ReswarmConfig{Environment: string(common.PRODUCTION)})
	builder := NewTunnelConfigBuilder(cfg)

	for _, tc := range []struct {
		protocol Protocol
		port     uint64
	}{
		{HTTP, 55000},
		{HTTPS, 55001},
		{TCP, 1883},
		{UDP, 51820},
	} {
		builder.AddTunnelConfig(TunnelConfig{
			Subdomain:  CreateSubdomain(tc.protocol, 4749, "trumpfqds", tc.port, common.PROD),
			Protocol:   tc.protocol,
			LocalPort:  40000 + tc.port,
			RemotePort: 30000,
		})
	}

	configs := mustConfigs(t, &builder)
	require.Len(t, configs, 4)

	for _, got := range configs {
		expectedName := CreateTunnelID(CreateSubdomain(got.Protocol, 4749, "trumpfqds", got.DeclaredPort, common.PROD), string(got.Protocol))

		// The name frpc reports status under, carried through verbatim.
		assert.Equal(t, expectedName, got.Name, "%s: proxy name must survive the round-trip", got.Protocol)
		// ...and still reconstructible from the subdomain, which TCP/UDP do
		// not persist and must recover from the name.
		assert.Equal(t, expectedName, CreateTunnelID(got.Subdomain, string(got.Protocol)),
			"%s: subdomain must rebuild the frpc proxy name", got.Protocol)
		assert.Equal(t, "trumpfqds", got.AppName, "%s: app name", got.Protocol)
	}
}

// A tunnel whose subdomain did not round-trip (e.g. a config written before
// the subdomain was persisted for its protocol) must still be identifiable
// from the proxy name alone.
func TestGetTunnelConfigRecoversSubdomainFromName(t *testing.T) {
	cfg := builderConfig(t, &config.ReswarmConfig{Environment: string(common.PRODUCTION)})
	builder := NewTunnelConfigBuilder(cfg)

	subdomain := CreateSubdomain(HTTP, 4749, "trumpfqds", 55000, common.PROD)
	builder.AddTunnelConfig(TunnelConfig{Subdomain: subdomain, Protocol: HTTP, LocalPort: 40003})

	// Simulate the subdomain key being absent from the stored proxy.
	builder.yamlConfig.Proxies[0].SubDomain = ""

	configs := mustConfigs(t, &builder)
	require.Len(t, configs, 1)

	assert.Equal(t, subdomain, configs[0].Subdomain, "subdomain must be recovered from the proxy name")
	assert.Equal(t, uint64(55000), configs[0].DeclaredPort)
	assert.Equal(t, "trumpfqds", configs[0].AppName)
}

// mustConfigs is a small assertion helper local to this file.
func mustConfigs(t *testing.T, builder *TunnelConfigBuilder) []TunnelConfig {
	t.Helper()
	configs, err := builder.GetTunnelConfig()
	require.NoError(t, err)
	return configs
}

// The subdomain encodes the app's DECLARED port; LocalPort is the (possibly
// different) agent-managed host port frpc dials. GetTunnelConfig must recover
// the declared port so TunnelState.Port keeps matching the UI's port entries.
func TestGetTunnelConfigParsesDeclaredPort(t *testing.T) {
	cfg := builderConfig(t, &config.ReswarmConfig{Environment: string(common.PRODUCTION)})
	builder := NewTunnelConfigBuilder(cfg)

	builder.AddTunnelConfig(TunnelConfig{
		Subdomain: CreateSubdomain(HTTP, 9, "web", 8080, common.PROD),
		Protocol:  HTTP,
		LocalPort: 41234, // managed host port, not the declared port
	})
	builder.AddTunnelConfig(TunnelConfig{
		Subdomain:  CreateSubdomain(TCP, 9, "ssh", 22, common.PROD),
		Protocol:   TCP,
		LocalPort:  41235,
		RemotePort: 30022,
	})

	configs, err := builder.GetTunnelConfig()
	require.NoError(t, err)
	require.Len(t, configs, 2)

	byProto := map[Protocol]TunnelConfig{}
	for _, c := range configs {
		byProto[c.Protocol] = c
	}

	assert.Equal(t, uint64(8080), byProto[HTTP].DeclaredPort)
	assert.Equal(t, uint64(41234), byProto[HTTP].LocalPort)
	assert.Equal(t, uint64(22), byProto[TCP].DeclaredPort)
	assert.Equal(t, uint64(41235), byProto[TCP].LocalPort)
}

// The cloud is nudged to resync only when the fingerprint changes, so an
// unchanged read must fingerprint identically (no publish every 10s), and the
// order GetState happens to return proxies in must not look like a change.
func TestTunnelStateFingerprintStableAndOrderIndependent(t *testing.T) {
	a := TunnelState{Status: &TunnelStatus{Name: "9-web-8080-http", Status: "running"}, Active: true, URL: "https://a"}
	b := TunnelState{Status: &TunnelStatus{Name: "9-ssh-22-tcp", Status: "running", RemotePort: 30022}, Active: true, URL: "tcp://b"}

	first := tunnelStateFingerprint([]TunnelState{a, b})

	assert.Equal(t, first, tunnelStateFingerprint([]TunnelState{a, b}), "an identical re-read must not look like a change")
	assert.Equal(t, first, tunnelStateFingerprint([]TunnelState{b, a}), "ordering must not look like a change")
}

// Every field the UI renders a port square from has to move the fingerprint,
// or the cloud keeps showing a stale tunnel.
func TestTunnelStateFingerprintDetectsChanges(t *testing.T) {
	newState := func() []TunnelState {
		return []TunnelState{{
			Status: &TunnelStatus{Name: "9-ssh-22-tcp", Status: "running", RemotePort: 30022},
			Active: true,
		}}
	}
	baseline := tunnelStateFingerprint(newState())

	tests := []struct {
		name   string
		mutate func(s *TunnelState)
	}{
		{"proxy stopped running", func(s *TunnelState) { s.Status.Status = "error" }},
		{"remote port re-granted", func(s *TunnelState) { s.Status.RemotePort = 30099 }},
		{"went inactive", func(s *TunnelState) { s.Active = false }},
		{"error raised", func(s *TunnelState) { s.Error = true; s.ErrorMessage = "port already used" }},
		{"url changed", func(s *TunnelState) { s.URL = "tcp://new" }},
		{"proxy disappeared", func(s *TunnelState) { *s = TunnelState{} }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := newState()
			tt.mutate(&changed[0])
			assert.NotEqual(t, baseline, tunnelStateFingerprint(changed))
		})
	}
}

// A device with no proxies must fingerprint empty, so it never nudges the cloud
// about nothing.
func TestTunnelStateFingerprintEmptyWhenNoProxies(t *testing.T) {
	assert.Empty(t, tunnelStateFingerprint(nil))
	assert.Empty(t, tunnelStateFingerprint([]TunnelState{}))
}

// A configured proxy that frpc reports no status for arrives with a nil Status.
func TestTunnelStateFingerprintHandlesNilStatus(t *testing.T) {
	assert.NotPanics(t, func() {
		tunnelStateFingerprint([]TunnelState{{Active: false}})
	})
}

// A proxy left in frpc.yaml by a previous agent run may dial a stale host
// port; AddTunnelConfig must update it in place (and keep the granted remote
// port) instead of skipping.
func TestConfigBuilderAddTunnelConfigUpdatesStaleLocalPort(t *testing.T) {
	cfg := builderConfig(t, &config.ReswarmConfig{Environment: string(common.PRODUCTION)})
	builder := NewTunnelConfigBuilder(cfg)

	subdomain := CreateSubdomain(TCP, 9, "ssh", 22, common.PROD)
	builder.AddTunnelConfig(TunnelConfig{
		Subdomain:  subdomain,
		Protocol:   TCP,
		LocalPort:  41235,
		RemotePort: 30022,
	})

	// Same tunnel, new host port, no remote port supplied by the caller.
	builder.AddTunnelConfig(TunnelConfig{
		Subdomain: subdomain,
		Protocol:  TCP,
		LocalPort: 41999,
	})

	configs, err := builder.GetTunnelConfig()
	require.NoError(t, err)
	require.Len(t, configs, 1, "update must not append a duplicate proxy")
	assert.Equal(t, uint64(41999), configs[0].LocalPort)
	assert.Equal(t, uint64(30022), configs[0].RemotePort, "granted remote port survives the update")
}

// A changed reservation must win over a stale stored remote port: same local
// port and IP but a different non-zero requested remote is an update, not a
// dedup skip — while a zero-remote upsert still preserves the granted port.
func TestConfigBuilderAddTunnelConfigNonZeroRemoteWins(t *testing.T) {
	cfg := builderConfig(t, &config.ReswarmConfig{Environment: string(common.PRODUCTION)})
	builder := NewTunnelConfigBuilder(cfg)

	conf := TunnelConfig{
		Subdomain:  CreateSubdomain(TCP, 9, "ssh", 22, common.PROD),
		Protocol:   TCP,
		LocalPort:  22,
		RemotePort: 30022,
	}
	builder.AddTunnelConfig(conf)

	conf.RemotePort = 30500
	builder.AddTunnelConfig(conf)

	configs := mustConfigs(t, &builder)
	require.Len(t, configs, 1)
	assert.Equal(t, uint64(30500), configs[0].RemotePort)

	conf.RemotePort = 0
	conf.LocalPort = 2200
	builder.AddTunnelConfig(conf)

	configs = mustConfigs(t, &builder)
	require.Len(t, configs, 1)
	assert.Equal(t, uint64(2200), configs[0].LocalPort)
	assert.Equal(t, uint64(30500), configs[0].RemotePort, "a zero-remote update keeps the granted port")
}

// reserveRemotePort sends device_key and the reserved flag on the expose_port
// payload, tolerates any numeric wire type on the response, and no longer
// swallows backend errors (the old "Duplicate value" shortcut).
func TestReserveRemotePortPayloadAndErrors(t *testing.T) {
	msg := fakes.NewMessenger()
	cfg := builderConfig(t, &config.ReswarmConfig{Environment: string(common.PRODUCTION), DeviceKey: 77})

	frpTm := &FrpTunnelManager{messenger: msg, config: cfg}

	msg.CallResponses[string(topics.ExposePort)] = fakes.CallResponse{
		Result: messenger.Result{Arguments: []interface{}{map[string]interface{}{
			// float64 is what a JSON serializer hands back; the old uint64-only
			// assertion rejected it.
			"remote_port": float64(30500),
		}}},
	}

	port, err := frpTm.reserveRemotePort(30500, TCP, true)
	require.NoError(t, err)
	assert.Equal(t, uint64(30500), port)

	require.Len(t, msg.CallCalls, 1)
	payload, ok := msg.CallCalls[0].Args[0].(common.Dict)
	require.True(t, ok)
	assert.Equal(t, 77, payload["device_key"])
	assert.Equal(t, true, payload["reserved"])
	assert.Equal(t, "tcp", payload["protocol"])

	msg.CallErrors[string(topics.ExposePort)] = errors.New("Duplicate value: port already exposed")
	_, err = frpTm.reserveRemotePort(30500, TCP, true)
	require.Error(t, err, "backend errors must propagate, not be swallowed")
}
