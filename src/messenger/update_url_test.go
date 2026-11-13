package messenger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"reagent/config"

	"github.com/ironflock/nexus/v3/wamp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyHandedOffUpdateURL(t *testing.T) {
	const lan = "http://10.0.0.5:15001/dl"

	tests := []struct {
		name        string
		current     string
		boardModel  string
		value       any
		wantChanged bool
		wantURL     string
		wantCfgURL  string
	}{
		{name: "absent key (nil) leaves config untouched", value: nil, wantURL: ""},
		{name: "non-string value is ignored", value: 42, wantURL: ""},
		{name: "empty string is ignored", value: "", wantURL: ""},
		{name: "same value is a no-op", current: lan, value: lan, wantURL: lan, wantCfgURL: lan},
		{name: "new value is applied", value: lan, wantChanged: true, wantURL: lan, wantCfgURL: lan},
		{name: "replaces a different existing value", current: "https://storage.googleapis.com", value: lan, wantChanged: true, wantURL: lan, wantCfgURL: lan},
		{name: "appliance host keeps its installer-written base", current: "http://localhost:15001/dl", boardModel: "appliance", value: lan, wantURL: lan, wantCfgURL: "http://localhost:15001/dl"},
		{name: "appliance host with empty base still not handed off", boardModel: "appliance", value: lan, wantURL: lan, wantCfgURL: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.ReswarmConfig{UpdateURL: tt.current}
			cfg.Board.Model = tt.boardModel

			changed, url := applyHandedOffUpdateURL(cfg, tt.value)

			assert.Equal(t, tt.wantChanged, changed, "changed")
			assert.Equal(t, tt.wantURL, url, "returned url")
			assert.Equal(t, tt.wantCfgURL, cfg.UpdateURL, "cfg.UpdateURL after apply")
		})
	}

	t.Run("nil config is tolerated", func(t *testing.T) {
		changed, url := applyHandedOffUpdateURL(nil, "http://x/dl")
		assert.False(t, changed)
		assert.Equal(t, "", url)
	})
}

// heartbeatResult builds the map the backend returns from update_device_status.
func heartbeatResult(fields map[string]any) *wamp.Result {
	return &wamp.Result{Arguments: wamp.List{fields}}
}

func readFlock(t *testing.T, path string) config.ReswarmConfig {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "the .flock should exist at %s", path)
	var cfg config.ReswarmConfig
	require.NoError(t, json.Unmarshal(raw, &cfg))
	return cfg
}

func newHeartbeatSession(t *testing.T, cfg *config.Config) (*WampSession, *MockNexusClient) {
	t.Helper()
	socketConfig := &SocketConfig{ConnectionTimeout: time.Millisecond * 100}
	mockClient := NewMockClient()
	session, err := NewWampSession(cfg, socketConfig, nil, mockClient.ConnectNet)
	require.NoError(t, err)
	t.Cleanup(func() { session.Close() })
	return session, mockClient.LastClient()
}

func TestUpdateRemoteDeviceStatus_HandsOffUpdateURL(t *testing.T) {
	const lan = "http://10.0.0.5:15001/dl"

	t.Run("persists a handed-off update base to the .flock", func(t *testing.T) {
		cfg := testConfig()
		flock := filepath.Join(t.TempDir(), "device.flock")
		cfg.CommandLineArguments.ConfigFileLocation = flock
		session, client := newHeartbeatSession(t, cfg)
		client.SetCallResult(heartbeatResult(map[string]any{
			"reswarmBaseURL": "https://appliance.example",
			"agentUpdateURL": lan,
		}))

		require.NoError(t, session.UpdateRemoteDeviceStatus(CONNECTED))

		assert.Equal(t, lan, cfg.ReswarmConfig.UpdateURL, "in-memory config carries the handed-off base")
		assert.Equal(t, "https://appliance.example", cfg.ReswarmConfig.ReswarmBaseURL)
		assert.Equal(t, lan, readFlock(t, flock).UpdateURL, "the .flock on disk carries update_url")
	})

	t.Run("does not rewrite the .flock when the base is unchanged", func(t *testing.T) {
		cfg := testConfig()
		cfg.ReswarmConfig.UpdateURL = lan
		flock := filepath.Join(t.TempDir(), "device.flock")
		cfg.CommandLineArguments.ConfigFileLocation = flock
		session, client := newHeartbeatSession(t, cfg)
		client.SetCallResult(heartbeatResult(map[string]any{"agentUpdateURL": lan}))

		require.NoError(t, session.UpdateRemoteDeviceStatus(CONNECTED))

		_, err := os.Stat(flock)
		assert.True(t, os.IsNotExist(err), "no write when nothing changed")
	})

	t.Run("cloud backend without the key leaves update_url and the .flock alone", func(t *testing.T) {
		cfg := testConfig()
		flock := filepath.Join(t.TempDir(), "device.flock")
		cfg.CommandLineArguments.ConfigFileLocation = flock
		session, client := newHeartbeatSession(t, cfg)
		client.SetCallResult(heartbeatResult(map[string]any{"reswarmBaseURL": "https://app.ironflock.com"}))

		require.NoError(t, session.UpdateRemoteDeviceStatus(CONNECTED))

		assert.Equal(t, "", cfg.ReswarmConfig.UpdateURL)
		_, err := os.Stat(flock)
		assert.True(t, os.IsNotExist(err))
	})

	t.Run("appliance host keeps its installer-written base", func(t *testing.T) {
		cfg := testConfig()
		cfg.ReswarmConfig.Board.Model = "appliance"
		cfg.ReswarmConfig.UpdateURL = "http://localhost:15001/dl"
		flock := filepath.Join(t.TempDir(), "device.flock")
		cfg.CommandLineArguments.ConfigFileLocation = flock
		session, client := newHeartbeatSession(t, cfg)
		client.SetCallResult(heartbeatResult(map[string]any{"agentUpdateURL": lan}))

		require.NoError(t, session.UpdateRemoteDeviceStatus(CONNECTED))

		assert.Equal(t, "http://localhost:15001/dl", cfg.ReswarmConfig.UpdateURL)
		_, err := os.Stat(flock)
		assert.True(t, os.IsNotExist(err))
	})

	t.Run("saves the Studio's base URL to the .flock when it is new", func(t *testing.T) {
		cfg := testConfig()
		cfg.ReswarmConfig.ReswarmBaseURL = "" // as after a start: the .flock named none yet
		flock := filepath.Join(t.TempDir(), "device.flock")
		cfg.CommandLineArguments.ConfigFileLocation = flock
		session, client := newHeartbeatSession(t, cfg)
		client.SetCallResult(heartbeatResult(map[string]any{"reswarmBaseURL": "https://appliance.example"}))

		require.NoError(t, session.UpdateRemoteDeviceStatus(CONNECTED))

		assert.Equal(t, "https://appliance.example", cfg.ReswarmConfig.ReswarmBaseURL)
		assert.Equal(t, "https://appliance.example", readFlock(t, flock).ReswarmBaseURL,
			"the agent's next start has it before it is connected")
	})

	t.Run("does not rewrite the .flock for the Studio it already names", func(t *testing.T) {
		cfg := testConfig()
		cfg.ReswarmConfig.ReswarmBaseURL = "https://appliance.example"
		flock := filepath.Join(t.TempDir(), "device.flock")
		cfg.CommandLineArguments.ConfigFileLocation = flock
		session, client := newHeartbeatSession(t, cfg)
		client.SetCallResult(heartbeatResult(map[string]any{"reswarmBaseURL": "https://appliance.example"}))

		require.NoError(t, session.UpdateRemoteDeviceStatus(CONNECTED))

		_, err := os.Stat(flock)
		assert.True(t, os.IsNotExist(err), "no write when nothing changed")
	})

	t.Run("absent reswarmBaseURL does not poison the cached base with <nil>", func(t *testing.T) {
		cfg := testConfig()
		cfg.CommandLineArguments.ConfigFileLocation = filepath.Join(t.TempDir(), "device.flock")
		session, client := newHeartbeatSession(t, cfg)
		client.SetCallResult(heartbeatResult(map[string]any{}))

		require.NoError(t, session.UpdateRemoteDeviceStatus(CONNECTED))

		assert.Equal(t, "https://app.ironflock.com", cfg.ReswarmConfig.ReswarmBaseURL, "previous value survives an absent key")
	})

	t.Run("a failed .flock write does not fail the heartbeat", func(t *testing.T) {
		cfg := testConfig()
		// A directory where the file should be: SaveReswarmConfig cannot write it.
		cfg.CommandLineArguments.ConfigFileLocation = t.TempDir()
		session, client := newHeartbeatSession(t, cfg)
		client.SetCallResult(heartbeatResult(map[string]any{"agentUpdateURL": lan}))

		require.NoError(t, session.UpdateRemoteDeviceStatus(CONNECTED))
		assert.Equal(t, lan, cfg.ReswarmConfig.UpdateURL, "in-memory handoff still applies")
	})
}
