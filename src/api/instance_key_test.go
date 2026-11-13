package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reagent/apps"
	"reagent/config"
	"reagent/logging"
	"reagent/store"
	"reagent/testutil/fakes"
	"reagent/testutil/mocks"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// flockInstanceKey is the instance key the .flock on disk names: 0 for none,
// ok false while there is no file.
func flockInstanceKey(t *testing.T, path string) (instanceKey uint64, ok bool) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, false
	}
	require.NoError(t, err)
	var saved config.ReswarmConfig
	require.NoError(t, json.Unmarshal(raw, &saved))
	return saved.InstanceKey, true
}

func instanceKeyEx(t *testing.T) (*External, string) {
	t.Helper()
	cfg := testConfig()
	flock := filepath.Join(t.TempDir(), "device.flock")
	cfg.CommandLineArguments.ConfigFileLocation = flock
	return &External{Config: cfg}, flock
}

// The instance key of an app-state push goes into the .flock, where the apps'
// INSTANCE_KEY comes from on every start, also on one from the stored state
// before the agent is connected.
func TestLearnInstanceKey(t *testing.T) {
	t.Run("a key is in force at once and saved", func(t *testing.T) {
		for name, value := range map[string]interface{}{
			"uint64":  uint64(11),
			"int64":   int64(11),
			"float64": float64(11),
			"string":  "11",
		} {
			t.Run(name, func(t *testing.T) {
				ex, flock := instanceKeyEx(t)

				ex.learnInstanceKey(value)

				_, inForce := ex.Config.Platform()
				assert.Equal(t, uint64(11), inForce)
				saved, ok := flockInstanceKey(t, flock)
				require.True(t, ok, "the .flock is written")
				assert.Equal(t, uint64(11), saved)
			})
		}
	})

	t.Run("the same key again does not rewrite the .flock", func(t *testing.T) {
		ex, flock := instanceKeyEx(t)
		ex.Config.ReswarmConfig.InstanceKey = 11

		ex.learnInstanceKey(uint64(11))

		_, ok := flockInstanceKey(t, flock)
		assert.False(t, ok, "no write when nothing changed")
	})

	// A cloud backend's push names no key, and neither does an appliance's
	// that has lost its licence: the device then belongs to no instance, and
	// the next container must not get a stale INSTANCE_KEY.
	t.Run("a push without a key takes the key back", func(t *testing.T) {
		ex, flock := instanceKeyEx(t)
		ex.Config.ReswarmConfig.InstanceKey = 11

		ex.learnInstanceKey(nil)

		_, inForce := ex.Config.Platform()
		assert.Zero(t, inForce)
		saved, ok := flockInstanceKey(t, flock)
		require.True(t, ok)
		assert.Zero(t, saved)
	})

	t.Run("a key that cannot be read changes nothing", func(t *testing.T) {
		for name, value := range map[string]interface{}{
			"text":     "eleven",
			"negative": int64(-1),
			"fraction": 11.5,
			"bool":     true,
		} {
			t.Run(name, func(t *testing.T) {
				ex, flock := instanceKeyEx(t)
				ex.Config.ReswarmConfig.InstanceKey = 11

				ex.learnInstanceKey(value)

				_, inForce := ex.Config.Platform()
				assert.Equal(t, uint64(11), inForce)
				_, ok := flockInstanceKey(t, flock)
				assert.False(t, ok)
			})
		}
	})

	t.Run("a failed save keeps the key in force", func(t *testing.T) {
		ex, _ := instanceKeyEx(t)
		// A directory where the file should be: it cannot be written.
		ex.Config.CommandLineArguments.ConfigFileLocation = t.TempDir()

		ex.learnInstanceKey(uint64(11))

		_, inForce := ex.Config.Platform()
		assert.Equal(t, uint64(11), inForce)
	})
}

// Through the handler the backend calls, so the test fails when the handler
// stops learning the key. The app store's database is closed: the handler
// returns at CreateOrUpdateApp, before it starts a transition, and has learned
// the key by then, as the transition would need it.
func TestRequestAppStateHandlerLearnsInstanceKey(t *testing.T) {
	db := newLogDB(t)
	msg := fakes.NewMessenger()
	cont := mocks.NewContainer(t)
	appStore := store.NewAppStore(db, msg)
	lm := logging.NewLogManager(cont, msg, db, appStore)
	observer := apps.NewObserver(cont, &appStore, nil)
	sm := apps.NewStateMachine(cont, &lm, &observer, nil)
	am := apps.NewAppManager(&sm, &appStore, &observer, mocks.NewTunnelManager(t))
	require.NoError(t, db.Close())

	ex, flock := instanceKeyEx(t)
	ex.AppManager = am

	response := createBasicWAMPResponse(42, "kiosk", "PROD", "RUNNING")
	response.ArgumentsKw["instance_key"] = uint64(11)

	_, err := ex.requestAppStateHandler(context.Background(), response)
	require.Error(t, err, "the closed database stops the request before a transition starts")

	_, inForce := ex.Config.Platform()
	assert.Equal(t, uint64(11), inForce)
	saved, ok := flockInstanceKey(t, flock)
	require.True(t, ok)
	assert.Equal(t, uint64(11), saved)

	// A push the agent cannot parse changes nothing.
	malformed := createBasicWAMPResponse(42, "kiosk", "PROD", "RUNNING")
	malformed.ArgumentsKw["app_key"] = "forty-two"
	malformed.ArgumentsKw["instance_key"] = uint64(12)
	_, err = ex.requestAppStateHandler(context.Background(), malformed)
	require.Error(t, err)
	_, inForce = ex.Config.Platform()
	assert.Equal(t, uint64(11), inForce)
}
