package apps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"reagent/common"
	containerpkg "reagent/container"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Image pins: a release names each image by the manifest digest the store
// held when the release was created (x-image-digest); a device installs and
// runs exactly that manifest, whatever the tag points to later.
// =============================================================================

const (
	pinDigestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pinDigestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func pinTestCompose(services map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"services": services}
}

func pinService(compose map[string]interface{}, name string) map[string]interface{} {
	return compose["services"].(map[string]interface{})[name].(map[string]interface{})
}

func TestPinComposeImages(t *testing.T) {
	t.Run("a pinned service names its image by the digest, keeping the tag", func(t *testing.T) {
		compose := pinTestCompose(map[string]interface{}{
			"app": map[string]interface{}{"image": "registry.test/apps/prod_amd64_7_x_1:1.0.0", composeImageDigestField: pinDigestA},
			"db":  map[string]interface{}{"image": "registry.test/apps/prod_amd64_7_x_2:1.0.0"},
		})

		pinned, err := pinComposeImages(compose)
		require.NoError(t, err)
		assert.Equal(t, 1, pinned)
		assert.Equal(t, "registry.test/apps/prod_amd64_7_x_1:1.0.0@"+pinDigestA, pinService(compose, "app")["image"])
		assert.Equal(t, "registry.test/apps/prod_amd64_7_x_2:1.0.0", pinService(compose, "db")["image"], "an unpinned service is left as it is")
	})

	t.Run("an image already pinned in its reference is left as it is", func(t *testing.T) {
		compose := pinTestCompose(map[string]interface{}{
			"cache": map[string]interface{}{"image": "redis@" + pinDigestB, composeImageDigestField: pinDigestA},
		})

		pinned, err := pinComposeImages(compose)
		require.NoError(t, err)
		assert.Equal(t, 0, pinned)
		assert.Equal(t, "redis@"+pinDigestB, pinService(compose, "cache")["image"])
	})

	for name, digest := range map[string]interface{}{
		"too short":       "sha256:abc",
		"not sha256":      "md5:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"upper case":      "sha256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"trailing text":   pinDigestA + "\n",
		"not a string":    42,
		"injected option": pinDigestA + " --insecure",
	} {
		t.Run("an invalid pin fails instead of running unpinned: "+name, func(t *testing.T) {
			compose := pinTestCompose(map[string]interface{}{
				"app": map[string]interface{}{"image": "registry.test/apps/x:1.0.0", composeImageDigestField: digest},
			})

			_, err := pinComposeImages(compose)
			require.Error(t, err)
			assert.Equal(t, "registry.test/apps/x:1.0.0", pinService(compose, "app")["image"])
		})
	}

	t.Run("a pin without an image fails", func(t *testing.T) {
		compose := pinTestCompose(map[string]interface{}{
			"app": map[string]interface{}{"build": ".", composeImageDigestField: pinDigestA},
		})

		_, err := pinComposeImages(compose)
		require.Error(t, err)
	})
}

func TestPinnedImageTags(t *testing.T) {
	compose := pinTestCompose(map[string]interface{}{
		"app":      map[string]interface{}{"image": "registry.test/apps/x_1:1.0.0@" + pinDigestA},
		"port":     map[string]interface{}{"image": "localhost:5000/apps/x_2:2.0@" + pinDigestB},
		"untagged": map[string]interface{}{"image": "localhost:5000/apps/x_3@" + pinDigestB},
		"plain":    map[string]interface{}{"image": "registry.test/apps/x_4:1.0.0"},
	})

	assert.ElementsMatch(t, []pinnedImageTag{
		{Source: "registry.test/apps/x_1@" + pinDigestA, Target: "registry.test/apps/x_1:1.0.0"},
		{Source: "localhost:5000/apps/x_2@" + pinDigestB, Target: "localhost:5000/apps/x_2:2.0"},
	}, pinnedImageTags(compose))
}

func TestComposeServicesWithMissingImagesHonoursPins(t *testing.T) {
	compose := pinTestCompose(map[string]interface{}{
		"app": map[string]interface{}{"image": "registry.test/apps/x_1:1.0.0", composeImageDigestField: pinDigestA},
	})

	t.Run("present: an image holds the pinned digest", func(t *testing.T) {
		local := []containerpkg.ImageResult{{RepoDigests: []string{"registry.test/apps/x_1@" + pinDigestA}}}
		missing, err := composeServicesWithMissingImages(compose, local)
		require.NoError(t, err)
		assert.Empty(t, missing)
	})

	t.Run("missing: the tag is there, but with other content", func(t *testing.T) {
		local := []containerpkg.ImageResult{{
			RepoTags:    []string{"registry.test/apps/x_1:1.0.0"},
			RepoDigests: []string{"registry.test/apps/x_1@" + pinDigestB},
		}}
		missing, err := composeServicesWithMissingImages(compose, local)
		require.NoError(t, err)
		assert.Equal(t, []string{"app"}, missing)
	})

	t.Run("missing: the digest belongs to another repository", func(t *testing.T) {
		local := []containerpkg.ImageResult{{RepoDigests: []string{"registry.test/apps/x_2@" + pinDigestA}}}
		missing, err := composeServicesWithMissingImages(compose, local)
		require.NoError(t, err)
		assert.Equal(t, []string{"app"}, missing)
	})
}

// SetupComposeFiles is where the pin takes effect: the compose file a PROD
// app is pulled and started from names the image by digest. The digest
// belongs to the content, so the host resolution runs first.
func TestSetupComposeFilesPinsReleaseImages(t *testing.T) {
	render := func(t *testing.T, payload common.TransitionPayload) (map[string]interface{}, error) {
		t.Helper()
		am, mockContainer, _, _, _, cfg := amHarness(t)
		cfg.ReswarmConfig.DockerMainRepository = "apps/"
		mockContainer.EXPECT().GetComposePublishedPorts(mock.Anything, mock.Anything).
			Return(map[string]uint64{}, nil).Maybe()

		app := &common.App{AppKey: payload.AppKey, AppName: payload.AppName, Stage: payload.Stage}
		composePath, err := am.StateMachine.SetupComposeFiles(payload, app, false)
		if err != nil {
			return nil, err
		}
		rendered, err := os.ReadFile(composePath)
		require.NoError(t, err)
		var compose map[string]interface{}
		require.NoError(t, json.Unmarshal(rendered, &compose))
		return compose, nil
	}

	releaseCompose := func(digest interface{}) map[string]interface{} {
		return pinTestCompose(map[string]interface{}{
			// Minted under the cloud's registry host; this device reads it
			// from registry.test (resolveComposeStoreImages).
			"app": map[string]interface{}{"image": "registry.ironflock.com/apps/prod_amd64_7_pinapp_1:1.0.0", composeImageDigestField: digest},
		})
	}

	t.Run("PROD: the image is resolved onto this device's registry, then pinned", func(t *testing.T) {
		payload := common.TransitionPayload{Stage: common.PROD, AppKey: 7, AppName: "pinapp", DockerCompose: releaseCompose(pinDigestA)}

		compose, err := render(t, payload)
		require.NoError(t, err)
		assert.Equal(t, "registry.test/apps/prod_amd64_7_pinapp_1:1.0.0@"+pinDigestA, pinService(compose, "app")["image"])
		assert.Equal(t, "registry.ironflock.com/apps/prod_amd64_7_pinapp_1:1.0.0", pinService(payload.DockerCompose, "app")["image"],
			"the payload keeps the release's definition")
	})

	t.Run("PROD: an invalid pin fails the app", func(t *testing.T) {
		payload := common.TransitionPayload{Stage: common.PROD, AppKey: 7, AppName: "pinapp", DockerCompose: releaseCompose("sha256:nope")}

		_, err := render(t, payload)
		require.Error(t, err)
	})

	t.Run("DEV: a build pins nothing", func(t *testing.T) {
		payload := common.TransitionPayload{Stage: common.DEV, AppKey: 7, AppName: "pinapp", DockerCompose: releaseCompose(pinDigestA)}

		compose, err := render(t, payload)
		require.NoError(t, err)
		assert.NotContains(t, pinService(compose, "app")["image"], "@")
	})
}

// After a pull, every pinned image gets its tag back: docker stores an image
// pulled by digest untagged, and the clean-up paths find app images by tag.
func TestPullComposeImagesTagsPinnedImages(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the compose CLI stub is a POSIX shell script")
	}

	am, mc, _, _, _, cfg := amHarness(t)
	sm := am.StateMachine

	dir := t.TempDir()
	binPath := filepath.Join(dir, "fake-docker")
	require.NoError(t, os.WriteFile(binPath, []byte("#!/bin/sh\nexit 0\n"), 0o755)) //nolint:gosec // G306: test stub must be executable
	mc.EXPECT().Compose().Return(containerpkg.NewComposeWithBinary(cfg, binPath)).Maybe()
	mc.EXPECT().ListContainers(mock.Anything, mock.Anything).Return(nil, nil).Maybe()
	fwdAllowLogs(mc)

	composePath := filepath.Join(dir, "docker-compose.yml")
	compose := pinTestCompose(map[string]interface{}{
		"app":   map[string]interface{}{"image": "registry.test/apps/x_1:1.0.0@" + pinDigestA},
		"plain": map[string]interface{}{"image": "registry.test/apps/x_2:1.0.0"},
	})
	content, err := json.Marshal(compose)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(composePath, content, 0o600))

	mc.EXPECT().Tag(mock.Anything, "registry.test/apps/x_1@"+pinDigestA, "registry.test/apps/x_1:1.0.0").Return(nil).Once()

	payload := common.TransitionPayload{Stage: common.PROD, AppKey: 7, AppName: "x"}
	payload.ContainerName.Prod = common.BuildComposeContainerName(common.PROD, 7, "x")
	require.NoError(t, sm.pullComposeImages(context.Background(), payload, composePath, nil))
}
