package apps

import (
	"os"
	"path/filepath"
	"reagent/common"
	"reagent/config"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func envFilesTestConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		CommandLineArguments: &config.CommandLineArguments{AppsDirectory: t.TempDir()},
		ReswarmConfig:        &config.ReswarmConfig{},
	}
}

func readEnvFile(t *testing.T, cfg *config.Config, name string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(appEnvFilesHostDir(cfg, common.PROD, "MyApp"), name+".txt"))
	if err != nil {
		require.True(t, os.IsNotExist(err))
		return "", false
	}
	return string(data), true
}

// The live-refresh channel: {NAME}.txt and {NAME}_CLOUD.txt track the current
// tunnel ports; a cloud port that disappeared removes its file so SDKs fall
// back to the LAN URL.
func TestRefreshRemotePortEnvFiles(t *testing.T) {
	cfg := envFilesTestConfig(t)

	rules := []common.PortForwardRule{
		{RuleName: "vpn", Port: 51820, Protocol: "udp", RemotePortEnvironment: "WG_PORT", RemotePort: 30022, CloudRemotePort: 31099},
		{RuleName: "web", Port: 8080, Protocol: "http"}, // no remote_port_environment — ignored
	}

	refreshRemotePortEnvFiles(cfg, common.PROD, "MyApp", rules)

	value, ok := readEnvFile(t, cfg, "WG_PORT")
	assert.True(t, ok)
	assert.Equal(t, "30022", value)

	value, ok = readEnvFile(t, cfg, "WG_PORT_CLOUD")
	assert.True(t, ok)
	assert.Equal(t, "31099", value)

	// Cloud port changes → file content follows.
	rules[0].CloudRemotePort = 31100
	refreshRemotePortEnvFiles(cfg, common.PROD, "MyApp", rules)
	value, _ = readEnvFile(t, cfg, "WG_PORT_CLOUD")
	assert.Equal(t, "31100", value)

	// Forwarding off → cloud file removed, base port file stays.
	rules[0].CloudRemotePort = 0
	refreshRemotePortEnvFiles(cfg, common.PROD, "MyApp", rules)
	_, ok = readEnvFile(t, cfg, "WG_PORT_CLOUD")
	assert.False(t, ok)
	_, ok = readEnvFile(t, cfg, "WG_PORT")
	assert.True(t, ok)
}

// A development run announces only the tunnel standing right now: a rule
// without one removes its port file. A PROD install keeps its file, as its
// rule's port stays its address.
func TestRefreshRemotePortEnvFilesDevRemovesGonePort(t *testing.T) {
	cfg := envFilesTestConfig(t)
	read := func(stage common.Stage, name string) bool {
		_, err := os.Stat(filepath.Join(appEnvFilesHostDir(cfg, stage, "MyApp"), name+".txt"))
		return err == nil
	}
	up := []common.PortForwardRule{{RuleName: "media", Port: 10000, Protocol: "udp", RemotePortEnvironment: "MEDIA_PORT", RemotePort: 30400}}
	gone := []common.PortForwardRule{{RuleName: "media", Port: 10000, Protocol: "udp", RemotePortEnvironment: "MEDIA_PORT"}}

	for _, stage := range []common.Stage{common.DEV, common.PROD} {
		refreshRemotePortEnvFiles(cfg, stage, "MyApp", up)
		assert.True(t, read(stage, "REMOTE_PORT_FOR_10000"), stage)
		assert.True(t, read(stage, "MEDIA_PORT"), stage)
		refreshRemotePortEnvFiles(cfg, stage, "MyApp", gone)
	}
	assert.False(t, read(common.DEV, "REMOTE_PORT_FOR_10000"))
	assert.False(t, read(common.DEV, "MEDIA_PORT"))
	assert.True(t, read(common.PROD, "REMOTE_PORT_FOR_10000"))
	assert.True(t, read(common.PROD, "MEDIA_PORT"))
}

// Apps without remote-port env rules must not even get an env dir created.
func TestRefreshRemotePortEnvFilesNoRelevantRules(t *testing.T) {
	cfg := envFilesTestConfig(t)

	refreshRemotePortEnvFiles(cfg, common.PROD, "MyApp", []common.PortForwardRule{
		{RuleName: "web", Port: 8080, Protocol: "http"},
	})

	_, err := os.Stat(appEnvFilesHostDir(cfg, common.PROD, "MyApp"))
	assert.True(t, os.IsNotExist(err))
}

// The env mirror must be readable for a container running as any user, also
// where an older agent made it root-only (0700 dir, 0600 files) — including a
// file the agent no longer rewrites. The stale DEVICE_SECRET.txt is removed,
// never opened up.
func TestEnvMirrorReadableByAnyUser(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes")
	}

	for name, write := range map[string]func(cfg *config.Config){
		"start-time mirror": func(cfg *config.Config) {
			envDir := appEnvFilesHostDir(cfg, common.PROD, "MyApp")
			require.NoError(t, writeEnvironmentVariablesToFiles(filepath.Dir(envDir), []string{"FOO=bar"}))
		},
		"live port refresh": func(cfg *config.Config) {
			refreshRemotePortEnvFiles(cfg, common.PROD, "MyApp", []common.PortForwardRule{
				{RuleName: "vpn", Port: 51820, Protocol: "udp", RemotePort: 30022},
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := envFilesTestConfig(t)
			envDir := appEnvFilesHostDir(cfg, common.PROD, "MyApp")
			require.NoError(t, os.MkdirAll(envDir, 0700))
			require.NoError(t, os.Chmod(envDir, 0700))
			require.NoError(t, os.WriteFile(filepath.Join(envDir, "APP_AUTH_SECRET.txt"), []byte("s"), 0600))
			require.NoError(t, os.WriteFile(filepath.Join(envDir, "DEVICE_SECRET.txt"), []byte("d"), 0600))

			write(cfg)

			info, err := os.Stat(envDir)
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0755), info.Mode().Perm())

			entries, err := os.ReadDir(envDir)
			require.NoError(t, err)
			assert.Len(t, entries, 2, "the old file plus the one just written")
			for _, entry := range entries {
				info, err := entry.Info()
				require.NoError(t, err)
				assert.Equalf(t, os.FileMode(0644), info.Mode().Perm(), "%s", entry.Name())
			}

			_, err = os.Stat(filepath.Join(envDir, "DEVICE_SECRET.txt"))
			assert.True(t, os.IsNotExist(err))
		})
	}
}

// /data must be writable for a container running as any user, also where an
// older agent created it 0755; what the app keeps inside keeps its modes.
func TestAppDataDirWritableByAnyUser(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes")
	}

	cfg := envFilesTestConfig(t)
	dataDir := filepath.Dir(appEnvFilesHostDir(cfg, common.PROD, "MyApp"))
	require.NoError(t, os.MkdirAll(dataDir, 0755))
	require.NoError(t, os.Chmod(dataDir, 0755))
	keyFile := filepath.Join(dataDir, "key.pem")
	require.NoError(t, os.WriteFile(keyFile, []byte("k"), 0600))

	mounts, err := computeMounts(common.PROD, "MyApp", cfg)
	require.NoError(t, err)

	var source string
	for _, m := range mounts {
		if m.Target == "/data" {
			source = m.Source
			assert.False(t, m.ReadOnly)
		}
	}
	require.NotEmpty(t, source)

	info, err := os.Stat(source)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0777), info.Mode().Perm())

	info, err = os.Stat(keyFile)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm(), "the app's own files keep their modes")
}

func TestAddComposeEnvFilesMount(t *testing.T) {
	const hostDir = "/apps/prod/myapp/env"
	expected := hostDir + ":/data/env:ro"

	t.Run("adds when absent", func(t *testing.T) {
		service := map[string]interface{}{}
		addComposeEnvFilesMount(service, hostDir)
		assert.Equal(t, []interface{}{expected}, service["volumes"])
	})

	t.Run("appends to authored volumes", func(t *testing.T) {
		service := map[string]interface{}{"volumes": []interface{}{"mydata:/data"}}
		addComposeEnvFilesMount(service, hostDir)
		assert.Equal(t, []interface{}{"mydata:/data", expected}, service["volumes"])
	})

	t.Run("respects authored /data/env mount", func(t *testing.T) {
		service := map[string]interface{}{"volumes": []interface{}{"own:/data/env"}}
		addComposeEnvFilesMount(service, hostDir)
		assert.Equal(t, []interface{}{"own:/data/env"}, service["volumes"])
	})

	t.Run("respects long-syntax /data/env mount", func(t *testing.T) {
		authored := map[string]interface{}{"type": "bind", "source": "/x", "target": "/data/env"}
		service := map[string]interface{}{"volumes": []interface{}{authored}}
		addComposeEnvFilesMount(service, hostDir)
		assert.Equal(t, []interface{}{authored}, service["volumes"])
	})
}
