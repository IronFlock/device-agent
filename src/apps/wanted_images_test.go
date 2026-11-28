package apps

import (
	"reagent/common"
	"reagent/config"
	"testing"

	"github.com/stretchr/testify/assert"
)

const testDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestWantedAppImages(t *testing.T) {
	reswarmConfig := &config.ReswarmConfig{DockerRegistryURL: "localhost:15001/", DockerMainRepository: "apps/"}
	single := common.TransitionPayload{
		AppKey: 3, AppName: "meter", Stage: common.PROD,
		RegistryImageName: common.StageBasedResult{
			Prod: "localhost:15001/apps/prod_arm64_3_meter",
			Dev:  "localhost:15001/apps/dev_arm64_3_meter",
		},
		PresentVersion: "1.0.0", NewestVersion: "1.1.0",
	}
	compose := common.TransitionPayload{
		AppKey: 7, AppName: "Plant", Stage: common.PROD,
		RegistryImageName: common.StageBasedResult{Prod: "localhost:15001/apps/prod_arm64_7_plant"},
		PresentVersion:    "1.2.0", NewestVersion: "1.3.0",
		DockerCompose: map[string]interface{}{"services": map[string]interface{}{
			// minted with the platform's registry name, pulled from this
			// device's registry, pinned to a digest
			"web": map[string]interface{}{"image": "registry.example.com/apps/prod_arm64_7_plant_1:1.2.0", composeImageDigestField: testDigest},
			"db":  map[string]interface{}{"image": "localhost:15001/apps/prod_arm64_7_plant_2:1.2.0"},
			// pinned in its own reference
			"cache": map[string]interface{}{"image": "localhost:15001/library/redis:7@" + testDigest},
			// by digest only: Docker keeps no tag to keep
			"proxy": map[string]interface{}{"image": "localhost:15001/library/caddy@" + testDigest},
			"bus":   map[string]interface{}{"image": "localhost:15001/library/mosquitto"},
		}},
		NewDockerCompose: map[string]interface{}{"services": map[string]interface{}{
			"web": map[string]interface{}{"image": "localhost:15001/apps/prod_arm64_7_plant_1:1.3.0"},
		}},
	}

	wanted := WantedAppImages(reswarmConfig, []common.TransitionPayload{single, compose})

	for _, tag := range []string{
		// the single images, as before
		"localhost:15001/apps/prod_arm64_3_meter:1.0.0",
		"localhost:15001/apps/prod_arm64_3_meter:1.1.0",
		"localhost:15001/apps/dev_arm64_3_meter:1.0.0",
		"localhost:15001/apps/prod_arm64_7_plant:1.2.0",
		// the Compose images: as pulled, as released, and the update's
		"localhost:15001/apps/prod_arm64_7_plant_1:1.2.0",
		"registry.example.com/apps/prod_arm64_7_plant_1:1.2.0",
		"localhost:15001/apps/prod_arm64_7_plant_2:1.2.0",
		"localhost:15001/library/redis:7",
		"localhost:15001/library/mosquitto:latest",
		"localhost:15001/apps/prod_arm64_7_plant_1:1.3.0",
	} {
		assert.True(t, wanted[tag], "keeps %s", tag)
	}
	assert.False(t, wanted["localhost:15001/library/caddy:latest"])
	assert.False(t, wanted["localhost:15001/apps/prod_arm64_7_plant_1:1.1.0"], "an older version is not kept")
}

func TestWantedAppImagesWithoutCompose(t *testing.T) {
	wanted := WantedAppImages(nil, []common.TransitionPayload{{
		RegistryImageName: common.StageBasedResult{Prod: "localhost:15001/apps/prod_amd64_3_meter"},
		Version:           "2.0.0",
		DockerCompose:     map[string]interface{}{"services": "not a map"},
	}})

	assert.Equal(t, map[string]bool{"localhost:15001/apps/prod_amd64_3_meter:2.0.0": true}, wanted)
}

func TestImageTag(t *testing.T) {
	for ref, want := range map[string]string{
		"localhost:15001/apps/x:1.0":               "localhost:15001/apps/x:1.0",
		"localhost:15001/apps/x":                   "localhost:15001/apps/x:latest",
		"localhost:15001/apps/x:1.0@" + testDigest: "localhost:15001/apps/x:1.0",
		"localhost:15001/apps/x@" + testDigest:     "",
		"redis":                                    "redis:latest",
		"":                                         "",
	} {
		assert.Equal(t, want, imageTag(ref), "imageTag(%q)", ref)
	}
}
