package api

import (
	"reagent/common"
	"reagent/config"
	"reagent/messenger"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Test Setup Helpers
// =============================================================================

func testConfig() *config.Config {
	return &config.Config{
		CommandLineArguments: &config.CommandLineArguments{
			AgentDir:       "/opt/reagent",
			AppsDirectory:  "/opt/reagent/apps",
			AppsBuildDir:   "/opt/reagent/apps/build",
			AppsComposeDir: "/opt/reagent/apps/compose",
			AppsSharedDir:  "/opt/reagent/apps/shared",
			DownloadDir:    "/opt/reagent/downloads",
		},
		ReswarmConfig: &config.ReswarmConfig{
			Environment:       string(common.PRODUCTION),
			DeviceKey:         12345,
			Secret:            "test-secret",
			DockerRegistryURL: "registry.test.com",
		},
	}
}

func createBasicWAMPResponse(appKey uint64, appName string, stage string, requestedState string) messenger.Result {
	return messenger.Result{
		ArgumentsKw: common.Dict{
			"app_key":      appKey,
			"app_name":     appName,
			"stage":        stage,
			"target_state": requestedState,
		},
		Details: common.Dict{
			"caller_authid": "123",
		},
	}
}

func createFullWAMPResponse() messenger.Result {
	return messenger.Result{
		ArgumentsKw: common.Dict{
			"app_key":               uint64(42),
			"app_name":              "test-app",
			"stage":                 "PROD",
			"target_state":          "RUNNING",
			"current_state":         "PRESENT",
			"release_key":           uint64(100),
			"new_release_key":       uint64(101),
			"requestor_account_key": uint64(999),
			"version":               "1.0.0",
			"present_version":       "0.9.0",
			"newest_version":        "1.1.0",
			"request_update":        true,
			"cancel_transition":     false,
			"environment": map[string]interface{}{
				"ENV_VAR": "value",
			},
			"environment_template": map[string]interface{}{
				"TEMPLATE_VAR": "default",
			},
			"ports": []interface{}{
				map[string]interface{}{
					"port":     uint64(8080),
					"protocol": "http",
					"active":   true,
				},
			},
			"docker_compose": map[string]interface{}{
				"version": "3",
				"services": map[string]interface{}{
					"app": map[string]interface{}{
						"image": "test-image:latest",
					},
				},
			},
			"docker_credentials": map[string]interface{}{
				"registry.example.com": map[string]interface{}{
					"username": "user",
					"password": "pass",
				},
			},
		},
		Details: common.Dict{
			"caller_authid": "999",
		},
	}
}

// =============================================================================
// TestResponseToTransitionPayload - Tests for WAMP response parsing
// =============================================================================

func TestResponseToTransitionPayload(t *testing.T) {
	cfg := testConfig()

	t.Run("parses basic payload successfully", func(t *testing.T) {
		response := createBasicWAMPResponse(42, "my-app", "PROD", "RUNNING")

		payload, err := responseToTransitionPayload(cfg, response)

		require.NoError(t, err)
		assert.Equal(t, uint64(42), payload.AppKey)
		assert.Equal(t, "my-app", payload.AppName)
		assert.Equal(t, common.Stage("PROD"), payload.Stage)
		assert.Equal(t, common.AppState("RUNNING"), payload.RequestedState)
	})

	t.Run("parses full payload with all fields", func(t *testing.T) {
		response := createFullWAMPResponse()

		payload, err := responseToTransitionPayload(cfg, response)

		require.NoError(t, err)
		assert.Equal(t, uint64(42), payload.AppKey)
		assert.Equal(t, "test-app", payload.AppName)
		assert.Equal(t, common.Stage("PROD"), payload.Stage)
		assert.Equal(t, common.AppState("RUNNING"), payload.RequestedState)
		assert.Equal(t, common.AppState("PRESENT"), payload.CurrentState)
		assert.Equal(t, uint64(100), payload.ReleaseKey)
		assert.Equal(t, uint64(101), payload.NewReleaseKey)
		assert.Equal(t, uint64(999), payload.RequestorAccountKey)
		assert.Equal(t, "1.0.0", payload.Version)
		assert.Equal(t, "0.9.0", payload.PresentVersion)
		assert.Equal(t, "1.1.0", payload.NewestVersion)
		assert.True(t, payload.RequestUpdate)
		assert.False(t, payload.CancelTransition)
		assert.NotNil(t, payload.EnvironmentVariables)
		assert.NotNil(t, payload.EnvironmentTemplate)
		assert.NotNil(t, payload.Ports)
		assert.NotNil(t, payload.DockerCompose)
		assert.NotNil(t, payload.DockerCredentials)
	})

	t.Run("parses DEV stage", func(t *testing.T) {
		response := createBasicWAMPResponse(42, "dev-app", "DEV", "BUILT")

		payload, err := responseToTransitionPayload(cfg, response)

		require.NoError(t, err)
		assert.Equal(t, common.Stage("DEV"), payload.Stage)
		assert.Equal(t, common.AppState("BUILT"), payload.RequestedState)
	})

	t.Run("uses manually_requested_state when target_state is empty", func(t *testing.T) {
		response := messenger.Result{
			ArgumentsKw: common.Dict{
				"app_key":                  uint64(42),
				"app_name":                 "my-app",
				"stage":                    "PROD",
				"target_state":             "",
				"manually_requested_state": "PRESENT",
			},
		}

		payload, err := responseToTransitionPayload(cfg, response)

		require.NoError(t, err)
		assert.Equal(t, common.AppState("PRESENT"), payload.RequestedState)
	})

	t.Run("uses state field when current_state is empty", func(t *testing.T) {
		response := messenger.Result{
			ArgumentsKw: common.Dict{
				"app_key":      uint64(42),
				"app_name":     "my-app",
				"stage":        "PROD",
				"target_state": "RUNNING",
				"state":        "PRESENT",
			},
		}

		payload, err := responseToTransitionPayload(cfg, response)

		require.NoError(t, err)
		assert.Equal(t, common.AppState("PRESENT"), payload.CurrentState)
	})

	t.Run("uses account_id when requestor_account_key is empty", func(t *testing.T) {
		response := messenger.Result{
			ArgumentsKw: common.Dict{
				"app_key":      uint64(42),
				"app_name":     "my-app",
				"stage":        "PROD",
				"target_state": "RUNNING",
				"account_id":   uint64(555),
			},
		}

		payload, err := responseToTransitionPayload(cfg, response)

		require.NoError(t, err)
		assert.Equal(t, uint64(555), payload.RequestorAccountKey)
	})

	t.Run("parses requestor_account_key as string", func(t *testing.T) {
		response := messenger.Result{
			ArgumentsKw: common.Dict{
				"app_key":               uint64(42),
				"app_name":              "my-app",
				"stage":                 "PROD",
				"target_state":          "RUNNING",
				"requestor_account_key": "123",
			},
		}

		payload, err := responseToTransitionPayload(cfg, response)

		require.NoError(t, err)
		assert.Equal(t, uint64(123), payload.RequestorAccountKey)
	})

	t.Run("parses release_key as string", func(t *testing.T) {
		response := messenger.Result{
			ArgumentsKw: common.Dict{
				"app_key":      uint64(42),
				"app_name":     "my-app",
				"stage":        "PROD",
				"target_state": "RUNNING",
				"release_key":  "456",
			},
		}

		payload, err := responseToTransitionPayload(cfg, response)

		require.NoError(t, err)
		assert.Equal(t, uint64(456), payload.ReleaseKey)
	})

	t.Run("parses docker credentials correctly", func(t *testing.T) {
		response := messenger.Result{
			ArgumentsKw: common.Dict{
				"app_key":      uint64(42),
				"app_name":     "my-app",
				"stage":        "PROD",
				"target_state": "RUNNING",
				"docker_credentials": map[string]interface{}{
					"registry1.com": map[string]interface{}{
						"username": "user1",
						"password": "pass1",
					},
					"registry2.com": map[string]interface{}{
						"username": "user2",
						"password": "pass2",
					},
				},
			},
		}

		payload, err := responseToTransitionPayload(cfg, response)

		require.NoError(t, err)
		require.Len(t, payload.DockerCredentials, 2)
		assert.Equal(t, "user1", payload.DockerCredentials["registry1.com"].Username)
		assert.Equal(t, "pass1", payload.DockerCredentials["registry1.com"].Password)
		assert.Equal(t, "user2", payload.DockerCredentials["registry2.com"].Username)
		assert.Equal(t, "pass2", payload.DockerCredentials["registry2.com"].Password)
	})
}

// =============================================================================
// TestResponseToTransitionPayload_Errors - Error case tests
// =============================================================================

func TestResponseToTransitionPayload_Errors(t *testing.T) {
	cfg := testConfig()

	t.Run("returns error for invalid app_key type", func(t *testing.T) {
		response := messenger.Result{
			ArgumentsKw: common.Dict{
				"app_key":      "not-a-number", // Should be uint64
				"app_name":     "my-app",
				"stage":        "PROD",
				"target_state": "RUNNING",
			},
		}

		_, err := responseToTransitionPayload(cfg, response)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "app_key")
	})

	t.Run("returns error for zero app_key", func(t *testing.T) {
		response := messenger.Result{
			ArgumentsKw: common.Dict{
				"app_key":      uint64(0),
				"app_name":     "my-app",
				"stage":        "PROD",
				"target_state": "RUNNING",
			},
		}

		_, err := responseToTransitionPayload(cfg, response)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "app_key")
	})

	t.Run("returns error for invalid app_name type", func(t *testing.T) {
		response := messenger.Result{
			ArgumentsKw: common.Dict{
				"app_key":      uint64(42),
				"app_name":     123, // Should be string
				"stage":        "PROD",
				"target_state": "RUNNING",
			},
		}

		_, err := responseToTransitionPayload(cfg, response)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "appName")
	})

	t.Run("returns error for empty app_name", func(t *testing.T) {
		response := messenger.Result{
			ArgumentsKw: common.Dict{
				"app_key":      uint64(42),
				"app_name":     "",
				"stage":        "PROD",
				"target_state": "RUNNING",
			},
		}

		_, err := responseToTransitionPayload(cfg, response)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "appName")
	})

	t.Run("returns error for invalid stage type", func(t *testing.T) {
		response := messenger.Result{
			ArgumentsKw: common.Dict{
				"app_key":      uint64(42),
				"app_name":     "my-app",
				"stage":        123, // Should be string
				"target_state": "RUNNING",
			},
		}

		_, err := responseToTransitionPayload(cfg, response)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "stage")
	})

	t.Run("returns error for empty stage", func(t *testing.T) {
		response := messenger.Result{
			ArgumentsKw: common.Dict{
				"app_key":      uint64(42),
				"app_name":     "my-app",
				"stage":        "",
				"target_state": "RUNNING",
			},
		}

		_, err := responseToTransitionPayload(cfg, response)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "stage")
	})

	t.Run("returns error for invalid target_state type", func(t *testing.T) {
		response := messenger.Result{
			ArgumentsKw: common.Dict{
				"app_key":      uint64(42),
				"app_name":     "my-app",
				"stage":        "PROD",
				"target_state": 123, // Should be string
			},
		}

		_, err := responseToTransitionPayload(cfg, response)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "requestedState")
	})

	t.Run("returns error for invalid request_update type", func(t *testing.T) {
		response := messenger.Result{
			ArgumentsKw: common.Dict{
				"app_key":        uint64(42),
				"app_name":       "my-app",
				"stage":          "PROD",
				"target_state":   "RUNNING",
				"request_update": "yes", // Should be bool
			},
		}

		_, err := responseToTransitionPayload(cfg, response)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "requestUpdate")
	})

	t.Run("returns error for invalid environment type", func(t *testing.T) {
		response := messenger.Result{
			ArgumentsKw: common.Dict{
				"app_key":      uint64(42),
				"app_name":     "my-app",
				"stage":        "PROD",
				"target_state": "RUNNING",
				"environment":  "not-a-map", // Should be map[string]interface{}
			},
		}

		_, err := responseToTransitionPayload(cfg, response)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "environment")
	})

	t.Run("returns error for invalid ports type", func(t *testing.T) {
		response := messenger.Result{
			ArgumentsKw: common.Dict{
				"app_key":      uint64(42),
				"app_name":     "my-app",
				"stage":        "PROD",
				"target_state": "RUNNING",
				"ports":        "not-an-array", // Should be []interface{}
			},
		}

		_, err := responseToTransitionPayload(cfg, response)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "ports")
	})

	t.Run("returns error for invalid docker_compose type", func(t *testing.T) {
		response := messenger.Result{
			ArgumentsKw: common.Dict{
				"app_key":        uint64(42),
				"app_name":       "my-app",
				"stage":          "PROD",
				"target_state":   "RUNNING",
				"docker_compose": "not-a-map", // Should be map[string]interface{}
			},
		}

		_, err := responseToTransitionPayload(cfg, response)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "docker compose")
	})

	t.Run("returns error for invalid docker_credentials structure", func(t *testing.T) {
		response := messenger.Result{
			ArgumentsKw: common.Dict{
				"app_key":      uint64(42),
				"app_name":     "my-app",
				"stage":        "PROD",
				"target_state": "RUNNING",
				"docker_credentials": map[string]interface{}{
					"registry.com": "not-a-map", // Should be map with username/password
				},
			},
		}

		_, err := responseToTransitionPayload(cfg, response)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "docker credentials")
	})

	t.Run("returns error for docker_credentials missing username", func(t *testing.T) {
		response := messenger.Result{
			ArgumentsKw: common.Dict{
				"app_key":      uint64(42),
				"app_name":     "my-app",
				"stage":        "PROD",
				"target_state": "RUNNING",
				"docker_credentials": map[string]interface{}{
					"registry.com": map[string]interface{}{
						"password": "pass", // Missing username
					},
				},
			},
		}

		_, err := responseToTransitionPayload(cfg, response)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "docker credentials")
	})

	t.Run("returns error for invalid requestor_account_key string", func(t *testing.T) {
		response := messenger.Result{
			ArgumentsKw: common.Dict{
				"app_key":               uint64(42),
				"app_name":              "my-app",
				"stage":                 "PROD",
				"target_state":          "RUNNING",
				"requestor_account_key": "not-a-number",
			},
		}

		_, err := responseToTransitionPayload(cfg, response)

		require.Error(t, err)
	})
}

// =============================================================================
// TestResponseToTransitionPayload_AllStates - Tests for all app states
// =============================================================================

func TestResponseToTransitionPayload_AllStates(t *testing.T) {
	cfg := testConfig()

	states := []string{
		"PRESENT",
		"REMOVED",
		"UNINSTALLED",
		"FAILED",
		"BUILT",
		"BUILDING",
		"PUBLISHING",
		"PUBLISHED",
		"DOWNLOADING",
		"STARTING",
		"STOPPING",
		"STOPPED",
		"UPDATING",
		"DELETING",
		"RUNNING",
	}

	for _, state := range states {
		t.Run("parses_"+state+"_state", func(t *testing.T) {
			response := createBasicWAMPResponse(42, "my-app", "PROD", state)

			payload, err := responseToTransitionPayload(cfg, response)

			require.NoError(t, err)
			assert.Equal(t, common.AppState(state), payload.RequestedState)
		})
	}
}

// =============================================================================
// TestResponseToTransitionPayload_NilFields - Tests for nil/missing fields
// =============================================================================

func TestResponseToTransitionPayload_NilFields(t *testing.T) {
	cfg := testConfig()

	t.Run("handles nil optional fields gracefully", func(t *testing.T) {
		response := messenger.Result{
			ArgumentsKw: common.Dict{
				"app_key":      uint64(42),
				"app_name":     "my-app",
				"stage":        "PROD",
				"target_state": "RUNNING",
				// All optional fields are nil/missing
			},
		}

		payload, err := responseToTransitionPayload(cfg, response)

		require.NoError(t, err)
		assert.Equal(t, uint64(42), payload.AppKey)
		assert.Equal(t, "my-app", payload.AppName)
		assert.Equal(t, common.Stage("PROD"), payload.Stage)
		assert.Equal(t, common.AppState("RUNNING"), payload.RequestedState)
		assert.Empty(t, payload.CurrentState)
		assert.Equal(t, uint64(0), payload.ReleaseKey)
		assert.Equal(t, uint64(0), payload.RequestorAccountKey)
		assert.False(t, payload.RequestUpdate)
		assert.False(t, payload.CancelTransition)
		assert.Nil(t, payload.EnvironmentVariables)
		assert.Nil(t, payload.Ports)
		assert.Nil(t, payload.DockerCompose)
		assert.Nil(t, payload.DockerCredentials)
	})
}

// privileged / new_privileged: absent or null (an old backend) stay nil, which
// keeps the app's privileges as they were; booleans come through as sent.
func TestResponseToTransitionPayload_Privileged(t *testing.T) {
	cfg := testConfig()

	t.Run("absent", func(t *testing.T) {
		payload, err := responseToTransitionPayload(cfg, createBasicWAMPResponse(42, "my-app", "PROD", "RUNNING"))
		require.NoError(t, err)
		assert.Nil(t, payload.Privileged)
		assert.Nil(t, payload.NewPrivileged)
	})

	t.Run("null", func(t *testing.T) {
		response := createBasicWAMPResponse(42, "my-app", "PROD", "RUNNING")
		response.ArgumentsKw["privileged"] = nil
		response.ArgumentsKw["new_privileged"] = nil

		payload, err := responseToTransitionPayload(cfg, response)
		require.NoError(t, err)
		assert.Nil(t, payload.Privileged)
		assert.Nil(t, payload.NewPrivileged)
	})

	t.Run("booleans", func(t *testing.T) {
		response := createBasicWAMPResponse(42, "my-app", "PROD", "RUNNING")
		response.ArgumentsKw["privileged"] = false
		response.ArgumentsKw["new_privileged"] = true

		payload, err := responseToTransitionPayload(cfg, response)
		require.NoError(t, err)
		require.NotNil(t, payload.Privileged)
		require.NotNil(t, payload.NewPrivileged)
		assert.False(t, *payload.Privileged)
		assert.True(t, *payload.NewPrivileged)
	})

	t.Run("anything else is refused", func(t *testing.T) {
		response := createBasicWAMPResponse(42, "my-app", "PROD", "RUNNING")
		response.ArgumentsKw["privileged"] = "false"

		_, err := responseToTransitionPayload(cfg, response)
		require.Error(t, err)
	})
}
