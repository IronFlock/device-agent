package embedded

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"

	"github.com/rs/zerolog/log"
)

// FRP_VERSION is the frpc version built from the frp/ module (frp/go.mod).
// It must match the tunnel server's frp version (scripts/check-frp-version.sh
// checks this repository; the tunnel repository is kept equal by hand).
const FRP_VERSION = "0.71.1"

// frpcBinary holds the embedded frpc binary for the target platform
// The actual binary path is determined at build time based on GOOS/GOARCH
// This will be populated by the build script before compilation
//
//go:embed frpc_binary
var frpcBinary []byte

// GetEmbeddedFrpc returns the embedded frpc binary
func GetEmbeddedFrpc() ([]byte, error) {
	if len(frpcBinary) == 0 {
		return nil, fmt.Errorf("frpc binary not embedded - build may have failed")
	}
	return frpcBinary, nil
}

// ExtractFrpc writes the embedded frpc binary to the specified path
func ExtractFrpc(targetPath string) error {
	data, err := GetEmbeddedFrpc()
	if err != nil {
		return err
	}

	// Create directory if it doesn't exist
	dir := filepath.Dir(targetPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Write binary to file
	if err := os.WriteFile(targetPath, data, 0755); err != nil { //nolint:gosec // G306: frpc executable, must be executable
		return fmt.Errorf("failed to write frpc binary: %w", err)
	}

	log.Info().Msgf("Extracted embedded frpc v%s to %s (%d bytes)", FRP_VERSION, targetPath, len(data))
	return nil
}

// IsEmbedded checks if frpc binary is embedded (not empty)
func IsEmbedded() bool {
	return len(frpcBinary) > 0
}
