package main

import (
	"net"
	"reagent/config"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cidr(t *testing.T, s string) *net.IPNet {
	t.Helper()
	ip, network, err := net.ParseCIDR(s)
	require.NoError(t, err)
	network.IP = ip // as an interface reports its address
	return network
}

func list(s string) []string { return strings.Split(s, ",") }

// A device attached to an appliance in domain mode must reach it directly. The
// LAC edge PC went offline exactly here: NO_PROXY was fixed at
// localhost,127.0.0.1, so the agent sent its platform connection to the
// corporate proxy, which will not route back into the internal network.
func TestEffectiveNoProxyApplianceDevice(t *testing.T) {
	got := effectiveNoProxy(noProxyInputs{Flock: &config.ReswarmConfig{
		ApplianceDomain:   "tls-sf015.corp.example.com",
		DeviceEndpointURL: "wss://ws.tls-sf015.corp.example.com/ws",
		DockerRegistryURL: "136.230.111.59:15001/",
		UpdateURL:         "https://tls-sf015.corp.example.com:8443/dl",
	}})
	assert.Equal(t, []string{
		"localhost", "127.0.0.1", "::1",
		"ws.tls-sf015.corp.example.com", ".ws.tls-sf015.corp.example.com",
		"136.230.111.59",
		"tls-sf015.corp.example.com", ".tls-sf015.corp.example.com",
	}, list(got))
}

// A plain-mode appliance: no domain, the endpoint is an address.
func TestEffectiveNoProxyPlainApplianceDevice(t *testing.T) {
	got := effectiveNoProxy(noProxyInputs{Flock: &config.ReswarmConfig{
		DeviceEndpointURL:  "ws://192.168.20.5:8080/ws-re-dev",
		DockerRegistryURL:  "192.168.20.5:15001/",
		UpdateURL:          "http://192.168.20.5:8080/dl",
		InsecureRegistries: `["192.168.20.5:15001/", "[fd00::5]:15001"]`,
	}})
	assert.Equal(t, []string{"localhost", "127.0.0.1", "::1", "192.168.20.5", "fd00::5"}, list(got))
}

// Negative control: a cloud device's endpoints are public and DO need the
// proxy. None of them may be added, or configuring a proxy would stop the
// device from reaching the platform on a proxy-only network.
func TestEffectiveNoProxyCloudDeviceKeepsItsEndpointsOnTheProxy(t *testing.T) {
	got := effectiveNoProxy(noProxyInputs{
		Hostnames: []string{"EDGE-PC-7", "edge-pc-7.corp.example"},
		Networks:  []*net.IPNet{cidr(t, "10.20.30.40/24"), cidr(t, "fd12:3456::7/64")},
		Flock: &config.ReswarmConfig{
			DeviceEndpointURL: "wss://cbw.ironflock.com/ws",
			DockerRegistryURL: "registry.ironflock.com/",
			UpdateURL:         "https://instance-registry.ironflock.com/dl",
		},
	})
	assert.Equal(t, []string{
		"localhost", "127.0.0.1", "::1",
		"edge-pc-7", ".edge-pc-7", "edge-pc-7.corp.example", ".edge-pc-7.corp.example",
		"10.20.30.0/24", "fd12:3456::/64",
	}, list(got))
	assert.NotContains(t, got, "ironflock")
}

// A cloud device's host inside one of its networks is local; a plain-HTTP
// registry (explicit port) is a LAN registry, as service install has always
// taken it.
func TestEffectiveNoProxyCloudDeviceLocalHosts(t *testing.T) {
	got := effectiveNoProxy(noProxyInputs{
		Networks: []*net.IPNet{cidr(t, "10.20.30.40/24")},
		Flock: &config.ReswarmConfig{
			DeviceEndpointURL: "wss://cbw.ironflock.com/ws",
			UpdateURL:         "http://10.20.30.9:8080/dl",
			DockerRegistryURL: "registry.lan.example:15001/",
		},
	})
	assert.Contains(t, list(got), "10.20.30.9")
	assert.Contains(t, list(got), "registry.lan.example")
	assert.Contains(t, list(got), ".registry.lan.example")
	assert.NotContains(t, got, "cbw")
}

// The operator's entries are always kept, without ports, names in both forms.
func TestEffectiveNoProxyKeepsTheOperatorsEntries(t *testing.T) {
	got := effectiveNoProxy(noProxyInputs{User: " Internal.Corp ,,10.9.0.0/16, proxyless.example:8080, [fd00::1]:443, ::1, .suffix.example, localhost "})
	assert.Equal(t, []string{
		"localhost", "127.0.0.1", "::1",
		"internal.corp", ".internal.corp", "10.9.0.0/16", "proxyless.example", ".proxyless.example", "fd00::1", ".suffix.example",
	}, list(got))
	for _, entry := range list(got) {
		if strings.Contains(entry, "/") || net.ParseIP(entry) != nil {
			continue
		}
		assert.NotContains(t, entry, ":", "no port: %s", entry)
	}
}

func TestEffectiveNoProxyWithoutAFlock(t *testing.T) {
	assert.Equal(t, "localhost,127.0.0.1,::1", effectiveNoProxy(noProxyInputs{}))
}

func TestInterfaceNetworks(t *testing.T) {
	addrs := func(cidrs ...string) []net.Addr {
		var out []net.Addr
		for _, c := range cidrs {
			out = append(out, cidr(t, c))
		}
		return out
	}
	got := interfaceNetworks([]netInterface{
		{Up: true, Addrs: append(addrs("192.168.1.50/24", "fe80::1234/64", "2001:db8:1::5/64", "169.254.10.2/16"), &net.IPAddr{IP: net.ParseIP("10.0.0.1")})},
		{Up: true, Loopback: true, Addrs: addrs("127.0.0.1/8", "::1/128")},
		{Up: false, Addrs: addrs("10.1.1.1/16")},
		{Up: true, Addrs: addrs("192.168.1.51/24")},                                                          // the same network again
		{Up: true, Addrs: []net.Addr{&net.IPNet{IP: net.ParseIP("172.20.0.4"), Mask: net.CIDRMask(16, 32)}}}, // a 16-byte IPv4
	})
	var names []string
	for _, n := range got {
		names = append(names, n.String())
	}
	assert.Equal(t, []string{"192.168.1.0/24", "2001:db8:1::/64", "169.254.0.0/16", "172.20.0.0/16"}, names)
}
