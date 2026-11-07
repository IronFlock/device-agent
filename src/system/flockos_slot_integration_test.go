//go:build integration

package system

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"reagent/osupdate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// debugfs reads the os-release of an ext4 slot image as get_os_release reads
// an idle slot, without mounting it; an ext4 without one yields nothing.
func TestDebugfsOSRelease_Integration(t *testing.T) {
	if _, err := exec.LookPath("mke2fs"); err != nil {
		t.Skip("e2fsprogs is not installed")
	}
	if _, err := exec.LookPath("debugfs"); err != nil {
		t.Skip("e2fsprogs is not installed")
	}
	mkfs := func(tree string) string {
		img := filepath.Join(t.TempDir(), "rootfs.ext4")
		out, err := exec.Command("mke2fs", "-q", "-F", "-t", "ext4", "-d", tree, img, "16M").CombinedOutput()
		require.NoError(t, err, string(out))
		return img
	}

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "usr/lib"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "etc"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "usr/lib/os-release"), []byte(flashedOSRelease), 0o644))
	require.NoError(t, os.Symlink("../usr/lib/os-release", filepath.Join(root, "etc/os-release")))

	data, err := debugfsOSRelease(context.Background(), mkfs(root))
	require.NoError(t, err)
	fields := osupdate.ParseOSRelease(data)
	assert.Equal(t, "1.0.0", fields["IMAGE_VERSION"])
	assert.Equal(t, "20261107T122204Z-gef21459", fields["BUILD_ID"])

	data, err = debugfsOSRelease(context.Background(), mkfs(t.TempDir()))
	if err == nil {
		assert.Empty(t, osupdate.ParseOSRelease(data)["IMAGE_VERSION"], "an empty file system holds no image")
	}
}
