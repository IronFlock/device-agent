package apps

import (
	"testing"

	"reagent/config"
	containerpkg "reagent/container"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An appliance's own agent reads its registry on localhost, while the release
// names the appliance's domain: the app's images are on the device under the
// localhost name only (resolveComposeStoreImages). Before, the check looked
// for them only as the release names them, and judged them missing on every
// check: a PRESENT app without containers was reported UNINSTALLED and pulled
// again, a FAILED one never settled offline.
func TestComposeImagesAreFoundUnderTheDevicesRegistry(t *testing.T) {
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	compose := map[string]interface{}{"services": map[string]interface{}{
		"web": map[string]interface{}{
			"image":          "registry.appliance.example/apps/prod_1953_myapp_1:1.0.0",
			"x-image-digest": digest,
		},
		"worker": map[string]interface{}{
			"image": "registry.appliance.example/apps/prod_1953_myapp_2:1.0.0",
		},
	}}
	images := []containerpkg.ImageResult{
		{ID: "a", RepoTags: []string{"localhost:15001/apps/prod_1953_myapp_1:1.0.0"}, RepoDigests: []string{"localhost:15001/apps/prod_1953_myapp_1@" + digest}},
		{ID: "b", RepoTags: []string{"localhost:15001/apps/prod_1953_myapp_2:1.0.0"}},
	}
	colocated := &config.ReswarmConfig{DockerRegistryURL: "localhost:15001/", DockerMainRepository: "apps/"}

	missing, err := composeServicesWithMissingImages(colocated, 1953, "MyApp", compose, images)
	require.NoError(t, err)
	assert.Empty(t, missing)

	// negative control: as the release names them only
	missing, err = composeServicesWithMissingImages(nil, 1953, "MyApp", compose, images)
	require.NoError(t, err)
	assert.Equal(t, []string{"web", "worker"}, missing)

	// the pin still decides: the same name with another digest is missing
	other := []containerpkg.ImageResult{{ID: "a", RepoTags: images[0].RepoTags, RepoDigests: []string{"localhost:15001/apps/prod_1953_myapp_1@sha256:" + "1111111111111111111111111111111111111111111111111111111111111111"}}, images[1]}
	missing, err = composeServicesWithMissingImages(colocated, 1953, "MyApp", compose, other)
	require.NoError(t, err)
	assert.Equal(t, []string{"web"}, missing)

	// another app's image is not moved onto the device's registry
	missing, err = composeServicesWithMissingImages(colocated, 1954, "Other", compose, images)
	require.NoError(t, err)
	assert.Equal(t, []string{"web", "worker"}, missing)
}
