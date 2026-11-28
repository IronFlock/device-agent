package persistence

import (
	"reagent/common"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The platform's image names are stored with the requested state, read back
// by a start (offline too), and replaced by the next sync.
func TestRequestedStateImageNames(t *testing.T) {
	db := newTestDB(t)
	p := newRequestedPayload(t, "MyApp", 1953, common.PROD)
	p.PresentImageName = "prod_amd64_1953_myapp:1.0.0"
	p.NewImageName = "prod_1953_myapp:2.0.0"
	require.NoError(t, db.UpsertRequestedStateChange(p))

	one, err := db.GetRequestedState(1953, common.PROD)
	require.NoError(t, err)
	assert.Equal(t, "prod_amd64_1953_myapp:1.0.0", one.PresentImageName)
	assert.Equal(t, "prod_1953_myapp:2.0.0", one.NewImageName)

	// a sync of an older backend: none
	p.PresentImageName, p.NewImageName = "", ""
	require.NoError(t, db.BulkUpsertRequestedStateChanges([]common.TransitionPayload{p}))
	all, err := db.GetRequestedStates()
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Empty(t, all[0].PresentImageName)
	assert.Empty(t, all[0].NewImageName)
}

// A database whose columns for the names are missing (their single- script
// failed, e.g. on a full disk) reads and writes the requested states as
// before, without names: the apps still start.
func TestRequestedStatesWithoutImageNameColumns(t *testing.T) {
	db := newTestDB(t)
	for _, column := range []string{"present_image_name", "installed_image"} {
		_, err := db.db.Exec(`ALTER TABLE RequestedAppStates DROP COLUMN ` + column)
		require.NoError(t, err)
	}
	assert.Error(t, db.SetInstalledImage(1953, common.PROD, "1.0.0", "x:1.0.0"), "not recorded, nothing else changes")

	p := newRequestedPayload(t, "MyApp", 1953, common.PROD)
	p.PresentVersion = "1.0.0"
	p.PresentImageName = "prod_1953_myapp:1.0.0"
	require.NoError(t, db.UpsertRequestedStateChange(p))
	require.NoError(t, db.BulkUpsertRequestedStateChanges([]common.TransitionPayload{p}))

	all, err := db.GetRequestedStates()
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, "1.0.0", all[0].PresentVersion)
	assert.Empty(t, all[0].PresentImageName)

	one, err := db.GetRequestedState(1953, common.PROD)
	require.NoError(t, err)
	assert.Equal(t, "1.0.0", one.PresentVersion)
}

// The image an app's version was really pulled under is written by the agent
// alone: a sync (an upsert) never replaces it.
func TestInstalledImageSurvivesASync(t *testing.T) {
	db := newTestDB(t)
	p := newRequestedPayload(t, "MyApp", 1953, common.PROD)
	require.NoError(t, db.UpsertRequestedStateChange(p))
	require.NoError(t, db.SetInstalledImage(1953, common.PROD, "2.0.0", "registry.test/apps/prod_1953_myapp:2.0.0"))

	p.PresentImageName = "prod_amd64_1953_myapp:2.0.0"
	require.NoError(t, db.BulkUpsertRequestedStateChanges([]common.TransitionPayload{p}))
	require.NoError(t, db.UpsertRequestedStateChange(p))

	one, err := db.GetRequestedState(1953, common.PROD)
	require.NoError(t, err)
	assert.Equal(t, "registry.test/apps/prod_1953_myapp:2.0.0", one.InstalledImage)
	assert.Equal(t, "2.0.0", one.InstalledImageVersion)
	all, err := db.GetRequestedStates()
	require.NoError(t, err)
	assert.Equal(t, "registry.test/apps/prod_1953_myapp:2.0.0", all[0].InstalledImage)
}
