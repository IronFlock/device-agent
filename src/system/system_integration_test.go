//go:build integration

package system

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"reagent/embedded"
	"reagent/filesystem"
	"reagent/testutil/builders"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests exercise the real read-only / safe-exec IO paths of the System
// impl that the unit tests deliberately skip (they touch the host filesystem,
// run real binaries, or hit the network). Each test first checks that the
// resource it needs is actually present and t.Skip()s cleanly otherwise, so
// `just test-integration` is green on a host without /etc/os-release, without
// /boot config files, or without network. None of these paths mutate the host
// (no Reboot/Poweroff/InstallOSUpdate/UpdateSystem).

// TestGetOSReleaseCurrent_Integration reads the real /etc/os-release and
// verifies it parses into a non-empty key/value map. Skipped when the file is
// absent (e.g. macOS).
func TestGetOSReleaseCurrent_Integration(t *testing.T) {
	const osReleasePath = "/etc/os-release"

	if _, err := os.Stat(osReleasePath); err != nil {
		t.Skipf("skipping: %s not present (%v)", osReleasePath, err)
	}

	dict, err := GetOSReleaseCurrent()
	require.NoError(t, err)
	require.NotNil(t, dict)
	assert.NotEmpty(t, dict, "/etc/os-release should yield at least one key=value pair")

	// Cross-check: every parsed value must match what the raw file contains for
	// that key, and parsing must not leave surrounding double-quotes behind.
	raw, err := os.ReadFile(osReleasePath)
	require.NoError(t, err)
	for k, v := range dict {
		assert.NotContains(t, v, "\"", "value for %q should have quotes stripped", k)
		// The key must literally appear in the source file.
		assert.Contains(t, string(raw), k+"=", "key %q should appear in %s", k, osReleasePath)
	}
}

// TestGetDeviceConfig_Integration reads the real Raspberry-Pi / Debian device
// config files via the unexported getDeviceConfig. These paths only exist on an
// actual device, so this is skipped unless all three are present.
func TestGetDeviceConfig_Integration(t *testing.T) {
	for _, p := range []string{bootConfigPath, cmdlineConfigPath, networkInterfacesPath} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("skipping: device config path %s not present (%v)", p, err)
		}
	}

	conf, err := getDeviceConfig()
	require.NoError(t, err)

	// Each field must equal the raw contents of its backing file.
	boot, err := os.ReadFile(bootConfigPath)
	require.NoError(t, err)
	cmdline, err := os.ReadFile(cmdlineConfigPath)
	require.NoError(t, err)
	netIfaces, err := os.ReadFile(networkInterfacesPath)
	require.NoError(t, err)

	assert.Equal(t, string(boot), conf.BootConfig)
	assert.Equal(t, string(cmdline), conf.Cmdline)
	assert.Equal(t, string(netIfaces), conf.NetworkInterfaces)
}

// TestFrpcExtractAndVersion_Integration drives the real frpc-binary lifecycle:
// DownloadFrpIfNotExists extracts the embedded binary into a throwaway AgentDir,
// then GetFrpCurrentVersion executes the real `frpc --version`. It is skipped
// when frpc is not embedded in this build (e.g. Windows) or when the embedded
// binary cannot run on this host (cross-arch / cross-OS).
func TestFrpcExtractAndVersion_Integration(t *testing.T) {
	if !embedded.IsEmbedded() {
		t.Skip("skipping: frpc binary is not embedded in this build")
	}

	cfg := builders.DefaultTestConfig()
	// Point AgentDir at a temp dir so we never clobber a real /opt/reagent.
	cfg.CommandLineArguments.AgentDir = t.TempDir()
	sys := New(cfg, nil)

	frpcPath := filesystem.GetTunnelBinaryPath(cfg, "frpc")

	// Before extraction the binary must not exist; GetFrpCurrentVersion should
	// report not-found rather than panic.
	exists, err := filesystem.PathExists(frpcPath)
	require.NoError(t, err)
	require.False(t, exists, "temp AgentDir should start without an frpc binary")

	_, verErr := sys.GetFrpCurrentVersion()
	require.Error(t, verErr, "GetFrpCurrentVersion must error when frpc is absent")

	// Extract the embedded binary for real.
	require.NoError(t, sys.DownloadFrpIfNotExists())
	t.Cleanup(func() { _ = os.Remove(frpcPath) })

	exists, err = filesystem.PathExists(frpcPath)
	require.NoError(t, err)
	require.True(t, exists, "DownloadFrpIfNotExists should have written the frpc binary")

	// The embedded binary is built for the agent's target platform, which may
	// differ from this host. Probe whether it can actually run before asserting
	// on version output; if exec fails (e.g. exec format error on a mismatched
	// arch), skip the run-dependent assertions but keep the extraction ones.
	if probeErr := exec.Command(frpcPath, "--version").Run(); probeErr != nil {
		t.Skipf("skipping version assertions: embedded frpc not runnable on this host (%v)", probeErr)
	}

	version, err := sys.GetFrpCurrentVersion()
	require.NoError(t, err)
	version = strings.TrimSpace(version)
	assert.NotEmpty(t, version, "frpc --version should print a version string")
	// `frpc --version` prints just the semver; it must match the embedded const.
	assert.Equal(t, embedded.FRP_VERSION, version,
		"running frpc --version should report the embedded FRP_VERSION")

	// A second extract call should be a no-op (version matches) and stay green.
	require.NoError(t, sys.DownloadFrpIfNotExists())
}

// TestGetLatestVersion_Integration performs a real network fetch of the
// availableVersions.json manifest from the configured RemoteUpdateURL bucket.
// It is skipped when the network / bucket is unreachable so offline CI stays
// green. This is read-only (an HTTP GET); it never downloads or installs.
func TestGetLatestVersion_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping network fetch in -short mode")
	}

	cfg := builders.DefaultTestConfig()
	// The default test config leaves RemoteUpdateURL empty; point it at the real
	// public bucket so this test genuinely exercises the network fetch when
	// connectivity is available (and skips cleanly when it isn't).
	if cfg.CommandLineArguments.RemoteUpdateURL == "" {
		cfg.CommandLineArguments.RemoteUpdateURL = "https://storage.googleapis.com"
	}
	sys := New(cfg, nil)

	version, err := sys.GetLatestVersion()
	if err != nil {
		t.Skipf("skipping: could not reach update bucket %q (%v)",
			cfg.CommandLineArguments.RemoteUpdateURL, err)
	}

	// The manifest is environment-keyed; an empty result means this environment
	// (or "all") simply isn't published — that's a valid manifest, not a code
	// fault, so don't fail on it. When a version IS returned it should look like
	// a version string (contains a dot), confirming we parsed real JSON.
	if version == "" {
		t.Skipf("bucket reachable but no version published for env %q", GetEnvironment(cfg))
	}
	assert.Contains(t, version, ".", "returned version %q should look like a semver", version)
}

// guardOSFamily documents why the device-config test is conservative about
// platform: the /boot paths are Linux/Raspberry-Pi only.
func TestDeviceConfigPathsPlatform_Integration(t *testing.T) {
	if runtime.GOOS != "linux" {
		// getDeviceConfig is only meaningful on Linux devices; on other OSes the
		// paths never exist, so there is nothing to exercise.
		t.Skipf("skipping: device config paths are Linux-only (GOOS=%s)", runtime.GOOS)
	}
	// On Linux but without the Raspi files, getDeviceConfig must surface the
	// underlying os.ReadFile error rather than panic.
	if _, err := os.Stat(bootConfigPath); err != nil {
		_, gErr := getDeviceConfig()
		require.Error(t, gErr, "getDeviceConfig should error when %s is missing", bootConfigPath)
		return
	}
	// Files present: covered by TestGetDeviceConfig_Integration.
	t.Skip("device config files present; see TestGetDeviceConfig_Integration")
}
