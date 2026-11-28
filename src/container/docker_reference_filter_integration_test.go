//go:build integration

package container

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRemoveImagesByNameNeedsTheWholeName guards the contract the removal of
// an app relies on (removeDevApp, removeProdApp, the update-flow migration
// pass RegistryImageName): RemoveImagesByName matches a repository by its
// whole name, registry host and path included. A name without them matches
// nothing, which is why the orphan cleanup of DEV apps, which passed
// dev_amd64_<key>_<name>, removed nothing on any architecture.
func TestRemoveImagesByNameNeedsTheWholeName(t *testing.T) {
	docker := newIntegrationDocker(t)
	pullImage(t, docker, busyboxImage)

	shortName := fmt.Sprintf("dev_amd64_%d_refilter", os.Getpid())
	fullName := "registry.example.test/apps/" + shortName
	ref := fullName + ":latest"
	t.Cleanup(func() { forceRemoveImage(docker, fullName) })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	require.NoError(t, docker.Tag(ctx, busyboxImage, ref))

	require.NoError(t, docker.RemoveImagesByName(ctx, shortName, map[string]interface{}{"force": true}))
	_, err := docker.GetImage(ctx, fullName, "latest")
	assert.NoError(t, err, "the name without registry and repository matches nothing")

	require.NoError(t, docker.RemoveImagesByName(ctx, fullName, map[string]interface{}{"force": true}))
	_, err = docker.GetImage(ctx, fullName, "latest")
	assert.Error(t, err, "the whole name matches the image")
}
