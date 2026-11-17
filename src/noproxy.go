package main

// The NO_PROXY list the Windows agent's own process uses
// (docs/WINDOWS-PROXY.md): computed at every start of the service from the
// operator's own entries (the store's NO_PROXY) and what this device reaches
// directly, never through the proxy: loopback, the machine's names, the
// networks of its interfaces, and the hosts of its .flock that are local.
// Go's proxy rule (httpproxy) reads it, which matches address ranges.
//
// Linux is not computed here: the ironflock CLI writes the Linux agent's list
// into /etc/ironflock/proxy.env.

import (
	"encoding/json"
	"net"
	"net/url"
	"reagent/config"
	"strings"
)

// loopbackNoProxy is loopback as every list spells it.
var loopbackNoProxy = []string{"localhost", "127.0.0.1", "::1"}

// noProxyInputs is what the list is computed from.
type noProxyInputs struct {
	// User are the operator's own entries (-no-proxy), kept as given.
	User string
	// Hostnames are the machine's names.
	Hostnames []string
	// Networks are the networks of the interfaces that are up.
	Networks []*net.IPNet
	// Flock is the device's configuration; nil when it could not be read.
	Flock *config.ReswarmConfig
}

// noProxyList collects entries once each, lower case, in their order.
type noProxyList struct {
	entries []string
	seen    map[string]bool
}

func (l *noProxyList) add(entry string) {
	entry = strings.ToLower(strings.TrimSpace(entry))
	if entry == "" || entry == "." {
		return
	}
	if l.seen == nil {
		l.seen = map[string]bool{}
	}
	if l.seen[entry] {
		return
	}
	l.seen[entry] = true
	l.entries = append(l.entries, entry)
}

// addHost adds a host without a port: an address as it is (an IPv6 one
// without brackets), a name as name and .name.
func (l *noProxyList) addHost(host string) {
	host = strings.Trim(strings.ToLower(strings.TrimSpace(host)), "[]")
	if host == "" {
		return
	}
	if net.ParseIP(host) != nil {
		l.add(host)
		return
	}
	if host == "localhost" || strings.HasPrefix(host, ".") || strings.HasPrefix(host, "*") {
		l.add(host)
		return
	}
	l.add(host)
	l.add("." + host)
}

// withoutPort is a NO_PROXY entry without a port: Go narrows an entry with
// one to that port. Ranges and plain IPv6 addresses stay as they are.
func withoutPort(entry string) string {
	entry = strings.TrimSpace(entry)
	if strings.Contains(entry, "/") {
		return entry
	}
	if host, _, err := net.SplitHostPort(entry); err == nil {
		return host
	}
	return strings.Trim(entry, "[]")
}

// effectiveNoProxy is the list of the agent's process: loopback, the
// operator's entries, the machine's names, its networks, the .flock's local
// hosts.
func effectiveNoProxy(in noProxyInputs) string {
	var l noProxyList
	for _, entry := range loopbackNoProxy {
		l.add(entry)
	}
	for _, entry := range strings.Split(in.User, ",") {
		entry = withoutPort(entry)
		if strings.Contains(entry, "/") {
			l.add(entry)
		} else {
			l.addHost(entry)
		}
	}
	for _, name := range in.Hostnames {
		l.addHost(name)
	}
	for _, network := range in.Networks {
		l.add((&net.IPNet{IP: network.IP.Mask(network.Mask), Mask: network.Mask}).String())
	}
	for _, host := range flockNoProxyHosts(in.Flock, in.Networks) {
		l.addHost(host)
	}
	return strings.Join(l.entries, ",")
}

// flockNoProxyHosts are the hosts of the .flock that the device reaches
// directly: the device endpoint, the registry, the update server and the
// listed insecure registries, plus the appliance's domain.
//
// Only local ones: a device that belongs to an appliance (it has an
// appliance domain, or its endpoint is an address, which a cloud endpoint
// never is) takes all of them; a cloud device only a host inside one of its
// networks and the plain-HTTP registries (a registry with an explicit port,
// which is a LAN registry by construction, insecureRegistryEntries). A cloud
// device's endpoints are public and need the proxy: putting them here would
// take a device on a proxy-only network offline.
func flockNoProxyHosts(cfg *config.ReswarmConfig, networks []*net.IPNet) []string {
	if cfg == nil {
		return nil
	}
	endpoint := urlHost(cfg.DeviceEndpointURL)
	appliance := strings.TrimSpace(cfg.ApplianceDomain) != "" || net.ParseIP(endpoint) != nil

	candidates := []string{endpoint, proxyBypassHost(cfg.DockerRegistryURL), urlHost(cfg.UpdateURL)}
	for _, registry := range listedInsecureRegistries(cfg.InsecureRegistries) {
		candidates = append(candidates, proxyBypassHost(registry))
	}

	var hosts []string
	for _, host := range candidates {
		if host == "" {
			continue
		}
		if appliance || inNetworks(host, networks) {
			hosts = append(hosts, host)
		}
	}
	for _, registry := range insecureRegistryEntries(cfg) {
		hosts = append(hosts, proxyBypassHost(registry))
	}
	if domain := strings.TrimSpace(cfg.ApplianceDomain); domain != "" {
		hosts = append(hosts, domain)
	}
	return hosts
}

// urlHost is the host of a URL without its port; a value without a scheme
// is taken as host[:port][/path].
func urlHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		return proxyBypassHost(raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// listedInsecureRegistries reads the .flock's insecure-registries field: a
// JSON array, or one bare entry.
func listedInsecureRegistries(listed string) []string {
	listed = strings.TrimSpace(listed)
	if listed == "" {
		return nil
	}
	var entries []string
	if json.Unmarshal([]byte(listed), &entries) == nil {
		return entries
	}
	return []string{listed}
}

// inNetworks reports whether host is an address in one of networks, or a
// loopback address.
func inNetworks(host string, networks []*net.IPNet) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	for _, network := range networks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// netInterface is what interfaceNetworks needs of an interface.
type netInterface struct {
	Up, Loopback bool
	Addrs        []net.Addr
}

// interfaceNetworks are the networks of the interfaces that are up, as
// ranges: IPv4, and IPv6 except link-local. Loopback is spelled out
// elsewhere.
func interfaceNetworks(interfaces []netInterface) []*net.IPNet {
	var networks []*net.IPNet
	seen := map[string]bool{}
	for _, iface := range interfaces {
		if !iface.Up || iface.Loopback {
			continue
		}
		for _, addr := range iface.Addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP == nil {
				continue
			}
			if ipNet.IP.To4() == nil && ipNet.IP.IsLinkLocalUnicast() {
				continue
			}
			if ipNet.IP.IsLoopback() {
				continue
			}
			ip := ipNet.IP
			if v4 := ip.To4(); v4 != nil {
				ip = v4
			}
			mask := ipNet.Mask
			if len(mask) == net.IPv6len && len(ip) == net.IPv4len {
				mask = mask[12:]
			}
			network := &net.IPNet{IP: ip.Mask(mask), Mask: mask}
			if network.IP == nil || seen[network.String()] {
				continue
			}
			seen[network.String()] = true
			networks = append(networks, network)
		}
	}
	return networks
}
