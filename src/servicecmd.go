package main

// OS-neutral logic for the `device-agent service` subcommand on Windows: flag
// parsing, the migration decision, and generated file/argument contents.
// Kept free of syscalls so it is unit-testable on every platform; the
// Windows-only pieces live in servicecmd_windows.go, the names and scripts in
// package winservice.

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
	"reagent/winservice"
	"strings"
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

// agentDirFromArgs extracts the -agentDir value the installer baked into the
// service's ImagePath (decomposed by the caller), so uninstall can reverse
// the dir-scoped side effects (Defender exclusion, cert file). Returns ""
// when not found.
func agentDirFromArgs(fields []string) string {
	for i, f := range fields {
		if f == "-agentDir" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
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

// proxyEnvFileName is the durable home of a device's proxy configuration, the
// store of docs/WINDOWS-PROXY.md.
//
// It cannot live in the service's own registry key: `service install` refuses
// to run while the service exists, so every reinstall goes through
// `service uninstall`, which deletes the service key with it. Uninstall
// deliberately keeps the agent directory, so a file here is the only store that
// survives. The agent service reads it at its start (applyProxyAtStart), and
// only SYSTEM and Administrators can read it: the service's registry
// environment, where it went before, is readable by every user.
//
// Linux needs no equivalent — the ironflock CLI keeps the Linux agent's proxy
// in /etc/ironflock/proxy.env.
const proxyEnvFileName = winservice.ProxyEnvName

// proxyFlagsGiven reports whether any proxy URL flag of `service install` is
// set.
func (opts *serviceInstallOptions) proxyFlagsGiven() bool {
	return opts.Proxy != "" || opts.HTTPProxy != "" || opts.HTTPSProxy != ""
}

// resolveProxyConfig decides what proxy settings this install applies, and
// reports whether it seeded the store. write puts the store in place
// (atomically, with its ACL, on Windows); password is the content of
// -proxy-password-file, when given.
//
// An existing proxy.env WINS over the flags. That is the point: a reinstall
// must restore the site's configuration without the operator remembering which
// flags the last one used, and a site that edited the bypass list must not have
// that edit silently undone. -force-proxy is the explicit way to replace it.
func resolveProxyConfig(proxyEnvPath string, opts *serviceInstallOptions, password string, write func(path string, data []byte) error) (winservice.ProxySettings, bool, error) {
	existing, err := os.ReadFile(proxyEnvPath)
	switch {
	case err == nil && !opts.ForceProxy:
		cfg := winservice.ParseProxyEnv(existing)
		if opts.proxyFlagsGiven() {
			fmt.Fprintf(os.Stderr, "note: %s exists and wins over the proxy flags — pass -force-proxy to replace it, or change it with 'device-agent.exe service configure'\n", proxyEnvPath)
		}
		return cfg, false, nil
	case err != nil && !os.IsNotExist(err):
		return winservice.ProxySettings{}, false, fmt.Errorf("could not read %s: %w", proxyEnvPath, err)
	}

	if !opts.proxyFlagsGiven() {
		return winservice.ProxySettings{}, false, nil
	}

	// NO_PROXY holds the operator's own entries only: the agent adds what
	// this device reaches directly at every start (effectiveNoProxy).
	cfg := winservice.ProxySettings{
		HTTP:    opts.Proxy,
		HTTPS:   opts.Proxy,
		NoProxy: userNoProxy(opts.NoProxy),
	}
	if opts.HTTPProxy != "" {
		cfg.HTTP = withPassword(opts.HTTPProxy, password)
	}
	if opts.HTTPSProxy != "" {
		cfg.HTTPS = withPassword(opts.HTTPSProxy, password)
	}
	// Beside the .flock: a proxy URL can carry credentials; only SYSTEM and
	// Administrators read the store.
	err = write(proxyEnvPath, []byte(winservice.RenderProxyEnv(cfg)))
	if err != nil {
		return winservice.ProxySettings{}, false, fmt.Errorf("could not write %s: %w", proxyEnvPath, err)
	}
	return cfg, true, nil
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
	ConfigPath string
	AgentDir   string
	AppsDir    string
	Proxy      string
	HTTPProxy  string
	HTTPSProxy string
	NoProxy    string
	ForceProxy bool
	// PasswordFile is -proxy-password-file ("-": standard input).
	PasswordFile string
	StartNow     bool
	AgentDirSet  bool
}

// parseServiceInstallFlags parses `device-agent.exe service install` flags.
// programData is the value of %ProgramData% (injected for tests).
func parseServiceInstallFlags(args []string, programData string, output io.Writer) (*serviceInstallOptions, error) {
	flags := flag.NewFlagSet("service install", flag.ContinueOnError)
	flags.SetOutput(output)

	configPath := flags.String("config", "", "path to the device's .flock configuration file (required)")
	agentDir := flags.String("agentDir", "", "agent directory (default: %ProgramData%\\IronFlock\\"+winservice.Folder+")")
	appsDir := flags.String("appsDir", "", "apps directory (default: <agentDir>\\apps)")
	proxy := flags.String("proxy", "", "optional proxy URL for HTTP and HTTPS, kept in "+proxyEnvFileName+" in the agent directory")
	httpProxy := flags.String("http-proxy", "", "optional HTTP proxy, such as http://proxy:3128, or http://user@proxy:3128 with the password in -proxy-password-file (not with -proxy)")
	httpsProxy := flags.String("https-proxy", "", "optional HTTPS proxy, such as http://proxy:3128, or http://user@proxy:3128 with the password in -proxy-password-file (not with -proxy)")
	passwordFile := flags.String("proxy-password-file", "", "a file whose first line is the password of the user in -http-proxy/-https-proxy, or - for standard input")
	// The counterpart to `ironflock agent install --no-proxy` on Linux. Without
	// it the service environment's bypass list was fixed at localhost,127.0.0.1 and
	// every hand-added exemption was wiped by the next `service install`.
	noProxy := flags.String("no-proxy", "", "comma-separated hosts to reach directly, bypassing the proxy (merged with the device's own appliance and registry hosts)")
	forceProxy := flags.Bool("force-proxy", false, "overwrite an existing "+proxyEnvFileName+" with the values from the proxy flags")
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

	anyProxy := *proxy != "" || *httpProxy != "" || *httpsProxy != ""
	if *proxy != "" && (*httpProxy != "" || *httpsProxy != "") {
		return nil, fmt.Errorf("-proxy sets both protocols; give it alone, or -http-proxy and -https-proxy")
	}
	if *noProxy != "" && !anyProxy {
		return nil, fmt.Errorf("-no-proxy has no effect without -proxy, -http-proxy or -https-proxy")
	}
	if *forceProxy && !anyProxy {
		return nil, fmt.Errorf("-force-proxy has no effect without -proxy, -http-proxy or -https-proxy")
	}
	if *noProxy != "" {
		if err := checkNoProxy(*noProxy); err != nil {
			return nil, fmt.Errorf("-no-proxy %w", err)
		}
	}
	users := 0
	for _, s := range []struct{ name, value string }{{"http-proxy", *httpProxy}, {"https-proxy", *httpsProxy}} {
		if s.value == "" {
			continue
		}
		needs, err := checkProxyURLFlag(s.name, s.value)
		if err != nil {
			return nil, err
		}
		if needs {
			users++
			if *passwordFile == "" {
				return nil, fmt.Errorf("-%s names a user: give the password with -proxy-password-file (a file, or - for standard input), or an empty password in the URL (http://user:@proxy:3128)", s.name)
			}
		}
	}
	if *passwordFile != "" && users == 0 {
		return nil, fmt.Errorf("-proxy-password-file needs -http-proxy or -https-proxy with a user and no password part (http://user@proxy:3128)")
	}

	opts := serviceInstallOptions{
		ConfigPath:   *configPath,
		AgentDirSet:  *agentDir != "",
		Proxy:        *proxy,
		HTTPProxy:    *httpProxy,
		HTTPSProxy:   *httpsProxy,
		NoProxy:      *noProxy,
		ForceProxy:   *forceProxy,
		PasswordFile: *passwordFile,
		StartNow:     !*noStart,
	}

	opts.AgentDir = *agentDir
	if opts.AgentDir == "" {
		if programData == "" {
			return nil, fmt.Errorf("%%ProgramData%% is not set and no -agentDir was given")
		}
		opts.AgentDir = filepath.Join(winservice.Root(programData), winservice.Folder)
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

// serviceSpec is the service as `service install` registers it (ImagePath
// with every path explicit, so the os.UserHomeDir-based defaults —
// meaningless under LocalSystem — never apply at service runtime; recovery;
// the environment, nil to leave an existing value alone; it never holds the
// proxy, which the agent reads from its store).
func serviceSpec(opts *serviceInstallOptions, env []string) winservice.Spec {
	return winservice.AgentSpec(opts.AgentDir, opts.AppsDir, nil, env)
}

// migrationAbortMessage explains the choice when data from a previous
// installation exists: reuse it, or start fresh. Moving the data is
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

// legacyDatabaseRenames lists the renames that give a folder from before
// 1.4.0 (reagent.db) the current database name, so an install into it opens
// the device's data rather than an empty database. Nothing when the current
// database exists already. The names are relative to the folder.
func legacyDatabaseRenames(exists func(name string) bool) [][2]string {
	if exists("device-agent.db") || !exists("reagent.db") {
		return nil
	}
	var renames [][2]string
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if exists("reagent.db" + suffix) {
			renames = append(renames, [2]string{"reagent.db" + suffix, "device-agent.db" + suffix})
		}
	}
	return renames
}

// existingAgentDirCandidates lists where data from a previous installation
// may sit: the console agent's folder below every user profile (either
// database name) and the service folder from before 1.4.0. FlockFlasher's
// throwaway test-device dir (~\.Reflasher\agent) is deliberately not among
// them.
func existingAgentDirCandidates(systemDrive, programData string) []string {
	// "C:" alone would be drive-relative; the separator makes it the root.
	drive := systemDrive + string(filepath.Separator)
	return []string{
		filepath.Join(drive, "Users", "*", "reagent", "reagent.db"),
		filepath.Join(drive, "Users", "*", "reagent", "device-agent.db"),
		filepath.Join(winservice.Root(programData), winservice.LegacyFolder, "reagent.db"),
		filepath.Join(winservice.Root(programData), winservice.LegacyFolder, "device-agent.db"),
	}
}
