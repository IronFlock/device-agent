//go:build integration

// This test requires a live frps server. It is excluded from `just test` and
// runs only under `just test-integration` (-tags integration). See src/TESTING.md.
package tunnel

import (
	"os"
	"path/filepath"
	"reagent/common"
	"reagent/config"
	"reagent/logging"
	"reagent/messenger"
	"runtime"
	"testing"

	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
)

func init() {
	logging.SetupLogger(&config.CommandLineArguments{PrettyLogging: true, Debug: true})
}

// getTestAgentDir returns the appropriate agent directory for testing.
// On Mac/Windows (development), it uses the embedded binary from `just build-frpc`.
// On Linux (CI/production), it uses /opt/reagent.
func getTestAgentDir() string {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		// For local development, use the embedded binary built by `just build-frpc`
		// Find the src/embedded directory relative to this test file
		_, filename, _, ok := runtime.Caller(0)
		if !ok {
			log.Fatal().Msg("Failed to get current file path")
		}
		// filename is .../src/tunnel/tunnel_test.go, we need .../src/embedded
		srcDir := filepath.Dir(filepath.Dir(filename))
		embeddedDir := filepath.Join(srcDir, "embedded")

		// Ensure the frpc binary exists with the correct name
		srcBinary := filepath.Join(embeddedDir, "frpc_binary")
		dstBinary := filepath.Join(embeddedDir, "frpc")
		if _, err := os.Stat(srcBinary); err == nil {
			// Copy the binary to "frpc" if it doesn't exist
			if _, err := os.Stat(dstBinary); os.IsNotExist(err) {
				data, err := os.ReadFile(srcBinary)
				if err != nil {
					log.Fatal().Err(err).Msg("Failed to read frpc_binary")
				}
				if err := os.WriteFile(dstBinary, data, 0755); err != nil {
					log.Fatal().Err(err).Msg("Failed to write frpc")
				}
			}
		}
		return embeddedDir
	}
	return "/opt/reagent"
}

func setupTunnel() *FrpTunnelManager {
	generalConfig := &config.Config{
		CommandLineArguments: &config.CommandLineArguments{AgentDir: getTestAgentDir()},
		ReswarmConfig:        &config.ReswarmConfig{Environment: string(common.PRODUCTION)},
	}

	dummyMessenger := messenger.NewOffline(generalConfig)
	tunnelManager, err := NewFrpTunnelManager(dummyMessenger, generalConfig)
	if err != nil {
		log.Fatal().Msg("Failed to initialize FrpTunnelManager")
	}

	err = tunnelManager.Start()
	if err != nil {
		log.Fatal().Msg("Failed to start FrpTunnelManager")
	}

	return tunnelManager
}

func TestAddTunnel(t *testing.T) {
	// Skip cleanly unless a real frps server is reachable. Without this gate the
	// test reaches setupTunnel() -> frpc Start(), which blocks retrying the
	// connection and hangs until the go-test timeout. requireFrpsReachable lives
	// in tunnel_more_integration_test.go (same package + integration tag).
	requireFrpsReachable(t)

	assert := assert.New(t)

	tunnelManager := setupTunnel()

	err := tunnelManager.Reset()
	if err != nil {
		t.Fatalf("Failed to reset tunnel %s", err.Error())
	}

	subdomain := CreateSubdomain(HTTP, 1, "Test", 8080, common.PROD)
	initialConfig := TunnelConfig{Protocol: HTTP, Subdomain: subdomain, LocalPort: 8080, RemotePort: 0}
	cfg, err := tunnelManager.AddTunnel(initialConfig)
	if err != nil {
		t.Fatalf("Failed to add tunnel %s", err.Error())
	}

	tunnelID := CreateTunnelID(cfg.Subdomain, string(cfg.Protocol))
	status, err := tunnelManager.Status(tunnelID)
	if err != nil {
		t.Fatalf("Failed get tunnel status %s", err.Error())
	}

	for {
		status, err = tunnelManager.Status(tunnelID)
		if err != nil {
			t.Fatalf("Failed get tunnel status %s", err.Error())
		}

		if status.Error != "" {
			t.Fatalf("Tunnel failed to start %s", err.Error())
		}

		if status.Status == "running" {
			break
		}
	}

	configs, err := tunnelManager.GetTunnelConfig()
	if err != nil {
		t.Fatalf("Failed to get tunnel config %s", err.Error())
	}

	config := configs[0]
	assert.Equal(config.LocalPort, initialConfig.LocalPort)
	assert.Equal(config.Protocol, initialConfig.Protocol)
	assert.Equal(config.RemotePort, initialConfig.RemotePort)
	assert.Equal(config.LocalIP, initialConfig.LocalIP)
	assert.Equal(config.Subdomain, initialConfig.Subdomain)

	err = tunnelManager.RemoveTunnel(cfg)
	if err != nil {
		t.Fatalf("Failed to remove tunnel %s", err.Error())
	}

	t.Cleanup(func() {
		tunnelManager.Stop()
	})
}
