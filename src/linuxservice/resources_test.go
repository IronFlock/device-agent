package linuxservice

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSliceCarriesWeightAndProtection(t *testing.T) {
	slice := SliceUnit()
	assert.Contains(t, slice, "[Slice]\nCPUWeight=2048\nMemoryMin=96M\n")
	assert.NotContains(t, slice, "[Service]")
}

func TestDropInMovesTheUnitIntoTheSlice(t *testing.T) {
	dropIn := ResourcesDropIn(false)
	for _, want := range []string{"[Service]", "Slice=ironflock.slice", "CPUWeight=2048", "Nice=-16", "ManagedOOMPreference=avoid"} {
		assert.Contains(t, dropIn, want)
	}
	assert.NotContains(t, dropIn, "MemoryMin", "with memory_recursiveprot the slice's protection reaches the unit by itself")

	assert.Contains(t, ResourcesDropIn(true), "MemoryMin=96M", "without memory_recursiveprot the unit protects itself")
}

func TestCurrentUnitFromProcSelfCgroup(t *testing.T) {
	cases := map[string]struct {
		content string
		want    string
	}{
		"cgroup v2 in system.slice":  {"0::/system.slice/ironflock-device-agent.service\n", "ironflock-device-agent.service"},
		"cgroup v2 in its own slice": {"0::/ironflock.slice/ironflock-device-agent.service\n", "ironflock-device-agent.service"},
		"cgroup v1 legacy unit":      {"12:memory:/system.slice/reagent.service\n1:name=systemd:/system.slice/reagent.service\n", "reagent.service"},
		"hybrid prefers v2":          {"0::/system.slice/x.service\n1:name=systemd:/system.slice/y.service\n", "x.service"},
		"console run in a session":   {"0::/user.slice/user-1000.slice/session-3.scope\n", ""},
		"container":                  {"0::/\n", ""},
		"docker on v1":               {"1:name=systemd:/docker/0123abcd\n", ""},
		"empty":                      {"", ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, c.want, CurrentUnit(c.content))
		})
	}
}

func TestIsAgentUnit(t *testing.T) {
	assert.True(t, IsAgentUnit("ironflock-device-agent.service"))
	assert.True(t, IsAgentUnit("reagent.service"))
	assert.False(t, IsAgentUnit("docker.service"))
	assert.False(t, IsAgentUnit(""))
}

func TestMemoryRecursiveProtFromProcMounts(t *testing.T) {
	cases := map[string]struct {
		content string
		want    bool
	}{
		"v2 with the option":    {"cgroup2 /sys/fs/cgroup cgroup2 rw,nosuid,nodev,noexec,relatime,nsdelegate,memory_recursiveprot 0 0\n", true},
		"v2 without the option": {"cgroup2 /sys/fs/cgroup cgroup2 rw,nosuid,nodev,noexec,relatime,nsdelegate 0 0\n", false},
		"hybrid, memory on v1":  {"cgroup2 /sys/fs/cgroup/unified cgroup2 rw,nsdelegate,memory_recursiveprot 0 0\ncgroup /sys/fs/cgroup/memory cgroup rw,memory 0 0\n", false},
		"empty":                 {"", false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, c.want, MemoryRecursiveProt(c.content))
		})
	}
}

func TestWriteResourceFilesOnlyWhenTheyChange(t *testing.T) {
	root := t.TempDir()

	changed, err := WriteResourceFiles(root, UnitName, false)
	require.NoError(t, err)
	assert.True(t, changed, "the first write creates both files")

	slice, err := os.ReadFile(filepath.Join(root, SlicePath))
	require.NoError(t, err)
	assert.Equal(t, SliceUnit(), string(slice))
	dropIn, err := os.ReadFile(filepath.Join(root, "/etc/systemd/system/ironflock-device-agent.service.d", ResourcesDropInName))
	require.NoError(t, err)
	assert.Equal(t, ResourcesDropIn(false), string(dropIn))
	assert.NoFileExists(t, filepath.Join(root, SlicePath)+".tmp")

	changed, err = WriteResourceFiles(root, UnitName, false)
	require.NoError(t, err)
	assert.False(t, changed, "the same content again is no change, so no daemon-reload")

	changed, err = WriteResourceFiles(root, UnitName, true)
	require.NoError(t, err)
	assert.True(t, changed, "a different drop-in is a change")
	dropIn, err = os.ReadFile(filepath.Join(root, ResourcesDropInPath(UnitName)))
	require.NoError(t, err)
	assert.Contains(t, string(dropIn), "MemoryMin=96M")
}

func TestLegacyUnitGetsItsOwnDropIn(t *testing.T) {
	root := t.TempDir()
	_, err := WriteResourceFiles(root, LegacyUnitName, false)
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(root, "/etc/systemd/system/reagent.service.d", ResourcesDropInName))
	assert.NoDirExists(t, filepath.Join(root, "/etc/systemd/system/ironflock-device-agent.service.d"))
}

func TestRemoveResourceFiles(t *testing.T) {
	root := t.TempDir()
	_, err := WriteResourceFiles(root, UnitName, false)
	require.NoError(t, err)

	require.NoError(t, RemoveResourceFiles(root, UnitName))
	assert.NoFileExists(t, filepath.Join(root, SlicePath))
	assert.NoDirExists(t, filepath.Join(root, "/etc/systemd/system/ironflock-device-agent.service.d"), "an empty drop-in directory goes too")

	require.NoError(t, RemoveResourceFiles(root, UnitName), "removing what is not there is fine")

	// Another drop-in keeps the directory.
	_, err = WriteResourceFiles(root, UnitName, false)
	require.NoError(t, err)
	other := filepath.Join(root, "/etc/systemd/system/ironflock-device-agent.service.d/http-proxy.conf")
	require.NoError(t, os.WriteFile(other, []byte("[Service]\nEnvironment=HTTPS_PROXY=http://proxy:3128\n"), 0o644))
	require.NoError(t, RemoveResourceFiles(root, UnitName))
	assert.FileExists(t, other)
}
