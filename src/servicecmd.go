package main

// OS-neutral logic for the `reagent service` subcommand: flag parsing, the
// migration decision, and generated file/argument contents. Kept free of
// syscalls so it is unit-testable on every platform; the Windows-only pieces
// live in servicecmd_windows.go.

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reagent/common"
	"reagent/config"
	"strings"
)

const (
	serviceDisplayName = "IronFlock Device Agent"
	serviceDescription = "Connects this device to the IronFlock platform, runs apps via Docker, and keeps the agent up to date."
	repairTaskName     = "IronFlock Agent Repair"
	serviceConfigName  = "device.flock"
)

// certImportArgs builds `certutil -addstore -f <store> <certPath>` args to
// import our root into a device trust store. Root store makes the self-signed
// chain validate; TrustedPublisher makes UAC show a verified publisher and
// enables WDAC/AppLocker publisher rules.
func certImportArgs(store, certPath string) []string {
	return []string{"-addstore", "-f", store, certPath}
}

// certDeleteArgs builds `certutil -delstore <store> <name>` args to remove our
// imported root at uninstall (leaving a self-signed root behind is a lasting
// liability).
func certDeleteArgs(store, name string) []string {
	return []string{"-delstore", store, name}
}

// defenderAddExclusionCmd is the PowerShell that excludes exactly the frpc
// binary from Defender. frp is flagged as a dual-use tool (PUA); the narrow
// exclusion is the deterministic fix on devices we administer. Best-effort:
// Tamper Protection / Intune-managed devices ignore it.
func defenderAddExclusionCmd(frpcPath string) string {
	return "Add-MpPreference -ExclusionPath '" + frpcPath + "'"
}

// defenderRemoveExclusionCmd reverses the exclusion at uninstall.
func defenderRemoveExclusionCmd(frpcPath string) string {
	return "Remove-MpPreference -ExclusionPath '" + frpcPath + "'"
}

// agentDirFromImagePath extracts the -agentDir value the installer baked into
// the service's ImagePath, so uninstall can reverse the dir-scoped side
// effects (Defender exclusion, cert file). Returns "" when not found.
func agentDirFromImagePath(imagePath string) string {
	fields := splitCommandLine(imagePath)
	for i, f := range fields {
		if f == "-agentDir" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

// splitCommandLine splits a Windows ImagePath into fields, honoring
// double-quoted segments (paths under %ProgramData% have no spaces by default,
// but a custom -agentDir might).
func splitCommandLine(s string) []string {
	var fields []string
	var cur strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
		case r == ' ' && !inQuote:
			if cur.Len() > 0 {
				fields = append(fields, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		fields = append(fields, cur.String())
	}
	return fields
}

// normalizeRegistryHost reduces a registry reference from the .flock to the
// `host[:port]` form Docker matches insecure-registries against: no scheme,
// no trailing slash (`136.230.111.59:15001/` → `136.230.111.59:15001`).
// Delegates to the platform-wide canonical rule so daemon.json entries and
// docker_credentials keys can never diverge.
func normalizeRegistryHost(entry string) string {
	return common.NormalizeRegistryHost(entry)
}

// hostHasPort reports whether a normalized registry reference carries an
// explicit numeric port.
func hostHasPort(entry string) bool {
	entry = normalizeRegistryHost(entry)
	i := strings.LastIndex(entry, ":")
	if i < 0 || i == len(entry)-1 {
		return false
	}
	for _, r := range entry[i+1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// insecureRegistryEntries derives the Docker insecure-registries entries this
// device needs from its .flock. Two sources: the `insecure-registries` field
// (a JSON-array string on backend-issued device flocks, a bare `host:port/`
// string on installer-issued appliance flocks) and `docker_registry_url` — the
// registry the agent actually logs into. The listed field alone is not enough:
// appliance device flocks carry the appliance's *local* names (localhost), so
// the reachable registry URL is merged in whenever it names an explicit port.
// An explicit port is what marks the plain-HTTP LAN registry; HTTPS registries
// (cloud, domain-mode appliance) have none and need no insecure entry.
func insecureRegistryEntries(cfg *config.ReswarmConfig) []string {
	var raw []string
	if listed := strings.TrimSpace(cfg.InsecureRegistries); listed != "" {
		var arr []string
		if json.Unmarshal([]byte(listed), &arr) == nil {
			raw = arr
		} else {
			raw = []string{listed}
		}
	}
	if hostHasPort(cfg.DockerRegistryURL) {
		raw = append(raw, cfg.DockerRegistryURL)
	}

	entries := make([]string, 0, len(raw))
	seen := make(map[string]bool)
	for _, entry := range raw {
		entry = normalizeRegistryHost(entry)
		if entry == "" || seen[entry] {
			continue
		}
		seen[entry] = true
		entries = append(entries, entry)
	}
	return entries
}

// proxyEnvFileName is the durable home of a device's proxy configuration.
//
// It cannot live in the service's own registry key: `service install` refuses
// to run while the service exists, so every reinstall goes through
// `service uninstall`, which deletes the service key and the Environment value
// with it. Uninstall deliberately keeps the agent directory, so a file here is
// the only store that survives. This mirrors /opt/ironflock/.env on the
// appliance: the installer seeds it, the operator owns it afterwards, and it is
// what gets applied on every install.
//
// Linux needs no equivalent — systemd applies drop-ins in lexical order, so an
// operator's `systemctl edit reagent` override.conf already wins over the
// installer's http-proxy.conf and is never rewritten.
const proxyEnvFileName = "proxy.env"

// proxyConfig is the content of proxy.env.
type proxyConfig struct {
	HTTP    string
	HTTPS   string
	NoProxy string
}

// configured reports whether this device talks to the world through a proxy at
// all. A device with none writes no service environment and behaves as before.
func (c proxyConfig) configured() bool {
	return c.HTTP != "" || c.HTTPS != ""
}

// environmentEntries renders the REG_MULTI_SZ body for the service key.
func (c proxyConfig) environmentEntries() []string { //nolint:unused // used by the Windows service installer (servicecmd_windows.go)
	return []string{
		"HTTP_PROXY=" + c.HTTP,
		"HTTPS_PROXY=" + c.HTTPS,
		"NO_PROXY=" + c.NoProxy,
	}
}

// parseProxyEnv reads a proxy.env. Unknown keys and comments are ignored, so an
// operator can annotate the file without breaking it.
func parseProxyEnv(data []byte) proxyConfig {
	var cfg proxyConfig
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"`)
		switch strings.ToUpper(strings.TrimSpace(name)) {
		case "HTTP_PROXY":
			cfg.HTTP = value
		case "HTTPS_PROXY":
			cfg.HTTPS = value
		case "NO_PROXY":
			cfg.NoProxy = value
		}
	}
	return cfg
}

// renderProxyEnv writes the seed file. The derived bypass entries are written
// out literally rather than recomputed at apply time, because that is what lets
// an operator DELETE one: a site whose appliance is reachable only through the
// proxy removes it from NO_PROXY and nothing puts it back.
func renderProxyEnv(cfg proxyConfig) string {
	var b strings.Builder
	b.WriteString("# IronFlock agent proxy configuration.\n")
	b.WriteString("#\n")
	b.WriteString("# This file is the durable source for the reagent service's proxy settings.\n")
	b.WriteString("# `reagent service install` seeds it once and applies it to the Windows\n")
	b.WriteString("# service environment; `service uninstall` leaves it in place, so edits\n")
	b.WriteString("# survive a reinstall. To change the proxy, edit this file and re-run\n")
	b.WriteString("# `reagent service install`.\n")
	b.WriteString("#\n")
	b.WriteString("# NO_PROXY matches the host NAME a client dials, never the address that name\n")
	b.WriteString("# resolves to, so an appliance reached by name has to be listed here. A bare\n")
	b.WriteString("# domain covers the domain and all of its subdomains.\n")
	b.WriteString("#\n")
	b.WriteString("# Remove an entry if this site reaches the appliance THROUGH the proxy rather\n")
	b.WriteString("# than directly — both deployments exist and the installer cannot tell them\n")
	b.WriteString("# apart.\n")
	b.WriteString("#\n")
	b.WriteString("# This does NOT configure Docker. Image pulls are made by the Docker daemon,\n")
	b.WriteString("# which reads its own settings (Docker Desktop -> Settings -> Resources ->\n")
	b.WriteString("# Proxies). A proxy fixed here does not fix `docker pull`.\n")
	fmt.Fprintf(&b, "\nHTTP_PROXY=%s\n", cfg.HTTP)
	fmt.Fprintf(&b, "HTTPS_PROXY=%s\n", cfg.HTTPS)
	fmt.Fprintf(&b, "NO_PROXY=%s\n", cfg.NoProxy)
	return b.String()
}

// resolveProxyConfig decides what proxy settings this install applies, and
// reports whether it seeded the file.
//
// An existing proxy.env WINS over the flags. That is the point: a reinstall
// must restore the site's configuration without the operator remembering which
// flags the last one used, and a site that edited the bypass list must not have
// that edit silently undone. -force-proxy is the explicit way to replace it.
func resolveProxyConfig(proxyEnvPath string, opts *serviceInstallOptions, flockPath string) (proxyConfig, bool, error) {
	existing, err := os.ReadFile(proxyEnvPath)
	switch {
	case err == nil && !opts.ForceProxy:
		cfg := parseProxyEnv(existing)
		if opts.Proxy != "" && opts.Proxy != cfg.HTTP && opts.Proxy != cfg.HTTPS {
			fmt.Fprintf(os.Stderr, "note: -proxy differs from %s, which wins — pass -force-proxy to replace it\n", proxyEnvPath)
		}
		return cfg, false, nil
	case err != nil && !os.IsNotExist(err):
		return proxyConfig{}, false, fmt.Errorf("could not read %s: %w", proxyEnvPath, err)
	}

	if opts.Proxy == "" {
		return proxyConfig{}, false, nil
	}

	flockCfg, err := readFlockConfig(flockPath)
	if err != nil {
		return proxyConfig{}, false, fmt.Errorf("could not read the .flock config to build the proxy bypass list: %w", err)
	}

	cfg := proxyConfig{
		HTTP:    opts.Proxy,
		HTTPS:   opts.Proxy,
		NoProxy: strings.Join(serviceNoProxyEntries(flockCfg, opts.NoProxy), ","),
	}
	// 0600 beside the .flock: a proxy URL can carry credentials, and the agent
	// dir is already restricted to SYSTEM and Administrators.
	err = os.WriteFile(proxyEnvPath, []byte(renderProxyEnv(cfg)), 0600)
	if err != nil {
		return proxyConfig{}, false, fmt.Errorf("could not write %s: %w", proxyEnvPath, err)
	}
	return cfg, true, nil
}

// readFlockConfig loads and parses a device .flock from disk.
func readFlockConfig(flockPath string) (*config.ReswarmConfig, error) {
	raw, err := os.ReadFile(flockPath)
	if err != nil {
		return nil, err
	}
	var flockCfg config.ReswarmConfig
	err = json.Unmarshal(raw, &flockCfg)
	if err != nil {
		return nil, fmt.Errorf("could not parse %s: %w", flockPath, err)
	}
	return &flockCfg, nil
}

// proxyBypassHost reduces a registry reference to the bare host name used in a
// NO_PROXY entry. The port is dropped on purpose: an entry carrying one
// exempts only that port, and the same appliance host is dialled on 443, 15001
// and 18080 depending on the mode it runs in.
func proxyBypassHost(entry string) string {
	entry = normalizeRegistryHost(entry)
	if entry == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(entry); err == nil {
		return host
	}
	return entry
}

// serviceNoProxyEntries derives the bypass list written into the agent
// service's NO_PROXY, from the device's own .flock plus whatever -no-proxy
// added.
//
// This exists because NO_PROXY matches on the host name a client *dials*,
// never on the address that name resolves to. An appliance reached by name is
// therefore not covered by an entry naming its IP, so the agent sends its
// platform connection out through the corporate proxy — and most proxies
// refuse to route back into the internal network, leaving the device offline.
//
// Go reads a bare domain as covering the domain and all of its subdomains
// ("foo.com" also matches "ws.foo.com"), which is all the agent itself needs;
// the leading-dot form is emitted alongside it for the tools that only honour
// that spelling, matching what app containers already receive.
//
// Cloud devices get nothing beyond the loopback defaults and the operator's
// own entries: their endpoints are public and DO need the proxy.
func serviceNoProxyEntries(cfg *config.ReswarmConfig, extra string) []string {
	entries := make([]string, 0, 8)
	seen := make(map[string]bool)
	add := func(entry string) {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" || seen[entry] {
			return
		}
		seen[entry] = true
		entries = append(entries, entry)
	}

	add("localhost")
	add("127.0.0.1")

	if cfg != nil {
		if domain := strings.TrimSpace(cfg.ApplianceDomain); domain != "" {
			add(domain)
			add("." + domain)
		}
		// A plain-HTTP registry is a LAN registry by construction, so the host
		// serving it is never reached through the corporate proxy.
		for _, registry := range insecureRegistryEntries(cfg) {
			add(proxyBypassHost(registry))
		}
	}

	for _, entry := range strings.Split(extra, ",") {
		add(entry)
	}

	return entries
}

// mergeCredentialHelperOptOut returns dockerConfigJSON with an empty
// credential-helper entry recorded for each registry, which makes the Docker
// CLI fall back to its plaintext file store for exactly those registries.
//
// This is required on Windows, not a preference: the agent logs in with the
// backend's registry token as the *username*, an RS256 JWT well over a
// thousand characters, and Windows Credential Manager rejects a username that
// long ("The array bounds are invalid."). The CLI auto-detects `wincred`
// whenever its helper binary exists, so clearing `credsStore` is not enough —
// only a per-registry entry opts out. Authentication succeeds either way; it
// is storing the credential that fails, which fails the whole login and with
// it every app install on the device.
func mergeCredentialHelperOptOut(dockerConfigJSON []byte, registries []string) (merged []byte, changed bool, err error) {
	dockerCfg := map[string]interface{}{}
	if len(bytes.TrimSpace(dockerConfigJSON)) > 0 {
		err = json.Unmarshal(dockerConfigJSON, &dockerCfg)
		if err != nil {
			return nil, false, fmt.Errorf("existing docker config.json is not valid JSON: %w", err)
		}
	}

	helpers, _ := dockerCfg["credHelpers"].(map[string]interface{})
	if helpers == nil {
		helpers = map[string]interface{}{}
	}

	for _, registry := range registries {
		if registry == "" {
			continue
		}
		// Never override an operator's deliberate helper choice.
		if existing, ok := helpers[registry]; ok {
			if s, isString := existing.(string); isString && s == "" {
				continue
			}
			continue
		}
		helpers[registry] = ""
		changed = true
	}
	if !changed {
		return dockerConfigJSON, false, nil
	}
	dockerCfg["credHelpers"] = helpers

	merged, err = json.MarshalIndent(dockerCfg, "", "  ")
	if err != nil {
		return nil, false, err
	}
	return append(merged, '\n'), true, nil
}

// mergeInsecureRegistries returns daemonJSON with entries merged into its
// "insecure-registries" array, preserving every other key and every existing
// entry. Empty input starts from {}. Malformed JSON is an error — an
// operator's hand-edited daemon.json must never be clobbered.
func mergeInsecureRegistries(daemonJSON []byte, entries []string) (merged []byte, changed bool, err error) {
	daemonCfg := map[string]interface{}{}
	if len(bytes.TrimSpace(daemonJSON)) > 0 {
		err = json.Unmarshal(daemonJSON, &daemonCfg)
		if err != nil {
			return nil, false, fmt.Errorf("existing daemon.json is not valid JSON: %w", err)
		}
	}

	existing, _ := daemonCfg["insecure-registries"].([]interface{})
	seen := make(map[string]bool)
	for _, v := range existing {
		if s, ok := v.(string); ok {
			seen[normalizeRegistryHost(s)] = true
		}
	}

	for _, entry := range entries {
		if seen[entry] {
			continue
		}
		existing = append(existing, entry)
		seen[entry] = true
		changed = true
	}
	if !changed {
		return daemonJSON, false, nil
	}
	daemonCfg["insecure-registries"] = existing

	merged, err = json.MarshalIndent(daemonCfg, "", "  ")
	if err != nil {
		return nil, false, err
	}
	return append(merged, '\n'), true, nil
}

type serviceInstallOptions struct {
	ConfigPath  string
	AgentDir    string
	AppsDir     string
	Proxy       string
	NoProxy     string
	ForceProxy  bool
	StartNow    bool
	AgentDirSet bool
}

// parseServiceInstallFlags parses `reagent service install` flags.
// programData is the value of %ProgramData% (injected for tests).
func parseServiceInstallFlags(args []string, programData string, output io.Writer) (*serviceInstallOptions, error) {
	flags := flag.NewFlagSet("service install", flag.ContinueOnError)
	flags.SetOutput(output)

	configPath := flags.String("config", "", "path to the device's .flock configuration file (required)")
	agentDir := flags.String("agentDir", "", "agent directory (default: %ProgramData%\\IronFlock\\Reagent)")
	appsDir := flags.String("appsDir", "", "apps directory (default: <agentDir>\\apps)")
	proxy := flags.String("proxy", "", "optional HTTP(S) proxy URL, written to the service environment")
	// The counterpart to ironflock-init's --no-proxy on Linux. Without it the
	// service environment's bypass list was fixed at localhost,127.0.0.1 and
	// every hand-added exemption was wiped by the next `service install`.
	noProxy := flags.String("no-proxy", "", "comma-separated hosts to reach directly, bypassing -proxy (merged with the device's own appliance and registry hosts)")
	forceProxy := flags.Bool("force-proxy", false, "overwrite an existing "+proxyEnvFileName+" with the values from -proxy/-no-proxy")
	// Installing the agent and leaving it stopped is never what an operator wants — a device
	// that was just registered should come online — so installing starts the service. `-start`
	// stays accepted because every published doc and provisioning script passes it, but it is
	// now the default and therefore a no-op.
	noStart := flags.Bool("noStart", false, "install the service but do not start it")
	_ = flags.Bool("start", false, "deprecated: installing already starts the service")

	err := flags.Parse(args)
	if err != nil {
		return nil, err
	}

	if *configPath == "" {
		return nil, fmt.Errorf("-config <path-to-.flock> is required")
	}

	if *noProxy != "" && *proxy == "" {
		return nil, fmt.Errorf("-no-proxy has no effect without -proxy")
	}
	if *forceProxy && *proxy == "" {
		return nil, fmt.Errorf("-force-proxy has no effect without -proxy")
	}

	opts := serviceInstallOptions{
		ConfigPath:  *configPath,
		AgentDirSet: *agentDir != "",
		Proxy:       *proxy,
		NoProxy:     *noProxy,
		ForceProxy:  *forceProxy,
		StartNow:    !*noStart,
	}

	opts.AgentDir = *agentDir
	if opts.AgentDir == "" {
		if programData == "" {
			return nil, fmt.Errorf("%%ProgramData%% is not set and no -agentDir was given")
		}
		opts.AgentDir = filepath.Join(programData, "IronFlock", "Reagent")
	}

	opts.AppsDir = *appsDir
	if opts.AppsDir == "" {
		opts.AppsDir = filepath.Join(opts.AgentDir, "apps")
	}

	return &opts, nil
}

// installCliArgs builds the CommandLineArguments the installer needs for
// filesystem.InitDirectories, mirroring how config.GetCliArguments derives
// sub-directories from -agentDir/-appsDir.
func installCliArgs(opts *serviceInstallOptions) *config.CommandLineArguments {
	return &config.CommandLineArguments{
		AgentDir:       opts.AgentDir,
		AppsDirectory:  opts.AppsDir,
		AppsBuildDir:   opts.AppsDir + "/build",
		AppsComposeDir: opts.AppsDir + "/compose",
		AppsSharedDir:  opts.AppsDir + "/shared",
		DownloadDir:    opts.AgentDir + "/downloads",
	}
}

// serviceBinaryArgs are the arguments baked into the service ImagePath. Every
// path is explicit so the os.UserHomeDir-based defaults (meaningless under
// LocalSystem) never apply at service runtime.
func serviceBinaryArgs(opts *serviceInstallOptions) []string {
	return []string{
		"-config", filepath.Join(opts.AgentDir, serviceConfigName),
		"-agentDir", opts.AgentDir,
		"-appsDir", opts.AppsDir,
		"-dbFileName", filepath.Join(opts.AgentDir, "reagent.db"),
		"-logFile", filepath.Join(opts.AgentDir, "reagent.log"),
	}
}

// migrationAbortMessage explains the choice when data from a previous manual
// (console) installation exists: reuse it, or start fresh. Moving the data is
// deliberately not offered — app /data bind-mount sources are baked into the
// existing containers, so a silent move would strand their data.
func migrationAbortMessage(existingDirs []string, defaultAgentDir string) string {
	var b strings.Builder
	b.WriteString("Found data from a previous agent installation:\n")
	for _, dir := range existingDirs {
		b.WriteString("  " + dir + "\n")
	}
	b.WriteString("\nRe-run with an explicit -agentDir to choose:\n")
	b.WriteString("  keep using the existing device data:  -agentDir \"" + existingDirs[0] + "\"\n")
	b.WriteString("  start fresh (existing app data stays behind):  -agentDir \"" + defaultAgentDir + "\"\n")
	return b.String()
}

// repairCmdContent is the boot-time repair script registered as an ONSTART
// scheduled task. It closes the one gap in-process code cannot: an
// interrupted self-update swap that left the service ImagePath vacant (SCM
// recovery actions do not fire for a service that fails to START). Prefers
// the known-good previous binary, falls back to the newest downloaded one.
func repairCmdContent(agentDir string) string {
	return strings.Join([]string{
		"@echo off",
		"rem IronFlock agent repair (registered as scheduled task '" + repairTaskName + "')",
		"rem Restores the service binary if an interrupted self-update left it missing.",
		"set \"AGENTDIR=" + agentDir + "\"",
		"if exist \"%AGENTDIR%\\reagent.exe\" goto start",
		"if exist \"%AGENTDIR%\\reagent-prev.exe\" (",
		"  ren \"%AGENTDIR%\\reagent-prev.exe\" \"reagent.exe\"",
		"  goto start",
		")",
		"for /f \"delims=\" %%F in ('dir /b /o-d \"%AGENTDIR%\\reagent-v*.exe\" 2^>nul') do (",
		"  ren \"%AGENTDIR%\\%%F\" \"reagent.exe\"",
		"  goto start",
		")",
		":start",
		"sc start reagent",
		"",
	}, "\r\n")
}
