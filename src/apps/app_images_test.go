package apps

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	dockercontainer "github.com/docker/docker/api/types/container"

	"reagent/common"
	"reagent/config"
	containerpkg "reagent/container"
	"reagent/errdefs"
	"reagent/messenger/topics"
	"reagent/release"
	"reagent/store"
	"reagent/testutil/mocks"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// The image of a single-image app: the name comes from the platform
// (app_images.go). Helpers are prefixed img*.
//
// The app is 1953 "MyApp" on registry.test/ with the main repository apps/.
// imgOwn is the name this agent forms itself (its build architecture),
// imgIndex the platform's name for a release with one name per version,
// imgOther a name from before for an architecture that is not this agent's.
// =============================================================================

const imgAppKey = uint64(1953)

func imgOwnArch() string {
	sysInfo := release.GetSystemInfo()
	return sysInfo.Arch + sysInfo.Variant
}

func imgOtherArch() string {
	if imgOwnArch() == "armv6" {
		return "amd64"
	}
	return "armv6"
}

func imgOwn(version string) string {
	return "registry.test/apps/prod_" + imgOwnArch() + "_1953_myapp:" + version
}

func imgIndex(version string) string {
	return "registry.test/apps/prod_1953_myapp:" + version
}

func imgOther(version string) string {
	return "registry.test/apps/prod_" + imgOtherArch() + "_1953_myapp:" + version
}

// imgConfig puts the test config on registry.test/ with the main repository apps/.
func imgConfig(cfg *config.Config) *config.Config {
	cfg.ReswarmConfig.DockerRegistryURL = "registry.test/"
	cfg.ReswarmConfig.DockerMainRepository = "apps/"
	return cfg
}

// imgPayload is a sync answer for the app on this device: the version it
// runs and the one it is to go to, without image names (an older backend).
func imgPayload(cfg *config.Config, present, newest string) common.TransitionPayload {
	p := common.BuildTransitionPayload(imgAppKey, "MyApp", 0, common.PROD, common.PRESENT, common.RUNNING, 101, 202, cfg)
	p.PresentVersion = present
	p.NewestVersion = newest
	p.Version = present
	return p
}

func imgTestConfig() *config.Config {
	cfg := &config.Config{ReswarmConfig: &config.ReswarmConfig{}}
	return imgConfig(cfg)
}

func TestPlatformImageRepository(t *testing.T) {
	cfg := imgTestConfig()
	p := imgPayload(cfg, "1.0.0", "2.0.0")

	understood := map[string]string{
		"prod_1953_myapp:2.0.0":                      "prod_1953_myapp",
		"prod_amd64_1953_myapp:2.0.0":                "prod_amd64_1953_myapp",
		"prod_armv7_1953_myapp:2.0.0":                "prod_armv7_1953_myapp",
		"prod_" + imgOwnArch() + "_1953_myapp:2.0.0": "prod_" + imgOwnArch() + "_1953_myapp",
	}
	for name, repository := range understood {
		got, tag, ok := platformImageRepository(p, name, "2.0.0")
		assert.True(t, ok, name)
		assert.Equal(t, repository, got, name)
		assert.Equal(t, "2.0.0", tag, name)
	}

	notUnderstood := []string{
		"",
		"prod_1953_myapp",                          // no tag
		"prod_1953_myapp:1.0.0",                    // another version
		"prod_1954_myapp:2.0.0",                    // another app's key
		"prod_19530_myapp:2.0.0",                   // another app's key
		"prod_1953_otherapp:2.0.0",                 // another app's name
		"prod_1953_myapp_1:2.0.0",                  // a Compose service's image
		"prod_amd64_1953_myapp_1:2.0.0",            // a Compose service's image from before
		"dev_amd64_1953_myapp:2.0.0",               // a development build
		"prod_64_1953_myapp:2.0.0",                 // a number where the architecture goes
		"prod_amd_64_1953_myapp:2.0.0",             // two parts where the architecture goes
		"apps/prod_1953_myapp:2.0.0",               // with the main repository
		"registry.test/apps/prod_1953_myapp:2.0.0", // with registry and main repository
		"prod_1953_myapp@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"prod_1953_myapp:2.0.0 ",
	}
	for _, name := range notUnderstood {
		_, _, ok := platformImageRepository(p, name, "2.0.0")
		assert.False(t, ok, "%q must not be understood", name)
	}

	t.Run("the tag is the version, letter case aside", func(t *testing.T) {
		_, tag, ok := platformImageRepository(p, "prod_1953_myapp:2.0.0-rc1", "2.0.0-RC1")
		assert.True(t, ok)
		assert.Equal(t, "2.0.0-rc1", tag, "the platform's tag, as the index is tagged")
	})

	t.Run("a DEV app takes no name from the platform", func(t *testing.T) {
		dev := p
		dev.Stage = common.DEV
		_, _, ok := platformImageRepository(dev, "prod_1953_myapp:2.0.0", "2.0.0")
		assert.False(t, ok)
	})
}

func TestProdImageFor(t *testing.T) {
	cfg := imgTestConfig()

	t.Run("an older backend names none: the agent's own name, as before", func(t *testing.T) {
		p := imgPayload(cfg, "1.0.0", "2.0.0")
		assert.Equal(t, imgOwn("2.0.0"), prodImageFor(cfg.ReswarmConfig, p, "2.0.0"))
		assert.Equal(t, imgOwn("1.0.0"), prodImageFor(cfg.ReswarmConfig, p, "1.0.0"))
	})

	t.Run("a release with one name per version", func(t *testing.T) {
		p := imgPayload(cfg, "1.0.0", "2.0.0")
		p.PresentImageName = "prod_" + imgOwnArch() + "_1953_myapp:1.0.0"
		p.NewImageName = "prod_1953_myapp:2.0.0"
		assert.Equal(t, imgIndex("2.0.0"), prodImageFor(cfg.ReswarmConfig, p, "2.0.0"))
		assert.Equal(t, imgOwn("1.0.0"), prodImageFor(cfg.ReswarmConfig, p, "1.0.0"))
	})

	t.Run("a release from before keeps exactly today's reference", func(t *testing.T) {
		p := imgPayload(cfg, "1.0.0", "2.0.0-RC1")
		p.NewImageName = "prod_" + imgOwnArch() + "_1953_myapp:2.0.0-rc1"
		assert.Equal(t, imgOwn("2.0.0-RC1"), prodImageFor(cfg.ReswarmConfig, p, "2.0.0-RC1"),
			"the version as the sync answer spells it, as the agent always pulled it")
	})

	t.Run("a release from before of another architecture", func(t *testing.T) {
		p := imgPayload(cfg, "1.0.0", "2.0.0")
		p.NewImageName = "prod_" + imgOtherArch() + "_1953_myapp:2.0.0"
		assert.Equal(t, imgOther("2.0.0"), prodImageFor(cfg.ReswarmConfig, p, "2.0.0"))
	})

	t.Run("a name of another shape is not used", func(t *testing.T) {
		for _, name := range []string{"apps/prod_1953_myapp:2.0.0", "prod_1953_other:2.0.0", "nginx:2.0.0", "prod_1953_myapp:2.0.1"} {
			p := imgPayload(cfg, "1.0.0", "2.0.0")
			p.NewImageName = name
			assert.Equal(t, imgOwn("2.0.0"), prodImageFor(cfg.ReswarmConfig, p, "2.0.0"), name)
		}
	})

	t.Run("the name of a Compose release names no image", func(t *testing.T) {
		p := imgPayload(cfg, "1.0.0", "2.0.0")
		p.NewImageName = "prod_1953_myapp:2.0.0"
		p.NewDockerCompose = map[string]interface{}{"services": map[string]interface{}{}}
		assert.Equal(t, imgOwn("2.0.0"), prodImageFor(cfg.ReswarmConfig, p, "2.0.0"))
	})

	t.Run("a DEV app", func(t *testing.T) {
		p := imgPayload(cfg, "1.0.0", "2.0.0")
		p.Stage = common.DEV
		p.NewImageName = "prod_1953_myapp:2.0.0"
		assert.Equal(t, imgOwn("2.0.0"), prodImageFor(cfg.ReswarmConfig, p, "2.0.0"))
	})
}

func TestProdImageRepositories(t *testing.T) {
	cfg := imgTestConfig()
	own := imageRepository(imgOwn("1"))

	p := imgPayload(cfg, "1.0.0", "2.0.0")
	assert.Equal(t, []string{own}, prodImageRepositories(cfg.ReswarmConfig, p), "an older backend")

	p.PresentImageName = "prod_" + imgOtherArch() + "_1953_myapp:1.0.0"
	p.NewImageName = "prod_1953_myapp:2.0.0"
	assert.Equal(t, []string{own, imageRepository(imgOther("1")), imageRepository(imgIndex("1"))},
		prodImageRepositories(cfg.ReswarmConfig, p))

	p.PresentImageName = "library/nginx:1.0.0"
	p.NewImageName = "prod_1954_myapp:2.0.0"
	assert.Equal(t, []string{own}, prodImageRepositories(cfg.ReswarmConfig, p), "names of another shape add nothing")
}

func TestWantedAppImagesKeepsThePlatformsNames(t *testing.T) {
	cfg := imgTestConfig()
	p := imgPayload(cfg, "1.0.0", "2.0.0")
	p.PresentImageName = "prod_" + imgOwnArch() + "_1953_myapp:1.0.0"
	p.NewImageName = "prod_1953_myapp:2.0.0"

	wanted := WantedAppImages(cfg.ReswarmConfig, []common.TransitionPayload{p})
	for _, ref := range []string{imgOwn("1.0.0"), imgOwn("2.0.0"), imgIndex("2.0.0")} {
		assert.True(t, wanted[ref], ref)
	}
	assert.False(t, wanted[imgIndex("1.0.0")], "the platform names no such image")
}

// =============================================================================
// The state machine with the platform's names
// =============================================================================

// imgHarness is a state machine on the test registry with the app seeded at
// state and version, and its requested state stored as payload.
func imgHarness(t *testing.T, state common.AppState, version string) (*StateMachine, *mocks.Container, *store.AppStore, *config.Config, *common.App) {
	t.Helper()
	sm, mc, st, _, cfg := wiredRunBuildSM(t)
	imgConfig(cfg)

	seed := imgPayload(cfg, version, version)
	seed.CurrentState = state
	app, err := st.AddApp(seed)
	require.NoError(t, err)
	app.StateLock.Lock()
	app.Version = version
	app.StateLock.Unlock()
	return sm, mc, st, cfg, app
}

func imgStore(t *testing.T, st *store.AppStore, payload common.TransitionPayload) {
	t.Helper()
	if payload.CurrentState == "" {
		payload.CurrentState = common.PRESENT
	}
	require.NoError(t, st.UpdateLocalRequestedState(payload))
}

var imgRemoveTag = map[string]interface{}{"force": false, "pruneChildren": true}

func imageNotFound() error {
	return errdefs.ImageNotFound(errors.New("no such image"))
}

// imgExpectUpdate wires the container calls of a single-image update that
// pulls ref, with no old container to remove.
func imgExpectUpdate(mc *mocks.Container, payload common.TransitionPayload, ref string) {
	mc.EXPECT().HandleRegistryLogins(mock.Anything).Return(nil).Once()
	mc.EXPECT().Pull(mock.Anything, ref, mock.Anything).Return(fwdDockerStream(), nil).Once()
	mc.EXPECT().GetContainer(mock.Anything, payload.ContainerName.Prod).
		Return(dockercontainer.Summary{}, notFoundErr()).Once()
	fwdAllowLogs(mc)
}

func TestInstallUnderTheNewName(t *testing.T) {
	sm, mc, st, cfg, app := imgHarness(t, common.REMOVED, "")
	payload := imgPayload(cfg, "2.0.0", "2.0.0")
	payload.PresentImageName = "prod_1953_myapp:2.0.0"
	payload.NewImageName = "prod_1953_myapp:2.0.0"
	imgStore(t, st, payload)

	mc.EXPECT().HandleRegistryLogins(mock.Anything).Return(nil).Once()
	// by tag, no platform named: Docker chooses from the index
	mc.EXPECT().Pull(mock.Anything, imgIndex("2.0.0"), mock.Anything).Return(fwdDockerStream(), nil).Once()
	fwdAllowLogs(mc)

	require.NoError(t, sm.pullApp(payload, app))
}

func TestUpdateFromTheOldNameToTheNewName(t *testing.T) {
	newPayload := func(cfg *config.Config) common.TransitionPayload {
		p := imgPayload(cfg, "1.0.0", "2.0.0")
		p.RequestUpdate = true
		// The app was installed before this agent took names from the
		// platform: under the agent's own name, which the platform still
		// gives for 1.0.0.
		p.PresentImageName = "prod_" + imgOwnArch() + "_1953_myapp:1.0.0"
		p.NewImageName = "prod_1953_myapp:2.0.0"
		return p
	}

	t.Run("pulls the new name and removes the old tag", func(t *testing.T) {
		sm, mc, st, cfg, app := imgHarness(t, common.RUNNING, "1.0.0")
		payload := newPayload(cfg)
		imgStore(t, st, payload)

		imgExpectUpdate(mc, payload, imgIndex("2.0.0"))
		mc.EXPECT().RemoveImage(mock.Anything, imgOwn("1.0.0"), imgRemoveTag).Return(nil).Once()

		require.NoError(t, sm.updateApp(payload, app))

		stored, err := st.GetRequestedState(imgAppKey, common.PROD)
		require.NoError(t, err)
		assert.Equal(t, "2.0.0", stored.PresentVersion)
		assert.Equal(t, "prod_1953_myapp:2.0.0", stored.PresentImageName, "the installed version's name is stored with it")
	})

	t.Run("keeps the old tag while another installed app uses it", func(t *testing.T) {
		sm, mc, st, cfg, app := imgHarness(t, common.RUNNING, "1.0.0")
		payload := newPayload(cfg)
		imgStore(t, st, payload)

		// Another app's Compose file runs this app's image of 1.0.0.
		other := common.BuildTransitionPayload(77, "Other", 0, common.PROD, common.PRESENT, common.RUNNING, 0, 0, cfg)
		other.DockerCompose = map[string]interface{}{"services": map[string]interface{}{
			"web": map[string]interface{}{"image": imgOwn("1.0.0")},
		}}
		imgStore(t, st, other)

		imgExpectUpdate(mc, payload, imgIndex("2.0.0"))
		// no RemoveImage: the strict mock fails on one

		require.NoError(t, sm.updateApp(payload, app))
	})
}

func TestDowngradeFromTheNewNameToTheOldName(t *testing.T) {
	sm, mc, st, cfg, app := imgHarness(t, common.RUNNING, "2.0.0")
	payload := imgPayload(cfg, "2.0.0", "1.0.0")
	payload.RequestUpdate = true
	payload.PresentImageName = "prod_1953_myapp:2.0.0"
	payload.NewImageName = "prod_" + imgOwnArch() + "_1953_myapp:1.0.0"
	imgStore(t, st, payload)

	imgExpectUpdate(mc, payload, imgOwn("1.0.0"))
	mc.EXPECT().RemoveImage(mock.Anything, imgIndex("2.0.0"), imgRemoveTag).Return(nil).Once()
	mc.EXPECT().RemoveImage(mock.Anything, imgOwn("2.0.0"), imgRemoveTag).Return(imageNotFound()).Once()

	require.NoError(t, sm.updateApp(payload, app))

	stored, err := st.GetRequestedState(imgAppKey, common.PROD)
	require.NoError(t, err)
	assert.Equal(t, "prod_"+imgOwnArch()+"_1953_myapp:1.0.0", stored.PresentImageName)
}

func TestUpdateWithAnOlderBackendIsAsBefore(t *testing.T) {
	sm, mc, st, cfg, app := imgHarness(t, common.RUNNING, "1.0.0")
	payload := imgPayload(cfg, "1.0.0", "2.0.0")
	payload.RequestUpdate = true
	imgStore(t, st, payload)

	imgExpectUpdate(mc, payload, imgOwn("2.0.0"))
	mc.EXPECT().RemoveImage(mock.Anything, imgOwn("1.0.0"), imgRemoveTag).Return(nil).Once()

	require.NoError(t, sm.updateApp(payload, app))
}

func TestUpdateWithANameOfAnotherShape(t *testing.T) {
	sm, mc, st, cfg, app := imgHarness(t, common.RUNNING, "1.0.0")
	payload := imgPayload(cfg, "1.0.0", "2.0.0")
	payload.RequestUpdate = true
	payload.PresentImageName = "library/nginx:1.0.0"
	payload.NewImageName = "registry.evil/x:2.0.0"
	imgStore(t, st, payload)

	// pulls the agent's own name, and removes only the agent's own tag
	imgExpectUpdate(mc, payload, imgOwn("2.0.0"))
	mc.EXPECT().RemoveImage(mock.Anything, imgOwn("1.0.0"), imgRemoveTag).Return(nil).Once()

	require.NoError(t, sm.updateApp(payload, app))
}

func TestRemoveWithTheNamesOfThePlatform(t *testing.T) {
	t.Run("every repository of the app's image", func(t *testing.T) {
		sm, mc, st, cfg, app := imgHarness(t, common.PRESENT, "1.0.0")
		payload := imgPayload(cfg, "1.0.0", "2.0.0")
		payload.RequestedState = common.REMOVED
		payload.PresentImageName = "prod_" + imgOtherArch() + "_1953_myapp:1.0.0"
		payload.NewImageName = "prod_1953_myapp:2.0.0"
		imgStore(t, st, payload)

		mc.EXPECT().GetContainer(mock.Anything, payload.ContainerName.Prod).Return(dockercontainer.Summary{}, notFoundErr()).Once()
		for _, repository := range []string{imageRepository(imgOwn("1")), imageRepository(imgOther("1")), imageRepository(imgIndex("1"))} {
			mc.EXPECT().RemoveImagesByName(mock.Anything, repository, map[string]interface{}{"force": true}).Return(nil).Once()
		}
		fwdAllowLogs(mc)

		require.NoError(t, sm.removeProdApp(payload, app))
	})

	t.Run("a name of another shape removes nothing under it", func(t *testing.T) {
		sm, mc, st, cfg, app := imgHarness(t, common.PRESENT, "1.0.0")
		payload := imgPayload(cfg, "1.0.0", "2.0.0")
		payload.RequestedState = common.REMOVED
		payload.PresentImageName = "library/nginx:1.0.0"
		payload.NewImageName = "prod_1954_otherapp:2.0.0"
		imgStore(t, st, payload)

		mc.EXPECT().GetContainer(mock.Anything, payload.ContainerName.Prod).Return(dockercontainer.Summary{}, notFoundErr()).Once()
		mc.EXPECT().RemoveImagesByName(mock.Anything, imageRepository(imgOwn("1")), map[string]interface{}{"force": true}).Return(nil).Once()
		fwdAllowLogs(mc)

		require.NoError(t, sm.removeProdApp(payload, app))
	})
}

// The image an installed app starts with is the one the device has, under
// the name stored with its requested state; nothing is pulled (the strict
// mock fails on a pull).
func TestStartWithTheStoredName(t *testing.T) {
	// startImage stores stored, reads it back as a start does (offline too),
	// and computes the container's image with the device holding local(read).
	startImage := func(t *testing.T, stored common.TransitionPayload, local func(read common.TransitionPayload) string) (image string, want string) {
		t.Helper()
		sm, mc, st, _, app := imgHarness(t, common.PRESENT, stored.PresentVersion)
		imgStore(t, st, stored)

		payload, err := st.GetRequestedState(imgAppKey, common.PROD)
		require.NoError(t, err)
		want = local(payload)

		mc.EXPECT().GetImage(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
			func(_ context.Context, repository string, tag string) (containerpkg.ImageResult, error) {
				if repository+":"+tag == want {
					return containerpkg.ImageResult{ID: "sha256:local"}, nil
				}
				return containerpkg.ImageResult{}, imageNotFound()
			}).Maybe()

		containerConfig, _, err := sm.computeContainerConfigs(payload, app)
		require.NoError(t, err)
		return containerConfig.Image, want
	}

	t.Run("a stored name of the new form", func(t *testing.T) {
		cfg := imgTestConfig()
		stored := imgPayload(cfg, "2.0.0", "2.0.0")
		stored.PresentImageName = "prod_1953_myapp:2.0.0"
		stored.NewImageName = "prod_1953_myapp:2.0.0"
		image, _ := startImage(t, stored, func(common.TransitionPayload) string { return imgIndex("2.0.0") })
		assert.Equal(t, imgIndex("2.0.0"), image)
	})

	t.Run("an app installed before this agent: no stored name", func(t *testing.T) {
		cfg := imgTestConfig()
		stored := imgPayload(cfg, "1.0.0", "1.0.0")
		image, want := startImage(t, stored, func(read common.TransitionPayload) string {
			return read.RegistryImageName.Prod + ":1.0.0"
		})
		assert.Equal(t, want, image)
	})

	t.Run("a stored new name while the device has the version under the old one", func(t *testing.T) {
		// installed before the platform moved the version to its new name
		cfg := imgTestConfig()
		stored := imgPayload(cfg, "1.0.0", "1.0.0")
		stored.PresentImageName = "prod_1953_myapp:1.0.0"
		image, want := startImage(t, stored, func(read common.TransitionPayload) string {
			return read.RegistryImageName.Prod + ":1.0.0"
		})
		assert.Equal(t, want, image, "runs the image it has, offline too")
	})
}

func TestStoredImageNamesSurviveARestart(t *testing.T) {
	_, _, st, cfg, _ := imgHarness(t, common.RUNNING, "1.0.0")
	payload := imgPayload(cfg, "1.0.0", "2.0.0")
	payload.CurrentState = common.RUNNING
	payload.PresentImageName = "prod_1953_myapp:1.0.0"
	payload.NewImageName = "prod_1953_myapp:2.0.0"
	imgStore(t, st, payload)

	all, err := st.GetRequestedStates()
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, "prod_1953_myapp:1.0.0", all[0].PresentImageName)
	assert.Equal(t, "prod_1953_myapp:2.0.0", all[0].NewImageName)

	// a sync of an older backend names none: the agent's own names again
	payload.PresentImageName, payload.NewImageName = "", ""
	imgStore(t, st, payload)
	one, err := st.GetRequestedState(imgAppKey, common.PROD)
	require.NoError(t, err)
	assert.Empty(t, one.PresentImageName)
	assert.Empty(t, one.NewImageName)
}

// The observer finds a single-image app's image under the platform's name:
// before, it looked for the agent's own name only, and an app without a
// container whose image has the new name was reported REMOVED.
func TestObserverFindsTheImageUnderThePlatformsName(t *testing.T) {
	cfg := imgTestConfig()
	p := imgPayload(cfg, "2.0.0", "2.0.0")
	p.PresentImageName = "prod_1953_myapp:2.0.0"

	images := []containerpkg.ImageResult{{ID: "sha256:a", RepoTags: []string{"registry.test/apps/prod_19530_myapp:2.0.0", imgIndex("2.0.0")}}}
	assert.True(t, hasImageInRepositories(images, appImageRepositories(cfg.ReswarmConfig, p)))

	withoutName := p
	withoutName.PresentImageName = ""
	assert.False(t, hasImageInRepositories(images, appImageRepositories(cfg.ReswarmConfig, withoutName)),
		"negative control: without the platform's name the image is not the app's")

	prefixOnly := []containerpkg.ImageResult{{ID: "sha256:b", RepoTags: []string{imgOwn("2.0.0")[:len(imgOwn("2.0.0"))-len(":2.0.0")] + "x:2.0.0"}}}
	assert.False(t, hasImageInRepositories(prefixOnly, appImageRepositories(cfg.ReswarmConfig, withoutName)),
		"another app whose name starts like this one's")
}

// =============================================================================
// Point 2: the push side of a release build keeps the agent's own name
// =============================================================================

// A single-image release build is pushed as
// prod_<agent build architecture>_<app key>_<name>:<version>, which the
// backend computes from the device's agent_architecture: the platform's names
// for pulling never reach it. The PUBLISHED report names the digest the push
// answered (pushed_digests), where the push stream gave one.
func TestReleaseBuildPushesTheAgentsOwnName(t *testing.T) {
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	auxStream := func() io.ReadCloser {
		return fwdDockerStream(
			`{"status":"The push refers to repository [registry.test/apps/prod_x_1953_myapp]"}`,
			`{"status":"3.0.0: digest: `+digest+` size: 528"}`,
			`{"progressDetail":{},"aux":{"Tag":"3.0.0","Digest":"`+digest+`","Size":528}}`,
		)
	}

	publish := func(t *testing.T, stream io.ReadCloser) (*common.App, common.Dict, string) {
		t.Helper()
		sm, mc, st, msg, cfg := wiredRunBuildSM(t)
		imgConfig(cfg)

		payload := imgPayload(cfg, "1.0.0", "1.0.0")
		payload.Stage = common.DEV
		payload.Version = "3.0.0"
		payload.CurrentState = common.PRESENT
		payload.RequestedState = common.PUBLISHED
		payload.PresentImageName = "prod_1953_myapp:3.0.0"
		payload.NewImageName = "prod_1953_myapp:3.0.0"
		devApp, err := st.AddApp(payload)
		require.NoError(t, err)
		// a digest of an earlier build is never reported for this one
		devApp.PushedDigests = map[string]string{"registry.test/apps/old:1": digest}
		imgStore(t, st, payload)

		pushed := "registry.test/apps/prod_" + imgOwnArch() + "_1953_myapp:3.0.0"
		require.Equal(t, "registry.test/apps/dev_"+imgOwnArch()+"_1953_myapp", payload.RegistryImageName.Dev)

		tarPath := filepath.Join(cfg.CommandLineArguments.AppsBuildDir, payload.AppName+"."+cfg.CommandLineArguments.CompressedBuildExtension)
		imgWriteTgz(t, tarPath, map[string]string{"Dockerfile": "FROM scratch\n"})

		mc.EXPECT().Build(mock.Anything, tarPath, mock.Anything).Return(fwdDockerStream(), nil).Once()
		mc.EXPECT().Tag(mock.Anything, payload.RegistryImageName.Dev, pushed).Return(nil).Once()
		mc.EXPECT().HandleRegistryLogins(mock.Anything).Return(nil).Once()
		mc.EXPECT().Push(mock.Anything, pushed, mock.Anything).Return(stream, nil).Once()
		fwdAllowLogs(mc)

		require.NoError(t, sm.publishApp(payload, devApp))

		var report common.Dict
		for _, call := range msg.GetCallCalls() {
			if call.Topic != topics.SetActualAppOnDeviceState || len(call.Args) == 0 {
				continue
			}
			if dict, ok := call.Args[0].(common.Dict); ok && dict["state"] == common.PUBLISHED {
				report = dict
			}
		}
		require.NotNil(t, report, "the PUBLISHED report")
		return devApp, report, pushed
	}

	t.Run("the digest the push answered", func(t *testing.T) {
		_, report, pushed := publish(t, auxStream())
		assert.Equal(t, map[string]string{pushed: digest}, report["pushed_digests"])
	})

	t.Run("the containerd image store's status line", func(t *testing.T) {
		_, report, pushed := publish(t, fwdDockerStream(
			`{"status":"Pushed","progressDetail":{},"id":"5f70bf18a086"}`,
			`{"status":"3.0.0: digest: `+digest+` size: 856"}`,
		))
		assert.Equal(t, map[string]string{pushed: digest}, report["pushed_digests"])
	})

	t.Run("a push whose digest is unknown: the key is absent", func(t *testing.T) {
		devApp, report, _ := publish(t, fwdDockerStream())
		_, found := report["pushed_digests"]
		assert.False(t, found)
		assert.Nil(t, devApp.PushedDigests)
	})
}

// A Compose release build pushes the images under the names of the Compose
// file it is handed; the platform's names do not change them.
func TestComposeReleaseBuildKeepsTheComposeNames(t *testing.T) {
	am, mc, _, _, _, cfg := amHarness(t)
	imgConfig(cfg)
	mc.EXPECT().GetComposePublishedPorts(mock.Anything, mock.Anything).Return(map[string]uint64{}, nil).Maybe()

	payload := imgPayload(cfg, "1.0.0", "1.0.0")
	payload.Stage = common.DEV
	payload.Version = "3.0.0"
	payload.PresentImageName = "prod_1953_myapp:3.0.0"
	payload.NewImageName = "prod_1953_myapp:3.0.0"
	built := "registry.test/apps/prod_" + imgOwnArch() + "_1953_myapp_1:3.0.0"
	payload.DockerCompose = map[string]interface{}{"services": map[string]interface{}{
		"web": map[string]interface{}{"image": built, "build": "."},
	}}
	app := &common.App{AppKey: imgAppKey, AppName: "MyApp", Stage: common.DEV}

	path, err := am.StateMachine.SetupComposeFiles(payload, app, false)
	require.NoError(t, err)
	assert.Equal(t, []string{built}, composePushedImages(path))
}

// imgWriteTgz writes an app archive with the given files.
func imgWriteTgz(t *testing.T, path string, files map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	for name, body := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}))
		_, err := tw.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gzw.Close())
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o600))
}
