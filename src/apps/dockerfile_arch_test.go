package apps

import (
	"context"
	"path/filepath"
	"testing"

	dockerbuild "github.com/docker/docker/api/types/build"

	"reagent/common"
	"reagent/release"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestDockerfileArch(t *testing.T) {
	for architecture, want := range map[string]string{
		"amd64": "amd64",
		"arm64": "arm64",
		"armv7": "armv7",
		"armv6": "armv6",
		"armv5": "armv5",
		// from an older backend, or a value apps are not built for: the
		// agent's own, as before
		"":      release.GetBuildArch(),
		"ARM":   release.GetBuildArch(),
		"arm":   release.GetBuildArch(),
		"ARM64": release.GetBuildArch(),
		"riscv": release.GetBuildArch(),
	} {
		assert.Equal(t, want, dockerfileArch(common.TransitionPayload{Architecture: architecture}), architecture)
	}
}

// The build uses the Dockerfile of the architecture the platform registers it
// for, whatever the agent's own architecture and the engine's.
func TestBuildUsesTheDockerfileOfThePlatformsArchitecture(t *testing.T) {
	build := func(t *testing.T, architecture string, files map[string]string) string {
		t.Helper()
		sm, mc, st, _, cfg := wiredRunBuildSM(t)

		app := seedApp(t, st, "archapp", common.REMOVED, common.DEV)
		payload := execPayload("archapp", common.BUILT, common.DEV)
		payload.Architecture = architecture
		tarPath := filepath.Join(cfg.CommandLineArguments.AppsBuildDir, payload.AppName+"."+cfg.CommandLineArguments.CompressedBuildExtension)
		imgWriteTgz(t, tarPath, files)

		var used string
		mc.EXPECT().Build(mock.Anything, tarPath, mock.Anything).
			Run(func(_ context.Context, _ string, options dockerbuild.ImageBuildOptions) { used = options.Dockerfile }).
			Return(fwdDockerStream(), nil).Once()
		fwdAllowLogs(mc)

		require.NoError(t, sm.buildApp(payload, app))
		return used
	}
	all := map[string]string{
		"Dockerfile":       "FROM scratch\n",
		"Dockerfile.arm64": "FROM scratch\n",
		"Dockerfile.armv7": "FROM scratch\n",
		"Dockerfile.armv6": "FROM scratch\n",
		"Dockerfile.amd64": "FROM scratch\n",
	}

	// whatever this agent was built for (the test host): the platform's value decides
	assert.Equal(t, "Dockerfile.armv7", build(t, "armv7", all))
	assert.Equal(t, "Dockerfile.armv6", build(t, "armv6", all))
	assert.Equal(t, "Dockerfile.amd64", build(t, "amd64", all))
	assert.Equal(t, "Dockerfile.arm64", build(t, "arm64", all))

	// an app without the named file builds its Dockerfile, as before
	assert.Equal(t, "Dockerfile", build(t, "armv6", map[string]string{"Dockerfile": "FROM scratch\n", "Dockerfile.arm64": "FROM scratch\n"}))

	// no value (older backend): the agent's own architecture, as before
	own := "Dockerfile." + release.GetBuildArch()
	if _, found := all[own]; !found {
		own = "Dockerfile"
	}
	assert.Equal(t, own, build(t, "", all))
}
