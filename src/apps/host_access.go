package apps

import (
	"net"
	"net/url"
	"os"
	"reagent/common"
	"reagent/config"
	"reagent/trust"
	"reagent/tunnel"
	"strings"

	"github.com/rs/zerolog/log"
)

// hostGatewayEntry makes host.docker.internal resolve inside app containers
// on plain Linux docker; Docker Desktop (macOS/Windows) provides the name
// natively and ignores nothing — the explicit mapping is simply redundant
// there. Supported since docker 20.10.
const hostGatewayEntry = "host.docker.internal:host-gateway"

// appEndpointURL returns the device endpoint URL as seen from inside an app
// container. The agent dials the configured URL from the host network
// namespace, where a loopback host is the device itself; a bridge-networked
// app container's loopback is the container. Loopback endpoints (local dev
// setups — production devices point at a public or LAN address) are therefore
// rewritten to host.docker.internal, which the agent maps to the host gateway
// on every container it creates.
func appEndpointURL(endpointURL string) string {
	parsed, err := url.Parse(endpointURL)
	if err != nil {
		return endpointURL
	}

	host := parsed.Hostname()
	if host == "" {
		return endpointURL
	}

	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return endpointURL
		}
	}

	if port := parsed.Port(); port != "" {
		parsed.Host = "host.docker.internal:" + port
	} else {
		parsed.Host = "host.docker.internal"
	}

	return parsed.String()
}

// tunnelDomainForApps is the domain direct device tunnels are published
// under: the appliance/operator tunnel domain when configured, the
// environment's cloud tunnel edge otherwise. Mirrors the server-address
// resolution in tunnel.initialize().
func tunnelDomainForApps(cfg *config.Config) string {
	if cfg.ReswarmConfig.ApplianceDomain != "" {
		return cfg.ReswarmConfig.ApplianceDomain
	}

	switch cfg.ReswarmConfig.Environment {
	case string(common.TEST):
		return tunnel.TEST_SERVER_ADDR
	case string(common.LOCAL):
		return "localhost"
	default:
		return tunnel.PROD_SERVER_ADDR
	}
}

// addComposeEnvFilesMount mounts the agent's env-files directory read-only at
// /data/env in a compose service, mirroring the single-container /data bind:
// it is how live env updates (e.g. a late-allocated tunnel cloud port) reach
// running compose containers. Authored volumes are preserved; a service that
// already mounts something at /data/env keeps its own mount.
func addComposeEnvFilesMount(service map[string]interface{}, hostEnvDir string) {
	mountEntry := hostEnvDir + ":/data/env:ro"

	switch volumes := service["volumes"].(type) {
	case nil:
		service["volumes"] = []interface{}{mountEntry}
	case []interface{}:
		for _, volume := range volumes {
			switch entry := volume.(type) {
			case string:
				if strings.Contains(entry, ":/data/env") {
					return
				}
			case map[string]interface{}: // long syntax
				if target, ok := entry["target"].(string); ok && strings.HasPrefix(target, "/data/env") {
					return
				}
			}
		}
		service["volumes"] = append(volumes, mountEntry)
	default:
		// Unexpected shape — leave as authored.
	}
}

// addComposeExtraHost injects the host-gateway mapping into a compose
// service, preserving whatever extra_hosts the author already declared.
func addComposeExtraHost(service map[string]interface{}) {
	switch hosts := service["extra_hosts"].(type) {
	case nil:
		service["extra_hosts"] = []interface{}{hostGatewayEntry}
	case []interface{}:
		for _, entry := range hosts {
			if s, ok := entry.(string); ok && strings.HasPrefix(s, "host.docker.internal") {
				return
			}
		}
		service["extra_hosts"] = append(hosts, hostGatewayEntry)
	case map[string]interface{}:
		if _, present := hosts["host.docker.internal"]; !present {
			hosts["host.docker.internal"] = "host-gateway"
		}
	}
}

// appCABundleHostDir returns the device-side directory holding the CA bundle
// app containers should verify TLS against, or "" when this device provides
// none. Callers mount only what exists, so a device where the bundle could not
// be built behaves exactly as it did before.
//
// See the trust package for why this is scoped to appliance-domain devices and
// why the bundle carries the whole host store rather than just the private CA.
func appCABundleHostDir(cfg *config.Config) string {
	if cfg == nil || cfg.ReswarmConfig == nil || cfg.CommandLineArguments == nil {
		return ""
	}
	if !trust.Enabled(cfg.ReswarmConfig.ApplianceDomain) {
		return ""
	}
	if !trust.Available(cfg.CommandLineArguments.AppsDirectory) {
		return ""
	}
	return trust.HostDir(cfg.CommandLineArguments.AppsDirectory)
}

// caBundleEnvironmentVariables points the common runtimes at the mounted
// bundle. SSL_CERT_FILE covers OpenSSL-based stacks (Python's ssl, .NET on
// Linux, Go), REQUESTS_CA_BUNDLE covers Python requests — which uses certifi
// and ignores the OpenSSL paths entirely — and NODE_EXTRA_CA_CERTS covers
// Node, where it ADDS to the built-in set rather than replacing it.
// IRONFLOCK_CA_BUNDLE is the name the SDKs can read when a runtime needs the
// path explicitly.
//
// These are defaults: buildProdEnvironmentVariables appends app-supplied
// variables after them, so an app that sets its own SSL_CERT_FILE still wins.
func caBundleEnvironmentVariables(cfg *config.Config) []string {
	if appCABundleHostDir(cfg) == "" {
		return nil
	}
	return []string{
		"SSL_CERT_FILE=" + trust.ContainerPath,
		"REQUESTS_CA_BUNDLE=" + trust.ContainerPath,
		"NODE_EXTRA_CA_CERTS=" + trust.ContainerPath,
		"IRONFLOCK_CA_BUNDLE=" + trust.ContainerPath,
	}
}

// addComposeCABundleMount mounts the CA bundle directory read-only into a
// compose service. Authored volumes are preserved; a service that already
// mounts something there keeps its own.
func addComposeCABundleMount(service map[string]interface{}, hostCADir string) {
	if hostCADir == "" {
		return
	}

	mountEntry := hostCADir + ":" + trust.ContainerDir + ":ro"

	switch volumes := service["volumes"].(type) {
	case nil:
		service["volumes"] = []interface{}{mountEntry}
	case []interface{}:
		for _, volume := range volumes {
			switch entry := volume.(type) {
			case string:
				if strings.Contains(entry, ":"+trust.ContainerDir) {
					return
				}
			case map[string]interface{}: // long syntax
				if target, ok := entry["target"].(string); ok && strings.HasPrefix(target, trust.ContainerDir) {
					return
				}
			}
		}
		service["volumes"] = append(volumes, mountEntry)
	default:
		// Unexpected shape — leave as authored.
	}
}

// containerBypassEntries are the destinations an app container reaches without
// leaving the device, and which must therefore never be sent at a corporate
// proxy. The device's own NO_PROXY only carries the host's view of that
// (localhost and friends, per install_ironflock.sh / `ironflock agent install`),
// which is wrong from inside a bridge-networked container: its loopback is the
// container, and its siblings and the device itself sit on a docker bridge.
//
// The CIDR entries cover sibling containers and LAN peers addressed by IP.
// They are honoured by the stacks that implement CIDR matching (Go, Node,
// urllib3) and inertly ignored by the ones that only do hostname suffix
// matching (curl, requests' own matcher). Sibling compose services addressed
// by SERVICE NAME are single-label hostnames; most clients leave those alone,
// but an app that needs certainty should extend NO_PROXY itself.
var containerBypassEntries = []string{
	"localhost",
	"127.0.0.1",
	"::1",
	// The device itself, as seen from a bridge-networked container — the same
	// name addComposeExtraHost and appEndpointURL point apps at.
	"host.docker.internal",
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
}

// deviceProxy is the corporate HTTP proxy the AGENT itself runs behind. On a
// proxied site the installer writes it into the reagent service environment
// (/etc/systemd/system/reagent.service.d/http-proxy.conf), which is the same
// place the agent's OTA downloads (filesystem.Get) and the frpc control
// connection (resolveTunnelProxy) already take it from. Reading the process
// environment is therefore not a shortcut — it IS the device's proxy config.
type deviceProxy struct {
	HTTP    string
	HTTPS   string
	NoProxy string
}

// resolveDeviceProxy reads the standard proxy variables out of the agent's
// environment, preferring the upper-case spelling exactly as proxy.Resolve in
// ironflock (ironflock-cli's cli/proxy) does.
func resolveDeviceProxy() deviceProxy {
	pick := func(upper, lower string) string {
		if value := strings.TrimSpace(os.Getenv(upper)); value != "" {
			return value
		}
		return strings.TrimSpace(os.Getenv(lower))
	}

	return deviceProxy{
		HTTP:    pick("HTTP_PROXY", "http_proxy"),
		HTTPS:   pick("HTTPS_PROXY", "https_proxy"),
		NoProxy: pick("NO_PROXY", "no_proxy"),
	}
}

// enabled reports whether this device runs behind a proxy at all. A device
// with none injects nothing and behaves exactly as it did before.
func (p deviceProxy) enabled() bool {
	return p.HTTP != "" || p.HTTPS != ""
}

// containerNoProxy widens the device's NO_PROXY list to the container's
// vantage point. The device's own entries come first: a site that exempted an
// internal host from the proxy meant that for every process on the device.
func (p deviceProxy) containerNoProxy(cfg *config.Config, lanIP string) string {
	var entries []string
	seen := map[string]bool{}
	add := func(entry string) {
		entry = strings.TrimSpace(entry)
		if entry == "" || seen[entry] {
			return
		}
		seen[entry] = true
		entries = append(entries, entry)
	}

	for _, entry := range strings.Split(p.NoProxy, ",") {
		add(entry)
	}
	for _, entry := range containerBypassEntries {
		add(entry)
	}

	// The device's LAN address: an app reaching a port its own device
	// publishes uses this, and it is not covered when the site runs a public
	// address range.
	add(lanIP)

	// An appliance's domain resolves to a LAN address, and every app on it
	// talks to wss://ws.<domain>. Sending that at the corporate proxy is the
	// failure the CA bundle work already had to unpick. Cloud devices get
	// nothing here: their endpoints are public and DO need the proxy.
	if cfg != nil && cfg.ReswarmConfig != nil && cfg.ReswarmConfig.ApplianceDomain != "" {
		add(cfg.ReswarmConfig.ApplianceDomain)
		add("." + cfg.ReswarmConfig.ApplianceDomain)
	}

	return strings.Join(entries, ",")
}

// proxyEnvironmentVariables hands the device's corporate proxy to app
// containers. Each setting is emitted under the upper- and lower-case
// spellings, because runtimes disagree on which they read (Go and Python
// accept either, curl and many libraries only the lower-case one), plus an
// IRONFLOCK_-prefixed name.
//
// The IRONFLOCK_ names exist because the standard variables only reach HTTP
// clients. A raw MQTT or AMQP connection is a plain TLS socket, and its client
// has to be pointed at a proxy explicitly — so an app needs a name it can read
// the device proxy from even when it is configuring a non-HTTP transport, and
// one that still tells the truth when the app overrode HTTPS_PROXY itself.
//
// ALL_PROXY is deliberately NOT emitted: it makes SOCKS-capable stacks route
// everything through the proxy, including the traffic containerBypassEntries
// only bypasses by hostname, and a corporate CONNECT ACL is routinely 443-only
// anyway (the same trap resolveTunnelProxy warns about for the tunnel).
//
// These are defaults: buildProdEnvironmentVariables appends app-supplied
// variables after them, so an app that sets its own HTTPS_PROXY or NO_PROXY
// still wins.
func proxyEnvironmentVariables(cfg *config.Config, lanIP string) []string {
	proxy := resolveDeviceProxy()
	if !proxy.enabled() {
		return nil
	}

	var envs []string
	add := func(name, value string) {
		if value == "" {
			return
		}
		envs = append(envs,
			name+"="+value,
			strings.ToLower(name)+"="+value,
			"IRONFLOCK_"+name+"="+value,
		)
	}

	noProxy := proxy.containerNoProxy(cfg, lanIP)

	add("HTTP_PROXY", proxy.HTTP)
	add("HTTPS_PROXY", proxy.HTTPS)
	add("NO_PROXY", noProxy)

	// Said out loud, redacted, because "is this app even being told about the
	// proxy?" is otherwise only answerable by docker inspect on the device —
	// the same gap resolveTunnelProxy had to close for the tunnel.
	log.Debug().
		Str("http_proxy", redactProxyURL(proxy.HTTP)).
		Str("https_proxy", redactProxyURL(proxy.HTTPS)).
		Str("no_proxy", noProxy).
		Msg("Handing the device proxy to an app container")

	return envs
}

// redactProxyURL strips any credentials from a proxy URL so it can be logged.
// A corporate proxy URL routinely carries a shared service account.
func redactProxyURL(raw string) string {
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "<unparseable>"
	}
	return parsed.Redacted()
}
