package tunnel

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reagent/common"
	"reagent/config"
	"reagent/errdefs"
	"reagent/filesystem"
	"reagent/messenger"
	"reagent/messenger/topics"
	"reagent/safe"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	xproxy "golang.org/x/net/proxy"
)

type Protocol string

const (
	TCP   Protocol = "tcp"
	UDP   Protocol = "udp"
	HTTP  Protocol = "http"
	HTTPS Protocol = "https"
)

// TunnelCapability tracks whether this device can currently run tunnels. It is
// the per-device signal the UI uses to show/hide tunnel controls, and it lets
// syncPortState no-op cleanly (instead of erroring) when frpc is absent — e.g.
// on Windows where antivirus may quarantine frpc, or on an offline device that
// could never download it.
type TunnelCapability int32

const (
	// CapabilityUnknown: no start attempt has completed yet.
	CapabilityUnknown TunnelCapability = iota
	// CapabilityStarting: a start attempt is in progress.
	CapabilityStarting
	// CapabilityAvailable: frpc is present and logged in to the server.
	CapabilityAvailable
	// CapabilityUnavailable: frpc is missing/quarantined or repeatedly fails.
	CapabilityUnavailable
)

// defaultLoginDeadline bounds how long Start() waits for frpc to announce a
// successful login on its stdout before falling back to probing frps directly.
//
// A deadline is needed because loginFailExit=false (see frp.go) makes frpc retry
// a failed login forever instead of exiting, so the login ack may never arrive
// and Start() would otherwise block for good. The manager holds it as a field
// (frpTm.loginDeadline) so tests can shorten it.
//
// Note the ack usually does NOT arrive even on a perfectly healthy device: frp.go
// points frpc's logger at a FILE (log.to = /var/log/frpc.log), so
// "login to server success" never reaches the stdout this agent scans. Expiry
// therefore means "no news", NOT "broken" — which is why the deadline hands off
// to a reachability probe instead of declaring the device unavailable.
const defaultLoginDeadline = 30 * time.Second

const (
	// fastStartAttempts is how many failures the supervisor retries on the short
	// backoff (defaultStartRetryBackoff, doubling) before it settles on
	// defaultStartRetryInterval (see startRetryDelay).
	fastStartAttempts         = 6
	defaultStartRetryBackoff  = 2 * time.Second
	defaultStartRetryInterval = 10 * time.Minute
)

const (
	// frpsProbeTimeout bounds a single reachability dial to the frps control port.
	frpsProbeTimeout = 5 * time.Second
	// capabilityProbeInterval re-checks reachability so the reported capability
	// tracks reality instead of latching whatever happened to be true at boot.
	// It matters most on an appliance, where the agent (host) and the frps
	// container start together: the first probe can lose that race, and only a
	// repeat check clears the badge once frps finishes coming up.
	capabilityProbeInterval = 60 * time.Second
	// tunnelStatePollInterval is how often the device re-reads its own proxy
	// state to decide whether the cloud needs a resync nudge.
	tunnelStatePollInterval = 10 * time.Second
)

func (c TunnelCapability) String() string {
	switch c {
	case CapabilityStarting:
		return "starting"
	case CapabilityAvailable:
		return "available"
	case CapabilityUnavailable:
		return "unavailable"
	default:
		return "unknown"
	}
}

type FrpTunnelManager struct {
	TunnelManager
	tunnelsLock         *sync.RWMutex
	tunnelUpdateChan    chan TunnelUpdate
	activeTunnelConfigs map[string]*Tunnel
	clientProcess       *exec.Cmd
	// clientDone is closed once the current client process has exited and been
	// reaped, i.e. its listeners (the admin port) are actually released.
	clientDone chan struct{}
	// clientRun decides whether the current client's exit restarts it.
	clientRun *frpcRun
	// startMu serializes every spawn/kill of the frpc client. Unserialized,
	// the boot supervisor and a Reload/AllStatus-triggered restart could each
	// spawn an frpc — the loser exits with "bind: address already in use" on
	// the shared admin port and its failure clobbers the winner's tracking.
	startMu sync.Mutex
	// supervising makes SuperviseStart single-flight; a second caller returns
	// instead of running a competing retry loop.
	supervising atomic.Bool
	// startFailures counts the client's consecutive failures to start or to
	// keep running (see countStartFailure); it paces the supervisor's next
	// attempt (startRetryDelay).
	startFailures atomic.Int32
	// startRetryBackoff and startRetryInterval pace those attempts (see
	// startRetryDelay). Fields so tests can shorten them; zero falls back to
	// the defaults.
	startRetryBackoff  time.Duration
	startRetryInterval time.Duration
	// supervisionStop is closed by Stop, which ends a supervisor waiting to
	// retry: a client stopped on purpose stays stopped. Guarded by startMu.
	supervisionStop chan struct{}

	configBuilder TunnelConfigBuilder
	config        *config.Config
	messenger     messenger.Messenger
	loginChan     chan bool // Signals when frpc logs in to server
	isLoggedIn    bool
	loginMutex    sync.RWMutex

	capability   atomic.Int32
	capabilityMu sync.Mutex
	lastErr      string
	// loginDeadline is how long Start() waits for a login ack before falling back
	// to a reachability probe (see defaultLoginDeadline). A field so tests can
	// shorten it; zero falls back to defaultLoginDeadline.
	loginDeadline time.Duration
	// clientAlive is true while a spawned frpc process is running. The capability
	// probe only speaks for a running client: when frpc could not start at all
	// (binary missing/quarantined) that failure already owns the capability, and
	// a reachable frps must not overwrite it with "available".
	clientAlive atomic.Bool
	// monitorOnce keeps exactly one capability monitor running for the lifetime
	// of the manager, however many times Start() is called.
	monitorOnce sync.Once
	// reacquireFrpc re-fetches the frpc binary when it is found missing at
	// runtime (e.g. antivirus deleted it). Injected by the agent so the tunnel
	// package need not import system; nil in tests means "do not re-acquire".
	reacquireFrpc func() error
	// onAvailable reconciles the apps' tunnels whenever the device becomes
	// able to tunnel (see setCapability). Injected by the agent so the tunnel
	// package need not import apps; nil in tests. Atomic rather than a plain
	// field because the capability can flip before the agent wires it: the
	// boot reconcile in NewAgent already syncs ports, and an AddTunnel whose
	// Reload finds frpc not running re-supervises it while the WAMP session is
	// still being established.
	onAvailable atomic.Pointer[func()]
}

// frpcRun is the lifecycle of one spawned frpc process. Its exit restarts the
// client only when Start had handed it over (established) and nobody stopped
// it on purpose. A client that dies earlier is Start's failure, which only
// SuperviseStart retries; restarting it from its exit as well would retry an
// frpc that antivirus kills at once without any backoff.
type frpcRun struct {
	mu          sync.Mutex
	spawned     time.Time
	established bool
	stopped     bool
	exited      bool
}

// establish marks the client as handed over by Start. It reports false when
// the process has already exited, so Start fails instead of reporting a dead
// client as started.
func (run *frpcRun) establish() bool {
	run.mu.Lock()
	defer run.mu.Unlock()

	if run.exited {
		return false
	}
	run.established = true
	return true
}

// stop marks the coming exit as deliberate.
func (run *frpcRun) stop() {
	run.mu.Lock()
	defer run.mu.Unlock()

	run.stopped = true
}

// exit records the exit. It reports whether the client was lost (established
// and not stopped on purpose) and how long it ran.
func (run *frpcRun) exit() (lost bool, ranFor time.Duration) {
	run.mu.Lock()
	defer run.mu.Unlock()

	run.exited = true
	return run.established && !run.stopped, time.Since(run.spawned)
}

// errFrpcExitedDuringStart fails a Start whose client died before Start could
// hand it over.
var errFrpcExitedDuringStart = errors.New("frpc exited while it was starting")

type UpdateType string

const (
	STARTED UpdateType = "started"
	REMOVED UpdateType = "removed"
)

type TunnelUpdate struct {
	DeviceKey  uint64
	AppName    string
	LocalPort  uint64
	UpdateType UpdateType
	Protocol   Protocol
}

type FullURL struct {
	HttpURL  string `json:"httpURL"`
	HttpsURL string `json:"httpsURL"`
	TcpURL   string `json:"tcpURL"`
}

type Tunnel struct {
	Error  string
	Config TunnelConfig
}

type AppTunnel struct {
	Tunnel    *Tunnel
	Mutex     sync.Mutex
	Name      string
	DeviceKey uint64
	AppKey    uint64
	Main      bool
	Public    bool
	Running   bool
}

func InterfaceToPortForwardRule(dat []interface{}) ([]common.PortForwardRule, error) {
	portEntries := make([]common.PortForwardRule, 0, len(dat))

	for _, portEntry := range dat {
		jsonStr, err := json.Marshal(portEntry)
		if err != nil {
			return nil, err
		}

		var portEntry common.PortForwardRule
		err = json.Unmarshal(jsonStr, &portEntry)
		if err != nil {
			return nil, err
		}

		if portEntry.Protocol == "" {
			portEntry.Protocol = "http"
		}

		portEntries = append(portEntries, portEntry)

	}
	return portEntries, nil
}

func PortForwardRuleToInterface(portEntries []common.PortForwardRule) ([]interface{}, error) {
	portEntriesInterface := make([]interface{}, 0, len(portEntries))

	for _, portEntry := range portEntries {
		jsonStr, err := json.Marshal(portEntry)
		if err != nil {
			return nil, err
		}

		var portEntryInterface interface{}
		err = json.Unmarshal(jsonStr, &portEntryInterface)
		if err != nil {
			return nil, err
		}

		portEntriesInterface = append(portEntriesInterface, portEntryInterface)

	}
	return portEntriesInterface, nil
}

type TunnelManager interface {
	AddTunnel(config TunnelConfig) (TunnelConfig, error)
	GetState() ([]TunnelState, error)
	GetStateById(tunnelID string) (TunnelState, error)
	RemoveTunnel(conf TunnelConfig) error
	GetTunnelConfig() ([]TunnelConfig, error)
	Get(tunnelID string) *Tunnel
	Status(tunnelID string) (TunnelStatus, error)
	Reload() error
	Start() error
	// SuperviseStart brings the client up and keeps retrying until it runs;
	// meanwhile the device is in an unavailable-but-alive state.
	SuperviseStart()
	SaveRemotePorts(payload common.TransitionPayload) error
	// TunnelCapable reports whether tunnels can run on this device (true unless
	// definitively unavailable). syncPortState and the UI gate on this.
	TunnelCapable() bool
	// MarkUnavailable records that tunnels cannot run (e.g. an unsupported
	// platform), so the device degrades cleanly instead of retrying.
	MarkUnavailable(reason string)
}

type TunnelStatus struct {
	Name       string   `json:"name"`
	Status     string   `json:"status"`
	LocalAddr  string   `json:"localAddr"`
	Plugin     string   `json:"plugin"`
	RemoteAddr string   `json:"remoteAddr"`
	RemotePort uint64   `json:"remotePort"`
	Error      string   `json:"error"`
	Protocol   Protocol `json:"protocol"`
}

type FrpStatus struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Status     string `json:"status"`
	Err        string `json:"err"`
	LocalAddr  string `json:"local_addr"`
	Plugin     string `json:"plugin"`
	RemoteAddr string `json:"remote_addr"`
}

type TunnelState struct {
	Status       *TunnelStatus `json:"status"`
	AppName      string        `json:"app_name"`
	Port         uint64        `json:"port"`
	Active       bool          `json:"active"`
	Error        bool          `json:"error"`
	ErrorMessage string        `json:"error_message"`
	URL          string        `json:"url"`
	// Stage tells a development run's tunnel (DEV) from the PROD install's of
	// the same app and port.
	Stage common.Stage `json:"stage"`
}

func parseProxyStatus(text string) ([]TunnelStatus, error) {

	frpStatuses := make([]TunnelStatus, 0)

	frpStatusMap := make(map[string][]FrpStatus)
	err := json.Unmarshal([]byte(text), &frpStatusMap)
	if err != nil {
		return nil, err
	}

	for _, statusArray := range frpStatusMap {
		for _, frpStatus := range statusArray {

			tunnelStatus := TunnelStatus{
				Status:     frpStatus.Status,
				Name:       frpStatus.Name,
				LocalAddr:  frpStatus.LocalAddr,
				Plugin:     frpStatus.Plugin,
				RemoteAddr: frpStatus.RemoteAddr,
				Error:      frpStatus.Err,
				Protocol:   Protocol(frpStatus.Type),
			}

			if frpStatus.RemoteAddr != "" {
				parts := strings.Split(frpStatus.RemoteAddr, ":")
				if len(parts) > 1 {
					remotePortStr := parts[1]
					remotePort, err := strconv.ParseInt(remotePortStr, 10, 64)
					if err != nil {
						log.Error().Msgf("frpStatus.RemoteAddr: %s", frpStatus.RemoteAddr)
						return nil, err
					}
					tunnelStatus.RemotePort = uint64(remotePort)
				} else {
					log.Warn().Msgf("RemoteAddr does not contain port: %s", frpStatus.RemoteAddr)
				}
			}

			frpStatuses = append(frpStatuses, tunnelStatus)
		}
	}

	return frpStatuses, nil
}

var tunnelIdRegexp = regexp.MustCompile(`\[(([^\]]+)-(http|https|tcp|udp))]`)
var errMessageRegexp = regexp.MustCompile(`error: (.*)`)
var proxyNameRegex = regexp.MustCompile(`\[(\d+)-(.*)-(\d+)-(.*)\]`)

// SetReacquireFrpc wires the callback that re-fetches frpc when it is found
// missing at runtime. Called once by the agent after construction.
func (frpTm *FrpTunnelManager) SetReacquireFrpc(fn func() error) {
	frpTm.reacquireFrpc = fn
}

// SetOnAvailable wires the callback run each time the device becomes able to
// tunnel, i.e. the capability turns Available from any other value. Called
// once by the agent after construction.
func (frpTm *FrpTunnelManager) SetOnAvailable(fn func()) {
	frpTm.onAvailable.Store(&fn)
}

// TunnelCapable reports whether tunnels can run on this device. It is true
// while a device is bringing frpc up or has it running (Unknown/Starting/
// Available) and only false once tunnels are definitively unavailable
// (frpc missing/quarantined, or repeated start failures). Gating on "not
// unavailable" — rather than "logged in" — preserves the Linux boot path,
// where an app can reconcile before frpc finishes logging in and AddTunnel/
// Reload bring the client up.
func (frpTm *FrpTunnelManager) TunnelCapable() bool {
	return TunnelCapability(frpTm.capability.Load()) != CapabilityUnavailable
}

// MarkUnavailable records that tunnels cannot run on this device (e.g. a
// platform where frpc is not yet delivered). Used instead of attempting a
// start that is known to fail.
func (frpTm *FrpTunnelManager) MarkUnavailable(reason string) {
	frpTm.setCapability(CapabilityUnavailable, errors.New(reason))
}

// Capability returns the current capability and the last error string, for
// diagnostics surfaced through get_agent_metadata.
func (frpTm *FrpTunnelManager) Capability() (TunnelCapability, string) {
	frpTm.capabilityMu.Lock()
	defer frpTm.capabilityMu.Unlock()
	return TunnelCapability(frpTm.capability.Load()), frpTm.lastErr
}

// frpsReachable reports whether the route frpc is configured for reaches frps
// (see routeReachable).
//
// This is the agent's only trustworthy capability signal. Watching frpc's stdout
// for "login to server success" does not work in production: frp.go points
// frpc's logger at a file, so that line lands in /var/log/frpc.log and never in
// the pipe this process reads. A dial answers the question the UI badge actually
// asks — can this device reach the tunnel server — without depending on how frpc
// happens to be logging.
//
// It deliberately proves reachability only, not a completed frp handshake: a
// reachable-but-rejecting frps still reads as capable. That is the safe
// direction to be wrong in, and it matches the pre-existing behaviour of
// treating a device as capable unless proven otherwise.
//
// The one exception is the server certificate. Where frpc verifies it, the
// probe completes a TLS handshake with the very trust frpc gets (same CA file,
// same server name), because a certificate frpc refuses leaves the device
// without tunnels just as surely as a closed port, and "available" would hide
// the reason.
func (frpTm *FrpTunnelManager) frpsReachable() error {
	return frpTm.routeReachable(frpTm.configBuilder.route(), false)
}

// routeReachable dials frps over route as frpc would and reports whether the
// connection gets through: the route's proxy (dialTunnelServer), then TLS with
// frpc's trust where frpc verifies, and on the wss route TLS in any case — it
// is that transport.
//
// openWebsocket also asks a wss route for frp's websocket (requestFrpWebsocket)
// — for deciding to move a client onto a route or keep it there, never for the
// periodic capability probe. frps logs every connection that closes before
// sending a byte as a WARNING, which would be one per device per minute.
func (frpTm *FrpTunnelManager) routeReachable(route tunnelRoute, openWebsocket bool) error {
	yamlConfig := frpTm.configBuilder.yamlConfig
	addr := net.JoinHostPort(yamlConfig.ServerAddr, strconv.Itoa(route.Port))

	conn, err := dialTunnelServer(addr, route.ProxyURL)
	if errors.Is(err, errUnprobeableProxy) {
		log.Debug().Err(err).Stringer("route", route).Msg("Cannot probe the tunnel server through this proxy; taking it as reachable")
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	caFile := ""
	serverName := yamlConfig.ServerAddr // frpc's own fallback
	if yamlConfig.Transport != nil && yamlConfig.Transport.TLS != nil {
		caFile = yamlConfig.Transport.TLS.TrustedCaFile
		if yamlConfig.Transport.TLS.ServerName != "" {
			serverName = yamlConfig.Transport.TLS.ServerName
		}
	}

	if route.Protocol != "wss" {
		if caFile == "" {
			return nil
		}
		_, err = verifyTunnelServer(conn, caFile, serverName)
		return err
	}

	tlsConn, err := verifyTunnelServer(conn, caFile, serverName)
	if err != nil || !openWebsocket {
		return err
	}
	return requestFrpWebsocket(tlsConn, serverName)
}

// errUnprobeableProxy marks a proxy scheme frpc can use but the probe cannot.
var errUnprobeableProxy = errors.New("the tunnel probe cannot speak this proxy scheme")

// dialTunnelServer connects to addr the way frpc's control connection does:
// through proxyURL when there is one, directly otherwise. A direct dial while
// frpc goes through a proxy answers a different question, and on the networks
// the wss route exists for the proxy is the very thing refusing CONNECT to
// 7000.
//
// It speaks the proxies frpc's dialer does (fatedier/golib net): http and
// https with a CONNECT, and socks5. For the rest (ntlm) it dials nothing and
// returns errUnprobeableProxy.
func dialTunnelServer(addr, proxyURL string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), frpsProbeTimeout)
	defer cancel()

	if proxyURL == "" {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}

	proxy, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("tunnel proxy URL: %w", err)
	}

	switch proxy.Scheme {
	case "http", "https":
		return dialThroughHTTPProxy(ctx, proxy, addr)
	case "socks5":
		dialer, err := xproxy.FromURL(proxy, &net.Dialer{})
		if err != nil {
			return nil, err
		}
		contextDialer, ok := dialer.(xproxy.ContextDialer)
		if !ok {
			return nil, fmt.Errorf("%w: %s", errUnprobeableProxy, proxy.Scheme)
		}
		return contextDialer.DialContext(ctx, "tcp", addr)
	default:
		return nil, fmt.Errorf("%w: %s", errUnprobeableProxy, proxy.Scheme)
	}
}

// dialThroughHTTPProxy opens a tunnel to addr through an HTTP CONNECT proxy —
// over TLS to the proxy for an https URL, with basic credentials from the URL —
// sending what golib's httpProxyAfterHook sends for frpc.
func dialThroughHTTPProxy(ctx context.Context, proxy *url.URL, addr string) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", proxy.Host)
	if err != nil {
		return nil, err
	}
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)

	if proxy.Scheme == "https" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: proxy.Hostname()})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, err
		}
		conn = tlsConn
	}

	request := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: http.Header{},
	}
	if proxy.User != nil {
		password, _ := proxy.User.Password()
		credentials := base64.StdEncoding.EncodeToString([]byte(proxy.User.Username() + ":" + password))
		request.Header.Set("Proxy-Authorization", "Basic "+credentials)
	}
	// golib's, not Go's default: proxies filter on the user agent.
	request.Header.Set("User-Agent", "Mozilla/5.0")

	if err := request.Write(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), request)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy refused CONNECT to %s: %s", addr, response.Status)
	}

	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// verifyTunnelServer runs a TLS handshake on conn against the CA bundle at
// caFile and serverName, and returns the TLS connection. A certificate the
// bundle does not vouch for comes back as a *tls.CertificateVerificationError
// carrying the x509 reason, and names the escape hatch: the usual cause in the
// field is a network that intercepts TLS with a CA the device does not hold
// (on Windows, also a root the platform installs only on demand and the
// exported store therefore lacks).
//
// An empty caFile accepts any certificate, as frpc does without a
// trustedCaFile; only the wss route handshakes then, since TLS is its
// transport.
func verifyTunnelServer(conn net.Conn, caFile, serverName string) (*tls.Conn, error) {
	tlsConfig := &tls.Config{ServerName: serverName, InsecureSkipVerify: caFile == ""} //nolint:gosec // G402: documented plain-mode route: without a CA file there is no TLS to verify (see doc comment)
	if caFile != "" {
		bundle, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("tunnel CA bundle unreadable: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(bundle) {
			return nil, fmt.Errorf("tunnel CA bundle %s holds no certificates", caFile)
		}
		tlsConfig.RootCAs = pool
	}

	ctx, cancel := context.WithTimeout(context.Background(), frpsProbeTimeout)
	defer cancel()

	tlsConn := tls.Client(conn, tlsConfig)
	err := tlsConn.HandshakeContext(ctx)
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return nil, fmt.Errorf("tunnel server certificate for %s is not trusted (set %s=true in the agent's environment to skip verification): %w",
			serverName, TUNNEL_TLS_INSECURE_SKIP_VERIFY, err)
	}
	if err != nil {
		return nil, err
	}
	return tlsConn, nil
}

// requestFrpWebsocket asks for frp's websocket endpoint on conn as frpc's wss
// transport does once TLS is up, and requires the upgrade. TLS alone also
// succeeds against a 443 that does not relay that path to frps (an ingress
// switched off with FRPS_WS_INGRESS=false, or older than the relay), and
// moving a device onto such a route would trade a dead port for a dead path.
// Like the rest of the probe it stops short of frp's own login.
func requestFrpWebsocket(conn net.Conn, host string) error {
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		return err
	}

	request := &http.Request{
		Method: http.MethodGet,
		// RawPath keeps the "!" literal. Go would send Path alone as
		// /~%21frp, and frps picks websocket connections out of its bindPort
		// by the raw bytes "GET /~!frp" — anything else it hangs up on.
		URL:    &url.URL{Scheme: "https", Host: host, Path: frpWebsocketPath, RawPath: frpWebsocketPath},
		Host:   host,
		Header: http.Header{},
	}
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", base64.StdEncoding.EncodeToString(key))
	// frps's websocket handler (golang.org/x/net/websocket) answers 403 to a
	// request without an Origin; this is the one frpc sends.
	request.Header.Set("Origin", "http://"+host)
	// frpc sends none, and an inspecting proxy may filter on it; empty omits
	// Go's default.
	request.Header.Set("User-Agent", "")

	_ = conn.SetDeadline(time.Now().Add(frpsProbeTimeout))
	if err := request.Write(conn); err != nil {
		return err
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), request)
	if err != nil {
		return err
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusSwitchingProtocols {
		return fmt.Errorf("the tunnel server answered %q instead of opening frp's websocket at %s", response.Status, frpWebsocketPath)
	}
	return nil
}

// alternativeRoute returns the first of the device's other routes that reaches
// frps, for when current does not. A device with one route (every appliance
// and dev stack) has nothing to try and dials nothing here.
func (frpTm *FrpTunnelManager) alternativeRoute(current tunnelRoute) (tunnelRoute, bool) {
	for _, route := range frpTm.configBuilder.routes {
		if route == current {
			continue
		}
		err := frpTm.routeReachable(route, true)
		if err == nil {
			return route, true
		}
		log.Debug().Err(err).Stringer("route", route).Msg("The other tunnel route does not reach the tunnel server either")
	}
	return tunnelRoute{}, false
}

// selectRoute settles the route the client about to start will use. It keeps
// the configured route while that one reaches frps and otherwise moves the
// config to another that does; with none working it changes nothing, frpc
// retries where it is and the capability probe reports why. Sticky on purpose:
// a device that fell back to 443 stays there while 443 works instead of
// flipping on every restart, and tries 7000 first again when the agent starts.
// Callers must hold startMu.
func (frpTm *FrpTunnelManager) selectRoute() {
	if len(frpTm.configBuilder.routes) < 2 {
		return
	}

	current := frpTm.configBuilder.route()
	err := frpTm.routeReachable(current, true)
	if err == nil {
		return
	}

	route, ok := frpTm.alternativeRoute(current)
	if !ok {
		return
	}

	log.Warn().Err(err).Stringer("from", current).Stringer("to", route).
		Msg("The tunnel server is out of reach on the configured route; moving the tunnel client to one that reaches it")
	frpTm.configBuilder.useRoute(route)
}

// probeCapability sets the capability from a live reachability check. It is a
// no-op unless an frpc process is running, so a start failure that already
// latched Unavailable (missing or quarantined binary) is not overwritten by a
// frps that merely happens to be up.
func (frpTm *FrpTunnelManager) probeCapability() {
	if !frpTm.clientAlive.Load() {
		return
	}

	err := frpTm.frpsReachable()
	if err != nil {
		// The network changed under a running client: its route no longer
		// reaches frps but another one does. That is a restart, not a device
		// unable to tunnel — frpc keeps the route it started with, and Start
		// moves the config over (selectRoute).
		current := frpTm.configBuilder.route()
		if route, ok := frpTm.alternativeRoute(current); ok {
			log.Info().Err(err).Stringer("from", current).Stringer("to", route).
				Msg("The tunnel route in use no longer reaches the tunnel server; restarting the tunnel client")
			safe.Go(frpTm.SuperviseStart)
			return
		}
	}

	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		frpTm.setCapability(CapabilityUnavailable, err)
		return
	}
	if err != nil {
		frpTm.setCapability(CapabilityUnavailable, fmt.Errorf("frps unreachable: %w", err))
		return
	}

	frpTm.setCapability(CapabilityAvailable, nil)
}

// monitorCapability keeps the reported capability in step with whether frps is
// actually reachable, for as long as a client is running. Without it the verdict
// would be whatever a single probe saw moments after boot — permanently marking
// a device tunnel-disabled because frps came up a few seconds later, or claiming
// tunnels work long after the tunnel service was switched off.
func (frpTm *FrpTunnelManager) monitorCapability() {
	ticker := time.NewTicker(capabilityProbeInterval)
	defer ticker.Stop()

	for range ticker.C {
		frpTm.probeCapability()
	}
}

// tunnelStateFingerprint reduces the reported tunnel states to a canonical
// string so the monitor can tell a genuine change from an identical re-read.
// Sorted, because GetState's order follows the config file and must not be what
// decides whether the cloud gets nudged.
func tunnelStateFingerprint(states []TunnelState) string {
	parts := make([]string, 0, len(states))

	for _, state := range states {
		var name, status string
		var remotePort uint64
		if state.Status != nil {
			name = state.Status.Name
			status = state.Status.Status
			remotePort = state.Status.RemotePort
		}

		parts = append(parts, fmt.Sprintf("%s|%s|%d|%t|%t|%s|%s",
			name, status, remotePort, state.Active, state.Error, state.ErrorMessage, state.URL))
	}

	sort.Strings(parts)
	return strings.Join(parts, ";")
}

// monitorTunnelState nudges the cloud whenever this device's proxies change.
//
// The publish is only a trigger: REtunnel ignores the payload and re-queries
// frps for authoritative state, so this needs to be reliable, not detailed.
// Reliability is exactly what the previous mechanism lacked — it reacted to
// "start proxy success" on frpc's stdout, but frp.go points frpc's logger at a
// file, so those lines never arrive and the cloud was never nudged at all.
// Reading frpc's admin API instead keeps this working regardless of how frpc
// logs.
//
// GetState is called unconditionally: AllStatus already no-ops unless the device
// is Available (so an unreachable frps cannot start a restart loop). An frpc
// that died is restarted by its own exit (onClientExit).
func (frpTm *FrpTunnelManager) monitorTunnelState() {
	ticker := time.NewTicker(tunnelStatePollInterval)
	defer ticker.Stop()

	lastFingerprint := ""

	for range ticker.C {
		states, err := frpTm.GetState()
		if err != nil {
			// frpc restarting or its admin API not up yet — try again next tick.
			continue
		}

		fingerprint := tunnelStateFingerprint(states)
		if fingerprint == lastFingerprint {
			continue
		}

		lastFingerprint = fingerprint

		err = frpTm.PublishTunnelState()
		if err != nil {
			log.Debug().Err(err).Msg("failed to publish tunnel state change")
			// Re-publish next tick rather than sitting on an unsent change.
			lastFingerprint = ""
		}
	}
}

// setCapability records the capability and publishes the change so the UI
// reflects a mid-run flip (e.g. antivirus quarantining frpc) without waiting
// for a re-fetch of get_agent_metadata.
func (frpTm *FrpTunnelManager) setCapability(c TunnelCapability, err error) {
	frpTm.capabilityMu.Lock()
	prev := TunnelCapability(frpTm.capability.Load())
	frpTm.capability.Store(int32(c))
	if err != nil {
		frpTm.lastErr = err.Error()
	} else if c == CapabilityAvailable {
		frpTm.lastErr = ""
	}
	frpTm.capabilityMu.Unlock()

	if prev != c && (c == CapabilityAvailable || c == CapabilityUnavailable) {
		safe.Go(func() {
			pubErr := frpTm.PublishTunnelState()
			if pubErr != nil {
				log.Debug().Err(pubErr).Msg("failed to publish tunnel state on capability change")
			}
		})
	}

	// Becoming able to tunnel is when the tunnels syncPortState skipped while
	// the device could not, and the adds that failed while frpc was starting,
	// can finally come up — and no state push, reconnect or transition is
	// tied to this moment to ask for them.
	if prev != c && c == CapabilityAvailable {
		if onAvailable := frpTm.onAvailable.Load(); onAvailable != nil {
			safe.Go(*onAvailable)
		}
	}
}

// ensureFrpcBinary re-acquires frpc if it is missing on disk (antivirus can
// delete it after install). Returns nil when the binary is present.
func (frpTm *FrpTunnelManager) ensureFrpcBinary() error {
	frpcPath := filesystem.GetTunnelBinaryPath(frpTm.config, "frpc")
	_, err := os.Stat(frpcPath)
	if err == nil {
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}

	if frpTm.reacquireFrpc == nil {
		return fmt.Errorf("frpc binary missing at %s and no re-acquire available", frpcPath)
	}

	log.Warn().Msgf("frpc binary missing at %s, attempting to re-acquire", frpcPath)
	reErr := frpTm.reacquireFrpc()
	if reErr != nil {
		return fmt.Errorf("frpc binary missing and re-acquire failed: %w", reErr)
	}
	return nil
}

// stopClientLocked kills the tracked frpc client (if any) and waits until its
// reader goroutine has reaped it, so the admin port is actually released
// before a new client binds it. Callers must hold startMu.
func (frpTm *FrpTunnelManager) stopClientLocked() {
	if frpTm.clientProcess == nil || frpTm.clientProcess.Process == nil {
		frpTm.clientProcess = nil
		frpTm.clientDone = nil
		frpTm.clientRun = nil
		return
	}

	// Stopped on purpose: the exit must not restart the client.
	if frpTm.clientRun != nil {
		frpTm.clientRun.stop()
	}

	// Kill errors just mean the process is already gone; reaping below is what
	// guarantees the port is free either way.
	_ = frpTm.clientProcess.Process.Kill()

	if frpTm.clientDone != nil {
		select {
		case <-frpTm.clientDone:
		case <-time.After(10 * time.Second):
			log.Warn().Msg("timed out waiting for previous frpc process to be reaped")
		}
	}

	frpTm.clientProcess = nil
	frpTm.clientDone = nil
	frpTm.clientRun = nil
}

// onClientExit restarts a client that exited on its own after Start handed it
// over: antivirus ending it (Defender's PUA:Win32/FRProxy on a Windows PC), a
// crash, someone killing the process. Nothing else notices on Windows — the
// restart paths in AllStatus and Reload match a "connection refused" that
// Windows words differently — so the tunnels stayed down until the agent
// restarted.
//
// A client that ran for the retry interval worked, so whatever ends it (or a
// deliberate stop) starts the failure count over. One lost sooner counts as a
// failed start. Restarted at once instead, an frpc that antivirus lets log in
// and ends under a minute later came back every minute: on the LAC PC that was
// a Defender detection a minute, all night (2026-10-27).
func (frpTm *FrpTunnelManager) onClientExit(run *frpcRun) {
	lost, ranFor := run.exit()
	worked := ranFor >= frpTm.retryInterval()
	if worked {
		frpTm.startFailures.Store(0)
	}
	if !lost {
		return
	}

	if !worked {
		frpTm.countStartFailure()
	}
	log.Warn().Dur("ranFor", ranFor).Msg("the tunnel client exited unexpectedly; restarting it")
	safe.Go(frpTm.SuperviseStart)
}

// frpcEnv is the environment every frpc process is spawned with: the agent's
// own, minus every proxy variable.
//
// The proxy decision belongs to the agent — it is resolved against NO_PROXY and
// written into transport.proxyURL (resolveTunnelProxy in frp.go). frpc must not
// be able to override that from an inherited machine-wide http_proxy, which is
// precisely how a corporate-proxy Windows host ended up routing its frps
// control connection into a proxy that answered 407 while every other
// connection it made to the same appliance went direct.
//
// This is the half that actually enforces "no proxy": measured against frpc
// 0.70.0, a config carrying `proxyURL: ""` still dials http_proxy from the
// environment, and only stops once these variables are gone. A non-empty
// proxyURL in the config does win over the environment, so a device that
// genuinely needs a proxy is unaffected by the stripping.
func frpcEnv() []string {
	strip := map[string]bool{
		"http_proxy":  true,
		"https_proxy": true,
		"all_proxy":   true,
		"no_proxy":    true,
	}

	environ := os.Environ()
	env := make([]string, 0, len(environ))
	for _, entry := range environ {
		key, _, found := strings.Cut(entry, "=")
		if found && strip[strings.ToLower(key)] {
			continue
		}
		env = append(env, entry)
	}

	return env
}

func (frpTm *FrpTunnelManager) Start() error {
	frpTm.startMu.Lock()
	defer frpTm.startMu.Unlock()

	log.Debug().Msg("Starting tunnel client")

	// A previous client may still be alive (e.g. its admin server broke but
	// the process survived, or an earlier start raced). It holds the admin
	// port; spawning next to it guarantees "bind: address already in use".
	frpTm.stopClientLocked()

	// The new process has not logged in yet: reset the flag and drain stale
	// login signals so waitForLogin cannot pass on a token from the previous
	// client's session.
	frpTm.loginMutex.Lock()
	frpTm.isLoggedIn = false
	frpTm.loginMutex.Unlock()
	for {
		select {
		case <-frpTm.loginChan:
			continue
		default:
		}
		break
	}

	// The binary may be absent (never downloaded, or quarantined at runtime);
	// re-acquire it before spawning rather than looping on a missing file.
	err := frpTm.ensureFrpcBinary()
	if err != nil {
		frpTm.setCapability(CapabilityUnavailable, err)
		log.Error().Err(err).Msg("cannot start tunnel client")
		return err
	}

	// frpc loads its CA bundle once, at startup. Rebuilding it here hands a
	// restarted client whatever the host store has gained since the config
	// was last written (a renewed corporate root, say).
	frpTm.configBuilder.writeTunnelCA()

	frpTm.setCapability(CapabilityStarting, nil)

	// Before frpc reads the config: once running it cannot change routes.
	frpTm.selectRoute()

	frpcPath := filesystem.GetTunnelBinaryPath(frpTm.config, "frpc")

	ctx, cancelNotifyContext := signal.NotifyContext(context.Background(), os.Interrupt)
	frpCommand := exec.CommandContext(ctx, frpcPath, "-c", frpTm.configBuilder.ConfigPath) //nolint:gosec // G204: frpc binary installed by the agent itself, with its own config
	frpCommand.Dir = filepath.Dir(frpcPath)
	frpCommand.Env = frpcEnv()
	setPdeathsig(frpCommand)

	stdout, err := frpCommand.StdoutPipe()
	if err != nil {
		cancelNotifyContext()
		frpTm.setCapability(CapabilityUnavailable, err)
		log.Error().Stack().Err(err).Msg("Failed to get stdout pipe for frp command")
		return err
	}

	err = frpCommand.Start()
	if err != nil {
		cancelNotifyContext()
		frpTm.setCapability(CapabilityUnavailable, err)
		log.Error().Stack().Err(err).Msg("Failed to start frp command")
		return err
	}

	clientDone := make(chan struct{})
	run := &frpcRun{spawned: time.Now()}
	frpTm.clientProcess = frpCommand
	frpTm.clientDone = clientDone
	frpTm.clientRun = run
	frpTm.clientAlive.Store(true)

	// A client exists from here on, so keep re-checking whether it can still
	// reach frps and whether its proxies changed. Started here rather than at
	// boot because both are only meaningful once there is a process to speak for.
	frpTm.monitorOnce.Do(func() {
		safe.Go(frpTm.monitorCapability)
		safe.Go(frpTm.monitorTunnelState)
	})

	// Buffered + guarded so exactly one result is delivered: login success, or
	// the scanner ending (process exited) before login. Without the latter,
	// Start() blocked forever whenever frpc crashed before logging in.
	ackChan := make(chan error, 1)
	var ackOnce sync.Once
	ack := func(e error) { ackOnce.Do(func() { ackChan <- e }) }

	go func() {
		// Deferred order (LIFO): ack a pre-login death, cancel the context
		// (which kills frpc if it is somehow still alive after stdout EOF),
		// reap the process so it neither lingers as a zombie nor keeps the
		// admin port, and only then signal done to stopClientLocked waiters.
		// Last of all, restart a client that was lost rather than stopped.
		defer frpTm.onClientExit(run)
		defer close(clientDone)
		defer func() { _ = frpCommand.Wait() }()
		defer cancelNotifyContext()
		// No process left to speak for: park the capability probe until the
		// supervisor spawns a replacement.
		defer frpTm.clientAlive.Store(false)
		// If the loop exits without a prior ack, frpc died during Start. Not
		// necessarily before it logged in: its login line goes to the log
		// file, not to this pipe (see defaultLoginDeadline).
		defer ack(errFrpcExitedDuringStart)

		scanner := bufio.NewScanner(stdout)

		for scanner.Scan() {
			line := scanner.Text()

			if strings.Contains(line, "login to server success") {
				// Mark as logged in
				frpTm.loginMutex.Lock()
				frpTm.isLoggedIn = true
				frpTm.loginMutex.Unlock()

				// Signal login (non-blocking)
				select {
				case frpTm.loginChan <- true:
				default:
				}

				// Mark the device tunnel-capable here, not only via the startup
				// ack below: if Start() already returned CapabilityUnavailable on
				// the login deadline (frps was down/disabled), the ack is no
				// longer being read — but frpc keeps retrying, so a login that
				// lands later must still flip the device back to Available. This
				// is the self-heal path when a disabled frps is turned back on.
				frpTm.setCapability(CapabilityAvailable, nil)

				// Send initial ack for startup
				ack(nil)
			}

			// Error was found
			if strings.Contains(line, "[E]") {
				tunnelIdMatch := tunnelIdRegexp.FindStringSubmatch(line)
				if len(tunnelIdMatch) > 1 {
					tunnelID := tunnelIdMatch[1]

					errMatch := errMessageRegexp.FindStringSubmatch(line)
					if len(errMatch) > 1 {
						errMessage := errMatch[1]
						frpTm.tunnelsLock.Lock()
						activeTunnel := frpTm.activeTunnelConfigs[tunnelID]
						if activeTunnel != nil {
							activeTunnel.Error = errMessage
						} else {
							log.Error().Msgf("failed to get tunnel with ID %s", tunnelID)
						}
						frpTm.tunnelsLock.Unlock()
					}

				}

				log.Error().Msgf("frp-err: %s", line)

			} else {
				// Best-effort only. In production frp.go sends frpc's logger to a
				// file, so these lines never reach this pipe and none of the
				// branches below fire — monitorTunnelState is what actually keeps
				// the cloud in sync. Only startup-fatal output (e.g. the admin
				// server failing to bind, handled below) reliably lands here.
				proxyStarted := strings.Contains(line, "start proxy success")
				proxyRemoved := strings.Contains(line, "proxy removed")
				runAdminServerError := strings.Contains(line, "run admin server error")

				if runAdminServerError {
					log.Debug().Msgf("Tunnel process failed to setup admin server: %s", line)

					// Re-pick (and persist) the webserver port, then hand
					// control back to the supervisor via a failed ack rather
					// than recursively restarting from inside the scanner
					// goroutine. The early return triggers the deferred
					// context cancel, which kills this frpc — it must not
					// keep running admin-less next to its replacement.
					frpTm.configBuilder.SetAdminPort()
					ack(errors.New("frpc failed to start its admin server"))

					return
				}

				if proxyStarted || proxyRemoved {
					safe.Go(func() {
						matches := proxyNameRegex.FindStringSubmatch(line)

						if len(matches) > 1 {
							deviceKeyStr := matches[1]
							appName := matches[2]
							localPortStr := matches[3]
							protocol := matches[4]

							updateType := STARTED
							if proxyRemoved {
								updateType = REMOVED
							}

							deviceKey, err := strconv.ParseInt(deviceKeyStr, 10, 64)
							if err != nil {
								return
							}

							localPort, err := strconv.ParseInt(localPortStr, 10, 64)
							if err != nil {
								return
							}

							tunnelUpdate := TunnelUpdate{
								DeviceKey:  uint64(deviceKey),
								AppName:    appName,
								LocalPort:  uint64(localPort),
								UpdateType: updateType,
								Protocol:   Protocol(protocol),
							}

							// Non-blocking: the channel currently has no
							// consumer (AddTunnel's wait loop is retired), and
							// an unbuffered send would park this goroutine
							// forever — one leak per proxy event, growing on
							// every frpc restart.
							select {
							case frpTm.tunnelUpdateChan <- tunnelUpdate:
							default:
							}
						}

					})

					safe.Go(func() {
						// Wait for frpc to complete login to frps server
						// This prevents publishing "offline" state before connection is established
						frpTm.waitForLogin()

						pubErr := frpTm.PublishTunnelState()
						if pubErr != nil {
							log.Error().Err(pubErr).Msg("Failed to publish tunnel state")
						}
					})
				}

				log.Info().Msgf("frp-out: %s", line)
			}
		}
	}()

	deadline := frpTm.loginDeadline
	if deadline <= 0 {
		deadline = defaultLoginDeadline
	}

	select {
	case startErr := <-ackChan:
		if startErr == nil && !run.establish() {
			startErr = errFrpcExitedDuringStart
		}
		if startErr != nil {
			frpTm.setCapability(CapabilityUnavailable, startErr)
			return startErr
		}

		frpTm.setCapability(CapabilityAvailable, nil)
		return nil

	case <-time.After(deadline):
		if !run.establish() {
			frpTm.setCapability(CapabilityUnavailable, errFrpcExitedDuringStart)
			return errFrpcExitedDuringStart
		}

		// No login ack — which is the NORMAL case, not a failure: frpc logs to a
		// file rather than the stdout scanned above, so the success line is
		// invisible to us. Deciding "unavailable" here would mark every healthy
		// device tunnel-disabled. Ask the network instead: if the frps control
		// port accepts a connection the device can tunnel, otherwise it cannot.
		//
		// frpc is left running either way (it retries on its own), and the
		// monitor started above keeps re-checking, so this verdict is never
		// permanent in either direction.
		frpTm.probeCapability()
		return nil
	}
}

// SuperviseStart brings the tunnel client up and keeps trying until it runs,
// paced by startRetryDelay; the device reports CapabilityUnavailable while it
// fails, so it degrades cleanly instead of hanging. It used to give up after six
// attempts, which left a Windows PC whose antivirus had ended frpc for a few
// minutes without tunnels until the agent restarted (LAC PC, 2026-10-29).
// Returns once a client is handed over, or when Stop ends it — call it from a
// goroutine.
func (frpTm *FrpTunnelManager) SuperviseStart() {
	// Single-flight: the boot path, a failed Reload and a dead admin API can
	// all request supervision around the same moment. Competing loops used to
	// spawn frpc against frpc — the loser died on the shared admin port with
	// "bind: address already in use" and its failure marked tunnels
	// unavailable even while a healthy client was running.
	if !frpTm.supervising.CompareAndSwap(false, true) {
		log.Debug().Msg("tunnel client supervisor already running, skipping")
		return
	}
	defer frpTm.supervising.Store(false)

	frpTm.startMu.Lock()
	stopped := frpTm.supervisionStop
	frpTm.startMu.Unlock()

	for {
		if failures := int(frpTm.startFailures.Load()); failures > 0 {
			// A client lost soon after it started (onClientExit) left the
			// capability Available, though nothing runs while this waits.
			if failures >= fastStartAttempts {
				if capability, _ := frpTm.Capability(); capability != CapabilityUnavailable {
					frpTm.setCapability(CapabilityUnavailable, errors.New("the tunnel client keeps exiting soon after it starts"))
				}
			}

			select {
			case <-time.After(frpTm.startRetryDelay(failures)):
			case <-stopped:
				return
			}
		}

		err := frpTm.Start()
		if err == nil {
			log.Info().Msg("tunnel client started")
			return
		}

		failures := frpTm.countStartFailure()
		log.Warn().Err(err).Msgf("tunnel client start attempt failed (%d in a row); retrying in %s", failures, frpTm.startRetryDelay(failures))

		// A pre-login exit is regularly the admin port having been taken
		// while frpc was down (any outbound localhost connection can hold
		// it). Retrying on the same port then fails identically every time
		// — re-pick a free one for the next attempt.
		frpTm.configBuilder.SetAdminPort()
	}
}

// startRetryDelay is the supervisor's wait after failures (at least one)
// consecutive failures: the backoff doubling from startRetryBackoff (2 s to
// 32 s) for the first fastStartAttempts, then the retry interval, for as long
// as it takes.
// Antivirus that ends frpc does so in the minutes after a scan or a definitions
// update, and starts outside that window survive; an interval longer than the
// window lets the next attempt outlive it without a new detection a minute.
func (frpTm *FrpTunnelManager) startRetryDelay(failures int) time.Duration {
	if failures >= fastStartAttempts {
		return frpTm.retryInterval()
	}

	backoff := frpTm.startRetryBackoff
	if backoff <= 0 {
		backoff = defaultStartRetryBackoff
	}
	return backoff << (failures - 1)
}

// retryInterval is how often the supervisor retries after the fast attempts,
// and how long a client has to run to count as having worked (onClientExit).
func (frpTm *FrpTunnelManager) retryInterval() time.Duration {
	if frpTm.startRetryInterval <= 0 {
		return defaultStartRetryInterval
	}
	return frpTm.startRetryInterval
}

// countStartFailure counts a client that failed to start or did not run for
// the retry interval, and returns the count.
func (frpTm *FrpTunnelManager) countStartFailure() int {
	failures := int(frpTm.startFailures.Add(1))
	if failures == fastStartAttempts {
		log.Error().Msgf("the tunnel client keeps failing; tunnels are unavailable on this device, retrying every %s", frpTm.retryInterval())
	}
	return failures
}

func (frpTm *FrpTunnelManager) Stop() error {
	frpTm.startMu.Lock()
	defer frpTm.startMu.Unlock()

	if frpTm.supervisionStop != nil {
		close(frpTm.supervisionStop)
	}
	frpTm.supervisionStop = make(chan struct{})

	frpTm.stopClientLocked()
	return nil
}

func (frpTm *FrpTunnelManager) Reset() error {
	frpTm.configBuilder.Reset()

	return frpTm.Reload()
}

// waitForLogin waits for frpc to successfully login to frps server
// If already logged in, returns immediately
// If not logged in, waits up to 5 seconds for login confirmation
func (frpTm *FrpTunnelManager) waitForLogin() {
	frpTm.loginMutex.RLock()
	if frpTm.isLoggedIn {
		frpTm.loginMutex.RUnlock()
		log.Debug().Msg("frpc already logged in, publishing tunnel state immediately")
		return
	}
	frpTm.loginMutex.RUnlock()

	log.Debug().Msg("Waiting for frpc login confirmation before publishing tunnel state")

	// Wait for login signal with timeout
	select {
	case <-frpTm.loginChan:
		log.Info().Msg("frpc login confirmed, proceeding with tunnel state publication")
	case <-time.After(5 * time.Second):
		log.Warn().Msg("Timeout waiting for frpc login, publishing tunnel state anyway")
	}
}

func (frpTm *FrpTunnelManager) PublishTunnelState() error {
	updateTopic := common.BuildTunnelStateUpdate(frpTm.config.ReswarmConfig.SerialNumber)
	tunnelStates, err := frpTm.GetState()
	if err != nil {
		return err
	}

	var args []interface{}
	for _, tunnelState := range tunnelStates {
		args = append(args, tunnelState)
	}

	return frpTm.messenger.Publish(topics.Topic(updateTopic), args, nil, nil)
}

func NewFrpTunnelManager(messenger messenger.Messenger, config *config.Config) (*FrpTunnelManager, error) {

	configBuilder := NewTunnelConfigBuilder(config)
	frpTunnelManager := &FrpTunnelManager{
		configBuilder:       configBuilder,
		messenger:           messenger,
		config:              config,
		tunnelsLock:         &sync.RWMutex{},
		tunnelUpdateChan:    make(chan TunnelUpdate),
		activeTunnelConfigs: make(map[string]*Tunnel),
		loginChan:           make(chan bool, 10), // Buffered to avoid blocking
		isLoggedIn:          false,
		loginDeadline:       defaultLoginDeadline,
		startRetryBackoff:   defaultStartRetryBackoff,
		startRetryInterval:  defaultStartRetryInterval,
		supervisionStop:     make(chan struct{}),
	}

	return frpTunnelManager, nil
}

func (frpTm *FrpTunnelManager) SetMessenger(messenger messenger.Messenger) {
	frpTm.messenger = messenger
}

func (frpTm *FrpTunnelManager) reserveRemotePort(port uint64, protocol Protocol, reserved bool) (uint64, error) {
	args := []interface{}{
		common.Dict{
			"port":     port,
			"protocol": string(protocol),
			// device_key lets the port manager attribute the request; reserved
			// switches it to strict validation (range hard-fail, collision ->
			// error) instead of falling back to an allocated port. Older
			// servers ignore both keys.
			"device_key": frpTm.config.ReswarmConfig.DeviceKey,
			"reserved":   reserved,
		},
	}

	result, err := frpTm.messenger.Call(context.Background(), topics.ExposePort, args, common.Dict{}, nil, nil)
	if err != nil {
		return 0, err
	}

	if len(result.Arguments) == 0 {
		return 0, errors.New("arguments is empty")
	}

	payloadArg := result.Arguments[0]
	payload, ok := payloadArg.(map[string]interface{})
	if !ok {
		return 0, errors.New("failed to parse payload")
	}

	remotePort, ok := common.ToUint64(payload["remote_port"])
	if !ok || remotePort == 0 {
		return 0, errors.New("failed to parse remote_port")
	}

	return remotePort, nil
}

func (frpTm *FrpTunnelManager) Reload() error {
	frpcPath := filesystem.GetTunnelBinaryPath(frpTm.config, "frpc")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	reloadCommand := exec.CommandContext(ctx, frpcPath, "reload", "-c", frpTm.configBuilder.ConfigPath) //nolint:gosec // G204: frpc binary installed by the agent itself, with its own config
	reloadCommand.Env = frpcEnv()

	output, err := reloadCommand.CombinedOutput()
	if err != nil {
		// frpc service is not properly running. Windows words the refusal
		// differently (and localized), so a client known not to be running
		// counts as well.
		log.Error().Err(err).Msgf("Error while reloading frp client config: %s", string(output))
		if !frpTm.clientAlive.Load() || strings.Contains(string(output), "connect: connection refused") || strings.Contains(string(output), "api status code") {
			log.Warn().Msg("frpc appears to not be running, attempting to start it")
			safe.Go(func() {
				// Re-supervise so a binary that was quarantined/removed at
				// runtime is re-acquired and the capability flips correctly.
				frpTm.SuperviseStart()
			})
			return err
		}
		return err
	}

	return nil
}

func (frpTm *FrpTunnelManager) Get(tunnelID string) *Tunnel {
	frpTm.tunnelsLock.Lock()
	tunnel := frpTm.activeTunnelConfigs[tunnelID]
	frpTm.tunnelsLock.Unlock()

	return tunnel
}

func (frpTm *FrpTunnelManager) buildURL(protocol Protocol, subdomain string, remotePort uint64) string {
	protocolString := string(protocol)

	if remotePort != 0 && protocol != HTTP && protocol != HTTPS {
		return fmt.Sprintf("%s://%s.%s:%d", protocolString, subdomain, frpTm.configBuilder.BaseTunnelURL, remotePort)
	}

	// we always have HTTPS since we tunnel to our HTTPS service
	if protocolString == "http" {
		protocolString = "https"
	}

	return fmt.Sprintf("%s://%s.%s", protocolString, subdomain, frpTm.configBuilder.BaseTunnelURL)
}

func (frpTm *FrpTunnelManager) GetState() ([]TunnelState, error) {
	tunnelConfigs, err := frpTm.GetTunnelConfig()
	if err != nil {
		log.Error().Err(err).Msg("Failed to get tunnel configs")
		return nil, err
	}

	tunnelStatuses, err := frpTm.AllStatus()
	if err != nil {
		log.Error().Err(err).Msg("Failed to get tunnel statuses")
		return nil, err
	}

	tunnelStates := make([]TunnelState, 0)
	for _, tunnelConfig := range tunnelConfigs {
		// Match on the name frpc reports status under. Rebuilding the id from
		// Subdomain silently drops any proxy whose subdomain did not round-trip
		// through the config file.
		tunnelID := tunnelConfig.Name
		if tunnelID == "" {
			tunnelID = CreateTunnelID(tunnelConfig.Subdomain, string(tunnelConfig.Protocol))
		}

		for _, tunnelStatus := range tunnelStatuses {
			if tunnelID != tunnelStatus.Name {
				continue
			}

			tunnelState := TunnelState{
				Status: &tunnelStatus,
				// The declared port, NOT the host port frpc dials: the UI
				// matches tunnel states to port-template entries by this
				// number.
				Port:         tunnelConfig.DeclaredPort,
				AppName:      tunnelConfig.AppName,
				Stage:        tunnelConfig.Stage,
				Error:        tunnelStatus.Error != "",
				ErrorMessage: tunnelStatus.Error,
				Active:       tunnelStatus.Status == "running",
				URL:          frpTm.buildURL(tunnelConfig.Protocol, tunnelConfig.Subdomain, tunnelStatus.RemotePort),
			}

			tunnelStates = append(tunnelStates, tunnelState)
		}

	}

	return tunnelStates, nil
}

func (frpTm *FrpTunnelManager) GetStateById(tunnelID string) (TunnelState, error) {
	states, err := frpTm.GetState()
	if err != nil {
		return TunnelState{}, err
	}

	for _, state := range states {
		if state.Status.Name == tunnelID {
			return state, nil
		}
	}

	return TunnelState{}, errdefs.ErrNotFound
}

func (frpTm *FrpTunnelManager) GetTunnelConfig() ([]TunnelConfig, error) {
	return frpTm.configBuilder.GetTunnelConfig()
}

func (frpTm *FrpTunnelManager) AllStatus() ([]TunnelStatus, error) {
	// Only poll the admin API while frpc is supposed to be up. When the client
	// is Unavailable (frps unreachable, binary quarantined) or still Starting,
	// the connection-refused below would kick off yet another restart — on
	// devices without a reachable frps that turned into an endless
	// kill/sleep/start loop, retriggered by every state publish.
	if capability, _ := frpTm.Capability(); capability != CapabilityAvailable {
		return []TunnelStatus{}, nil
	}

	adminPort, err := frpTm.configBuilder.GetAdminPort()
	if err != nil {
		log.Error().Err(err).Msg("Failed to get admin port from config")
		return []TunnelStatus{}, err
	}

	apiURL := fmt.Sprintf("http://127.0.0.1:%d/api/status", adminPort)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		log.Error().Err(err).Msg("Failed to create HTTP request")
		return []TunnelStatus{}, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if strings.Contains(err.Error(), "connection refused") {
			// frpc was Available but its admin API is gone. A dead process is
			// restarted by its exit (onClientExit); this catches a process
			// that lives on without its admin server (Linux wording only).
			// Re-supervise (single-flight, serialized, port re-picking) instead
			// of a bare kill+start — the latter permanently gave up whenever
			// the old admin port had been taken in the meantime.
			safe.Go(func() {
				frpTm.SuperviseStart()
			})
			return []TunnelStatus{}, nil
		}
		log.Error().Err(err).Msg("Failed to call frpc API")
		return []TunnelStatus{}, nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		log.Error().Msgf("frpc API returned status code: %d", resp.StatusCode)
		return []TunnelStatus{}, fmt.Errorf("API returned status code: %d", resp.StatusCode)
	}

	var statusResp map[string][]FrpStatus
	if err := json.NewDecoder(resp.Body).Decode(&statusResp); err != nil {
		log.Error().Err(err).Msg("Failed to decode JSON response")
		return []TunnelStatus{}, err
	}

	// Convert map to flat array
	tunnelStatuses := make([]TunnelStatus, 0)
	for _, statusArray := range statusResp {
		for _, frpStatus := range statusArray {
			tunnelStatus := TunnelStatus{
				Status:     frpStatus.Status,
				Name:       frpStatus.Name,
				LocalAddr:  frpStatus.LocalAddr,
				Plugin:     frpStatus.Plugin,
				RemoteAddr: frpStatus.RemoteAddr,
				Error:      frpStatus.Err,
				Protocol:   Protocol(frpStatus.Type),
			}

			if frpStatus.RemoteAddr != "" {
				parts := strings.Split(frpStatus.RemoteAddr, ":")
				if len(parts) > 1 {
					remotePortStr := parts[1]
					remotePort, err := strconv.ParseInt(remotePortStr, 10, 64)
					if err != nil {
						log.Error().Msgf("frpStatus.RemoteAddr: %s", frpStatus.RemoteAddr)
						return nil, err
					}
					tunnelStatus.RemotePort = uint64(remotePort)
				} else {
					log.Warn().Msgf("RemoteAddr does not contain port: %s", frpStatus.RemoteAddr)
				}
			}

			tunnelStatuses = append(tunnelStatuses, tunnelStatus)
		}
	}

	return tunnelStatuses, nil
}

func (frpTm *FrpTunnelManager) Status(tunnelID string) (TunnelStatus, error) {
	// Use AllStatus and filter for the specific tunnel
	tunnelStatuses, err := frpTm.AllStatus()
	if err != nil {
		log.Error().Err(err).Msg("Error while getting tunnel status")
		return TunnelStatus{}, err
	}

	for _, tunnelStatus := range tunnelStatuses {
		if tunnelStatus.Name == tunnelID {
			return tunnelStatus, nil
		}
	}

	return TunnelStatus{}, errdefs.ErrNotFound
}

// Tries to reserve an external port for kubernetes, updates the frpc client config and reloads the config file
func (frpTm *FrpTunnelManager) AddTunnel(config TunnelConfig) (TunnelConfig, error) {
	log.Debug().Str("subdomain", config.Subdomain).Str("protocol", string(config.Protocol)).Msg("AddTunnel called")
	tunnelId := CreateTunnelID(config.Subdomain, string(config.Protocol))

	// Claim the tunnel id FIRST, and make a duplicate add an idempotent
	// SUCCESS. The old shape checked the map last: two concurrent syncs for
	// the same rule (several apps reconfiguring at once fan out overlapping
	// port syncs) both reserved a remote port and reloaded, the loser then
	// errored "tunnel already exists" — AFTER its config upsert had
	// overwritten the winner's file entry with a different remote port. The
	// caller recorded a failed add for a tunnel that was actually up, and the
	// file/memory skew persisted the wrong remote port upstream.
	frpTm.tunnelsLock.Lock()
	if existing := frpTm.activeTunnelConfigs[tunnelId]; existing != nil {
		cfg := existing.Config
		frpTm.tunnelsLock.Unlock()
		return cfg, nil
	}
	claimed := &Tunnel{Config: config}
	frpTm.activeTunnelConfigs[tunnelId] = claimed
	frpTm.tunnelsLock.Unlock()

	// Any failure must release the claim — leaving it would make every later
	// sync "skip add" against a tunnel that never came up.
	fail := func(err error) (TunnelConfig, error) {
		frpTm.tunnelsLock.Lock()
		if frpTm.activeTunnelConfigs[tunnelId] == claimed {
			delete(frpTm.activeTunnelConfigs, tunnelId)
		}
		frpTm.tunnelsLock.Unlock()
		return TunnelConfig{}, err
	}

	// Don't need to reserve a port if the user starts an HTTP tunnel
	if config.Protocol != HTTP && config.Protocol != HTTPS {
		// If no remote port is set, we will allocate one
		remotePort, err := frpTm.reserveRemotePort(config.RemotePort, config.Protocol, config.Reserved)
		if err != nil {
			log.Error().Err(err).Msg("Error while reserving remote port")
			return fail(err)
		}
		config.RemotePort = remotePort

		// Publish the reserved port on the claim immediately: a concurrent
		// sync that hits the idempotent-success path above must see the real
		// remote port, not a zero that would be persisted upstream.
		frpTm.tunnelsLock.Lock()
		claimed.Config = config
		frpTm.tunnelsLock.Unlock()
	}

	frpTm.configBuilder.AddTunnelConfig(config)

	if err := frpTm.Reload(); err != nil {
		// Rollback the config change if reload fails
		frpTm.configBuilder.RemoveTunnelConfig(config)
		return fail(err)
	}

	frpTm.tunnelsLock.Lock()
	claimed.Config = config
	frpTm.tunnelsLock.Unlock()

	return config, nil
}

func (frpTm *FrpTunnelManager) RemoveTunnel(conf TunnelConfig) error {
	log.Debug().Str("subdomain", conf.Subdomain).Str("protocol", string(conf.Protocol)).Msg("RemoveTunnel called")
	tunnelId := CreateTunnelID(conf.Subdomain, string(conf.Protocol))

	// Remove from active tunnels in memory (if exists)
	frpTm.tunnelsLock.Lock()
	if frpTm.activeTunnelConfigs[tunnelId] != nil {
		delete(frpTm.activeTunnelConfigs, tunnelId)
		log.Debug().Str("tunnelId", tunnelId).Msg("Removed tunnel from active configs")
	} else {
		log.Debug().Str("tunnelId", tunnelId).Msg("Tunnel not in active configs, but will remove from config file")
	}
	frpTm.tunnelsLock.Unlock()

	// Always remove from config file regardless of whether it's in memory
	frpTm.configBuilder.RemoveTunnelConfig(conf)

	err := frpTm.Reload()
	if err != nil {
		log.Error().Stack().Err(err).Msg("Failed to reload after removing tunnel")
		return err
	}

	return nil
}

func (frpTm *FrpTunnelManager) SaveRemotePorts(payload common.TransitionPayload) error {
	// log.Debug().Str("app_key", fmt.Sprintf("%v", payload.AppKey)).Interface("payload", payload).Msg("SaveRemotePort called")

	update := []interface{}{common.Dict{
		"app_key":    payload.AppKey,
		"device_key": frpTm.config.ReswarmConfig.DeviceKey,
		"swarm_key":  frpTm.config.ReswarmConfig.SwarmKey,
		"stage":      payload.Stage,
		"ports":      payload.Ports,
	}}
	log.Debug().Interface("update", update).Msg("Saving remote port with update payload")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	_, err := frpTm.messenger.Call(ctx, topics.SetActualAppOnDeviceState, update, nil, nil, nil)
	if err != nil {
		log.Error().Stack().Err(err).Msg("Failed to save remote port")
		return err
	}

	log.Info().Str("app_key", fmt.Sprintf("%v", payload.AppKey)).Msg("Remote port saved successfully")
	return nil
}

func init() {
	// Set zerolog to use a pretty console writer for human-friendly logs
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: "2006-01-02 15:04:05"})
}
