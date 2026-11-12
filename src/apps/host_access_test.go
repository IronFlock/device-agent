package apps

import (
	"reagent/common"
	"reagent/config"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAppEndpointURL(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		expected string
	}{
		{name: "localhost rewritten", in: "ws://localhost:8080/ws-re-dev", expected: "ws://host.docker.internal:8080/ws-re-dev"},
		{name: "loopback ip rewritten", in: "wss://127.0.0.1:8080/ws-re-dev", expected: "wss://host.docker.internal:8080/ws-re-dev"},
		{name: "ipv6 loopback rewritten", in: "ws://[::1]:8080/ws", expected: "ws://host.docker.internal:8080/ws"},
		{name: "no port", in: "ws://localhost/ws", expected: "ws://host.docker.internal/ws"},
		{name: "public host untouched", in: "wss://cbw.ironflock.com/ws-re-dev", expected: "wss://cbw.ironflock.com/ws-re-dev"},
		{name: "lan ip untouched", in: "wss://192.168.0.21:18080/ws-re-dev", expected: "wss://192.168.0.21:18080/ws-re-dev"},
		{name: "appliance host untouched", in: "wss://appliance.local:18080/ws-re-dev", expected: "wss://appliance.local:18080/ws-re-dev"},
		{name: "empty untouched", in: "", expected: ""},
		{name: "garbage untouched", in: "::not a url::", expected: "::not a url::"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, appEndpointURL(tc.in))
		})
	}
}

func TestAddComposeExtraHost(t *testing.T) {
	t.Run("adds when absent", func(t *testing.T) {
		service := map[string]interface{}{}
		addComposeExtraHost(service)
		assert.Equal(t, []interface{}{hostGatewayEntry}, service["extra_hosts"])
	})

	t.Run("appends to authored list", func(t *testing.T) {
		service := map[string]interface{}{"extra_hosts": []interface{}{"db.local:10.0.0.5"}}
		addComposeExtraHost(service)
		assert.Equal(t, []interface{}{"db.local:10.0.0.5", hostGatewayEntry}, service["extra_hosts"])
	})

	t.Run("respects authored mapping of the same name", func(t *testing.T) {
		service := map[string]interface{}{"extra_hosts": []interface{}{"host.docker.internal:172.17.0.1"}}
		addComposeExtraHost(service)
		assert.Equal(t, []interface{}{"host.docker.internal:172.17.0.1"}, service["extra_hosts"])
	})

	t.Run("map form", func(t *testing.T) {
		service := map[string]interface{}{"extra_hosts": map[string]interface{}{"db.local": "10.0.0.5"}}
		addComposeExtraHost(service)
		assert.Equal(t, map[string]interface{}{"db.local": "10.0.0.5", "host.docker.internal": "host-gateway"}, service["extra_hosts"])
	})
}

func TestTunnelDomainForApps(t *testing.T) {
	cases := []struct {
		name     string
		reswarm  config.ReswarmConfig
		expected string
	}{
		{name: "appliance domain wins", reswarm: config.ReswarmConfig{ApplianceDomain: "tunnel.factory.example", Environment: "production"}, expected: "tunnel.factory.example"},
		{name: "production cloud edge", reswarm: config.ReswarmConfig{Environment: "production"}, expected: "app.ironflock.com"},
		{name: "test cloud edge", reswarm: config.ReswarmConfig{Environment: "test"}, expected: "app.ironflock.dev"},
		{name: "local", reswarm: config.ReswarmConfig{Environment: "local"}, expected: "localhost"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reswarm := tc.reswarm
			cfg := &config.Config{ReswarmConfig: &reswarm}
			assert.Equal(t, tc.expected, tunnelDomainForApps(cfg))
		})
	}
}

// The env the agent hands every app: TUNNEL_DOMAIN always, INSTANCE_KEY only
// on instance devices — what SDK getRemoteAccessUrlForPort composes URLs from.
func TestBuildDefaultEnvironmentVariablesTunnelRouting(t *testing.T) {
	cfg := &config.Config{ReswarmConfig: &config.ReswarmConfig{Environment: "production", DeviceKey: 42}}
	app := &common.App{AppKey: 7, AppName: "myapp"}

	cloudEnv := buildDefaultEnvironmentVariables(cfg, common.TransitionPayload{}, common.PROD, app, "")
	assert.Contains(t, cloudEnv, "TUNNEL_DOMAIN=app.ironflock.com")
	for _, env := range cloudEnv {
		assert.NotContains(t, env, "INSTANCE_KEY=", "cloud devices must not get an instance key")
	}

	instanceEnv := buildDefaultEnvironmentVariables(cfg, common.TransitionPayload{InstanceKey: 5}, common.PROD, app, "")
	assert.Contains(t, instanceEnv, "INSTANCE_KEY=5")
}

// DEVICE_URL is the device's page in Studio, under /<user>/projects/ (Studio
// moved it from /<user>/swarms/ in 4.0.43 and still opens the old address).
func TestBuildDefaultEnvironmentVariablesDeviceURL(t *testing.T) {
	cfg := &config.Config{ReswarmConfig: &config.ReswarmConfig{
		Environment: "production", DeviceKey: 42, ReswarmBaseURL: "https://studio.ironflock.com",
		SwarmOwnerName: "alice", SwarmName: "plant", Name: "press-1",
	}}
	app := &common.App{AppKey: 7, AppName: "myapp"}

	env := buildDefaultEnvironmentVariables(cfg, common.TransitionPayload{}, common.PROD, app, "")
	assert.Contains(t, env, "RESWARM_URL=https://studio.ironflock.com")
	assert.Contains(t, env, "STUDIO_URL=https://studio.ironflock.com")
	assert.Contains(t, env, "DEVICE_URL=https://studio.ironflock.com/alice/projects/plant/settings/devices/press-1")

	cfg.ReswarmConfig.ReswarmBaseURL = ""
	for _, e := range buildDefaultEnvironmentVariables(cfg, common.TransitionPayload{}, common.PROD, app, "") {
		assert.NotContains(t, e, "DEVICE_URL=", "no Studio address, no device link")
		assert.NotContains(t, e, "STUDIO_URL=", "never blanked")
	}
}

// The device's realm1 credential must never reach an app container: realm1's
// swarm_device role is allow-all, so an app holding it could act as its own
// device. Per-app credentials replace it — and are omitted entirely (never
// blanked) while the per-device key is unavailable, so the SDK's legacy
// fallback keeps the app connected.
func TestBuildDefaultEnvironmentVariablesAppCredential(t *testing.T) {
	cfg := &config.Config{ReswarmConfig: &config.ReswarmConfig{
		Environment: "production", DeviceKey: 42, SerialNumber: "serial-abc", Secret: "device-secret",
	}}
	app := &common.App{AppKey: 7, AppName: "myapp"}
	payload := common.TransitionPayload{AppKey: 7, Stage: common.PROD, AppCredEpoch: 3}

	noKey := buildDefaultEnvironmentVariables(cfg, payload, common.PROD, app, "")
	for _, env := range noKey {
		assert.NotContains(t, env, "DEVICE_SECRET=", "the device credential must not be injected")
		assert.NotContains(t, env, "APP_AUTH_ID=", "no credential without the per-device key")
		assert.NotContains(t, env, "APP_AUTH_SECRET=", "no credential without the per-device key")
	}

	withKey := buildDefaultEnvironmentVariables(cfg, payload, common.PROD, app, "test-cred-key")
	assert.Contains(t, withKey, "APP_AUTH_ID=app-7-prod-e3@serial-abc")
	assert.Contains(t, withKey,
		"APP_AUTH_SECRET="+appCredentialSecret("test-cred-key", "serial-abc", 7, common.PROD, 3))
	for _, env := range withKey {
		assert.NotContains(t, env, "DEVICE_SECRET=")
	}

	// An absent epoch is epoch 1 by definition (backend predating the column).
	noEpoch := buildDefaultEnvironmentVariables(
		cfg, common.TransitionPayload{AppKey: 7, Stage: common.PROD}, common.PROD, app, "test-cred-key")
	assert.Contains(t, noEpoch, "APP_AUTH_ID=app-7-prod-e1@serial-abc")
}

// An app must not be able to volunteer a foreign identity: Docker takes the
// last occurrence of a duplicate env var, and payload env is appended after
// the defaults.
func TestBuildProdEnvironmentVariablesRejectsReservedOverrides(t *testing.T) {
	defaults := []string{"APP_AUTH_ID=app-7-prod-e1@serial", "APP_AUTH_SECRET=real"}
	// payload env arrives as {name: {"value": ...}} from the sync response
	result := buildProdEnvironmentVariables(defaults, map[string]interface{}{
		"APP_AUTH_ID":     map[string]interface{}{"value": "app-9-prod-e1@serial"},
		"APP_AUTH_SECRET": map[string]interface{}{"value": "forged"},
		"MY_OWN_VAR":      map[string]interface{}{"value": "kept"},
	})

	assert.Contains(t, result, "MY_OWN_VAR=kept")
	for _, env := range result {
		assert.NotEqual(t, "APP_AUTH_ID=app-9-prod-e1@serial", env)
		assert.NotEqual(t, "APP_AUTH_SECRET=forged", env)
	}
}
