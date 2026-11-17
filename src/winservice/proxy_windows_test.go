//go:build windows

package winservice

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

// The store's ACL against the real Windows security API: written protected,
// a loosened one is found and repaired. A file whose DACL leaves out the
// current user can only be read back by an administrator, so the test needs
// an elevated process (the CI runner is one).
func TestWriteProtectedFileAndRepair(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("needs an elevated process")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, ProxyEnvName)

	require.NoError(t, WriteProtectedFile(path, []byte("HTTP_PROXY=http://u:secret@p:1\n")))
	ok, err := FileProtected(path)
	require.NoError(t, err)
	assert.True(t, ok)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "HTTP_PROXY=http://u:secret@p:1\n", string(data))
	assert.NoFileExists(t, path+".tmp")

	// Replacing keeps the protection.
	require.NoError(t, WriteProtectedFile(path, []byte("HTTP_PROXY=http://p:2\n")))
	ok, err = FileProtected(path)
	require.NoError(t, err)
	assert.True(t, ok)

	// Users may read it, inherited from the folder: not protected.
	loose, err := windows.SecurityDescriptorFromString("D:(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;BU)")
	require.NoError(t, err)
	dacl, _, err := loose.DACL()
	require.NoError(t, err)
	require.NoError(t, windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil))
	ok, err = FileProtected(path)
	require.NoError(t, err)
	assert.False(t, ok)

	repaired, err := ProtectFile(path)
	require.NoError(t, err)
	assert.True(t, repaired)
	ok, err = FileProtected(path)
	require.NoError(t, err)
	assert.True(t, ok)

	repaired, err = ProtectFile(path)
	require.NoError(t, err)
	assert.False(t, repaired)
}

// A file the agent writes in a folder that grants Users access does not
// inherit it.
func TestWriteProtectedFileDoesNotInherit(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("needs an elevated process")
	}
	dir := t.TempDir()
	open, err := windows.SecurityDescriptorFromString("D:(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;BU)")
	require.NoError(t, err)
	dacl, _, err := open.DACL()
	require.NoError(t, err)
	require.NoError(t, windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil))

	path := filepath.Join(dir, ProxyEnvName)
	require.NoError(t, WriteProtectedFile(path, []byte("x")))
	ok, err := FileProtected(path)
	require.NoError(t, err)
	assert.True(t, ok)
}
