package apps

import (
	"context"
	"errors"
	"testing"

	dockercontainer "github.com/docker/docker/api/types/container"

	"reagent/common"
	"reagent/config"
	containerpkg "reagent/container"
	"reagent/errdefs"
	"reagent/store"
	"reagent/testutil/builders"
	"reagent/testutil/fakes"
	"reagent/testutil/mocks"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Second round: the pull under the platform's name falls back to the agent's
// own name; the name an image was really pulled under is recorded and used;
// the uninstall keeps what another app wants. Helpers from app_images_test.go.
// =============================================================================

// imgStored is the stored requested state of the test app.
func imgStored(t *testing.T, st *store.AppStore) common.TransitionPayload {
	t.Helper()
	stored, err := st.GetRequestedState(imgAppKey, common.PROD)
	require.NoError(t, err)
	return stored
}

// A 64-bit agent next to a 32-bit engine: the version's index holds the
// agent's platform only, and the pull of the index by tag fails ("no matching
// manifest"). The agent pulls its own per-architecture name, as before.
func TestInstallFallsBackToTheAgentsOwnName(t *testing.T) {
	newPayload := func(cfg *config.Config) common.TransitionPayload {
		p := imgPayload(cfg, "2.0.0", "2.0.0")
		p.PresentImageName = "prod_1953_myapp:2.0.0"
		p.NewImageName = "prod_1953_myapp:2.0.0"
		return p
	}

	t.Run("the index has no image for the engine: the stream fails", func(t *testing.T) {
		sm, mc, st, cfg, app := imgHarness(t, common.REMOVED, "")
		payload := newPayload(cfg)
		imgStore(t, st, payload)

		mc.EXPECT().HandleRegistryLogins(mock.Anything).Return(nil).Once()
		mc.EXPECT().Pull(mock.Anything, imgIndex("2.0.0"), mock.Anything).
			Return(fwdErrorStream("no matching manifest for linux/arm/v7 in the manifest list entries"), nil).Once()
		mc.EXPECT().Pull(mock.Anything, imgOwn("2.0.0"), mock.Anything).Return(fwdDockerStream(), nil).Once()
		fwdAllowLogs(mc)

		require.NoError(t, sm.pullApp(payload, app))
		stored := imgStored(t, st)
		assert.Equal(t, imgOwn("2.0.0"), stored.InstalledImage, "the name it really got")
		assert.Equal(t, "2.0.0", stored.InstalledImageVersion)
	})

	t.Run("the pull call itself fails", func(t *testing.T) {
		sm, mc, st, cfg, app := imgHarness(t, common.REMOVED, "")
		payload := newPayload(cfg)
		imgStore(t, st, payload)

		mc.EXPECT().HandleRegistryLogins(mock.Anything).Return(nil).Once()
		mc.EXPECT().Pull(mock.Anything, imgIndex("2.0.0"), mock.Anything).Return(nil, errors.New("manifest unknown")).Once()
		mc.EXPECT().Pull(mock.Anything, imgOwn("2.0.0"), mock.Anything).Return(fwdDockerStream(), nil).Once()
		fwdAllowLogs(mc)

		require.NoError(t, sm.pullApp(payload, app))
		assert.Equal(t, imgOwn("2.0.0"), imgStored(t, st).InstalledImage)
	})

	t.Run("both fail: the install fails with the second error", func(t *testing.T) {
		sm, mc, st, cfg, app := imgHarness(t, common.REMOVED, "")
		payload := newPayload(cfg)
		imgStore(t, st, payload)

		mc.EXPECT().HandleRegistryLogins(mock.Anything).Return(nil).Once()
		mc.EXPECT().Pull(mock.Anything, imgIndex("2.0.0"), mock.Anything).Return(nil, errors.New("manifest unknown")).Once()
		mc.EXPECT().Pull(mock.Anything, imgOwn("2.0.0"), mock.Anything).Return(nil, errors.New("registry unreachable")).Once()
		fwdAllowLogs(mc)

		err := sm.pullApp(payload, app)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "registry unreachable")
		assert.Empty(t, imgStored(t, st).InstalledImage)
	})

	t.Run("a canceled pull is not tried again", func(t *testing.T) {
		sm, mc, st, cfg, app := imgHarness(t, common.REMOVED, "")
		payload := newPayload(cfg)
		imgStore(t, st, payload)

		mc.EXPECT().HandleRegistryLogins(mock.Anything).Return(nil).Once()
		mc.EXPECT().Pull(mock.Anything, imgIndex("2.0.0"), mock.Anything).
			Return(nil, errdefs.DockerStreamCanceled(errors.New("canceled"))).Once()
		fwdAllowLogs(mc)

		err := sm.pullApp(payload, app)
		assert.True(t, errdefs.IsDockerStreamCanceled(err))
	})
}

func TestUpdateFallsBackToTheAgentsOwnName(t *testing.T) {
	sm, mc, st, cfg, app := imgHarness(t, common.RUNNING, "1.0.0")
	payload := imgPayload(cfg, "1.0.0", "2.0.0")
	payload.RequestUpdate = true
	payload.NewImageName = "prod_1953_myapp:2.0.0"
	imgStore(t, st, payload)

	mc.EXPECT().HandleRegistryLogins(mock.Anything).Return(nil).Once()
	mc.EXPECT().Pull(mock.Anything, imgIndex("2.0.0"), mock.Anything).
		Return(fwdErrorStream("no matching manifest for linux/arm/v7 in the manifest list entries"), nil).Once()
	mc.EXPECT().Pull(mock.Anything, imgOwn("2.0.0"), mock.Anything).Return(fwdDockerStream(), nil).Once()
	mc.EXPECT().GetContainer(mock.Anything, payload.ContainerName.Prod).Return(dockercontainer.Summary{}, notFoundErr()).Once()
	mc.EXPECT().RemoveImage(mock.Anything, imgOwn("1.0.0"), imgRemoveTag).Return(nil).Once()
	fwdAllowLogs(mc)

	require.NoError(t, sm.updateApp(payload, app))
	stored := imgStored(t, st)
	assert.Equal(t, "2.0.0", stored.PresentVersion)
	assert.Equal(t, imgOwn("2.0.0"), stored.InstalledImage)
}

// The start finds the image under the name it was recorded under, also after
// the platform renamed the version; nothing is pulled (strict mock).
func TestStartWithTheRecordedName(t *testing.T) {
	start := func(t *testing.T, stored common.TransitionPayload, recorded string, lookup func(ref string) error) (string, common.TransitionPayload) {
		t.Helper()
		sm, mc, st, _, app := imgHarness(t, common.PRESENT, stored.PresentVersion)
		imgStore(t, st, stored)
		if recorded != "" {
			require.NoError(t, st.SetInstalledImage(imgAppKey, common.PROD, stored.PresentVersion, recorded))
		}
		payload, err := st.GetRequestedState(imgAppKey, common.PROD)
		require.NoError(t, err)

		mc.EXPECT().GetImage(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
			func(_ context.Context, repository string, tag string) (containerpkg.ImageResult, error) {
				if err := lookup(repository + ":" + tag); err != nil {
					return containerpkg.ImageResult{}, err
				}
				return containerpkg.ImageResult{ID: "sha256:local"}, nil
			}).Maybe()

		containerConfig, _, err := sm.computeContainerConfigs(payload, app)
		require.NoError(t, err)
		return containerConfig.Image, payload
	}
	onlyLocal := func(want string) func(string) error {
		return func(ref string) error {
			if ref == want {
				return nil
			}
			return imageNotFound()
		}
	}

	t.Run("the platform renamed the version", func(t *testing.T) {
		cfg := imgTestConfig()
		stored := imgPayload(cfg, "2.0.0", "2.0.0")
		stored.PresentImageName = "prod_" + imgOtherArch() + "_1953_myapp:2.0.0"
		image, _ := start(t, stored, imgIndex("2.0.0"), onlyLocal(imgIndex("2.0.0")))
		assert.Equal(t, imgIndex("2.0.0"), image)
	})

	t.Run("after the fallback: the agent's own name, recorded", func(t *testing.T) {
		cfg := imgTestConfig()
		stored := imgPayload(cfg, "2.0.0", "2.0.0")
		stored.PresentImageName = "prod_1953_myapp:2.0.0"
		// the store forms the agent's own name with its configuration
		own := common.BuildTransitionPayload(imgAppKey, "MyApp", 0, common.PROD, "", "", 0, 0, builders.DefaultTestConfig()).RegistryImageName.Prod + ":2.0.0"
		image, _ := start(t, stored, own, onlyLocal(own))
		assert.Equal(t, own, image)
	})

	t.Run("Docker cannot say: the recorded name, never the platform's unverified one", func(t *testing.T) {
		cfg := imgTestConfig()
		stored := imgPayload(cfg, "2.0.0", "2.0.0")
		stored.PresentImageName = "prod_" + imgOtherArch() + "_1953_myapp:2.0.0"
		image, _ := start(t, stored, imgIndex("2.0.0"), func(string) error { return errors.New("daemon busy") })
		assert.Equal(t, imgIndex("2.0.0"), image)
	})

	t.Run("Docker cannot say, nothing recorded: the agent's own name, as before", func(t *testing.T) {
		cfg := imgTestConfig()
		stored := imgPayload(cfg, "2.0.0", "2.0.0")
		stored.PresentImageName = "prod_1953_myapp:2.0.0"
		image, read := start(t, stored, "", func(string) error { return errors.New("daemon busy") })
		assert.Equal(t, read.RegistryImageName.Prod+":2.0.0", image)
	})
}

// The observer, the cleanup and the uninstall know the recorded name.
func TestTheRecordedNameIsTheAppsImage(t *testing.T) {
	cfg := imgTestConfig()

	t.Run("observer", func(t *testing.T) {
		p := imgPayload(cfg, "2.0.0", "2.0.0") // an older backend names nothing
		images := []containerpkg.ImageResult{{ID: "a", RepoTags: []string{imgIndex("2.0.0")}}}
		assert.False(t, hasImageInRepositories(images, appImageRepositories(cfg.ReswarmConfig, p)))
		p.InstalledImage, p.InstalledImageVersion = imgIndex("2.0.0"), "2.0.0"
		assert.True(t, hasImageInRepositories(images, appImageRepositories(cfg.ReswarmConfig, p)))
	})

	t.Run("a recorded reference of another app is not this app's", func(t *testing.T) {
		p := imgPayload(cfg, "2.0.0", "2.0.0")
		p.InstalledImage, p.InstalledImageVersion = "registry.test/apps/prod_1954_myapp:2.0.0", "2.0.0"
		assert.Equal(t, []string{imageRepository(imgOwn("1"))}, prodImageRepositories(cfg.ReswarmConfig, p))
		assert.Empty(t, installedProdImage(p, "2.0.0"))
	})

	t.Run("update cleanup", func(t *testing.T) {
		sm, mc, st, cfg, app := imgHarness(t, common.RUNNING, "2.0.0")
		imgConfig(cfg)
		payload := imgPayload(cfg, "2.0.0", "3.0.0")
		payload.RequestUpdate = true
		imgStore(t, st, payload)
		require.NoError(t, st.SetInstalledImage(imgAppKey, common.PROD, "2.0.0", imgIndex("2.0.0")))

		imgExpectUpdate(mc, payload, payload.RegistryImageName.Prod+":3.0.0")
		mc.EXPECT().RemoveImage(mock.Anything, imgIndex("2.0.0"), imgRemoveTag).Return(nil).Once()
		mc.EXPECT().RemoveImage(mock.Anything, payload.RegistryImageName.Prod+":2.0.0", imgRemoveTag).Return(imageNotFound()).Once()

		require.NoError(t, sm.updateApp(payload, app))
	})

	t.Run("uninstall", func(t *testing.T) {
		sm, mc, st, cfg, app := imgHarness(t, common.PRESENT, "2.0.0")
		payload := imgPayload(cfg, "2.0.0", "2.0.0")
		payload.RequestedState = common.REMOVED
		imgStore(t, st, payload)
		require.NoError(t, st.SetInstalledImage(imgAppKey, common.PROD, "2.0.0", imgIndex("2.0.0")))

		mc.EXPECT().GetContainer(mock.Anything, payload.ContainerName.Prod).Return(dockercontainer.Summary{}, notFoundErr()).Once()
		mc.EXPECT().RemoveImagesByName(mock.Anything, payload.RegistryImageName.Prod, map[string]interface{}{"force": true}).Return(nil).Once()
		mc.EXPECT().RemoveImagesByName(mock.Anything, imageRepository(imgIndex("1")), map[string]interface{}{"force": true}).Return(nil).Once()
		fwdAllowLogs(mc)

		require.NoError(t, sm.removeProdApp(payload, app))
	})
}

// Without a configuration a platform's name never becomes a reference: it
// would have no registry.
func TestNoConfigurationNoPlatformReference(t *testing.T) {
	cfg := imgTestConfig()
	p := imgPayload(cfg, "2.0.0", "2.0.0")
	p.NewImageName = "prod_1953_myapp:2.0.0"
	assert.Empty(t, platformProdImage(nil, p, p.NewImageName, "2.0.0"))
	assert.Equal(t, imgOwn("2.0.0"), prodImageFor(nil, p, "2.0.0"))

	// the agent's own form stays the agent's own reference
	p.NewImageName = "prod_" + imgOwnArch() + "_1953_myapp:2.0.0"
	assert.Equal(t, imgOwn("2.0.0"), platformProdImage(nil, p, p.NewImageName, "2.0.0"))
}

// The uninstall keeps every tag another app's requested state wants, also in
// the app's own repositories.
func TestUninstallKeepsWhatAnotherAppWants(t *testing.T) {
	sm, mc, st, cfg, app := imgHarness(t, common.PRESENT, "1.0.0")
	payload := imgPayload(cfg, "1.0.0", "2.0.0")
	payload.RequestedState = common.REMOVED
	payload.PresentImageName = "prod_1953_myapp:1.0.0"
	payload.NewImageName = "prod_1953_myapp:2.0.0"
	imgStore(t, st, payload)

	other := common.BuildTransitionPayload(77, "Other", 0, common.PROD, common.PRESENT, common.RUNNING, 0, 0, cfg)
	other.DockerCompose = map[string]interface{}{"services": map[string]interface{}{
		"web": map[string]interface{}{"image": imgIndex("1.0.0")},
	}}
	imgStore(t, st, other)

	force := map[string]interface{}{"force": true}
	mc.EXPECT().GetContainer(mock.Anything, payload.ContainerName.Prod).Return(dockercontainer.Summary{}, notFoundErr()).Once()
	mc.EXPECT().RemoveImagesByName(mock.Anything, imageRepository(imgOwn("1")), force).Return(nil).Once()
	mc.EXPECT().GetImages(mock.Anything, imageRepository(imgIndex("1"))).Return([]containerpkg.ImageResult{
		{ID: "a", RepoTags: []string{imgIndex("1.0.0")}},
		{ID: "b", RepoTags: []string{imgIndex("2.0.0"), "registry.test/apps/other:1"}},
	}, nil).Once()
	mc.EXPECT().RemoveImage(mock.Anything, imgIndex("2.0.0"), force).Return(nil).Once()
	fwdAllowLogs(mc)

	require.NoError(t, sm.removeProdApp(payload, app))
}

// A store whose requested states cannot be read: nothing is removed.
func TestNothingIsRemovedWhenTheRequestedStatesCannotBeRead(t *testing.T) {
	mc := mocks.NewContainer(t)
	db := mocks.NewDatabase(t)
	db.EXPECT().GetRequestedState(mock.Anything, mock.Anything).Return(common.TransitionPayload{}, errors.New("disk I/O error")).Maybe()
	db.EXPECT().GetRequestedStates().Return(nil, errors.New("disk I/O error")).Maybe()
	appStore := store.NewAppStore(db, fakes.NewMessenger())
	observer := NewObserver(mc, &appStore, nil)
	sm := &StateMachine{Container: mc, StateObserver: &observer}

	cfg := imgTestConfig()
	payload := imgPayload(cfg, "1.0.0", "2.0.0")
	sm.removeSupersededProdImage(payload, imgOwn("2.0.0"))
	require.NoError(t, sm.removeProdImageRepositories(payload, map[string]interface{}{"force": true}))
	// the strict mocks fail on any RemoveImage / RemoveImagesByName
}

// The tag an update installed is never removed as the old version's: with the
// platform's lower-case names, 2.0.0-RC1 and 2.0.0-rc1 are one reference.
func TestUpdateCleanupKeepsTheInstalledTag(t *testing.T) {
	sm, mc, st, cfg, app := imgHarness(t, common.RUNNING, "2.0.0-RC1")
	payload := imgPayload(cfg, "2.0.0-RC1", "2.0.0-rc1")
	payload.RequestUpdate = true
	payload.PresentImageName = "prod_1953_myapp:2.0.0-rc1"
	payload.NewImageName = "prod_1953_myapp:2.0.0-rc1"
	imgStore(t, st, payload)

	imgExpectUpdate(mc, payload, imgIndex("2.0.0-rc1"))
	mc.EXPECT().RemoveImage(mock.Anything, imgOwn("2.0.0-RC1"), imgRemoveTag).Return(nil).Once()
	// no RemoveImage of imgIndex("2.0.0-rc1"): the strict mock fails on it

	require.NoError(t, sm.updateApp(payload, app))
}
