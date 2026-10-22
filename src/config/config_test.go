package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
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

	// ReswarmBaseURL is tagged json:"-" and must never be serialized.
	_, present := asMap["ReswarmBaseURL"]
	assert.False(t, present)
	_, presentSnake := asMap["reswarm_base_url"]
	assert.False(t, presentSnake)
	assert.Empty(t, reloaded.ReswarmBaseURL)
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
