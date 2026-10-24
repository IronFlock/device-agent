package winmigrate

import (
	"fmt"
	"os"
	"path/filepath"
	"reagent/winservice"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The agent's executable is lost after rename-files (quarantined): the
// attempt rolls back, and ensure-agent restores the legacy service's
// executable from the migrator so the device reconnects; the next boot
// keeps it running.
func TestRollbackRestoresALostExecutable(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	e.Logf = func(format string, args ...any) {
		if fmt.Sprintf(format, args...) == "migrate: move-folder" {
			require.NoError(t, os.Remove(filepath.Join(d.legacyDir, "device-agent.exe")))
		}
	}
	require.ErrorContains(t, d.run(e), "start-agent")
	j := d.journal()
	assert.True(t, j.Failed())
	assert.Equal(t, winservice.LegacyServiceName, j.EnsuredService, j.EnsureError)
	assert.Equal(t, winservice.LegacyServiceName, d.connectedService())
	assert.Equal(t, testVersion, d.content(filepath.Join(d.legacyDir, "reagent.exe")))
	d.boot()
	d.runMigratorAfterBoot()
	assert.Equal(t, winservice.LegacyServiceName, d.connectedService(), "still connected after a boot")
}
