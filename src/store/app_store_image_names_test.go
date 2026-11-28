package store

import (
	"testing"

	"reagent/common"
	"reagent/messenger"
	"reagent/messenger/topics"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The sync answer names the app's image for the present and the newest
// version; a value that is no string is left out, the rest of the answer is
// taken.
func TestFetchRequestedAppStatesTakesTheImageNames(t *testing.T) {
	st, msg := newTestStore(t)

	msg.SetCallResponse(string(topics.GetRequestedAppStates), messenger.Result{
		Arguments: []interface{}{[]interface{}{
			map[string]interface{}{
				"app_key": 1953, "app_name": "MyApp", "stage": "PROD",
				"current_state": "RUNNING", "target_state": "RUNNING",
				"present_version": "1.0.0", "newest_version": "2.0.0",
				"present_image_name": "prod_amd64_1953_myapp:1.0.0",
				"architecture":       "armv7",
				"new_image_name":     "prod_1953_myapp:2.0.0",
			},
			map[string]interface{}{
				"app_key": 1954, "app_name": "Other", "stage": "PROD",
				"current_state": "RUNNING", "target_state": "RUNNING",
				"present_version": "1.0.0", "newest_version": "1.0.0",
				"present_image_name": 17,
			},
		}},
	}, nil)

	payloads, err := st.FetchRequestedAppStates()
	require.NoError(t, err)
	require.Len(t, payloads, 2)
	assert.Equal(t, "prod_amd64_1953_myapp:1.0.0", payloads[0].PresentImageName)
	assert.Equal(t, "prod_1953_myapp:2.0.0", payloads[0].NewImageName)
	assert.Equal(t, "armv7", payloads[0].Architecture)
	assert.Empty(t, payloads[1].PresentImageName)
	assert.Equal(t, "1.0.0", payloads[1].PresentVersion, "the rest of the answer is taken")
}

// The PUBLISHED report of a release build names what it pushed
// (pushed_digests), where that is known; no other report does.
func TestUpdateRemoteAppStateReportsThePushedDigests(t *testing.T) {
	pushed := map[string]string{
		"registry.example/apps/prod_arm64_12_demo:1.0.0": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	report := func(t *testing.T, digests map[string]string, state common.AppState) common.Dict {
		t.Helper()
		st, msg := newTestStore(t)
		app := &common.App{AppKey: 12, AppName: "demo", Stage: common.DEV, ReleaseBuild: true, PushedDigests: digests}
		require.NoError(t, st.UpdateRemoteAppState(t.Context(), app, state))
		calls := msg.GetCallCalls()
		require.NotEmpty(t, calls)
		dict, ok := calls[len(calls)-1].Args[0].(common.Dict)
		require.True(t, ok)
		return dict
	}

	t.Run("published with digests", func(t *testing.T) {
		dict := report(t, pushed, common.PUBLISHED)
		assert.Equal(t, pushed, dict["pushed_digests"])
	})
	t.Run("published, nothing known: the key is absent", func(t *testing.T) {
		dict := report(t, nil, common.PUBLISHED)
		_, found := dict["pushed_digests"]
		assert.False(t, found)
		dict = report(t, map[string]string{}, common.PUBLISHED)
		_, found = dict["pushed_digests"]
		assert.False(t, found)
	})
	t.Run("another state never carries them", func(t *testing.T) {
		dict := report(t, pushed, common.PUBLISHING)
		_, found := dict["pushed_digests"]
		assert.False(t, found)
	})
}
