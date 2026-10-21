package apps

import (
	"path/filepath"
	"testing"

	"reagent/common"

	"github.com/docker/docker/api/types/mount"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func dbusMount(mounts []mount.Mount) *mount.Mount {
	for i := range mounts {
		if mounts[i].Target == "/run/dbus" {
			return &mounts[i]
		}
	}
	return nil
}

func TestComputeMountsBindsTheHostDBus(t *testing.T) {
	original := hostDBusDir
	t.Cleanup(func() { hostDBusDir = original })

	cfg := caTestConfig(t, "", false)
	cfg.CommandLineArguments.AppsSharedDir = filepath.Join(cfg.CommandLineArguments.AppsDirectory, "shared")

	t.Run("bound when the host has a system bus", func(t *testing.T) {
		hostDBusDir = t.TempDir()

		mounts, err := computeMounts(common.PROD, "myapp", cfg)
		require.NoError(t, err)

		m := dbusMount(mounts)
		require.NotNil(t, m, "apps reach BlueZ only through the host's bluetoothd")
		assert.Equal(t, mount.TypeBind, m.Type)
		assert.Equal(t, hostDBusDir, m.Source, "the directory, so a dbus restart does not strand a stale socket")
	})

	t.Run("absent when the host has none", func(t *testing.T) {
		hostDBusDir = filepath.Join(t.TempDir(), "missing")

		mounts, err := computeMounts(common.PROD, "myapp", cfg)
		require.NoError(t, err)
		assert.Nil(t, dbusMount(mounts), "a missing source would fail container creation")
	})
}
