package store

import (
	"path/filepath"
	"testing"

	"reagent/common"
	"reagent/messenger"
	"reagent/messenger/topics"
	"reagent/persistence"
	"reagent/testutil/builders"
	"reagent/testutil/fakes"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestDB builds a real, isolated SQLite-backed persistence DB in a temp dir
// and returns it ready for use (Init already run). It registers a cleanup that
// closes the DB.
func newTestDB(t *testing.T) persistence.Database {
	t.Helper()

	cfg := builders.DefaultTestConfig()
	cfg.CommandLineArguments.DatabaseFileName = filepath.Join(t.TempDir(), "reagent_test.db")

	db, err := persistence.NewSQLiteDb(cfg)
	require.NoError(t, err)
	require.NoError(t, db.Init())

	t.Cleanup(func() {
		_ = db.Close()
	})

	return db
}

// newTestStore builds an AppStore backed by a real DB and a fake messenger.
func newTestStore(t *testing.T) (*AppStore, *fakes.Messenger) {
	t.Helper()

	db := newTestDB(t)
	msg := fakes.NewMessenger()
	st := NewAppStore(db, msg)
	return &st, msg
}

func TestAddApp(t *testing.T) {
	t.Run("persists app and returns populated App", func(t *testing.T) {
		st, _ := newTestStore(t)

		payload := builders.BuildTransitionPayload("my-app", common.PRESENT, common.PROD)
		payload.AppKey = 42
		payload.CurrentState = common.RUNNING
		payload.ReleaseKey = 7
		payload.PresentVersion = "2.3.4"

		app, err := st.AddApp(payload)
		require.NoError(t, err)
		require.NotNil(t, app)

		assert.Equal(t, uint64(42), app.AppKey)
		assert.Equal(t, "my-app", app.AppName)
		assert.Equal(t, common.RUNNING, app.CurrentState)
		assert.Equal(t, common.PROD, app.Stage)
		assert.Equal(t, uint64(7), app.ReleaseKey)
		assert.Equal(t, "2.3.4", app.Version)
		require.NotNil(t, app.TransitionLock)

		// It should now be discoverable via the database.
		fromDB, err := st.database.GetAppState(42, common.PROD)
		require.NoError(t, err)
		require.NotNil(t, fromDB)
		assert.Equal(t, "my-app", fromDB.AppName)
		assert.Equal(t, common.RUNNING, fromDB.CurrentState)
	})

	t.Run("empty current state defaults to REMOVED", func(t *testing.T) {
		st, _ := newTestStore(t)

		payload := builders.BuildTransitionPayload("blank-app", common.PRESENT, common.DEV)
		payload.AppKey = 99
		payload.CurrentState = "" // no current state

		app, err := st.AddApp(payload)
		require.NoError(t, err)
		require.NotNil(t, app)
		assert.Equal(t, common.REMOVED, app.CurrentState)

		fromDB, err := st.database.GetAppState(99, common.DEV)
		require.NoError(t, err)
		require.NotNil(t, fromDB)
		assert.Equal(t, common.REMOVED, fromDB.CurrentState)
	})
}

func TestGetApp(t *testing.T) {
	t.Run("returns app added in-memory", func(t *testing.T) {
		st, _ := newTestStore(t)

		payload := builders.BuildTransitionPayload("cache-app", common.RUNNING, common.PROD)
		payload.AppKey = 5
		payload.CurrentState = common.RUNNING

		added, err := st.AddApp(payload)
		require.NoError(t, err)

		got, err := st.GetApp(5, common.PROD)
		require.NoError(t, err)
		require.NotNil(t, got)
		// Same pointer returned from the in-memory slice.
		assert.Same(t, added, got)
	})

	t.Run("loads from database when not in memory", func(t *testing.T) {
		st, _ := newTestStore(t)

		// Insert directly into the DB, bypassing the in-memory slice.
		app := builders.BuildApp("db-only-app", common.RUNNING, common.PROD)
		app.AppKey = 8
		app.ReleaseKey = 3
		_, err := st.database.UpsertAppState(app, common.RUNNING)
		require.NoError(t, err)

		got, err := st.GetApp(8, common.PROD)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, uint64(8), got.AppKey)
		assert.Equal(t, "db-only-app", got.AppName)
		assert.Equal(t, common.RUNNING, got.CurrentState)
		// A TransitionLock should have been initialized on load.
		require.NotNil(t, got.TransitionLock)

		// And it should now be cached: same pointer on a second call.
		again, err := st.GetApp(8, common.PROD)
		require.NoError(t, err)
		assert.Same(t, got, again)
	})

	t.Run("returns nil when app does not exist", func(t *testing.T) {
		st, _ := newTestStore(t)

		got, err := st.GetApp(1234, common.PROD)
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("distinguishes stage", func(t *testing.T) {
		st, _ := newTestStore(t)

		payload := builders.BuildTransitionPayload("staged-app", common.RUNNING, common.PROD)
		payload.AppKey = 11
		payload.CurrentState = common.RUNNING
		_, err := st.AddApp(payload)
		require.NoError(t, err)

		// Same key, different stage -> not found.
		got, err := st.GetApp(11, common.DEV)
		require.NoError(t, err)
		assert.Nil(t, got)
	})
}

func TestUpdateLocalAppState(t *testing.T) {
	st, _ := newTestStore(t)

	payload := builders.BuildTransitionPayload("upd-app", common.PRESENT, common.PROD)
	payload.AppKey = 21
	payload.CurrentState = common.PRESENT
	app, err := st.AddApp(payload)
	require.NoError(t, err)

	err = st.UpdateLocalAppState(app, common.RUNNING)
	require.NoError(t, err)

	// A real state change yields a non-empty timestamp persisted on the app.
	assert.NotEmpty(t, string(app.LastUpdated))

	fromDB, err := st.database.GetAppState(21, common.PROD)
	require.NoError(t, err)
	require.NotNil(t, fromDB)
	assert.Equal(t, common.RUNNING, fromDB.CurrentState)
}

func TestDeleteAppState(t *testing.T) {
	st, _ := newTestStore(t)

	payload := builders.BuildTransitionPayload("del-app", common.RUNNING, common.PROD)
	payload.AppKey = 31
	payload.CurrentState = common.RUNNING
	_, err := st.AddApp(payload)
	require.NoError(t, err)

	// Precondition: present in the DB.
	fromDB, err := st.database.GetAppState(31, common.PROD)
	require.NoError(t, err)
	require.NotNil(t, fromDB)

	err = st.DeleteAppState(31, common.PROD)
	require.NoError(t, err)

	gone, err := st.database.GetAppState(31, common.PROD)
	require.NoError(t, err)
	assert.Nil(t, gone)
}

func TestUpdateLocalRequestedStateAndDelete(t *testing.T) {
	st, _ := newTestStore(t)

	payload := builders.BuildTransitionPayload("req-app", common.PRESENT, common.PROD)
	payload.AppKey = 41
	payload.RequestorAccountKey = 1001
	payload.CurrentState = common.PRESENT

	err := st.UpdateLocalRequestedState(payload)
	require.NoError(t, err)

	got, err := st.GetRequestedState(41, common.PROD)
	require.NoError(t, err)
	assert.Equal(t, uint64(41), got.AppKey)
	assert.Equal(t, "req-app", got.AppName)
	assert.Equal(t, common.PRESENT, got.RequestedState)
	assert.Equal(t, uint64(1001), got.RequestorAccountKey)

	all, err := st.GetRequestedStates()
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, uint64(41), all[0].AppKey)

	// Now delete it.
	err = st.DeleteRequestedState(41, common.PROD)
	require.NoError(t, err)

	all, err = st.GetRequestedStates()
	require.NoError(t, err)
	assert.Empty(t, all)
}

func TestGetAppByContainerName(t *testing.T) {
	st, _ := newTestStore(t)

	payload := builders.BuildTransitionPayload("MyApp", common.RUNNING, common.DEV)
	payload.AppKey = 77
	payload.CurrentState = common.RUNNING
	added, err := st.AddApp(payload)
	require.NoError(t, err)

	t.Run("exact match", func(t *testing.T) {
		got := st.GetAppByContainerName("DEV_77_MyApp")
		require.NotNil(t, got)
		assert.Same(t, added, got)
	})

	t.Run("case-insensitive match", func(t *testing.T) {
		got := st.GetAppByContainerName("dev_77_myapp")
		require.NotNil(t, got)
		assert.Same(t, added, got)
	})

	t.Run("no match returns nil", func(t *testing.T) {
		assert.Nil(t, st.GetAppByContainerName("PROD_77_MyApp"))
		assert.Nil(t, st.GetAppByContainerName("DEV_1_OtherApp"))
		assert.Nil(t, st.GetAppByContainerName("not-a-container"))
	})
}

func TestFetchRequestedAppStates(t *testing.T) {
	t.Run("maps response into transition payloads", func(t *testing.T) {
		st, msg := newTestStore(t)

		// The remote returns a list of DeviceSyncResponse-shaped objects in
		// Arguments[0]. FetchRequestedAppStates json-marshals/unmarshals it,
		// so a slice of structs is a faithful stand-in for the WAMP payload.
		responses := []common.DeviceSyncResponse{
			{
				AppKey:                100,
				AppName:               "alpha",
				Stage:                 string(common.PROD),
				CurrentState:          string(common.RUNNING),
				TargetState:           string(common.PRESENT),
				ReleaseKey:            3,
				NewReleaseKey:         4,
				RequestorAccountKey:   555,
				DeviceOwnerAccountKey: 9,
				RequestUpdate:         true,
				PresentVersion:        "1.2.3",
				NewestVersion:         "1.3.0",
				Environment:           map[string]interface{}{"FOO": "bar"},
				Ports:                 []interface{}{float64(8080)},
			},
			{
				AppKey:       200,
				AppName:      "beta",
				Stage:        string(common.DEV),
				CurrentState: string(common.REMOVED),
				TargetState:  string(common.PRESENT),
			},
		}

		msg.SetCallResponse(string(topics.GetRequestedAppStates), messenger.Result{
			Arguments: []interface{}{responses},
		}, nil)

		payloads, err := st.FetchRequestedAppStates()
		require.NoError(t, err)
		require.Len(t, payloads, 2)

		// Verify the topic was actually called with the device key from config.
		require.Len(t, msg.CallCalls, 1)
		call := msg.CallCalls[0]
		assert.Equal(t, topics.GetRequestedAppStates, call.Topic)
		require.Len(t, call.Args, 1)
		argDict, ok := call.Args[0].(common.Dict)
		require.True(t, ok)
		assert.Equal(t, msg.GetConfig().ReswarmConfig.DeviceKey, argDict["device_key"])

		alpha := payloads[0]
		assert.Equal(t, uint64(100), alpha.AppKey)
		assert.Equal(t, "alpha", alpha.AppName)
		assert.Equal(t, common.PROD, alpha.Stage)
		assert.Equal(t, common.RUNNING, alpha.CurrentState)
		assert.Equal(t, common.PRESENT, alpha.RequestedState)
		assert.Equal(t, uint64(3), alpha.ReleaseKey)
		assert.Equal(t, uint64(4), alpha.NewReleaseKey)
		assert.Equal(t, uint64(555), alpha.RequestorAccountKey)
		assert.Equal(t, uint64(9), alpha.DeviceOwnerAccountKey)
		assert.True(t, alpha.RequestUpdate)
		assert.Equal(t, "1.2.3", alpha.PresentVersion)
		assert.Equal(t, "1.2.3", alpha.Version)
		assert.Equal(t, "1.3.0", alpha.NewestVersion)
		assert.Equal(t, "bar", alpha.EnvironmentVariables["FOO"])
		require.Len(t, alpha.Ports, 1)

		beta := payloads[1]
		assert.Equal(t, uint64(200), beta.AppKey)
		assert.Equal(t, "beta", beta.AppName)
		assert.Equal(t, common.DEV, beta.Stage)
		assert.Equal(t, common.REMOVED, beta.CurrentState)
		assert.Equal(t, common.PRESENT, beta.RequestedState)
	})

	t.Run("maps privileged access, absent and null stay nil", func(t *testing.T) {
		st, msg := newTestStore(t)

		// Raw rows as the backend sends them: an old backend has no fields.
		rows := []interface{}{
			map[string]interface{}{"app_key": 1, "app_name": "old", "stage": "PROD", "current_state": "RUNNING", "target_state": "RUNNING"},
			map[string]interface{}{"app_key": 2, "app_name": "nulls", "stage": "PROD", "current_state": "RUNNING", "target_state": "RUNNING",
				"privileged": nil, "new_privileged": nil},
			map[string]interface{}{"app_key": 3, "app_name": "set", "stage": "PROD", "current_state": "RUNNING", "target_state": "RUNNING",
				"privileged": false, "new_privileged": true},
		}
		msg.SetCallResponse(string(topics.GetRequestedAppStates), messenger.Result{
			Arguments: []interface{}{rows},
		}, nil)

		payloads, err := st.FetchRequestedAppStates()
		require.NoError(t, err)
		require.Len(t, payloads, 3)

		assert.Nil(t, payloads[0].Privileged)
		assert.Nil(t, payloads[0].NewPrivileged)
		assert.Nil(t, payloads[1].Privileged)
		assert.Nil(t, payloads[1].NewPrivileged)
		require.NotNil(t, payloads[2].Privileged)
		require.NotNil(t, payloads[2].NewPrivileged)
		assert.False(t, *payloads[2].Privileged)
		assert.True(t, *payloads[2].NewPrivileged)
	})

	t.Run("empty response yields empty slice", func(t *testing.T) {
		st, msg := newTestStore(t)

		msg.SetCallResponse(string(topics.GetRequestedAppStates), messenger.Result{
			Arguments: []interface{}{[]common.DeviceSyncResponse{}},
		}, nil)

		payloads, err := st.FetchRequestedAppStates()
		require.NoError(t, err)
		assert.Empty(t, payloads)
	})

	t.Run("propagates messenger call error", func(t *testing.T) {
		st, msg := newTestStore(t)

		msg.SetCallError(string(topics.GetRequestedAppStates), assert.AnError)

		payloads, err := st.FetchRequestedAppStates()
		require.Error(t, err)
		assert.Empty(t, payloads)
	})
}

func TestUpdateRequestedStatesWithRemote(t *testing.T) {
	st, msg := newTestStore(t)

	responses := []common.DeviceSyncResponse{
		{
			AppKey:       300,
			AppName:      "gamma",
			Stage:        string(common.PROD),
			CurrentState: string(common.RUNNING),
			TargetState:  string(common.PRESENT),
			ReleaseKey:   1,
		},
	}

	msg.SetCallResponse(string(topics.GetRequestedAppStates), messenger.Result{
		Arguments: []interface{}{responses},
	}, nil)

	err := st.UpdateRequestedStatesWithRemote()
	require.NoError(t, err)

	// The fetched remote state should now be persisted as a requested state.
	got, err := st.GetRequestedState(300, common.PROD)
	require.NoError(t, err)
	assert.Equal(t, uint64(300), got.AppKey)
	assert.Equal(t, "gamma", got.AppName)
	assert.Equal(t, common.PRESENT, got.RequestedState)
}

func TestGetRegistryToken(t *testing.T) {
	t.Run("returns token from response", func(t *testing.T) {
		st, msg := newTestStore(t)

		msg.SetCallResponse(string(topics.GetRegistryToken), messenger.Result{
			Arguments: []interface{}{"secret-token"},
		}, nil)

		token, err := st.GetRegistryToken(42)
		require.NoError(t, err)
		assert.Equal(t, "secret-token", token)

		require.Len(t, msg.CallCalls, 1)
		assert.Equal(t, topics.GetRegistryToken, msg.CallCalls[0].Topic)
	})

	t.Run("empty arguments yields empty token", func(t *testing.T) {
		st, msg := newTestStore(t)

		msg.SetCallResponse(string(topics.GetRegistryToken), messenger.Result{
			Arguments: nil,
		}, nil)

		token, err := st.GetRegistryToken(42)
		require.NoError(t, err)
		assert.Empty(t, token)
	})

	t.Run("non-string payload errors", func(t *testing.T) {
		st, msg := newTestStore(t)

		msg.SetCallResponse(string(topics.GetRegistryToken), messenger.Result{
			Arguments: []interface{}{12345},
		}, nil)

		_, err := st.GetRegistryToken(42)
		require.Error(t, err)
	})

	t.Run("propagates call error", func(t *testing.T) {
		st, msg := newTestStore(t)

		msg.SetCallError(string(topics.GetRegistryToken), assert.AnError)

		_, err := st.GetRegistryToken(42)
		require.Error(t, err)
	})
}

func TestUpdateRemoteAppState(t *testing.T) {
	st, msg := newTestStore(t)

	msg.SetCallResponse(string(topics.SetActualAppOnDeviceState), messenger.Result{}, nil)

	app := builders.BuildApp("remote-app", common.RUNNING, common.PROD)
	app.AppKey = 50
	app.UpdateStatus = common.PENDING_REMOTE_CONFIRMATION

	err := st.UpdateRemoteAppState(t.Context(), app, common.RUNNING)
	require.NoError(t, err)

	// The pending status should be flipped to COMPLETED on success.
	assert.Equal(t, common.COMPLETED, app.UpdateStatus)

	require.Len(t, msg.CallCalls, 1)
	call := msg.CallCalls[0]
	assert.Equal(t, topics.SetActualAppOnDeviceState, call.Topic)
	require.Len(t, call.Args, 1)
	dict, ok := call.Args[0].(common.Dict)
	require.True(t, ok)
	assert.Equal(t, app.AppKey, dict["app_key"])
	assert.Equal(t, common.RUNNING, dict["state"])
}

// Compile-time assertion: the fake messenger satisfies the interface AppStore needs.
var _ messenger.Messenger = (*fakes.Messenger)(nil)
