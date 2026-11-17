package winservice

// The agent's proxy store on Windows (docs/WINDOWS-PROXY.md): the file
// <agentDir>\proxy.env, which only SYSTEM and Administrators read, and the
// helpers that move proxy values in and out of the service's registry
// environment. Kept free of syscalls so it is testable on every platform; the
// ACL and registry calls are in proxy_windows.go.

import (
	"fmt"
	"strings"
)

const (
	// ProxyEnvName is the store, in the agent dir.
	ProxyEnvName = "proxy.env"
	// ProxyEnvOldName keeps a store's earlier content once, when the move
	// from the registry environment finds a store with other values.
	ProxyEnvOldName = "proxy.env.old"
	// ProxyStoreSDDL is the store's DACL: inheritance off, SYSTEM and
	// Administrators full control, nobody else.
	ProxyStoreSDDL = "D:P(A;;FA;;;SY)(A;;FA;;;BA)"
	// ParametersKey is the subkey of the service key that holds the
	// agent's non-secret settings; ProxyConfiguredValue says whether the
	// agent has a proxy (REG_DWORD 1 or 0), for every user to read.
	ParametersKey        = "Parameters"
	ProxyConfiguredValue = "ProxyConfigured"
)

// ProxyVariables are the names the store holds, and the ones the move takes
// out of the service's registry environment (in any case).
var ProxyVariables = []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"}

// ProxySettings are the agent's proxy settings.
type ProxySettings struct {
	HTTP    string
	HTTPS   string
	NoProxy string
}

// Configured reports whether the settings name a proxy at all. Settings
// without one mean: no proxy.
func (s ProxySettings) Configured() bool {
	return s.HTTP != "" || s.HTTPS != ""
}

// Values are the settings by variable name, in ProxyVariables' order.
func (s ProxySettings) Values() [][2]string {
	return [][2]string{{"HTTP_PROXY", s.HTTP}, {"HTTPS_PROXY", s.HTTPS}, {"NO_PROXY", s.NoProxy}}
}

// EnvironmentEntries renders the settings as NAME=value entries.
func (s ProxySettings) EnvironmentEntries() []string {
	entries := make([]string, 0, 3)
	for _, v := range s.Values() {
		entries = append(entries, v[0]+"="+v[1])
	}
	return entries
}

// ParseProxyEnv reads a store. Unknown names, comments and blank lines are
// ignored, so an operator can annotate the file; a value may be enclosed in
// double or single quotes.
func ParseProxyEnv(data []byte) ProxySettings {
	var s ProxySettings
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = unquote(strings.TrimSpace(value))
		switch strings.ToUpper(strings.TrimSpace(name)) {
		case "HTTP_PROXY":
			s.HTTP = value
		case "HTTPS_PROXY":
			s.HTTPS = value
		case "NO_PROXY":
			s.NoProxy = value
		}
	}
	return s
}

func unquote(value string) string {
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
		return value[1 : len(value)-1]
	}
	return value
}

// RenderProxyEnv writes a store. NO_PROXY holds the operator's own entries;
// the agent adds what the device reaches directly at every start.
func RenderProxyEnv(s ProxySettings) string {
	var b strings.Builder
	b.WriteString("# IronFlock agent proxy configuration.\n")
	b.WriteString("#\n")
	b.WriteString("# The device agent service reads this file at its start; only SYSTEM and\n")
	b.WriteString("# Administrators can read it. Change it with\n")
	b.WriteString("#   device-agent.exe service configure -http-proxy URL -https-proxy URL -no-proxy HOSTS\n")
	b.WriteString("# (as administrator; see docs/WINDOWS-PROXY.md), or edit it and restart the\n")
	b.WriteString("# service. `service uninstall` leaves it in place, so it survives a reinstall.\n")
	b.WriteString("#\n")
	b.WriteString("# NO_PROXY holds your own entries. The agent always adds what this device\n")
	b.WriteString("# reaches directly: localhost, 127.0.0.1, ::1, this machine's names, the\n")
	b.WriteString("# networks of its interfaces, and the local hosts of its .flock (an\n")
	b.WriteString("# appliance's domain, endpoint, registry and update server). NO_PROXY matches\n")
	b.WriteString("# the host NAME a client dials, so a host reached by name is listed by name.\n")
	b.WriteString("#\n")
	b.WriteString("# This does NOT configure Docker. Image pulls are made by the Docker daemon,\n")
	b.WriteString("# which reads its own settings (Docker Desktop -> Settings -> Resources ->\n")
	b.WriteString("# Proxies). A proxy fixed here does not fix `docker pull`.\n")
	fmt.Fprintf(&b, "\nHTTP_PROXY=%s\n", s.HTTP)
	fmt.Fprintf(&b, "HTTPS_PROXY=%s\n", s.HTTPS)
	fmt.Fprintf(&b, "NO_PROXY=%s\n", s.NoProxy)
	return b.String()
}

// isProxyVariable reports whether an environment entry's name is one of
// ProxyVariables, in any case (Windows' environment is case-insensitive).
func isProxyVariable(name string) bool {
	for _, v := range ProxyVariables {
		if strings.EqualFold(name, v) {
			return true
		}
	}
	return false
}

// ProxyFromEnvironment reads the proxy settings out of a service's registry
// environment (NAME=value entries). found reports whether any proxy variable
// is there at all, also an empty one: an older agent's install wrote all
// three, empty or not. The upper-case spelling wins over another one.
func ProxyFromEnvironment(env []string) (settings ProxySettings, found bool) {
	pick := map[string]string{}
	exact := map[string]bool{}
	for _, entry := range env {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || !isProxyVariable(name) {
			continue
		}
		found = true
		upper := strings.ToUpper(name)
		if exact[upper] {
			continue
		}
		pick[upper] = value
		if name == upper {
			exact[upper] = true
		}
	}
	return ProxySettings{HTTP: pick["HTTP_PROXY"], HTTPS: pick["HTTPS_PROXY"], NoProxy: pick["NO_PROXY"]}, found
}

// WithoutProxyVariables is env without its proxy variables (any case); every
// other entry stays, in its order.
func WithoutProxyVariables(env []string) []string {
	kept := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if isProxyVariable(name) {
			continue
		}
		kept = append(kept, entry)
	}
	return kept
}

// WithProxyVariables is env with its proxy variables replaced by those of s
// (appended after the other entries); none when s names no proxy.
func WithProxyVariables(env []string, s ProxySettings) []string {
	kept := WithoutProxyVariables(env)
	if s.Configured() {
		kept = append(kept, s.EnvironmentEntries()...)
	}
	return kept
}
