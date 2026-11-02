package apps

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"reagent/common"
	"reagent/config"
	"reagent/trust"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// caTestConfig builds a config whose apps directory is a temp dir, optionally
// with a CA bundle already written into it.
func caTestConfig(t *testing.T, applianceDomain string, bundleWritten bool) *config.Config {
	t.Helper()

	appsDir := t.TempDir()
	if bundleWritten {
		require.NoError(t, os.MkdirAll(trust.HostDir(appsDir), 0o755))
		require.NoError(t, os.WriteFile(trust.HostPath(appsDir), []byte("-----BEGIN CERTIFICATE-----\n"), 0o644))
	}

	return &config.Config{
		ReswarmConfig:        &config.ReswarmConfig{Environment: "production", DeviceKey: 42, ApplianceDomain: applianceDomain},
		CommandLineArguments: &config.CommandLineArguments{AppsDirectory: appsDir},
	}
}

func TestAppCABundleHostDir(t *testing.T) {
	t.Run("appliance device with a bundle", func(t *testing.T) {
		cfg := caTestConfig(t, "tls-sf015.corp.trumpf.com", true)
		assert.Equal(t, trust.HostDir(cfg.CommandLineArguments.AppsDirectory), appCABundleHostDir(cfg))
	})

	t.Run("cloud device gets nothing", func(t *testing.T) {
		cfg := caTestConfig(t, "", true)
		assert.Empty(t, appCABundleHostDir(cfg),
			"pointing SSL_CERT_FILE at a bundle replaces a container's own store; the cloud fleet must keep its images'")
	})

	t.Run("appliance device whose bundle could not be built", func(t *testing.T) {
		cfg := caTestConfig(t, "tls-sf015.corp.trumpf.com", false)
		assert.Empty(t, appCABundleHostDir(cfg), "mount only what exists")
	})

	t.Run("nil config sections", func(t *testing.T) {
		assert.Empty(t, appCABundleHostDir(nil))
		assert.Empty(t, appCABundleHostDir(&config.Config{}))
	})
}

func TestCABundleEnvironmentVariables(t *testing.T) {
	cfg := caTestConfig(t, "tls-sf015.corp.trumpf.com", true)

	envs := caBundleEnvironmentVariables(cfg)
	assert.ElementsMatch(t, []string{
		"SSL_CERT_FILE=" + trust.ContainerPath,
		"REQUESTS_CA_BUNDLE=" + trust.ContainerPath,
		"NODE_EXTRA_CA_CERTS=" + trust.ContainerPath,
		"IRONFLOCK_CA_BUNDLE=" + trust.ContainerPath,
	}, envs)

	assert.Nil(t, caBundleEnvironmentVariables(caTestConfig(t, "", true)))
}

// The failure this fixes: an app container on a corporate-cert appliance
// cannot verify wss://ws.<appliance domain> and reconnects forever.
func TestBuildDefaultEnvironmentVariablesCarriesTheCABundle(t *testing.T) {
	app := &common.App{AppKey: 7, AppName: "myapp"}

	applianceEnv := buildDefaultEnvironmentVariables(
		caTestConfig(t, "tls-sf015.corp.trumpf.com", true), common.TransitionPayload{}, common.PROD, app, "")
	assert.Contains(t, applianceEnv, "SSL_CERT_FILE="+trust.ContainerPath)
	assert.Contains(t, applianceEnv, "REQUESTS_CA_BUNDLE="+trust.ContainerPath)

	cloudEnv := buildDefaultEnvironmentVariables(
		caTestConfig(t, "", true), common.TransitionPayload{}, common.PROD, app, "")
	for _, env := range cloudEnv {
		assert.NotContains(t, env, "SSL_CERT_FILE=")
		assert.NotContains(t, env, "REQUESTS_CA_BUNDLE=")
	}
}

// An app may still override the trust store it uses: payload env is appended
// after the defaults and SSL_CERT_FILE is not a reserved name.
func TestCABundleEnvironmentVariablesAreOverridable(t *testing.T) {
	defaults := []string{"SSL_CERT_FILE=" + trust.ContainerPath}
	merged := buildProdEnvironmentVariables(defaults, map[string]interface{}{"SSL_CERT_FILE": map[string]interface{}{"value": "/app/custom.pem"}})

	require.Len(t, merged, 2)
	assert.Equal(t, "SSL_CERT_FILE=/app/custom.pem", merged[len(merged)-1],
		"docker takes the last occurrence, so the app's own value must come last")
}

func TestAddComposeCABundleMount(t *testing.T) {
	hostDir := filepath.FromSlash("/apps/certs")
	expected := hostDir + ":" + trust.ContainerDir + ":ro"

	t.Run("adds when absent", func(t *testing.T) {
		service := map[string]interface{}{}
		addComposeCABundleMount(service, hostDir)
		assert.Equal(t, []interface{}{expected}, service["volumes"])
	})

	t.Run("appends to authored volumes", func(t *testing.T) {
		service := map[string]interface{}{"volumes": []interface{}{"./data:/data"}}
		addComposeCABundleMount(service, hostDir)
		assert.Equal(t, []interface{}{"./data:/data", expected}, service["volumes"])
	})

	t.Run("respects an authored mount at the same target", func(t *testing.T) {
		authored := []interface{}{"./certs:" + trust.ContainerDir}
		service := map[string]interface{}{"volumes": authored}
		addComposeCABundleMount(service, hostDir)
		assert.Equal(t, authored, service["volumes"])
	})

	t.Run("respects an authored long-syntax mount", func(t *testing.T) {
		authored := []interface{}{map[string]interface{}{
			"type": "bind", "source": "./certs", "target": trust.ContainerDir,
		}}
		service := map[string]interface{}{"volumes": authored}
		addComposeCABundleMount(service, hostDir)
		assert.Equal(t, authored, service["volumes"])
	})

	t.Run("no-op for a device that provides no bundle", func(t *testing.T) {
		service := map[string]interface{}{}
		addComposeCABundleMount(service, "")
		assert.Nil(t, service["volumes"])
	})
}

func TestComputeMountsCarriesTheCABundle(t *testing.T) {
	cfg := caTestConfig(t, "tls-sf015.corp.trumpf.com", true)
	cfg.CommandLineArguments.AppsSharedDir = filepath.Join(cfg.CommandLineArguments.AppsDirectory, "shared")

	mounts, err := computeMounts(common.PROD, "myapp", cfg, true)
	require.NoError(t, err)

	var found bool
	for _, mount := range mounts {
		if mount.Target == trust.ContainerDir {
			found = true
			assert.Equal(t, trust.HostDir(cfg.CommandLineArguments.AppsDirectory), mount.Source)
			assert.True(t, mount.ReadOnly, "an app must not be able to rewrite the device's trust set")
		}
	}
	assert.True(t, found, "single-container apps need the bundle too")

	cloudCfg := caTestConfig(t, "", true)
	cloudCfg.CommandLineArguments.AppsSharedDir = filepath.Join(cloudCfg.CommandLineArguments.AppsDirectory, "shared")
	cloudMounts, err := computeMounts(common.PROD, "myapp", cloudCfg, true)
	require.NoError(t, err)
	for _, mount := range cloudMounts {
		assert.NotEqual(t, trust.ContainerDir, mount.Target)
	}
}

// The failing case in the field was a COMPOSE app: SetupComposeFiles is what
// renders the definition every service actually runs with, so the mount has to
// land there, on every service, alongside the authored volumes.
func TestSetupComposeFilesMountsTheCABundleOnEveryService(t *testing.T) {
	am, mockContainer, _, _, _, cfg := amHarness(t)

	cfg.ReswarmConfig.ApplianceDomain = "tls-sf015.corp.trumpf.com"
	require.NoError(t, os.MkdirAll(trust.HostDir(cfg.CommandLineArguments.AppsDirectory), 0o755))
	require.NoError(t, os.WriteFile(
		trust.HostPath(cfg.CommandLineArguments.AppsDirectory),
		[]byte("-----BEGIN CERTIFICATE-----\n"), 0o644))

	mockContainer.EXPECT().GetComposePublishedPorts(mock.Anything, mock.Anything).
		Return(map[string]uint64{}, nil).Maybe()

	app := &common.App{AppKey: 6, AppName: "trumpfqds", Stage: common.PROD}
	payload := common.TransitionPayload{
		Stage:   common.PROD,
		AppKey:  6,
		AppName: "trumpfqds",
		DockerCompose: map[string]interface{}{
			"services": map[string]interface{}{
				"collector": map[string]interface{}{"image": "registry.test/collector:1"},
				"qds":       map[string]interface{}{"image": "registry.test/qds:1", "volumes": []interface{}{"./data:/data"}},
			},
		},
	}

	composePath, err := am.StateMachine.SetupComposeFiles(payload, app, false)
	require.NoError(t, err)

	rendered, err := os.ReadFile(composePath)
	require.NoError(t, err)

	var compose map[string]interface{}
	require.NoError(t, json.Unmarshal(rendered, &compose))

	expected := trust.HostDir(cfg.CommandLineArguments.AppsDirectory) + ":" + trust.ContainerDir + ":ro"
	services := compose["services"].(map[string]interface{})
	require.Len(t, services, 2)

	for name, raw := range services {
		volumes, ok := raw.(map[string]interface{})["volumes"].([]interface{})
		require.True(t, ok, "service %s lost its volumes", name)
		assert.Contains(t, volumes, expected, "service %s cannot verify the appliance without the bundle", name)
	}

	authored := services["qds"].(map[string]interface{})["volumes"].([]interface{})
	assert.Contains(t, authored, "./data:/data", "authored volumes must survive")
}
