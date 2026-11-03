package container

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reagent/common"
	"strings"
	"sync"
)

// The docker CLI keeps a registry login in a credential helper when one is
// configured (Docker Desktop sets `credsStore: desktop`) or found on PATH
// (docker-credential-wincred on Windows), and in config.json otherwise. On
// Windows both helpers store into Credential Manager, which rejects the
// agent's login: the registry token used as the username is an RS256 JWT well
// over a thousand characters ("The array bounds are invalid."). Authentication
// succeeds; storing the result fails, and with it the login, every image pull
// and every app install on the device. So when the helper refuses a login,
// the agent opts that registry out of the helper in the config the CLI reads —
// whoever the agent runs as: LocalSystem for the service, the signed-in user
// when FlockFlasher or a console starts it — and logs in again.

// credentialConfigMu serializes the agent's read-modify-write of the CLI's
// config.json across concurrent logins.
var credentialConfigMu sync.Mutex

// credentialHelperRefused reports whether `docker login` failed at storing
// the credential rather than at authenticating.
func credentialHelperRefused(output string) bool {
	return strings.Contains(strings.ToLower(output), "error saving credentials")
}

// dockerCLIConfigPath is the config.json the docker CLI reads, resolved the
// way the CLI resolves it: $DOCKER_CONFIG, else .docker in the home directory
// (%USERPROFILE% on Windows, the systemprofile folder under LocalSystem).
func dockerCLIConfigPath() (string, error) {
	if dir := os.Getenv("DOCKER_CONFIG"); dir != "" {
		return filepath.Join(dir, "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".docker", "config.json"), nil
}

// credentialStoreKey is the name the docker CLI looks a registry's credential
// helper up by: the bare host, except Docker Hub, whose logins it files under
// the index server URL.
func credentialStoreKey(registryURL string) string {
	host := common.NormalizeRegistryHost(registryURL)
	if host == "docker.io" {
		return "https://index.docker.io/v1/"
	}
	return host
}

// useFileCredentialStore opts registryURL out of the credential helper in the
// CLI's config.json, so its next login is kept in that file. switched is
// false when the file already names a helper for the registry: an operator's
// explicit choice is never overridden.
func useFileCredentialStore(registryURL string) (configPath string, switched bool, err error) {
	configPath, err = dockerCLIConfigPath()
	if err != nil {
		return "", false, err
	}

	credentialConfigMu.Lock()
	defer credentialConfigMu.Unlock()

	current, err := os.ReadFile(configPath) //nolint:gosec // G304: the docker CLI's own config path
	if err != nil && !os.IsNotExist(err) {
		return configPath, false, err
	}

	merged, changed, err := mergeCredentialHelperOptOut(current, []string{credentialStoreKey(registryURL)})
	if err != nil {
		return configPath, false, fmt.Errorf("%s: %w", configPath, err)
	}
	if !changed {
		return configPath, false, nil
	}

	err = writeConfigAtomic(configPath, merged)
	if err != nil {
		return configPath, false, err
	}
	return configPath, true, nil
}

// mergeCredentialHelperOptOut returns dockerConfigJSON with an empty
// credential-helper entry recorded for each registry, which makes the docker
// CLI use its plaintext file store for exactly those registries. Clearing
// `credsStore` would not do: the CLI auto-detects `wincred` whenever its
// helper binary exists, so only a per-registry entry opts out. Every other key
// survives.
func mergeCredentialHelperOptOut(dockerConfigJSON []byte, registries []string) (merged []byte, changed bool, err error) {
	dockerCfg := map[string]interface{}{}
	if len(bytes.TrimSpace(dockerConfigJSON)) > 0 {
		err = json.Unmarshal(dockerConfigJSON, &dockerCfg)
		if err != nil {
			return nil, false, fmt.Errorf("existing docker config.json is not valid JSON: %w", err)
		}
	}

	helpers, _ := dockerCfg["credHelpers"].(map[string]interface{})
	if helpers == nil {
		helpers = map[string]interface{}{}
	}

	for _, registry := range registries {
		if registry == "" {
			continue
		}
		// Already opted out, or an operator's deliberate helper choice.
		if _, ok := helpers[registry]; ok {
			continue
		}
		helpers[registry] = ""
		changed = true
	}
	if !changed {
		return dockerConfigJSON, false, nil
	}
	dockerCfg["credHelpers"] = helpers

	merged, err = json.MarshalIndent(dockerCfg, "", "  ")
	if err != nil {
		return nil, false, err
	}
	return append(merged, '\n'), true, nil
}

// writeConfigAtomic replaces path with data through a temp file in the same
// folder and a rename, so the docker CLI never reads a half-written config.
func writeConfigAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	err := os.MkdirAll(dir, 0700)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".config.json-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	_, err = tmp.Write(data)
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmpName, path)
	}
	if err != nil {
		_ = os.Remove(tmpName)
	}
	return err
}
