package config_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"reagent/config"
	"reagent/testutil/builders"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sampleReswarmJSON returns a representative .flock/.reswarm config document as
// raw JSON bytes plus the values that should round-trip back out of it.
func sampleReswarmJSON(t *testing.T) []byte {
	t.Helper()

	doc := map[string]interface{}{
		"name":   "test-device",
		"secret": "super-secret",
		"board": map[string]interface{}{
			"cpu":          "arm",
			"docs":         nil,
			"board":        "rpi4",
			"model":        "B",
			"boardname":    "Raspberry Pi 4",
			"modelname":    "Model B",
			"reflasher":    true,
			"architecture": "arm64",
		},
		"status":        "registered",
		"password":      "hunter2",
		"wlanssid":      "my-wifi",
		"swarm_key":     42,
		"device_key":    7,
		"swarm_name":    "production-swarm",
		"description":   nil,
		"architecture":  "arm64",
		"serial_number": "SN-001",
		"authentication": map[string]interface{}{
			"key":         "auth-key",
			"certificate": "auth-cert",
		},
		"swarm_owner_name":       "owner",
		"config_passphrase":      "passphrase",
		"device_endpoint_url":    "wss://cbw.ironflock.com/ws-re-dev",
		"environment":            "production",
		"docker_registry_url":    "registry.ironflock.com/",
		"docker_main_repository": "main",
		"appliance_domain":       "appliance.example.com",
	}

	b, err := json.Marshal(doc)
	require.NoError(t, err)
	return b
}

func TestNew(t *testing.T) {
	cliArgs := &config.CommandLineArguments{
		AgentDir:    "/opt/reagent",
		Environment: "production",
		Debug:       true,
	}
	reswarmConfig := &config.ReswarmConfig{
		Name:      "dev",
		SwarmKey:  3,
		DeviceKey: 9,
	}

	cfg := config.New(cliArgs, reswarmConfig)

	// New wires the two pointers through unchanged.
	assert.Same(t, cliArgs, cfg.CommandLineArguments)
	assert.Same(t, reswarmConfig, cfg.ReswarmConfig)
	// StartupLogChannel is not initialized by New.
	assert.Nil(t, cfg.StartupLogChannel)
}

func TestLoadReswarmConfig_ParsesFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "device.flock")
	require.NoError(t, os.WriteFile(path, sampleReswarmJSON(t), 0o644))

	cfg, err := config.LoadReswarmConfig(path)
	require.NoError(t, err)
	require.NotNil(t, cfg)

	assert.Equal(t, "test-device", cfg.Name)
	assert.Equal(t, "super-secret", cfg.Secret)
	assert.Equal(t, "registered", cfg.Status)
	assert.Equal(t, "hunter2", cfg.Password)
	assert.Equal(t, "my-wifi", cfg.Wlanssid)
	assert.Equal(t, 42, cfg.SwarmKey)
	assert.Equal(t, 7, cfg.DeviceKey)
	assert.Equal(t, "production-swarm", cfg.SwarmName)
	assert.Equal(t, "arm64", cfg.Architecture)
	assert.Equal(t, "SN-001", cfg.SerialNumber)
	assert.Equal(t, "owner", cfg.SwarmOwnerName)
	assert.Equal(t, "passphrase", cfg.ConfigPassphrase)
	assert.Equal(t, "production", cfg.Environment)
	assert.Equal(t, "main", cfg.DockerMainRepository)
	assert.Equal(t, "appliance.example.com", cfg.ApplianceDomain)

	// Nested board struct.
	assert.Equal(t, "arm", cfg.Board.CPU)
	assert.Equal(t, "rpi4", cfg.Board.Board)
	assert.Equal(t, "Raspberry Pi 4", cfg.Board.Boardname)
	assert.True(t, cfg.Board.Reflasher)
	assert.Equal(t, "arm64", cfg.Board.Architecture)

	// Nested authentication struct.
	assert.Equal(t, "auth-key", cfg.Authentication.Key)
	assert.Equal(t, "auth-cert", cfg.Authentication.Certificate)

	// Nullable interface fields decode to nil.
	assert.Nil(t, cfg.Description)
	assert.Nil(t, cfg.Board.Docs)
}

func TestLoadReswarmConfig_RewritesLegacyURLs(t *testing.T) {
	tests := []struct {
		name        string
		registryIn  string
		endpointIn  string
		registryOut string
		endpointOut string
	}{
		{
			name:        "legacy values are rewritten",
			registryIn:  "registry.reswarm.io/",
			endpointIn:  "wss://cbw.record-evolution.com/ws-re-dev",
			registryOut: "registry.ironflock.com/",
			endpointOut: "wss://cbw.ironflock.com/ws-re-dev",
		},
		{
			name:        "retired pre-router endpoint devices.reswarm.io:8080 is rewritten",
			registryIn:  "registry.ironflock.com/",
			endpointIn:  "wss://devices.reswarm.io:8080/ws",
			registryOut: "registry.ironflock.com/",
			endpointOut: "wss://cbw.ironflock.com/ws-re-dev",
		},
		{
			name:        "retired pre-router endpoint devices.record-evolution.com:8080 is rewritten",
			registryIn:  "registry.ironflock.com/",
			endpointIn:  "wss://devices.record-evolution.com:8080/ws",
			registryOut: "registry.ironflock.com/",
			endpointOut: "wss://cbw.ironflock.com/ws-re-dev",
		},
		{
			name:        "already-migrated values are left untouched",
			registryIn:  "registry.ironflock.com/",
			endpointIn:  "wss://cbw.ironflock.com/ws-re-dev",
			registryOut: "registry.ironflock.com/",
			endpointOut: "wss://cbw.ironflock.com/ws-re-dev",
		},
		{
			name:        "unrelated custom values are not rewritten",
			registryIn:  "my-registry.local/",
			endpointIn:  "wss://my-endpoint.local/ws",
			registryOut: "my-registry.local/",
			endpointOut: "wss://my-endpoint.local/ws",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "device.flock")

			doc := map[string]interface{}{
				"name":                "d",
				"docker_registry_url": tt.registryIn,
				"device_endpoint_url": tt.endpointIn,
			}
			raw, err := json.Marshal(doc)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, raw, 0o644))

			cfg, err := config.LoadReswarmConfig(path)
			require.NoError(t, err)

			assert.Equal(t, tt.registryOut, cfg.DockerRegistryURL)
			assert.Equal(t, tt.endpointOut, cfg.DeviceEndpointURL)

			// LoadReswarmConfig persists the (possibly rewritten) config back to
			// disk; reloading must yield the same migrated values.
			reloaded, err := config.LoadReswarmConfig(path)
			require.NoError(t, err)
			assert.Equal(t, tt.registryOut, reloaded.DockerRegistryURL)
			assert.Equal(t, tt.endpointOut, reloaded.DeviceEndpointURL)
		})
	}
}

func TestSaveReswarmConfig_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "saved.flock")

	original := builders.DefaultTestConfig().ReswarmConfig
	// Endpoint must avoid the legacy-rewrite branch so the round-trip is exact.
	require.NotEqual(t, "wss://cbw.record-evolution.com/ws-re-dev", original.DeviceEndpointURL)

	require.NoError(t, config.SaveReswarmConfig(path, original))

	// The file exists and is valid JSON.
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var asMap map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &asMap))

	reloaded, err := config.LoadReswarmConfig(path)
	require.NoError(t, err)

	assert.Equal(t, original.Environment, reloaded.Environment)
	assert.Equal(t, original.SwarmKey, reloaded.SwarmKey)
	assert.Equal(t, original.DeviceKey, reloaded.DeviceKey)
	assert.Equal(t, original.SerialNumber, reloaded.SerialNumber)
	assert.Equal(t, original.Secret, reloaded.Secret)
	assert.Equal(t, original.DeviceEndpointURL, reloaded.DeviceEndpointURL)

	// What the backend said about the platform is saved: the Studio's base
	// URL as studio_url, the appliance's instance key as instance_key.
	assert.Equal(t, original.ReswarmBaseURL, asMap["studio_url"])
	assert.Equal(t, original.ReswarmBaseURL, reloaded.ReswarmBaseURL)
	_, hasInstanceKey := asMap["instance_key"]
	assert.False(t, hasInstanceKey, "a cloud device's .flock names no instance key")
	assert.Zero(t, reloaded.InstanceKey)
}

// A .flock from before the platform values were saved, and one of a device
// that has learned none yet, load without them; and such a device's saved
// .flock has no new key, so nothing in it is new to the agent version a failed
// update rolls back to. (That version ignores keys it does not know.)
func TestReswarmConfig_PlatformValuesAreOptional(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.flock")
	require.NoError(t, os.WriteFile(path, sampleReswarmJSON(t), 0o600))

	loaded, err := config.LoadReswarmConfig(path)
	require.NoError(t, err)
	assert.Empty(t, loaded.ReswarmBaseURL)
	assert.Zero(t, loaded.InstanceKey)

	require.NoError(t, config.SaveReswarmConfig(path, loaded))
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var saved map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &saved))
	assert.NotContains(t, saved, "studio_url")
	assert.NotContains(t, saved, "instance_key")
}

func flockConfig(t *testing.T, path string) *config.Config {
	t.Helper()
	return &config.Config{
		CommandLineArguments: &config.CommandLineArguments{ConfigFileLocation: path},
		ReswarmConfig: &config.ReswarmConfig{
			Name: "press-1", DeviceKey: 4, DeviceEndpointURL: "wss://ws.appliance.example/ws",
		},
	}
}

// What the backend tells the agent while it runs is in force at once, and on
// disk after Persist, where the agent's next start finds it.
func TestConfig_LearnPersistPlatform(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device.flock")
	cfg := flockConfig(t, path)

	studioURL, instanceKey := cfg.Platform()
	assert.Empty(t, studioURL)
	assert.Zero(t, instanceKey)

	assert.False(t, cfg.Learn(func(*config.ReswarmConfig) bool { return false }), "nothing changed")
	_, err := os.Stat(path)
	assert.True(t, os.IsNotExist(err), "Learn never writes the .flock")

	assert.True(t, cfg.Learn(func(rc *config.ReswarmConfig) bool {
		rc.ReswarmBaseURL = "https://appliance.example"
		rc.InstanceKey = 11
		return true
	}))
	studioURL, instanceKey = cfg.Platform()
	assert.Equal(t, "https://appliance.example", studioURL)
	assert.Equal(t, uint64(11), instanceKey)

	require.NoError(t, cfg.Persist())
	reloaded, err := config.LoadReswarmConfig(path)
	require.NoError(t, err)
	assert.Equal(t, "https://appliance.example", reloaded.ReswarmBaseURL)
	assert.Equal(t, uint64(11), reloaded.InstanceKey)
	assert.Equal(t, "press-1", reloaded.Name, "the rest of the .flock is saved with it")
}

// Without a path there is nothing to save to: an error, and no file anywhere.
func TestConfig_PersistWithoutPath(t *testing.T) {
	t.Chdir(t.TempDir())
	for name, cliArgs := range map[string]*config.CommandLineArguments{
		"no arguments": nil,
		"no path":      {},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := &config.Config{CommandLineArguments: cliArgs, ReswarmConfig: &config.ReswarmConfig{}}
			require.Error(t, cfg.Persist())
			entries, err := os.ReadDir(".")
			require.NoError(t, err)
			assert.Empty(t, entries)
		})
	}
}

// The heartbeat, an app-state push and the save after a connect change and
// save the .flock from different goroutines while apps start. Whatever the
// order, the file ends with the values in force, and (under -race) nothing
// reads a value while another goroutine writes it.
func TestConfig_ConcurrentLearnPersistPlatform(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device.flock")
	cfg := flockConfig(t, path)

	const writers = 8
	var wg sync.WaitGroup
	for i := 1; i <= writers; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if cfg.Learn(func(rc *config.ReswarmConfig) bool {
				rc.InstanceKey = uint64(i)
				rc.ReswarmBaseURL = fmt.Sprintf("https://appliance-%d.example", i)
				return true
			}) {
				assert.NoError(t, cfg.Persist())
			}
		}()
		go func() {
			defer wg.Done()
			studioURL, instanceKey := cfg.Platform()
			if instanceKey != 0 {
				assert.Equal(t, fmt.Sprintf("https://appliance-%d.example", instanceKey), studioURL,
					"both values come from one change")
			}
		}()
	}
	wg.Wait()

	studioURL, instanceKey := cfg.Platform()
	reloaded, err := config.LoadReswarmConfig(path)
	require.NoError(t, err)
	assert.Equal(t, instanceKey, reloaded.InstanceKey, "the last save wrote the values in force")
	assert.Equal(t, studioURL, reloaded.ReswarmBaseURL)
}

func TestSaveReswarmConfig_OverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "overwrite.flock")

	first := &config.ReswarmConfig{Name: "first", SwarmKey: 1, DeviceEndpointURL: "wss://cbw.ironflock.com/ws-re-dev"}
	require.NoError(t, config.SaveReswarmConfig(path, first))

	second := &config.ReswarmConfig{Name: "second", SwarmKey: 2, DeviceEndpointURL: "wss://cbw.ironflock.com/ws-re-dev"}
	require.NoError(t, config.SaveReswarmConfig(path, second))

	reloaded, err := config.LoadReswarmConfig(path)
	require.NoError(t, err)
	assert.Equal(t, "second", reloaded.Name)
	assert.Equal(t, 2, reloaded.SwarmKey)
}

func TestLoadReswarmConfig_MissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.flock")

	cfg, err := config.LoadReswarmConfig(path)
	require.Error(t, err)
	assert.Nil(t, cfg)
}

// A .flock cut short by a power loss, emptied, or without its endpoint must
// fail loudly and stay on disk exactly as found. Loading used to ignore the
// parse error and write the empty result back, so the device dialled an empty
// URL ("invalid url: ") on every start from then on.
func TestLoadReswarmConfig_DamagedFileIsRefusedAndLeftUntouched(t *testing.T) {
	full := sampleReswarmJSON(t)
	cases := map[string][]byte{
		"truncated":   full[:len(full)/2],
		"empty":       {},
		"no endpoint": []byte(`{"name":"d","device_key":7,"swarm_key":42}`),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "device.flock")
			require.NoError(t, os.WriteFile(path, content, 0o644))

			cfg, err := config.LoadReswarmConfig(path)
			require.Error(t, err)
			assert.Nil(t, cfg)

			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, content, after, "a .flock that failed to load must not be rewritten")
		})
	}
}

// A current .flock needs no migration and must not be rewritten on every
// start: each rewrite is a window in which a power cut damages the file.
func TestLoadReswarmConfig_CurrentFileIsNotRewritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device.flock")
	content := sampleReswarmJSON(t)
	require.NoError(t, os.WriteFile(path, content, 0o644))

	_, err := config.LoadReswarmConfig(path)
	require.NoError(t, err)

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, content, after)
}

// Devices reach their .flock through a symlink into /boot. Saving replaces
// the file the link points at, never the link itself.
func TestSaveReswarmConfig_KeepsSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "boot", "device.flock")
	require.NoError(t, os.MkdirAll(filepath.Dir(real), 0o755))
	require.NoError(t, os.WriteFile(real, sampleReswarmJSON(t), 0o644))
	link := filepath.Join(dir, "device-config.flock")
	require.NoError(t, os.Symlink(real, link))

	updated := &config.ReswarmConfig{Name: "updated", DeviceEndpointURL: "wss://cbw.ironflock.com/ws-re-dev"}
	require.NoError(t, config.SaveReswarmConfig(link, updated))

	info, err := os.Lstat(link)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink, "the link must still be a symlink")

	reloaded, err := config.LoadReswarmConfig(real)
	require.NoError(t, err)
	assert.Equal(t, "updated", reloaded.Name)

	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(real), ".flock-*.tmp"))
	require.NoError(t, err)
	assert.Empty(t, leftovers, "no temporary file may be left behind")
}
