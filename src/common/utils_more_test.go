package common

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListDirectories(t *testing.T) {
	t.Run("lists only subdirectories, not files", func(t *testing.T) {
		root := t.TempDir()
		// create three subdirectories
		for _, d := range []string{"alpha", "beta", "gamma"} {
			require.NoError(t, os.Mkdir(filepath.Join(root, d), 0o755))
		}
		// create a couple of regular files that must be ignored
		require.NoError(t, os.WriteFile(filepath.Join(root, "afile.txt"), []byte("x"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(root, "another"), []byte("y"), 0o644))

		got, err := ListDirectories(root)
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"alpha", "beta", "gamma"}, got)
	})

	t.Run("empty directory yields no directories", func(t *testing.T) {
		root := t.TempDir()
		got, err := ListDirectories(root)
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("directory with only files yields no directories", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(root, "f1"), []byte("a"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(root, "f2"), []byte("b"), 0o644))

		got, err := ListDirectories(root)
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("non-existent path returns an error", func(t *testing.T) {
		got, err := ListDirectories(filepath.Join(t.TempDir(), "does-not-exist"))
		require.Error(t, err)
		assert.Nil(t, got)
	})

	t.Run("nested directories are not flattened, only top level returned", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(root, "top", "nested"), 0o755))

		got, err := ListDirectories(root)
		require.NoError(t, err)
		// only the immediate child "top" should be listed, never "nested"
		assert.Equal(t, []string{"top"}, got)
	})
}

func TestGetRandomFreePort(t *testing.T) {
	t.Run("returns a usable port in valid range", func(t *testing.T) {
		port, err := GetRandomFreePort()
		require.NoError(t, err)
		assert.Greater(t, port, 0)
		assert.LessOrEqual(t, port, 65535)

		// the returned port must be bindable now that the helper released it
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		require.NoError(t, err, "port reported as free should be bindable")
		require.NoError(t, l.Close())
	})

	t.Run("successive calls generally differ", func(t *testing.T) {
		// Random ephemeral allocation: across several calls we expect to see
		// more than one distinct value. (Not a strict guarantee per call.)
		seen := make(map[int]struct{})
		for i := 0; i < 10; i++ {
			port, err := GetRandomFreePort()
			require.NoError(t, err)
			seen[port] = struct{}{}
		}
		assert.Greater(t, len(seen), 1, "expected ephemeral ports to vary across calls")
	})
}

func TestGetFreePortFromStart(t *testing.T) {
	t.Run("returns startPort when it is free", func(t *testing.T) {
		// Find an actually-free port to use as the start, then ask the helper
		// to search from there. It should return that same port.
		start, err := GetRandomFreePort()
		require.NoError(t, err)

		got, err := GetFreePortFromStart(start)
		require.NoError(t, err)
		assert.Equal(t, start, got)
	})

	t.Run("skips occupied start port and returns a higher free one", func(t *testing.T) {
		// Occupy a known port, then search starting at it. The helper must
		// skip the occupied port and return a strictly higher free port.
		start, err := GetRandomFreePort()
		require.NoError(t, err)

		occupied, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", start))
		require.NoError(t, err)
		defer occupied.Close()

		got, err := GetFreePortFromStart(start)
		require.NoError(t, err)
		assert.Greater(t, got, start, "should skip the occupied start port")
		assert.LessOrEqual(t, got, 65535)
	})

	t.Run("returned port is bindable", func(t *testing.T) {
		start, err := GetRandomFreePort()
		require.NoError(t, err)

		got, err := GetFreePortFromStart(start)
		require.NoError(t, err)

		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", got))
		require.NoError(t, err)
		require.NoError(t, l.Close())
	})

	t.Run("no free port above range yields error", func(t *testing.T) {
		// startPort just above the max valid port: the loop body never runs
		// and the helper reports no free port available.
		got, err := GetFreePortFromStart(65536)
		require.Error(t, err)
		assert.Equal(t, 0, got)
	})
}

func TestBuildProgressTopics(t *testing.T) {
	// These builders embed the topicPrefix and the per-purpose topic suffix.
	tests := []struct {
		name     string
		build    func(string) string
		serial   string
		expected string
	}{
		{
			name:     "agent update progress",
			build:    BuildAgentUpdateProgress,
			serial:   "serial-1",
			expected: "re.mgmt.serial-1.agent_update_progress",
		},
		{
			name:     "download os update progress",
			build:    BuildDownloadOSUpdateProgress,
			serial:   "serial-1",
			expected: "re.mgmt.serial-1.download_os_update_progress",
		},
		{
			name:     "install os update progress",
			build:    BuildInstallOSUpdateProgress,
			serial:   "serial-1",
			expected: "re.mgmt.serial-1.install_os_update_progress",
		},
		{
			name:     "install firmware update progress",
			build:    BuildInstallFirmwareUpdateProgress,
			serial:   "serial-1",
			expected: "re.mgmt.serial-1.install_firmware_update_progress",
		},
		{
			name:     "perform os update progress",
			build:    BuildPerformOSUpdateProgress,
			serial:   "serial-1",
			expected: "re.mgmt.serial-1.perform_os_update_progress",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.build(tt.serial))
		})
	}
}
