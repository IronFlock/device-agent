package persistence

import (
	"reagent/common"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func boolPtr(value bool) *bool { return &value }

// The privileged access survives an agent restart: an offline boot starts the
// apps from these rows. nil (a backend that predates the grant) must come
// back as nil, not as false: false would strip apps that were never meant to
// lose their privileges.
func TestRequestedStatePrivilegedRoundTrip(t *testing.T) {
	cases := []struct {
		name          string
		privileged    *bool
		newPrivileged *bool
	}{
		{name: "nil", privileged: nil, newPrivileged: nil},
		{name: "true", privileged: boolPtr(true), newPrivileged: boolPtr(true)},
		{name: "false", privileged: boolPtr(false), newPrivileged: boolPtr(false)},
		{name: "mixed", privileged: boolPtr(true), newPrivileged: boolPtr(false)},
		{name: "only the newest release", privileged: nil, newPrivileged: boolPtr(true)},
	}

	db := newTestDB(t)

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := uint64(700 + i)
			payload := newRequestedPayload(t, "priv-app", key, common.PROD)
			payload.Privileged = tc.privileged
			payload.NewPrivileged = tc.newPrivileged

			require.NoError(t, db.UpsertRequestedStateChange(payload))

			got, err := db.GetRequestedState(key, common.PROD)
			require.NoError(t, err)
			assert.Equal(t, tc.privileged, got.Privileged)
			assert.Equal(t, tc.newPrivileged, got.NewPrivileged)

			all, err := db.GetRequestedStates()
			require.NoError(t, err)
			found := false
			for _, row := range all {
				if row.AppKey == key {
					found = true
					assert.Equal(t, tc.privileged, row.Privileged)
					assert.Equal(t, tc.newPrivileged, row.NewPrivileged)
				}
			}
			assert.True(t, found)
		})
	}
}

func TestRequestedStatePrivilegedOverwrites(t *testing.T) {
	db := newTestDB(t)

	payload := newRequestedPayload(t, "priv-upd", 800, common.PROD)
	payload.Privileged = boolPtr(false)
	payload.NewPrivileged = boolPtr(false)
	require.NoError(t, db.UpsertRequestedStateChange(payload))

	// A later sync from an old backend carries no value: it replaces the
	// stored one with nil rather than keeping a stale false.
	payload.Privileged = nil
	payload.NewPrivileged = nil
	require.NoError(t, db.UpsertRequestedStateChange(payload))

	got, err := db.GetRequestedState(800, common.PROD)
	require.NoError(t, err)
	assert.Nil(t, got.Privileged)
	assert.Nil(t, got.NewPrivileged)

	// The bulk path (sync) writes them too.
	payload.Privileged = boolPtr(true)
	payload.NewPrivileged = boolPtr(false)
	require.NoError(t, db.BulkUpsertRequestedStateChanges([]common.TransitionPayload{payload}))

	got, err = db.GetRequestedState(800, common.PROD)
	require.NoError(t, err)
	assert.Equal(t, boolPtr(true), got.Privileged)
	assert.Equal(t, boolPtr(false), got.NewPrivileged)

	// A second Init (the next agent start) leaves the columns and values.
	require.NoError(t, db.Init())
	got, err = db.GetRequestedState(800, common.PROD)
	require.NoError(t, err)
	assert.Equal(t, boolPtr(true), got.Privileged)
	assert.Equal(t, boolPtr(false), got.NewPrivileged)
}
