package apps

import (
	"reagent/common"
	"reagent/config"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The repository a release build's single image is pushed under: the name
// per architecture of the platform the build is for, which is the agent's own
// name on every device whose apps are matched by the agent's architecture.
func TestReleaseBuildRepository(t *testing.T) {
	rc := &config.ReswarmConfig{DockerRegistryURL: "registry.test/", DockerMainRepository: "apps/"}
	// An arm64 agent: the names it forms itself.
	own := common.TransitionPayload{
		AppKey:  7,
		AppName: "MyApp",
		Stage:   common.DEV,
		RegistryImageName: common.StageBasedResult{
			Dev:  "registry.test/apps/dev_arm64_7_myapp",
			Prod: "registry.test/apps/prod_arm64_7_myapp",
		},
	}

	t.Run("a device whose apps are matched by the agent's architecture pushes under the agent's own name, as always", func(t *testing.T) {
		payload := own
		payload.Architecture = "arm64"
		assert.Equal(t, own.RegistryImageName.Prod, releaseBuildRepository(rc, payload))
	})

	t.Run("a device whose apps are matched by its Docker engine pushes under the engine's architecture", func(t *testing.T) {
		// arm64 agent, 32-bit engine: the platform says armv7, the build is an
		// armv7 image, and prod_arm64_... is the arm64 platform's tag.
		payload := own
		payload.Architecture = "armv7"
		got := releaseBuildRepository(rc, payload)
		assert.Equal(t, "registry.test/apps/prod_armv7_7_myapp", got)
		assert.NotEqual(t, own.RegistryImageName.Prod, got, "negative control: the agent's own name is another")
		// the same architecture the build's Dockerfile is chosen for
		assert.Equal(t, "armv7", dockerfileArch(payload))
	})

	t.Run("without an architecture from the platform, or one no app is built for: the agent's own name", func(t *testing.T) {
		for _, architecture := range []string{"", "ARM", "arm", "riscv64"} {
			payload := own
			payload.Architecture = architecture
			assert.Equal(t, own.RegistryImageName.Prod, releaseBuildRepository(rc, payload), architecture)
		}
	})

	t.Run("without a configuration to form the name with: the agent's own name", func(t *testing.T) {
		payload := own
		payload.Architecture = "armv7"
		assert.Equal(t, own.RegistryImageName.Prod, releaseBuildRepository(nil, payload))
	})
}
