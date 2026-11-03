package container

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reagent/common"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The registry token is used as the docker *username* and is far longer than
// Windows Credential Manager accepts, so the registry must be opted out of the
// credential helper or every login fails at the store step.
func TestMergeCredentialHelperOptOutCreatesEntry(t *testing.T) {
	merged, changed, err := mergeCredentialHelperOptOut(nil, []string{"136.230.111.59:15001"})
	require.NoError(t, err)
	assert.True(t, changed)

	var cfg map[string]interface{}
	require.NoError(t, json.Unmarshal(merged, &cfg))
	helpers := cfg["credHelpers"].(map[string]interface{})
	assert.Equal(t, "", helpers["136.230.111.59:15001"])
}

// Docker Desktop machines have a config.json already; auths, contexts and any
// other keys must survive untouched.
func TestMergeCredentialHelperOptOutPreservesConfig(t *testing.T) {
	existing := []byte(`{"auths":{"registry.example":{"auth":"x"}},"currentContext":"desktop-linux"}`)
	merged, changed, err := mergeCredentialHelperOptOut(existing, []string{"136.230.111.59:15001"})
	require.NoError(t, err)
	assert.True(t, changed)

	var cfg map[string]interface{}
	require.NoError(t, json.Unmarshal(merged, &cfg))
	assert.Contains(t, cfg, "auths")
	assert.Equal(t, "desktop-linux", cfg["currentContext"])
	assert.Equal(t, "", cfg["credHelpers"].(map[string]interface{})["136.230.111.59:15001"])
}

// An operator who deliberately configured a helper for this registry keeps it.
func TestMergeCredentialHelperOptOutKeepsOperatorChoice(t *testing.T) {
	existing := []byte(`{"credHelpers":{"136.230.111.59:15001":"wincred"}}`)
	merged, changed, err := mergeCredentialHelperOptOut(existing, []string{"136.230.111.59:15001"})
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, existing, merged)
}

func TestMergeCredentialHelperOptOutIdempotent(t *testing.T) {
	existing := []byte(`{"credHelpers":{"136.230.111.59:15001":""}}`)
	_, changed, err := mergeCredentialHelperOptOut(existing, []string{"136.230.111.59:15001"})
	require.NoError(t, err)
	assert.False(t, changed)
}

func TestMergeCredentialHelperOptOutRejectsMalformed(t *testing.T) {
	_, _, err := mergeCredentialHelperOptOut([]byte(`{"credHelpers":`), []string{"x:1"})
	assert.Error(t, err)
}

func TestDockerCLIConfigPathFollowsDockerConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", dir)

	path, err := dockerCLIConfigPath()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "config.json"), path)

	t.Setenv("DOCKER_CONFIG", "")
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	path, err = dockerCLIConfigPath()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, ".docker", "config.json"), path)
}

func TestCredentialStoreKey(t *testing.T) {
	assert.Equal(t, "registry.ironflock.com", credentialStoreKey("https://Registry.IronFlock.com/"))
	assert.Equal(t, "localhost:15001", credentialStoreKey("localhost:15001"))
	assert.Equal(t, "https://index.docker.io/v1/", credentialStoreKey("docker.io"))
}

func TestCredentialHelperRefused(t *testing.T) {
	assert.True(t, credentialHelperRefused("error saving credentials: error storing credentials - err: exit status 1, out: `The array bounds are invalid.`"))
	assert.True(t, credentialHelperRefused("Error saving credentials: error storing credentials - err: exit status 1"))
	assert.False(t, credentialHelperRefused(`Error response from daemon: Get "https://registry.ironflock.com/v2/": unauthorized`))
}

// fakeDockerLogin puts a `docker` on PATH that answers `docker login` like a
// CLI with a credential helper that cannot hold the registry token: the login
// fails at the store step until config.json opts registry.ironflock.com out
// of the helper. mode "unauthorized" fails authentication instead. It returns
// the file that counts the invocations.
func fakeDockerLogin(t *testing.T, mode string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake docker CLI is a shell script")
	}

	binDir := t.TempDir()
	script := `#!/bin/sh
echo login >> "$FAKE_DOCKER_CALLS"
cat > /dev/null
if [ "$FAKE_DOCKER_MODE" = unauthorized ]; then
  echo 'Error response from daemon: Get "https://registry.ironflock.com/v2/": unauthorized'
  exit 1
fi
if grep -q '"registry.ironflock.com": ""' "$DOCKER_CONFIG/config.json" 2>/dev/null; then
  echo 'Login Succeeded'
  exit 0
fi
echo 'error saving credentials: error storing credentials - err: exit status 1, out: The array bounds are invalid.'
exit 1
`
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "docker"), []byte(script), 0700)) //nolint:gosec // G306: test fake must be executable

	calls := filepath.Join(t.TempDir(), "calls")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_DOCKER_CALLS", calls)
	t.Setenv("FAKE_DOCKER_MODE", mode)
	return calls
}

func countCalls(t *testing.T, callsFile string) int {
	t.Helper()
	raw, err := os.ReadFile(callsFile) //nolint:gosec // G304: test temp file
	require.NoError(t, err)
	return strings.Count(string(raw), "login")
}

var storeCredentials = map[string]common.DockerCredential{
	"registry.ironflock.com": {Username: "store-jwt", Password: "device-secret"},
}

// KRIAA, 2026-11-03: the agent ran under FlockFlasher as the signed-in user,
// whose config.json Docker Desktop had set to `credsStore: desktop`, and every
// app install failed with the helper's "The array bounds are invalid.".
func TestHandleRegistryLoginsMovesRefusedLoginToFileStore(t *testing.T) {
	calls := fakeDockerLogin(t, "")
	configDir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", configDir)
	configPath := filepath.Join(configDir, "config.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{"auths":{"https://index.docker.io/v1/":{}},"credsStore":"desktop","currentContext":"desktop-linux"}`), 0600))

	err := (&Docker{}).HandleRegistryLogins(storeCredentials)
	require.NoError(t, err)
	assert.Equal(t, 2, countCalls(t, calls), "one refused login, one after the opt-out")

	raw, err := os.ReadFile(configPath) //nolint:gosec // G304: test temp file
	require.NoError(t, err)
	var cfg map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &cfg))
	assert.Equal(t, "desktop", cfg["credsStore"], "the user's own helper stays for every other registry")
	assert.Equal(t, "desktop-linux", cfg["currentContext"])
	assert.Contains(t, cfg["auths"], "https://index.docker.io/v1/")
	assert.Equal(t, map[string]interface{}{"registry.ironflock.com": ""}, cfg["credHelpers"])

	// The next login finds the opt-out and succeeds at once.
	require.NoError(t, (&Docker{}).HandleRegistryLogins(storeCredentials))
	assert.Equal(t, 3, countCalls(t, calls))
}

// A fresh profile (the LocalSystem service before its first login) has no
// config.json yet.
func TestHandleRegistryLoginsCreatesMissingConfig(t *testing.T) {
	calls := fakeDockerLogin(t, "")
	configDir := filepath.Join(t.TempDir(), ".docker")
	t.Setenv("DOCKER_CONFIG", configDir)

	require.NoError(t, (&Docker{}).HandleRegistryLogins(storeCredentials))
	assert.Equal(t, 2, countCalls(t, calls))

	raw, err := os.ReadFile(filepath.Join(configDir, "config.json")) //nolint:gosec // G304: test temp file
	require.NoError(t, err)
	assert.JSONEq(t, `{"credHelpers":{"registry.ironflock.com":""}}`, string(raw))
}

// A helper the operator named for the registry is never overridden: the
// login fails with the CLI's own error and is not retried.
func TestHandleRegistryLoginsKeepsOperatorHelper(t *testing.T) {
	calls := fakeDockerLogin(t, "")
	configDir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", configDir)
	configPath := filepath.Join(configDir, "config.json")
	original := []byte(`{"credHelpers":{"registry.ironflock.com":"wincred"}}`)
	require.NoError(t, os.WriteFile(configPath, original, 0600))

	err := (&Docker{}).HandleRegistryLogins(storeCredentials)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "error saving credentials")
	assert.Equal(t, 1, countCalls(t, calls))

	raw, err := os.ReadFile(configPath) //nolint:gosec // G304: test temp file
	require.NoError(t, err)
	assert.Equal(t, original, raw)
}

// A rejected credential is not a store failure: the config stays untouched.
func TestHandleRegistryLoginsLeavesConfigOnAuthFailure(t *testing.T) {
	calls := fakeDockerLogin(t, "unauthorized")
	configDir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", configDir)

	err := (&Docker{}).HandleRegistryLogins(storeCredentials)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unauthorized")
	assert.Equal(t, 1, countCalls(t, calls))
	assert.NoFileExists(t, filepath.Join(configDir, "config.json"))
}
