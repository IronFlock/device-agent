package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reagent/config"
	"reagent/winservice"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseServiceInstallFlagsDefaults(t *testing.T) {
	opts, err := parseServiceInstallFlags([]string{"-config", "C:\\tmp\\dev.flock"}, `C:\ProgramData`, io.Discard)
	require.NoError(t, err)

	assert.Equal(t, "C:\\tmp\\dev.flock", opts.ConfigPath)
	assert.False(t, opts.AgentDirSet)
	assert.Equal(t, filepath.Join(`C:\ProgramData`, "IronFlock", "DeviceAgent"), opts.AgentDir)
	assert.Equal(t, filepath.Join(opts.AgentDir, "apps"), opts.AppsDir)
	assert.True(t, opts.StartNow, "installing must start the service by default")
	assert.Empty(t, opts.Proxy)
}

// -start predates the start-by-default behaviour and is passed by every published
// doc and provisioning script, so it must keep parsing (as a no-op).
func TestParseServiceInstallFlagsStartIsAcceptedNoOp(t *testing.T) {
	opts, err := parseServiceInstallFlags([]string{"-config", "dev.flock", "-start"}, `C:\ProgramData`, io.Discard)
	require.NoError(t, err)
	assert.True(t, opts.StartNow)
}

func TestParseServiceInstallFlagsNoStart(t *testing.T) {
	opts, err := parseServiceInstallFlags([]string{"-config", "dev.flock", "-noStart"}, `C:\ProgramData`, io.Discard)
	require.NoError(t, err)
	assert.False(t, opts.StartNow)

	// -noStart wins even when the legacy -start is also present.
	opts, err = parseServiceInstallFlags([]string{"-config", "dev.flock", "-start", "-noStart"}, `C:\ProgramData`, io.Discard)
	require.NoError(t, err)
	assert.False(t, opts.StartNow)
}

func TestParseServiceInstallFlagsExplicit(t *testing.T) {
	opts, err := parseServiceInstallFlags([]string{
		"-config", "dev.flock",
		"-agentDir", `D:\data\reagent`,
		"-proxy", "http://proxy:3128",
		"-start",
	}, `C:\ProgramData`, io.Discard)
	require.NoError(t, err)

	assert.True(t, opts.AgentDirSet)
	assert.Equal(t, `D:\data\reagent`, opts.AgentDir)
	assert.Equal(t, filepath.Join(`D:\data\reagent`, "apps"), opts.AppsDir)
	assert.Equal(t, "http://proxy:3128", opts.Proxy)
	assert.True(t, opts.StartNow)
}

func TestParseServiceInstallFlagsRejectsUnknown(t *testing.T) {
	_, err := parseServiceInstallFlags([]string{"-config", "dev.flock", "-bogus"}, `C:\ProgramData`, io.Discard)
	require.Error(t, err)
}

func TestParseServiceInstallFlagsRequiresConfig(t *testing.T) {
	_, err := parseServiceInstallFlags([]string{}, `C:\ProgramData`, io.Discard)
	require.ErrorContains(t, err, "-config")
}

func TestParseServiceInstallFlagsRequiresProgramData(t *testing.T) {
	_, err := parseServiceInstallFlags([]string{"-config", "dev.flock"}, "", io.Discard)
	require.ErrorContains(t, err, "ProgramData")

	// but an explicit -agentDir works without %ProgramData%
	opts, err := parseServiceInstallFlags([]string{"-config", "dev.flock", "-agentDir", `D:\reagent`}, "", io.Discard)
	require.NoError(t, err)
	assert.Equal(t, `D:\reagent`, opts.AgentDir)
}

// The ImagePath arguments must pin every path the agent would otherwise
// derive from os.UserHomeDir (meaningless under LocalSystem), under the
// current names (device-agent.db, device-agent.log), and the service carries
// the proxy environment it was installed with.
func TestServiceSpec(t *testing.T) {
	opts := &serviceInstallOptions{AgentDir: `C:\ProgramData\IronFlock\DeviceAgent`, AppsDir: `C:\ProgramData\IronFlock\DeviceAgent\apps`}
	spec := serviceSpec(opts, []string{"HTTP_PROXY=http://proxy:3128"})

	assert.Equal(t, winservice.ServiceName, spec.Name)
	assert.Equal(t, filepath.Join(opts.AgentDir, "device-agent.exe"), spec.Exe)
	joined := strings.Join(spec.Args, " ")
	assert.Contains(t, joined, "-config "+filepath.Join(opts.AgentDir, winservice.ConfigName))
	assert.Contains(t, joined, "-agentDir "+opts.AgentDir)
	assert.Contains(t, joined, "-appsDir "+opts.AppsDir)
	assert.Contains(t, joined, "-dbFileName "+filepath.Join(opts.AgentDir, "device-agent.db"))
	assert.Contains(t, joined, "-logFile "+filepath.Join(opts.AgentDir, "device-agent.log"))
	assert.NotContains(t, joined, "reagent")
	assert.Equal(t, []string{"HTTP_PROXY=http://proxy:3128"}, spec.Env)
	assert.Nil(t, serviceSpec(opts, nil).Env, "no proxy: an existing environment (a WSL engine's DOCKER_HOST) is left alone")
}

// Installing into a folder from before 1.4.0 (the old service folder, a
// console agent's) renames the database first: the service would otherwise
// open an empty device-agent.db and treat every app container as an orphan.
func TestLegacyDatabaseRenames(t *testing.T) {
	files := func(names ...string) func(string) bool {
		return func(name string) bool {
			for _, n := range names {
				if n == name {
					return true
				}
			}
			return false
		}
	}
	assert.Equal(t, [][2]string{{"reagent.db", "device-agent.db"}, {"reagent.db-wal", "device-agent.db-wal"}, {"reagent.db-shm", "device-agent.db-shm"}},
		legacyDatabaseRenames(files("reagent.db", "reagent.db-wal", "reagent.db-shm", "reagent.exe")))
	assert.Equal(t, [][2]string{{"reagent.db", "device-agent.db"}}, legacyDatabaseRenames(files("reagent.db")))
	assert.Nil(t, legacyDatabaseRenames(files("device-agent.db", "reagent.db")), "the current database wins over a stale legacy one")
	assert.Nil(t, legacyDatabaseRenames(files("reagent.db-wal")), "no database, only a stray journal")
	assert.Nil(t, legacyDatabaseRenames(files()))
}

// An install without -agentDir must notice the data of every earlier kind
// of installation: console agents below the user profiles under either
// database name, and the service folder from before 1.4.0.
func TestExistingAgentDirCandidates(t *testing.T) {
	drive := "C:" + string(filepath.Separator)
	programData := filepath.Join(drive, "ProgramData")
	assert.Equal(t, []string{
		filepath.Join(drive, "Users", "*", "reagent", "reagent.db"),
		filepath.Join(drive, "Users", "*", "reagent", "device-agent.db"),
		filepath.Join(programData, "IronFlock", "Reagent", "reagent.db"),
		filepath.Join(programData, "IronFlock", "Reagent", "device-agent.db"),
	}, existingAgentDirCandidates("C:", programData))
}

func TestInstallCliArgsDerivesSubdirs(t *testing.T) {
	opts := &serviceInstallOptions{AgentDir: `C:\agent`, AppsDir: `C:\agent\apps`}
	cliArgs := installCliArgs(opts)

	assert.Equal(t, `C:\agent`, cliArgs.AgentDir)
	assert.Equal(t, `C:\agent\apps`, cliArgs.AppsDirectory)
	assert.Equal(t, `C:\agent\apps/build`, cliArgs.AppsBuildDir)
	assert.Equal(t, `C:\agent\apps/compose`, cliArgs.AppsComposeDir)
	assert.Equal(t, `C:\agent\apps/shared`, cliArgs.AppsSharedDir)
	assert.Equal(t, `C:\agent/downloads`, cliArgs.DownloadDir)
}

func TestMigrationAbortMessageOffersBothChoices(t *testing.T) {
	message := migrationAbortMessage([]string{`C:\Users\alice\reagent`}, `C:\ProgramData\IronFlock\DeviceAgent`)

	assert.Contains(t, message, `C:\Users\alice\reagent`)
	assert.Contains(t, message, `-agentDir "C:\Users\alice\reagent"`)
	assert.Contains(t, message, `-agentDir "C:\ProgramData\IronFlock\DeviceAgent"`)
}

func TestCertImportArgs(t *testing.T) {
	args := certImportArgs("Root", `C:\ProgramData\IronFlock\DeviceAgent\ironflock-root.crt`)
	assert.Equal(t, []string{"-addstore", "-f", "Root", `C:\ProgramData\IronFlock\DeviceAgent\ironflock-root.crt`}, args)

	pub := certImportArgs("TrustedPublisher", `C:\x\root.crt`)
	assert.Equal(t, "TrustedPublisher", pub[2])
}

func TestCertDeleteArgs(t *testing.T) {
	assert.Equal(t, []string{"-delstore", "Root", "IronFlock Code Signing Root"},
		certDeleteArgs("Root", "IronFlock Code Signing Root"))
}

func TestDefenderExclusionCmds(t *testing.T) {
	frpc := `C:\ProgramData\IronFlock\DeviceAgent\frpc.exe`
	add := defenderAddExclusionCmd(frpc)
	assert.Contains(t, add, "Add-MpPreference -ExclusionPath")
	assert.Contains(t, add, frpc)

	rm := defenderRemoveExclusionCmd(frpc)
	assert.Contains(t, rm, "Remove-MpPreference -ExclusionPath")
	assert.Contains(t, rm, frpc)
}

// The -agentDir the installer baked into the ImagePath, from the decomposed
// command line (agentDirFromImagePath, Windows, decomposes it as the SCM
// composed it).
func TestAgentDirFromArgs(t *testing.T) {
	assert.Equal(t, `C:\ProgramData\IronFlock\DeviceAgent`, agentDirFromArgs([]string{
		`C:\ProgramData\IronFlock\DeviceAgent\device-agent.exe`, "-config", `C:\ProgramData\IronFlock\DeviceAgent\device.flock`,
		"-agentDir", `C:\ProgramData\IronFlock\DeviceAgent`, "-appsDir", `C:\ProgramData\IronFlock\DeviceAgent\apps`}))
	assert.Equal(t, `D:\my data\agent`, agentDirFromArgs([]string{`C:\Program Files\IronFlock\device-agent.exe`, "-agentDir", `D:\my data\agent`}))
	assert.Equal(t, "", agentDirFromArgs([]string{`C:\x\device-agent.exe`, "-config", "c.flock"}))
	assert.Equal(t, "", agentDirFromArgs([]string{`C:\x\device-agent.exe`, "-agentDir"}), "a flag without its value")
}

// Appliance-issued device flocks list only the appliance's *local* registry
// names; the externally reachable registry comes from docker_registry_url and
// must be merged in.
func TestInsecureRegistryEntriesApplianceDeviceFlock(t *testing.T) {
	entries := insecureRegistryEntries(&config.ReswarmConfig{
		InsecureRegistries: `["localhost:15001", "appstore-registry:5000"]`,
		DockerRegistryURL:  "136.230.111.59:15001/",
	})
	assert.Equal(t, []string{"localhost:15001", "appstore-registry:5000", "136.230.111.59:15001"}, entries)
}

// The appliance installer writes the agent's own flock with a bare
// `host:port/` string (not a JSON array); it duplicates docker_registry_url.
func TestInsecureRegistryEntriesBareLegacyString(t *testing.T) {
	entries := insecureRegistryEntries(&config.ReswarmConfig{
		InsecureRegistries: "136.230.111.59:15001/",
		DockerRegistryURL:  "136.230.111.59:15001/",
	})
	assert.Equal(t, []string{"136.230.111.59:15001"}, entries)
}

// Cloud and domain-mode registries are HTTPS with no explicit port — nothing
// must be marked insecure for them.
func TestInsecureRegistryEntriesHTTPSRegistries(t *testing.T) {
	assert.Empty(t, insecureRegistryEntries(&config.ReswarmConfig{
		DockerRegistryURL: "registry.ironflock.com/",
	}))
	assert.Empty(t, insecureRegistryEntries(&config.ReswarmConfig{
		DockerRegistryURL: "registry.customer.example/",
	}))
}

func TestMergeInsecureRegistriesCreatesFromEmpty(t *testing.T) {
	merged, changed, err := mergeInsecureRegistries(nil, []string{"136.230.111.59:15001"})
	require.NoError(t, err)
	assert.True(t, changed)

	var daemonCfg map[string]interface{}
	require.NoError(t, json.Unmarshal(merged, &daemonCfg))
	assert.Equal(t, []interface{}{"136.230.111.59:15001"}, daemonCfg["insecure-registries"])
}

// Docker Desktop machines commonly have a daemon.json already — other keys and
// existing entries must survive the merge untouched.
func TestMergeInsecureRegistriesPreservesExisting(t *testing.T) {
	existing := []byte(`{
  "builder": {"gc": {"enabled": true}},
  "insecure-registries": ["hubproxy.docker.internal:5555"]
}`)
	merged, changed, err := mergeInsecureRegistries(existing, []string{"136.230.111.59:15001"})
	require.NoError(t, err)
	assert.True(t, changed)

	var daemonCfg map[string]interface{}
	require.NoError(t, json.Unmarshal(merged, &daemonCfg))
	assert.Equal(t, []interface{}{"hubproxy.docker.internal:5555", "136.230.111.59:15001"}, daemonCfg["insecure-registries"])
	assert.Contains(t, daemonCfg, "builder")
}

// A rerun (or an operator who already added the entry by hand) must be a
// byte-for-byte no-op.
func TestMergeInsecureRegistriesIdempotent(t *testing.T) {
	existing := []byte(`{"insecure-registries": ["136.230.111.59:15001"]}`)
	merged, changed, err := mergeInsecureRegistries(existing, []string{"136.230.111.59:15001"})
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, existing, merged)
}

// A hand-edited daemon.json that no longer parses must never be clobbered.
func TestMergeInsecureRegistriesRejectsMalformed(t *testing.T) {
	_, _, err := mergeInsecureRegistries([]byte(`{"insecure-registries": [`), []string{"x:1"})
	assert.Error(t, err)
}

// The registry token is used as the docker *username* and is far longer than
// Windows Credential Manager accepts, so the app registry must be opted out of
// the credential helper or every login fails at the store step.
func TestMergeCredentialHelperOptOutCreatesEntry(t *testing.T) {
	merged, changed, err := mergeCredentialHelperOptOut(nil, []string{"136.230.111.59:15001"})
	require.NoError(t, err)
	assert.True(t, changed)

	var cfg map[string]interface{}
	require.NoError(t, json.Unmarshal(merged, &cfg))
	helpers := cfg["credHelpers"].(map[string]interface{})
	assert.Equal(t, "", helpers["136.230.111.59:15001"])
}

// Docker Desktop machines have a config.json already; auths, contexts and any
// other keys must survive untouched.
func TestMergeCredentialHelperOptOutPreservesConfig(t *testing.T) {
	existing := []byte(`{"auths":{"registry.example":{"auth":"x"}},"currentContext":"desktop-linux"}`)
	merged, changed, err := mergeCredentialHelperOptOut(existing, []string{"136.230.111.59:15001"})
	require.NoError(t, err)
	assert.True(t, changed)

	var cfg map[string]interface{}
	require.NoError(t, json.Unmarshal(merged, &cfg))
	assert.Contains(t, cfg, "auths")
	assert.Equal(t, "desktop-linux", cfg["currentContext"])
	assert.Equal(t, "", cfg["credHelpers"].(map[string]interface{})["136.230.111.59:15001"])
}

// An operator who deliberately configured a helper for this registry keeps it.
func TestMergeCredentialHelperOptOutKeepsOperatorChoice(t *testing.T) {
	existing := []byte(`{"credHelpers":{"136.230.111.59:15001":"wincred"}}`)
	merged, changed, err := mergeCredentialHelperOptOut(existing, []string{"136.230.111.59:15001"})
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, existing, merged)
}

func TestMergeCredentialHelperOptOutIdempotent(t *testing.T) {
	existing := []byte(`{"credHelpers":{"136.230.111.59:15001":""}}`)
	_, changed, err := mergeCredentialHelperOptOut(existing, []string{"136.230.111.59:15001"})
	require.NoError(t, err)
	assert.False(t, changed)
}

func TestMergeCredentialHelperOptOutRejectsMalformed(t *testing.T) {
	_, _, err := mergeCredentialHelperOptOut([]byte(`{"credHelpers":`), []string{"x:1"})
	assert.Error(t, err)
}

// A device attached to an appliance in domain mode must reach it directly. The
// LAC edge PC went offline exactly here: NO_PROXY was fixed at
// localhost,127.0.0.1, so the agent sent its platform connection to the
// corporate proxy, which will not route back into the internal network.
func TestServiceNoProxyEntriesApplianceDevice(t *testing.T) {
	entries := serviceNoProxyEntries(&config.ReswarmConfig{
		ApplianceDomain:   "tls-sf015.corp.trumpf.com",
		DeviceEndpointURL: "wss://ws.tls-sf015.corp.trumpf.com/ws",
		DockerRegistryURL: "136.230.111.59:15001/",
	}, "")

	assert.Equal(t, []string{
		"localhost",
		"127.0.0.1",
		"tls-sf015.corp.trumpf.com",
		".tls-sf015.corp.trumpf.com",
		"136.230.111.59",
	}, entries)
}

// Negative control: a cloud device's endpoints are public and DO need the
// proxy. Nothing may be added beyond the loopback defaults, or configuring a
// proxy would stop the device from reaching the platform at all.
func TestServiceNoProxyEntriesCloudDeviceAddsNothing(t *testing.T) {
	entries := serviceNoProxyEntries(&config.ReswarmConfig{
		DeviceEndpointURL: "wss://cbw.ironflock.com/ws",
		DockerRegistryURL: "registry.ironflock.com/",
	}, "")

	assert.Equal(t, []string{"localhost", "127.0.0.1"}, entries)
}

// The registry port is dropped: an entry carrying one exempts only that port,
// and the same appliance host is dialled on 443, 15001 and 18080 depending on
// the mode it runs in.
func TestServiceNoProxyEntriesDropsRegistryPort(t *testing.T) {
	entries := serviceNoProxyEntries(&config.ReswarmConfig{
		DockerRegistryURL: "appliance.corp.example:15001/",
	}, "")

	assert.Equal(t, []string{"localhost", "127.0.0.1", "appliance.corp.example"}, entries)
}

func TestServiceNoProxyEntriesMergesFlagWithoutDuplicating(t *testing.T) {
	entries := serviceNoProxyEntries(&config.ReswarmConfig{
		ApplianceDomain: "tls-sf015.corp.trumpf.com",
	}, " TLS-SF015.corp.trumpf.com , 10.0.0.0/8 ,, localhost ")

	assert.Equal(t, []string{
		"localhost",
		"127.0.0.1",
		"tls-sf015.corp.trumpf.com",
		".tls-sf015.corp.trumpf.com",
		"10.0.0.0/8",
	}, entries)
}

func TestParseServiceInstallFlagsNoProxy(t *testing.T) {
	opts, err := parseServiceInstallFlags([]string{
		"-config", "dev.flock",
		"-proxy", "http://srv01prox.corp.trumpf.com:80",
		"-no-proxy", "appliance.corp.example",
	}, `C:\ProgramData`, io.Discard)
	require.NoError(t, err)

	assert.Equal(t, "http://srv01prox.corp.trumpf.com:80", opts.Proxy)
	assert.Equal(t, "appliance.corp.example", opts.NoProxy)
}

// The service environment is only written when a proxy is configured, so
// -no-proxy on its own would be silently discarded.
func TestParseServiceInstallFlagsNoProxyRequiresProxy(t *testing.T) {
	_, err := parseServiceInstallFlags([]string{
		"-config", "dev.flock",
		"-no-proxy", "appliance.corp.example",
	}, `C:\ProgramData`, io.Discard)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "-no-proxy has no effect without -proxy")
}

func TestParseProxyEnvIgnoresCommentsAndQuotes(t *testing.T) {
	cfg := parseProxyEnv([]byte(`
# a comment
HTTP_PROXY=http://proxy.example:3128
HTTPS_PROXY="http://proxy.example:3128"
  no_proxy = localhost,appliance.corp.example
UNKNOWN_KEY=ignored
not-a-pair
`))

	assert.Equal(t, "http://proxy.example:3128", cfg.HTTP)
	assert.Equal(t, "http://proxy.example:3128", cfg.HTTPS)
	assert.Equal(t, "localhost,appliance.corp.example", cfg.NoProxy)
}

func TestRenderProxyEnvRoundTrips(t *testing.T) {
	cfg := proxyConfig{
		HTTP:    "http://proxy.example:3128",
		HTTPS:   "http://proxy.example:3128",
		NoProxy: "localhost,127.0.0.1,appliance.corp.example",
	}
	assert.Equal(t, cfg, parseProxyEnv([]byte(renderProxyEnv(cfg))))
}

// The seeded file must name the derived hosts literally, or an operator whose
// appliance is only reachable THROUGH the proxy has nothing to delete.
func TestRenderProxyEnvNamesDerivedHosts(t *testing.T) {
	rendered := renderProxyEnv(proxyConfig{
		HTTP:    "http://proxy.example:3128",
		HTTPS:   "http://proxy.example:3128",
		NoProxy: "localhost,127.0.0.1,tls-sf015.corp.trumpf.com",
	})
	assert.Contains(t, rendered, "NO_PROXY=localhost,127.0.0.1,tls-sf015.corp.trumpf.com")
}

func writeTestFlock(t *testing.T, dir string, cfg config.ReswarmConfig) string {
	t.Helper()
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	path := filepath.Join(dir, "device.flock")
	require.NoError(t, os.WriteFile(path, raw, 0600))
	return path
}

func TestResolveProxyConfigSeedsFromFlags(t *testing.T) {
	dir := t.TempDir()
	flock := writeTestFlock(t, dir, config.ReswarmConfig{ApplianceDomain: "appliance.corp.example"})
	envPath := filepath.Join(dir, proxyEnvFileName)

	cfg, seeded, err := resolveProxyConfig(envPath, &serviceInstallOptions{Proxy: "http://proxy.example:3128"}, flock)
	require.NoError(t, err)
	assert.True(t, seeded)
	assert.Equal(t, "http://proxy.example:3128", cfg.HTTPS)
	assert.Equal(t, "localhost,127.0.0.1,appliance.corp.example,.appliance.corp.example", cfg.NoProxy)

	written, err := os.ReadFile(envPath)
	require.NoError(t, err)
	assert.Equal(t, cfg, parseProxyEnv(written))
}

// The whole point of the file: a reinstall restores the site's settings with no
// flags at all, because uninstall destroyed the service key that held them.
func TestResolveProxyConfigExistingFileWinsWithoutFlags(t *testing.T) {
	dir := t.TempDir()
	flock := writeTestFlock(t, dir, config.ReswarmConfig{ApplianceDomain: "appliance.corp.example"})
	envPath := filepath.Join(dir, proxyEnvFileName)
	require.NoError(t, os.WriteFile(envPath, []byte("HTTP_PROXY=http://old:80\nHTTPS_PROXY=http://old:80\nNO_PROXY=localhost,edited.example\n"), 0600))

	cfg, seeded, err := resolveProxyConfig(envPath, &serviceInstallOptions{}, flock)
	require.NoError(t, err)
	assert.False(t, seeded)
	assert.Equal(t, "http://old:80", cfg.HTTPS)
	assert.Equal(t, "localhost,edited.example", cfg.NoProxy)
}

// An operator who deleted the appliance from NO_PROXY (appliance reachable only
// THROUGH the proxy) must not have that undone by a reinstall that passes -proxy.
func TestResolveProxyConfigDoesNotUndoAnEdit(t *testing.T) {
	dir := t.TempDir()
	flock := writeTestFlock(t, dir, config.ReswarmConfig{ApplianceDomain: "appliance.corp.example"})
	envPath := filepath.Join(dir, proxyEnvFileName)
	require.NoError(t, os.WriteFile(envPath, []byte("HTTP_PROXY=http://proxy.example:3128\nHTTPS_PROXY=http://proxy.example:3128\nNO_PROXY=localhost,127.0.0.1\n"), 0600))

	cfg, _, err := resolveProxyConfig(envPath, &serviceInstallOptions{Proxy: "http://proxy.example:3128", NoProxy: "something.else"}, flock)
	require.NoError(t, err)
	assert.Equal(t, "localhost,127.0.0.1", cfg.NoProxy)
	assert.NotContains(t, cfg.NoProxy, "appliance.corp.example")
}

func TestResolveProxyConfigForceReplaces(t *testing.T) {
	dir := t.TempDir()
	flock := writeTestFlock(t, dir, config.ReswarmConfig{ApplianceDomain: "appliance.corp.example"})
	envPath := filepath.Join(dir, proxyEnvFileName)
	require.NoError(t, os.WriteFile(envPath, []byte("HTTP_PROXY=http://old:80\nHTTPS_PROXY=http://old:80\nNO_PROXY=localhost,edited.example\n"), 0600))

	cfg, seeded, err := resolveProxyConfig(envPath, &serviceInstallOptions{Proxy: "http://new:3128", ForceProxy: true}, flock)
	require.NoError(t, err)
	assert.True(t, seeded)
	assert.Equal(t, "http://new:3128", cfg.HTTPS)
	assert.Contains(t, cfg.NoProxy, "appliance.corp.example")
}

// Negative control: a device with no proxy and no file writes no service
// environment at all, exactly as before this existed.
func TestResolveProxyConfigNoProxyNoFile(t *testing.T) {
	dir := t.TempDir()
	flock := writeTestFlock(t, dir, config.ReswarmConfig{})
	envPath := filepath.Join(dir, proxyEnvFileName)

	cfg, seeded, err := resolveProxyConfig(envPath, &serviceInstallOptions{}, flock)
	require.NoError(t, err)
	assert.False(t, seeded)
	assert.False(t, cfg.configured())
	assert.NoFileExists(t, envPath)
}

func TestParseServiceInstallFlagsForceProxyRequiresProxy(t *testing.T) {
	_, err := parseServiceInstallFlags([]string{"-config", "dev.flock", "-force-proxy"}, `C:\ProgramData`, io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "-force-proxy has no effect without -proxy")
}
