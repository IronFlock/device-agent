package tunnel

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reagent/common"
	"reagent/config"
	"reagent/trust"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/rs/zerolog/log"
	"golang.org/x/net/http/httpproxy"
	"gopkg.in/yaml.v3"
)

type Frp struct {
	Config *config.Config
}

type TunnelConfig struct {
	Subdomain  string
	AppName    string
	Protocol   Protocol
	LocalPort  uint64
	LocalIP    string
	RemotePort uint64
	// DeclaredPort is the app's declared port encoded in the subdomain. It is
	// the stable identity the UI matches tunnel state against; LocalPort is
	// merely the host port the agent published it on.
	DeclaredPort uint64
	// Name is the frpc proxy name, and the identity frpc reports status under.
	// Set by GetTunnelConfig when reading the config back; AddTunnel derives it
	// from Subdomain instead, so callers building a config to add need not set
	// it.
	Name string
	// Reserved marks RemotePort as a user reservation: expose_port then
	// validates strictly (range hard-fail, collision -> error) instead of
	// falling back to an allocated port. Not serialized to frpc.yaml, so it
	// does not survive a config read-back.
	Reserved bool
	// Stage is the install the tunnel belongs to: DEV for a development run
	// (its label ends in -dev), PROD otherwise. Not written to frpc.yaml;
	// GetTunnelConfig parses it back from the proxy name.
	Stage common.Stage
}

// YAML config structures matching frp v0.65.0 format
// frpcConfigMu guards the frpc config's route fields (ServerPort,
// Transport.Protocol/ProxyURL), the admin port and its marshalling in
// SaveConfig: the capability probe reads the route while a client restart
// moves it (useRoute), and a status publish reads the admin port while a
// restart re-picks it (SetAdminPort). One agent runs one tunnel manager, so one lock suffices; it
// lives outside the config struct because yaml reads that by reflection.
var frpcConfigMu sync.Mutex

type FrpcYamlConfig struct {
	ServerAddr string     `yaml:"serverAddr"`
	ServerPort int        `yaml:"serverPort"`
	Transport  *Transport `yaml:"transport,omitempty"`
	// Metadatas are client-level key/values frp forwards to the frps server-side
	// authz plugin (as user.metas on Login/NewProxy). We put a tunnel_proof here
	// — derived at runtime from the device's own .flock secret (tunnelAuthProof)
	// — which the plugin forwards to the backend to authorize only this device's
	// subdomains. Nothing minted, nothing to refresh. This is a TOP-LEVEL frp
	// client field (ClientCommonConfig.metadatas), NOT a transport field —
	// frpc parses with DisallowUnknownFields and refuses to start if it is
	// nested under transport.
	Metadatas map[string]string `yaml:"metadatas,omitempty"`
	WebServer *WebServer        `yaml:"webServer,omitempty"`
	Log       *LogConfig        `yaml:"log,omitempty"`
	// Deliberately not omitempty: the value we want is false, which omitempty
	// drops — and frp defaults loginFailExit to true, so frpc would exit after
	// the first failed login instead of retrying. A frps that is briefly
	// unreachable would then take tunnels down until the agent restarts.
	LoginFailExit bool          `yaml:"loginFailExit"`
	Proxies       []ProxyConfig `yaml:"proxies,omitempty"`
}

type Transport struct {
	// Protocol selects frp's underlying transport. Empty means frp's default
	// (tcp). "wss" (websocket over TLS) is used against domain-mode appliances,
	// where the control connection rides the appliance's 443 ingress on frp's
	// fixed /~!frp path instead of the raw frps port 7000, and by cloud devices
	// that cannot reach 7000 (see tunnelRoute).
	Protocol string `yaml:"protocol,omitempty"`
	// ProxyURL routes the control connection through an HTTP CONNECT proxy.
	// The agent resolves it for EVERY transport (resolveTunnelProxy) out of the
	// same HTTP(S)_PROXY/NO_PROXY environment the rest of the agent honors, and
	// writes the decision here explicitly.
	//
	// Not omitempty, so the "dial frps directly" decision is visible in the
	// file a support engineer reads rather than inferred from an absent key.
	// Be clear about what that does NOT buy, measured against frpc 0.70.0:
	// an explicit `proxyURL: ""` does not stop frpc falling back to http_proxy
	// from its own environment — it dials the proxy either way. Only a
	// non-empty value here overrides the environment. Suppressing an inherited
	// proxy is frpcEnv()'s job (tunnel.go), and that half is load-bearing.
	ProxyURL string     `yaml:"proxyURL"`
	TLS      *TLSConfig `yaml:"tls,omitempty"`
}

type TLSConfig struct {
	Enable bool `yaml:"enable"`
	// TrustedCaFile is the PEM bundle frpc verifies the frps certificate
	// against; empty makes frpc skip verification entirely (see
	// tunnelTLSServerName). ServerName is the name the certificate must carry
	// and doubles as SNI; frpc falls back to serverAddr when it is empty.
	TrustedCaFile string `yaml:"trustedCaFile,omitempty"`
	ServerName    string `yaml:"serverName,omitempty"`
}

type WebServer struct {
	Addr string `yaml:"addr"`
	Port int    `yaml:"port"`
}

type LogConfig struct {
	To      string `yaml:"to"`
	Level   string `yaml:"level"`
	MaxDays int    `yaml:"maxDays"`
}

type ProxyConfig struct {
	Name       string          `yaml:"name"`
	Type       string          `yaml:"type"`
	LocalIP    string          `yaml:"localIP,omitempty"`
	LocalPort  int             `yaml:"localPort"`
	RemotePort int             `yaml:"remotePort,omitempty"`
	SubDomain  string          `yaml:"subdomain,omitempty"`
	Transport  *ProxyTransport `yaml:"transport,omitempty"`
}

type ProxyTransport struct {
	UseEncryption bool `yaml:"useEncryption,omitempty"`
}

type TunnelConfigBuilder struct {
	yamlConfig    *FrpcYamlConfig
	appConfig     *config.Config
	ConfigPath    string
	BaseTunnelURL string
	// routes are the ways frpc may reach frps, most preferred first. The
	// config carries one of them at a time (useRoute); only a cloud device has
	// more than one (see initialize).
	routes []tunnelRoute
}

// frpWebsocketPath is the fixed path frp serves its websocket transport on
// (frp's netpkg.FrpWebsocketPath); frpc's wss transport requests exactly it.
const frpWebsocketPath = "/~!frp"

// tunnelRoute is one way for frpc's control connection to reach frps: the
// port it dials, frp's transport on that port ("" for frp's default tcp+TLS,
// "wss" for websocket over TLS on frpWebsocketPath) and the proxy it goes
// through ("" to dial directly). The server address is the same for all of a
// device's routes.
type tunnelRoute struct {
	Port     int
	Protocol string
	ProxyURL string
}

// String names the route for logs. It says whether a proxy is in the path but
// never prints the proxy URL, which can carry credentials.
func (route tunnelRoute) String() string {
	protocol := route.Protocol
	if protocol == "" {
		protocol = "tcp"
	}
	name := fmt.Sprintf("%s/%d", protocol, route.Port)
	if route.ProxyURL != "" {
		name += " via proxy"
	}
	return name
}

type FrpcVariable string

const SERVER_ADDRESS FrpcVariable = "server_addr"
const SERVER_PORT FrpcVariable = "server_port"
const ENALBE_TLS FrpcVariable = "tls_enable"
const ADMIN_ADDRESS FrpcVariable = "admin_addr"
const ADMIN_PORT FrpcVariable = "admin_port"

const TYPE FrpcVariable = "type"
const SUBDOMAIN FrpcVariable = "subdomain"
const LOCAL_PORT FrpcVariable = "local_port"
const LOCAL_IP FrpcVariable = "local_ip"
const REMOTE_PORT FrpcVariable = "remote_port"

const PROD_SERVER_ADDR = "app.ironflock.com"
const TEST_SERVER_ADDR = "app.ironflock.dev"

// TUNNEL_TLS_INSECURE_SKIP_VERIFY is the escape hatch for frps certificate
// verification: set to "true" in the agent's service environment (the same
// systemd drop-in or Windows service environment that carries the proxy
// variables), frpc.yaml is written without trustedCaFile and frpc goes back to
// accepting any certificate, as every agent before this check did. Meant for a
// site whose network intercepts TLS with a CA the device does not trust. A
// warning is logged every time the config is built while it is set.
const TUNNEL_TLS_INSECURE_SKIP_VERIFY = "TUNNEL_TLS_INSECURE_SKIP_VERIFY"

// Matches a tunnel label, "[secure-]{deviceKey}-{appName}-{declaredPort}[-dev]",
// tolerating a trailing "-{protocol}" (a proxy name) as well. Group 1 is the
// app name, group 2 the declared port, group 3 the development-run marker.
// Anchored, so the marker cannot be mistaken for part of the app name or the
// other way round: a PROD label always ends in digits, a DEV label in -dev
// ("12-x-dev-80" is PROD app x-dev, "12-x-80-dev" DEV app x).
var subdomainRegex = regexp.MustCompile(`^(?:secure-)?\d+-(.+)-(\d+)(-dev)?(?:-(?:http|https|tcp|udp))?$`)

// devLabelSuffix marks the tunnel of a development run: the PROD install of
// the same app on the same device keeps its own label, URL and proxy. The
// tunnel server, the backend and Studio parse the same grammar.
const devLabelSuffix = "-dev"

// MaxLabelLength is the longest DNS label. An http(s) tunnel's subdomain is a
// label of its URL's host; a longer one never resolves.
const MaxLabelLength = 63

func NewTunnelConfigBuilder(cfg *config.Config) TunnelConfigBuilder {
	return initialize(cfg)
}

func CreateTunnelID(subdomain string, protocol string) string {
	return fmt.Sprintf("%s-%s", subdomain, protocol)
}

// tunnelAuthProof derives the proof-of-possession the frps authz plugin (and the
// backend behind it) verifies: base64url(HMAC-SHA256(secret, "tunnel-authz:v1:"
// + deviceKey)). Keyed by the device's own CRA secret, so no server key and no
// minted token are needed. MUST stay byte-compatible with the backend verifier
// (RESWARM devices.ts authorize_tunnel).
func tunnelAuthProof(secret string, deviceKey int) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("tunnel-authz:v1:" + strconv.Itoa(deviceKey)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// CreateSubdomain builds a tunnel's label: "{deviceKey}-{appName}-{port}",
// lower case, with "-dev" appended for a development run and "secure-"
// prefixed for https. PROD labels are unchanged by the stage.
func CreateSubdomain(protocol Protocol, deviceKey uint64, appName string, localPort uint64, stage common.Stage) string {
	baseSubdomain := strings.ToLower(fmt.Sprintf("%d-%s-%d", deviceKey, appName, localPort))
	if stage == common.DEV {
		baseSubdomain += devLabelSuffix
	}
	if protocol == HTTPS {
		return fmt.Sprintf("%s-%s", "secure", baseSubdomain)
	}
	return baseSubdomain
}

// ParseSubdomain reads the app name, declared port and stage back from a
// tunnel label (or a proxy name, see subdomainRegex). ok is false for a label
// of another shape.
func ParseSubdomain(subdomain string) (appName string, declaredPort uint64, stage common.Stage, ok bool) {
	result := subdomainRegex.FindStringSubmatch(subdomain)
	if result == nil {
		return "", 0, "", false
	}
	port, err := strconv.ParseUint(result[2], 10, 64)
	if err != nil {
		return "", 0, "", false
	}
	stage = common.PROD
	if result[3] != "" {
		stage = common.DEV
	}
	return result[1], port, stage, true
}

func initialize(cfg *config.Config) TunnelConfigBuilder {
	frpcConfigPath := filepath.Join(cfg.CommandLineArguments.AgentDir, "frpc.yaml")

	// The log path follows the agent's layout (/var/log/ironflock/frpc.log,
	// or /var/log/frpc.log in the legacy one). Windows keeps it under the
	// agent dir it owns: a POSIX path resolves to C:\var\log there.
	frpcLogPath := cfg.CommandLineArguments.FrpcLogFileLocation
	if frpcLogPath == "" {
		frpcLogPath = "/var/log/frpc.log"
	}
	if runtime.GOOS == "windows" {
		frpcLogPath = filepath.Join(cfg.CommandLineArguments.AgentDir, "frpc.log")
	}

	// Domain-mode appliance (corporate-cert TLS): the device endpoint is wss://
	// and the appliance fronts everything on 443 behind its TLS ingress —
	// including frp's fixed /~!frp websocket path, which the ingress relays to
	// frps. Ride that same port with frp's wss transport so a device needs no
	// firewall or proxy exception beyond outbound 443. Plain-mode appliances
	// (ws:// endpoint) and cloud devices (no appliance_domain) keep the classic
	// tcp+TLS control connection on port 7000; a cloud device falls back to the
	// same wss transport on 443 when 7000 is out of reach (see routes below).
	//
	// Resolved before the server address because a wss control connection must
	// be addressed by name (SNI + the wildcard certificate) while a plain one
	// need not be.
	wssMode := cfg.ReswarmConfig.ApplianceDomain != "" &&
		strings.HasPrefix(cfg.ReswarmConfig.DeviceEndpointURL, "wss://")

	// Extract the tunnel base address — the vhost domain every tunnel URL is
	// built under (BaseTunnelURL below), NOT necessarily the address frpc dials
	// (controlAddr, further down). Order of precedence:
	//   1. ReswarmConfig.ApplianceDomain (set on appliance installs from
	//      APPLIANCE_DOMAIN — the operator's tunnel domain, already correct).
	//   2. device_endpoint_url with the leading subdomain replaced by "app"
	//      (cloud case: api.ironflock.com -> app.ironflock.com). This rewrite
	//      is skipped when the hostname is an IP literal, since splitting on
	//      "." would mangle e.g. 192.168.0.21 into "app.168.0.21".
	//   3. Environment-based default.
	serverAddr := PROD_SERVER_ADDR // Default fallback

	if cfg.ReswarmConfig.ApplianceDomain != "" {
		serverAddr = cfg.ReswarmConfig.ApplianceDomain
		log.Debug().Msgf("Using tunnel server address from appliance_domain: %s", serverAddr)
	} else if cfg.ReswarmConfig.DeviceEndpointURL != "" {
		parsedURL, err := url.Parse(cfg.ReswarmConfig.DeviceEndpointURL)
		if err != nil {
			log.Warn().Err(err).Msgf("Failed to parse device_endpoint_url, using default: %s", serverAddr)
		} else {
			// Extract hostname (without port)
			hostname := parsedURL.Hostname()
			if hostname != "" {
				switch {
				case hostname == "localhost" || hostname == "127.0.0.1":
					serverAddr = hostname
				case hostname == "host.docker.internal":
					// The agent itself runs inside a container on a dev
					// machine; the name means "the machine hosting the dev
					// stack" (frps included) and must not be subdomain-
					// rewritten (app.docker.internal does not exist).
					serverAddr = hostname
				case net.ParseIP(hostname) != nil:
					// IP literal — no subdomain to replace; use as-is.
					serverAddr = hostname
				default:
					// Replace subdomain with "app"
					// e.g., "api.ironflock.com" -> "app.ironflock.com"
					parts := strings.Split(hostname, ".")
					if len(parts) >= 2 {
						parts[0] = "app"
						serverAddr = strings.Join(parts, ".")
					} else {
						serverAddr = hostname
					}
				}
				log.Debug().Msgf("Using tunnel server address from device_endpoint_url: %s", serverAddr)
			}
		}
	} else {
		// Fallback to environment-based configuration
		switch cfg.ReswarmConfig.Environment {
		case string(common.PRODUCTION):
			serverAddr = PROD_SERVER_ADDR
		case string(common.TEST):
			serverAddr = TEST_SERVER_ADDR
		case string(common.LOCAL):
			serverAddr = "localhost"
		}
		log.Debug().Msgf("Using tunnel server address from environment: %s", serverAddr)
	}

	port := pickAdminPort()
	log.Debug().Msgf("Using port %d for Frp webserver", port)

	// Where frpc dials frps, which is not always the name tunnel URLs are built
	// under: on a plain-mode appliance the control connection goes straight to
	// the box (see applianceDirectHost) while serverAddr stays the wildcard
	// domain that vhost routing — and therefore every user-facing URL — needs.
	controlAddr := serverAddr
	if directHost := applianceDirectHost(cfg.ReswarmConfig.DeviceEndpointURL, wssMode); directHost != "" {
		controlAddr = directHost
		log.Debug().Msgf("Dialing frps directly at the appliance endpoint: %s (tunnel URLs stay on %s)", controlAddr, serverAddr)
	}

	// Initialize YAML config structure
	frpcConfig := &FrpcYamlConfig{
		ServerAddr: controlAddr,
		ServerPort: 7000,
		Transport: &Transport{
			TLS: &TLSConfig{
				Enable: true,
			},
		},
		WebServer: &WebServer{
			Addr: "127.0.0.1",
			Port: port,
		},
		Log: &LogConfig{
			To:      frpcLogPath,
			Level:   "debug",
			MaxDays: 3,
		},
		LoginFailExit: false,
		Proxies:       []ProxyConfig{},
	}

	// Prove device identity to the frps NewProxy authz plugin using material
	// already in the .flock — no minted token, nothing to refresh. We send an
	// HMAC of a fixed context string keyed by the device's own CRA secret; the
	// plugin forwards it to the backend, which recomputes the HMAC against the
	// secret it stores for this device_key and authorizes only this device's
	// subdomains. Domain-separated ("tunnel-authz:v1:") so it can never collide
	// with a WAMP-CRA response derived from the same secret.
	if cfg.ReswarmConfig.Secret != "" && cfg.ReswarmConfig.DeviceKey > 0 {
		proof := tunnelAuthProof(cfg.ReswarmConfig.Secret, cfg.ReswarmConfig.DeviceKey)
		frpcConfig.Metadatas = map[string]string{"tunnel_proof": proof}
	}

	route := tunnelRoute{Port: 7000}
	if wssMode {
		route = tunnelRoute{Port: 443, Protocol: "wss"}
	}

	// For local development, use port 7400 to avoid conflicts with macOS
	// services. host.docker.internal is the same dev stack seen from inside a
	// containerized agent — the host publishes frps on 7400 there too.
	if controlAddr == "localhost" || controlAddr == "127.0.0.1" || controlAddr == "host.docker.internal" {
		route.Port = 7400
	}

	// Resolved last, once the endpoint the control connection actually dials is
	// final: both the wss branch and the dev-port rewrite above move it, and
	// NO_PROXY matching is host- and port-sensitive.
	route.ProxyURL = resolveTunnelProxy(controlAddr, route.Port, wssMode)
	routes := []tunnelRoute{route}

	// A cloud device also gets the cloud frps's websocket ingress on 443 as a
	// second route, for networks that only let 443 out: a firewall that drops
	// 7000, or a corporate proxy whose CONNECT ACL is 443-only (Squid's stock
	// SSL_ports). The tunnel server relays frpWebsocketPath on its public 443
	// listener to frps (REtunnel FRPS_WS_INGRESS), under the same certificate
	// for the same name, so the route needs nothing new on either side — it
	// is what a proxied appliance's cloud forwarder already uses. 7000 stays
	// first: it keeps the extra relay hop out of every device's tunnel traffic
	// where the network allows it. The wss route resolves its proxy the way
	// wss mode does, from the full HTTPS_PROXY environment.
	//
	// "Cloud" is what tunnelTLSServerName verifies outside wss mode: never an
	// appliance, never a local dev stack — those keep their one route.
	if tunnelTLSServerName(cfg.ReswarmConfig, false, controlAddr) != "" {
		routes = append(routes, tunnelRoute{
			Port:     443,
			Protocol: "wss",
			ProxyURL: resolveTunnelProxy(controlAddr, 443, true),
		})
	}

	frpcConfig.ServerPort = route.Port
	frpcConfig.Transport.Protocol = route.Protocol
	frpcConfig.Transport.ProxyURL = route.ProxyURL

	// The CA bundle itself is (re)written by SaveConfig, next to frpc.yaml.
	if serverName := tunnelTLSServerName(cfg.ReswarmConfig, wssMode, controlAddr); serverName != "" {
		if tunnelTLSVerifyDisabled() {
			log.Warn().Str("server", serverName).
				Msgf("WARNING: %s is set; the tunnel client will NOT verify the tunnel server's certificate, so anyone on the network path can impersonate it. Unset it once the device trusts the server's CA", TUNNEL_TLS_INSECURE_SKIP_VERIFY)
		} else {
			frpcConfig.Transport.TLS.TrustedCaFile = trust.TunnelCAPath(cfg.CommandLineArguments.AgentDir)
			frpcConfig.Transport.TLS.ServerName = serverName
		}
	} else {
		// Plain-mode appliances and local stacks: nothing can vouch for the
		// tunnel server here (see tunnelTLSServerName). A warning whenever the
		// config is built, so an operator sees that the mode is unverified.
		log.Warn().Str("target", controlAddr).
			Msg("Tunnel control connection is not certificate-verified (plain-mode appliance or local server): anyone on the network path can impersonate the tunnel server. Use the appliance's TLS mode to verify it")
	}

	configBuilder := TunnelConfigBuilder{
		yamlConfig:    frpcConfig,
		appConfig:     cfg,
		ConfigPath:    frpcConfigPath,
		BaseTunnelURL: serverAddr,
		routes:        routes,
	}

	configBuilder.SaveConfig()

	return configBuilder
}

// route returns the route frpc's config currently carries.
func (builder *TunnelConfigBuilder) route() tunnelRoute {
	frpcConfigMu.Lock()
	defer frpcConfigMu.Unlock()
	route := tunnelRoute{Port: builder.yamlConfig.ServerPort}
	if transport := builder.yamlConfig.Transport; transport != nil {
		route.Protocol = transport.Protocol
		route.ProxyURL = transport.ProxyURL
	}
	return route
}

// useRoute points frpc's control connection at route and saves the config.
// frpc reads these settings only at startup — a reload applies proxies alone —
// so a running client keeps its route until it is restarted.
func (builder *TunnelConfigBuilder) useRoute(route tunnelRoute) {
	frpcConfigMu.Lock()
	builder.yamlConfig.ServerPort = route.Port
	builder.yamlConfig.Transport.Protocol = route.Protocol
	builder.yamlConfig.Transport.ProxyURL = route.ProxyURL
	frpcConfigMu.Unlock()
	builder.SaveConfig()
}

// tunnelTLSServerName returns the name frpc must verify the frps certificate
// against, or "" when the control connection stays unverified.
//
//   - cloud device: the frps behind app.<domain> presents a publicly trusted
//     Let's Encrypt certificate for exactly the name dialed (serverAddr is
//     derived from the device endpoint or the environment default), on 7000
//     and on the tunnel server's 443 websocket ingress alike.
//   - domain-mode appliance (wss): the control connection rides the
//     appliance's TLS ingress, which presents the operator's corporate-PKI
//     certificate for the appliance domain. The device already trusts that CA
//     through its OS store, for WAMP and the registry alike, and the host store
//     is what goes into the bundle. controlAddr is the appliance domain here:
//     applianceDirectHost never substitutes an address in wss mode.
//   - plain-mode appliance: deliberately unverified. Its frps serves a
//     self-signed certificate baked into the appliance image, no CA a device
//     could hold vouches for it, and the mode is plaintext overall (ws://
//     WAMP), so verification here would only break tunnels without protecting
//     anything the plaintext session does not already expose.
//   - a cloud-shaped config with a plain ws:// endpoint, an IP literal, or a
//     local dev stack (localhost, host.docker.internal) is unverified for the
//     same reason: no certificate there can name what is being dialed.
func tunnelTLSServerName(reswarm *config.ReswarmConfig, wssMode bool, controlAddr string) string {
	switch controlAddr {
	case "", "localhost", "127.0.0.1", "host.docker.internal":
		return ""
	}
	if net.ParseIP(controlAddr) != nil {
		return ""
	}
	if wssMode {
		return controlAddr
	}
	if reswarm.ApplianceDomain != "" {
		return ""
	}
	if strings.HasPrefix(reswarm.DeviceEndpointURL, "ws://") || strings.HasPrefix(reswarm.DeviceEndpointURL, "http://") {
		return ""
	}
	return controlAddr
}

// tunnelTLSVerifyDisabled reports whether TUNNEL_TLS_INSECURE_SKIP_VERIFY asks
// for the unverified control connection. A value that is not a boolean keeps
// verification on rather than guessing.
func tunnelTLSVerifyDisabled() bool {
	value := strings.TrimSpace(os.Getenv(TUNNEL_TLS_INSECURE_SKIP_VERIFY))
	if value == "" {
		return false
	}
	disabled, err := strconv.ParseBool(value)
	if err != nil {
		log.Warn().Str("value", value).
			Msgf("Ignoring %s: not a boolean, tunnel server verification stays on", TUNNEL_TLS_INSECURE_SKIP_VERIFY)
		return false
	}
	return disabled
}

// tunnelHostRoots and tunnelEndpointCAs supply the tunnel CA bundle's inputs.
// Variables so the test binary can keep the endpoint probe off the network:
// several tests carry real production endpoint URLs.
var (
	tunnelHostRoots   = trust.HostRoots
	tunnelEndpointCAs = trust.EndpointCAs
)

// writeTunnelCA regenerates the bundle frpc.yaml's trustedCaFile points at, if
// it points at one.
func (builder *TunnelConfigBuilder) writeTunnelCA() {
	transport := builder.yamlConfig.Transport
	if transport == nil || transport.TLS == nil || transport.TLS.TrustedCaFile == "" {
		return
	}

	var endpointURL string
	if builder.appConfig != nil && builder.appConfig.ReswarmConfig != nil {
		endpointURL = builder.appConfig.ReswarmConfig.DeviceEndpointURL
	}

	refreshTunnelCA(transport.TLS.TrustedCaFile, tunnelHostRoots, func() ([][]byte, error) {
		return tunnelEndpointCAs(endpointURL)
	})
}

// refreshTunnelCA rewrites the tunnel CA bundle at caPath from the host store,
// the device endpoint's verified chain and the embedded Let's Encrypt roots.
//
// A host store that cannot be read keeps the bundle already on disk: it was
// built from that store when it could be read, and replacing it with the
// embedded roots alone would drop the corporate root a domain-mode appliance
// needs. Only when there is no earlier bundle is one built without the store.
// A failed write is logged and otherwise left alone: frpc then refuses the
// connection instead of silently going back to an unverified one.
func refreshTunnelCA(caPath string, hostRoots func() ([][]byte, error), endpointCAs func() ([][]byte, error)) {
	roots, err := hostRoots()
	if err != nil {
		if _, statErr := os.Stat(caPath); statErr == nil {
			log.Warn().Err(err).Str("path", caPath).Msg("Could not read the host CA store; keeping the existing tunnel CA bundle")
			return
		}
		log.Warn().Err(err).Msg("Could not read the host CA store and there is no earlier tunnel CA bundle; it carries only the device endpoint's chain and the embedded Let's Encrypt roots")
	}

	chain, err := endpointCAs()
	if err != nil {
		log.Debug().Err(err).Msg("Could not read the device endpoint's CA chain for the tunnel CA bundle")
	}

	path, err := trust.WriteTunnelCA(filepath.Dir(caPath), roots, chain)
	if err != nil {
		log.Error().Err(err).Msg("Failed to write the tunnel CA bundle; the tunnel client cannot verify the tunnel server")
		return
	}
	log.Debug().Str("path", path).Msg("Tunnel CA bundle written")
}

// applianceDirectHost returns the appliance's own address out of
// device_endpoint_url when the frps CONTROL connection should be made to it
// rather than to APPLIANCE_DOMAIN, or "" to keep the domain. It never affects
// the tunnel base URL: app UIs are routed by Host header and must keep the
// wildcard domain.
//
// frps' 7000 is address-routed: it does no SNI and no vhost matching, so a
// plain-mode control connection has no reason to be addressed by name. Only the
// DATA plane does — frps routes app UIs by Host header (subDomainHost), which is
// why an appliance has a wildcard domain at all, defaulting to the installer's
// <APPLIANCE_HOST>.nip.io. Dialing frps by that domain drags a public DNS lookup
// into bringing a tunnel up, and — the reason this exists — makes the tunnel
// server a different string from the host every other agent connection uses, so
// a NO_PROXY entry for the appliance exempts the WAMP session while leaving the
// tunnel proxied. That asymmetry took a site's remote access down while nothing
// else about the device looked wrong.
//
// Deliberately narrow. It substitutes only a bare IP literal, which is the
// installer's default shape (<ip>.nip.io over APPLIANCE_HOST) and cannot be a
// name that resolves anywhere the domain would not:
//   - wss mode keeps the domain: SNI and the wildcard certificate are issued for
//     it, and the ingress routes on it.
//   - loopback and the dev aliases keep the domain, so the local dev appliance
//     stack is not diverted onto the 7400 dev-port rewrite below.
//   - a hostname endpoint keeps the domain rather than gambling that a short
//     name resolves as widely.
func applianceDirectHost(endpointURL string, wssMode bool) string {
	if wssMode || endpointURL == "" {
		return ""
	}

	parsed, err := url.Parse(endpointURL)
	if err != nil {
		return ""
	}

	hostname := parsed.Hostname()
	ip := net.ParseIP(hostname)
	if ip == nil || ip.IsLoopback() {
		return ""
	}

	return hostname
}

// resolveTunnelProxy returns the HTTP CONNECT proxy frpc should route its
// control connection through, or "" when it must dial frps directly.
//
// frpc cannot be left to work this out. It reads exactly one variable, in one
// place — frp v0.70.0 pkg/config/v1/client.go:152,
// `c.ProxyURL = util.EmptyOr(c.ProxyURL, os.Getenv("http_proxy"))` — and applies
// NO_PROXY to nothing. So on a corporate network it sends the control connection
// to a proxy that commonly refuses CONNECT to anything but 443, observed in the
// field as a permanent "DialTcpByHttpProxy error, StatusCode [407]" retry loop
// on a device whose every other connection to the same appliance went direct.
//
// That one os.Getenv is also why the failure was Windows-only: it is case
// sensitive on Linux and case insensitive on Windows, and both installers write
// the variable uppercase (install_ironflock.sh, reswarmify's proxy drop-in). A
// Linux agent's frpc therefore never saw it and dialed direct.
//
// Hence the deliberate asymmetry below, which preserves this invariant: the
// agent may only ever move a connection from proxied to DIRECT, never the
// reverse.
//
//   - plain tcp: mirror frpc's own rule, the lowercase key through os.Getenv,
//     so the resolution matches frpc's per-OS behaviour exactly — then let
//     NO_PROXY subtract from it. Reading the uppercase spelling here instead
//     would newly proxy every Linux device whose service carries an uppercase
//     HTTP_PROXY, breaking tunnels that work today.
//   - wss: the full HTTPS_PROXY/NO_PROXY environment. This is the proxy-only
//     network path, riding 443 where a corporate CONNECT ACL actually permits
//     it, and it is the behaviour that already shipped.
//
// NO_PROXY is honoured in either spelling: accepting more exemptions can only
// remove proxying. Note it must exempt the address the CONTROL connection uses
// (see applianceDirectHost), which is not necessarily the tunnel domain.
func resolveTunnelProxy(controlAddr string, controlPort int, wssMode bool) string {
	scheme := "http"
	proxyConfig := &httpproxy.Config{HTTPProxy: os.Getenv("http_proxy")}

	if wssMode {
		scheme = "https"
		proxyConfig = httpproxy.FromEnvironment()
	} else if proxyConfig.NoProxy = os.Getenv("NO_PROXY"); proxyConfig.NoProxy == "" {
		proxyConfig.NoProxy = os.Getenv("no_proxy")
	}

	target := &url.URL{Scheme: scheme, Host: net.JoinHostPort(controlAddr, strconv.Itoa(controlPort))}

	proxyURL, err := proxyConfig.ProxyFunc()(target)
	if err != nil {
		log.Warn().Err(err).Str("target", target.Host).
			Msg("Could not resolve a tunnel proxy from the environment, frpc will dial frps directly")
		return ""
	}
	if proxyURL == nil {
		log.Debug().Str("target", target.Host).
			Msg("Tunnel control connection will dial frps directly (no proxy applies)")
		return ""
	}

	if !wssMode {
		// Honoured, because frpc would have used this same proxy and silently
		// dropping it would be its own surprise — but said out loud, because a
		// corporate CONNECT ACL is routinely 443-only (Squid's stock SSL_ports
		// refuses the rest), which is exactly why install_ironflock.sh puts the
		// appliance's own cloud forwarder on wss/443 whenever a proxy is in
		// play. "A proxy was configured and the tunnel is using it" is the line
		// that was missing from the logs while a site had no remote access.
		log.Warn().Str("target", target.Host).Str("proxy", proxyURL.Redacted()).
			Msg("The plain tcp tunnel control connection is going through a proxy; if tunnels never register, the proxy is likely refusing CONNECT to a non-443 port — exempt the tunnel host in NO_PROXY or put the tunnel on the wss transport")
	}

	log.Debug().Str("target", target.Host).Str("proxy", proxyURL.Redacted()).
		Msg("Tunnel control connection will go through a proxy")
	return proxyURL.String()
}

func (builder *TunnelConfigBuilder) GetTunnelConfig() ([]TunnelConfig, error) {
	tunnelConfigs := make([]TunnelConfig, 0)

	for _, proxy := range builder.yamlConfig.Proxies {
		// Only HTTP/HTTPS proxies persist a subdomain (AddTunnelConfig writes
		// remotePort instead for TCP/UDP), so recover it from the proxy name —
		// which is CreateTunnelID(subdomain, protocol) — whenever it is absent.
		// Callers rebuild the tunnel id from Subdomain to match a config to its
		// frpc status: leaving it empty drops the tunnel from the reported
		// state entirely, and makes buildURL emit a host with an empty label.
		subdomain := proxy.SubDomain
		if subdomain == "" {
			subdomain = strings.TrimSuffix(proxy.Name, "-"+proxy.Type)
		}

		tunnelConfig := TunnelConfig{
			Name:         proxy.Name,
			Protocol:     Protocol(proxy.Type),
			LocalPort:    uint64(proxy.LocalPort),
			RemotePort:   uint64(proxy.RemotePort),
			LocalIP:      proxy.LocalIP,
			Subdomain:    subdomain,
			DeclaredPort: uint64(proxy.LocalPort),
		}

		if appName, declaredPort, stage, ok := ParseSubdomain(subdomain); ok {
			tunnelConfig.AppName = appName
			tunnelConfig.DeclaredPort = declaredPort
			tunnelConfig.Stage = stage
		} else {
			log.Error().Str("subdomain", subdomain).Str("name", proxy.Name).Msg("Failed to parse app name from tunnel subdomain")
		}

		tunnelConfigs = append(tunnelConfigs, tunnelConfig)
	}

	return tunnelConfigs, nil
}

func (builder *TunnelConfigBuilder) AddTunnelConfig(conf TunnelConfig) {
	tunnelID := CreateTunnelID(conf.Subdomain, string(conf.Protocol))

	proxyConfig := ProxyConfig{
		Name:      tunnelID,
		Type:      string(conf.Protocol),
		LocalPort: int(conf.LocalPort),
	}

	if conf.LocalIP != "" {
		proxyConfig.LocalIP = conf.LocalIP
	}

	// subdomain is only valid for HTTP/HTTPS protocols
	if conf.Protocol == HTTP || conf.Protocol == HTTPS {
		proxyConfig.SubDomain = conf.Subdomain
	} else {
		// For TCP/UDP, use remotePort instead
		proxyConfig.RemotePort = int(conf.RemotePort)
	}

	// Upsert: a proxy left over from a previous agent run may point at a
	// stale local port (the app was republished on a different host port).
	// Skipping it would leave frpc dialing a dead port forever. A non-zero
	// requested remote port must equally win over a stale stored one (a
	// changed reservation), so a mismatch there is an update, not a skip.
	for i, proxy := range builder.yamlConfig.Proxies {
		if proxy.Name != tunnelID {
			continue
		}

		if proxy.LocalPort == proxyConfig.LocalPort && proxy.LocalIP == proxyConfig.LocalIP &&
			(proxyConfig.RemotePort == 0 || proxy.RemotePort == proxyConfig.RemotePort) {
			log.Debug().Str("tunnelID", tunnelID).Msg("Tunnel already exists in config, skipping add")
			return
		}

		log.Info().Str("tunnelID", tunnelID).Int("oldLocalPort", proxy.LocalPort).Int("newLocalPort", proxyConfig.LocalPort).Msg("Updating existing tunnel config")
		// Keep the remote port frps already granted this proxy unless the
		// caller supplies one; TCP/UDP proxies must not lose it on update.
		if proxyConfig.RemotePort == 0 {
			proxyConfig.RemotePort = proxy.RemotePort
		}
		builder.yamlConfig.Proxies[i] = proxyConfig
		builder.SaveConfig()
		return
	}

	builder.yamlConfig.Proxies = append(builder.yamlConfig.Proxies, proxyConfig)
	builder.SaveConfig()
}

func (builder *TunnelConfigBuilder) SetTunnelVariable(tunnelID string, key FrpcVariable, value string) {
	// Find and update existing proxy
	for i := range builder.yamlConfig.Proxies {
		if builder.yamlConfig.Proxies[i].Name == tunnelID {
			switch key {
			case TYPE:
				builder.yamlConfig.Proxies[i].Type = value
			case SUBDOMAIN:
				builder.yamlConfig.Proxies[i].SubDomain = value
			case LOCAL_PORT:
				var port int
				_, _ = fmt.Sscanf(value, "%d", &port)
				builder.yamlConfig.Proxies[i].LocalPort = port
			case LOCAL_IP:
				builder.yamlConfig.Proxies[i].LocalIP = value
			case REMOTE_PORT:
				var port int
				_, _ = fmt.Sscanf(value, "%d", &port)
				builder.yamlConfig.Proxies[i].RemotePort = port
			}
			return
		}
	}
}

func (builder *TunnelConfigBuilder) RemoveTunnelVariable(tunnelID string) {
	// Remove proxy with matching name
	newProxies := []ProxyConfig{}
	for _, proxy := range builder.yamlConfig.Proxies {
		if proxy.Name != tunnelID {
			newProxies = append(newProxies, proxy)
		}
	}
	builder.yamlConfig.Proxies = newProxies
}

func (builder *TunnelConfigBuilder) Reset() {
	*builder = initialize(builder.appConfig)
}

// adminPortScanStart is the base of the dedicated range for frpc's loopback
// admin webserver. It sits below every contested pool on a device: the OS
// ephemeral range (32768-60999 — every outbound localhost connection borrows a
// source port there, and such a port stays busy for the connection's whole
// lifetime), the agent's app host-port pool (40000-49999), and the tunnel
// data-plane range (30000+). 7400 itself is skipped: local-dev frps listens
// there.
const adminPortScanStart = 7411

// pickAdminPort chooses the loopback port for frpc's admin webserver by
// scanning up from adminPortScanStart. An OS-assigned port (localhost:0) is
// only the last resort: it comes from the ephemeral range, where the port that
// was free at pick time is routinely taken later by an outbound connection —
// frpc then exits at startup with "bind: address already in use", and kept
// doing so on every retry because nothing re-picked the port.
func pickAdminPort() int {
	port, err := common.GetFreePortFromStart(adminPortScanStart)
	if err == nil {
		return port
	}

	log.Warn().Err(err).Msg("admin port scan found no free port, falling back to an OS-assigned port")
	port, err = common.GetRandomFreePort()
	if err == nil {
		return port
	}

	return adminPortScanStart
}

// SetAdminPort re-picks the admin webserver port and persists it. Persisting
// matters: frpc reads frpc.yaml, not our in-memory config, so without
// SaveConfig a restart after "bind: address already in use" would spawn frpc
// on the very port that just failed.
func (builder *TunnelConfigBuilder) SetAdminPort() {
	port := pickAdminPort()

	log.Debug().Msgf("Using port %d for Frp webserver", port)

	frpcConfigMu.Lock()
	builder.yamlConfig.WebServer.Port = port
	frpcConfigMu.Unlock()
	builder.SaveConfig()
}

func (builder *TunnelConfigBuilder) SetCommonVariable(key FrpcVariable, value string) {
	switch key {
	case SERVER_ADDRESS:
		builder.yamlConfig.ServerAddr = value
	case SERVER_PORT:
		var port int
		_, _ = fmt.Sscanf(value, "%d", &port)
		builder.yamlConfig.ServerPort = port
	case ENALBE_TLS:
		builder.yamlConfig.Transport.TLS.Enable = (value == "true")
	case ADMIN_ADDRESS:
		builder.yamlConfig.WebServer.Addr = value
	case ADMIN_PORT:
		var port int
		_, _ = fmt.Sscanf(value, "%d", &port)
		builder.yamlConfig.WebServer.Port = port
	default:
		// Handle log and other settings
		if key == "log_file" {
			builder.yamlConfig.Log.To = value
		} else if key == "log_level" {
			builder.yamlConfig.Log.Level = value
		} else if key == "log_max_days" {
			var days int
			_, _ = fmt.Sscanf(value, "%d", &days)
			builder.yamlConfig.Log.MaxDays = days
		}
	}
}

func (builder *TunnelConfigBuilder) GetAdminPort() (int, error) {
	frpcConfigMu.Lock()
	defer frpcConfigMu.Unlock()
	if builder.yamlConfig.WebServer == nil {
		return 0, fmt.Errorf("webServer not configured")
	}
	return builder.yamlConfig.WebServer.Port, nil
}

func (builder *TunnelConfigBuilder) RemoveTunnelConfig(port TunnelConfig) {
	tunnelID := CreateTunnelID(port.Subdomain, string(port.Protocol))
	builder.RemoveTunnelVariable(tunnelID)
	builder.SaveConfig()
}

func (builder *TunnelConfigBuilder) SaveConfig() {
	// Every config frpc reads must find the bundle it names, current.
	builder.writeTunnelCA()

	frpcConfigMu.Lock()
	data, err := yaml.Marshal(builder.yamlConfig)
	frpcConfigMu.Unlock()
	if err != nil {
		log.Error().Err(err).Msg("Failed to marshal frpc config to YAML")
		return
	}

	// 0600: the config carries the device's tunnel_proof, a static credential
	// that authorizes tunnels in this device's name. Only frpc (root) reads it.
	// WriteFile keeps the mode of an existing file, hence the explicit chmod
	// for configs written by older agents with 0644.
	err = os.WriteFile(builder.ConfigPath, data, 0600)
	if err == nil {
		err = os.Chmod(builder.ConfigPath, 0600)
	}
	if err != nil {
		log.Error().Err(err).Msgf("Failed to write frpc config to %s", builder.ConfigPath)
		return
	}

	log.Debug().Msgf("Saved frpc config to %s", builder.ConfigPath)
}
