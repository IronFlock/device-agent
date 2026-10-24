package layout

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// linuxPathsOnly skips a test of Detect's Linux paths (/opt/..., /var/log/...)
// on Windows, where filepath spells them with backslashes; Detect runs on
// Linux only (Windows: DetectWindows).
func linuxPathsOnly(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Detect's Linux paths; the Windows layout is DetectWindows")
	}
}

// legacyInstall lays down what ironflock-init < 1.4.0 leaves on a device.
func legacyInstall(t *testing.T, root string) {
	t.Helper()
	unit := filepath.Join(root, LegacyUnitFile)
	require.NoError(t, os.MkdirAll(filepath.Dir(unit), 0o755))
	require.NoError(t, os.WriteFile(unit, []byte("[Unit]\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, LegacyEnrolled), nil, 0o644))
	link := filepath.Join(root, LegacyUnitLink)
	require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(root, LegacyUnitFile), link))
}

func TestFreshMachineGetsTheCurrentLayout(t *testing.T) {
	linuxPathsOnly(t)
	paths := Detect(t.TempDir())
	assert.Equal(t, Current, paths.Kind)
	assert.Equal(t, "/opt/ironflock/device-agent", paths.AgentDir)
	assert.Equal(t, "/opt/ironflock/device-agent/device-agent.db", paths.Database)
	assert.Equal(t, "/var/log/ironflock/device-agent.log", paths.LogFile)
	assert.Equal(t, "/var/log/ironflock/frpc.log", paths.FrpcLog)
	assert.Equal(t, "device-agent", paths.BinaryName)
}

func TestIronflockInitLegacyInstallKeepsItsPathsUntilMigrated(t *testing.T) {
	linuxPathsOnly(t)
	root := t.TempDir()
	legacyInstall(t, root)

	paths := Detect(root)
	assert.Equal(t, Legacy, paths.Kind)
	assert.Equal(t, "/opt/reagent", paths.AgentDir)
	assert.Equal(t, "/opt/reagent/reagent.db", paths.Database)
	assert.Equal(t, "/var/log/reagent.log", paths.LogFile)
	assert.Equal(t, "/var/log/frpc.log", paths.FrpcLog)
	// the legacy manager activates only reagent-v<version>
	assert.Equal(t, "reagent", paths.BinaryName)
}

func TestMigratedDeviceIsCurrentEvenWithTheCompatLink(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, AgentDir), 0o755))
	require.NoError(t, os.Symlink(filepath.Join(root, AgentDir), filepath.Join(root, LegacyAgentDir)))

	assert.Equal(t, Current, Detect(root).Kind)
}

func TestImageLayoutsStay(t *testing.T) {
	cases := map[string]func(t *testing.T, root string){
		"FlockOS marker": func(t *testing.T, root string) {
			legacyInstall(t, root)
			require.NoError(t, os.WriteFile(filepath.Join(root, legacyFlockOSMarker), nil, 0o644))
		},
		"FlockOS units": func(t *testing.T, root string) {
			legacyInstall(t, root)
			require.NoError(t, os.MkdirAll(filepath.Join(root, legacyFlockOSUnits), 0o755))
		},
		"real unit file (ReswarmOS, cubic, Jetson)": func(t *testing.T, root string) {
			legacyInstall(t, root)
			link := filepath.Join(root, LegacyUnitLink)
			require.NoError(t, os.Remove(link))
			require.NoError(t, os.WriteFile(link, []byte("[Unit]\n"), 0o644))
		},
		"unit links elsewhere": func(t *testing.T, root string) {
			legacyInstall(t, root)
			link := filepath.Join(root, LegacyUnitLink)
			require.NoError(t, os.Remove(link))
			require.NoError(t, os.Symlink("/usr/lib/systemd/system/reagent.service", link))
		},
		"not enrolled": func(t *testing.T, root string) {
			legacyInstall(t, root)
			require.NoError(t, os.Remove(filepath.Join(root, LegacyEnrolled)))
		},
		"pre-2024 (no reswarmify folder)": func(t *testing.T, root string) {
			require.NoError(t, os.MkdirAll(filepath.Join(root, LegacyAgentDir), 0o755))
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			setup(t, root)
			paths := Detect(root)
			assert.Equal(t, LegacyImage, paths.Kind)
			assert.Equal(t, "/opt/reagent", paths.AgentDir, "an image layout keeps its paths")
			assert.Equal(t, "reagent", paths.BinaryName)
		})
	}
}

func TestHomeLayouts(t *testing.T) {
	home := t.TempDir()
	assert.Equal(t, filepath.Join(home, "ironflock", "device-agent"), DetectHome(home).AgentDir)

	require.NoError(t, os.MkdirAll(filepath.Join(home, "reagent"), 0o755))
	legacy := DetectHome(home)
	assert.Equal(t, Legacy, legacy.Kind)
	assert.Equal(t, filepath.Join(home, "reagent", "reagent.db"), legacy.Database)
}

// A 1.3.x Windows service updates itself in place, so the first 1.4.0 on such
// a device runs as reagent.exe: it must keep the legacy names (the service's
// ImagePath, reagent-prev.exe as its rollback target) until the migration
// renames it. Anything else is the current layout, whatever the folder.
func TestWindowsLayoutFollowsTheExeName(t *testing.T) {
	home := t.TempDir()

	legacy := DetectWindows(home, "reagent.exe", true)
	assert.Equal(t, Legacy, legacy.Kind)
	assert.Equal(t, "reagent", legacy.BinaryName)
	assert.Equal(t, filepath.Join(home, "reagent"), legacy.AgentDir)
	assert.Equal(t, filepath.Join(home, "reagent", "reagent.db"), legacy.Database)
	assert.Equal(t, filepath.Join(home, "reagent", "reagent.log"), legacy.LogFile)
	assert.Equal(t, filepath.Join(home, "reagent", "frpc.log"), legacy.FrpcLog)

	// Windows file names are case-insensitive.
	assert.Equal(t, Legacy, DetectWindows(home, "Reagent.EXE", true).Kind)

	// No service supervises it (a console run, FlockFlasher's test device):
	// it keeps the legacy names but can never move by itself.
	console := DetectWindows(home, "reagent.exe", false)
	assert.Equal(t, LegacyConsole, console.Kind)
	assert.Equal(t, "reagent", console.BinaryName)

	for _, exe := range []string{"device-agent.exe", "device-agent-v1.4.0.exe", "agent.exe"} {
		current := DetectWindows(home, exe, true)
		assert.Equal(t, Current, current.Kind, exe)
		assert.Equal(t, "device-agent", current.BinaryName, exe)
		assert.Equal(t, filepath.Join(home, "ironflock", "device-agent"), current.AgentDir, exe)
		assert.Equal(t, filepath.Join(current.AgentDir, "device-agent.db"), current.Database, exe)
		assert.Equal(t, filepath.Join(current.AgentDir, "device-agent.log"), current.LogFile, exe)
		assert.Equal(t, Current, DetectWindows(home, exe, false).Kind, "a console run of the current layout is Current")
	}
}
