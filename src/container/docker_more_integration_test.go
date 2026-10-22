//go:build integration

package container

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dockerbuild "github.com/docker/docker/api/types/build"

	bcontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// uniqueName returns a docker-safe, run-unique identifier derived from the test
// name and the process id. Back-to-back integration runs share one daemon, so
// every image/container created here must carry a name that cannot collide with
// a leftover from a previous (possibly crashed) run.
func uniqueName(t *testing.T, prefix string) string {
	t.Helper()
	return fmt.Sprintf("%s-%s-%d", prefix, sanitize(t.Name()), os.Getpid())
}

// buildTinyImageTar writes a trivial build context (just a Dockerfile) to a tar
// file under t.TempDir() and returns its path. Docker's Build takes a path to a
// tar that serves as the build context; the caller supplies the Dockerfile body.
func buildTinyImageTar(t *testing.T, dockerfile string) string {
	t.Helper()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	body := []byte(dockerfile)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "Dockerfile",
		Mode: 0o600,
		Size: int64(len(body)),
	}))
	_, err := tw.Write(body)
	require.NoError(t, err)
	require.NoError(t, tw.Close())

	tarPath := filepath.Join(t.TempDir(), "context.tar")
	require.NoError(t, os.WriteFile(tarPath, buf.Bytes(), 0o600))
	return tarPath
}

// forceRemoveImage best-effort removes an image by reference, swallowing
// "no such"/not-found errors so cleanup never fails a run on a shared daemon.
func forceRemoveImage(docker *Docker, ref string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = docker.RemoveImagesByName(ctx, ref, map[string]interface{}{"force": true})
}

// forceRemoveContainer best-effort removes a container by id, ignoring missing
// containers so a partially-created test still cleans up.
func forceRemoveContainer(docker *Docker, id string) {
	if id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = docker.RemoveContainerByID(ctx, id, map[string]interface{}{"force": true, "removeVolumes": true})
}

// pullImage pulls a base image and drains the progress stream so the operation
// actually completes before the test proceeds.
func pullImage(t *testing.T, docker *Docker, ref string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	reader, err := docker.Pull(ctx, ref, PullOptions{})
	require.NoError(t, err, "pull of %s should succeed", ref)
	drain(t, reader)
}

const busyboxImage = "busybox:latest"

// TestIntegrationDockerBuildAndImageOps exercises Build (from an in-memory
// Dockerfile/tar), then GetImage / RemoveImageByName / Tag against the freshly
// built image. The build uses busybox as the base so it needs only a single
// small pull and no network at build time.
func TestIntegrationDockerBuildAndImageOps(t *testing.T) {
	docker := newIntegrationDocker(t)

	// Build's base image must be present locally or pullable. busybox is tiny.
	pullImage(t, docker, busyboxImage)

	repo := strings.ToLower(uniqueName(t, "reagent-build"))
	tag := "v1"
	ref := repo + ":" + tag

	t.Cleanup(func() { forceRemoveImage(docker, ref) })

	dockerfile := "FROM " + busyboxImage + "\nLABEL reagent.test=build\nCMD [\"true\"]\n"
	tarPath := buildTinyImageTar(t, dockerfile)

	buildCtx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	reader, err := docker.Build(buildCtx, tarPath, dockerbuild.ImageBuildOptions{
		Tags:        []string{ref},
		Remove:      true,
		ForceRemove: true,
	})
	require.NoError(t, err, "Build should succeed for a trivial Dockerfile")
	// The build only actually runs as the response body is consumed.
	drain(t, reader)

	// GetImage should now find the built image by repo+tag.
	getCtx, cancelGet := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelGet()
	img, err := docker.GetImage(getCtx, repo, tag)
	require.NoError(t, err, "GetImage should find the freshly built image")
	require.NotEmpty(t, img.ID, "built image should have an ID")
	assert.Contains(t, img.RepoTags, ref, "RepoTags should include our build tag")

	// Tag the built image under a second reference, then confirm GetImage finds it.
	tagRepo := strings.ToLower(uniqueName(t, "reagent-tagged"))
	tagRef := tagRepo + ":retag"
	t.Cleanup(func() { forceRemoveImage(docker, tagRef) })

	tagCtx, cancelTag := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelTag()
	require.NoError(t, docker.Tag(tagCtx, ref, tagRef), "Tag should create a new reference")

	getTagged, err := docker.GetImage(context.Background(), tagRepo, "retag")
	require.NoError(t, err, "GetImage should find the re-tagged image")
	assert.Equal(t, img.ID, getTagged.ID, "re-tagged image shares the source image ID")

	// RemoveImageByName resolves the reference to its image ID and removes by ID
	// (see Docker.RemoveImageByName -> RemoveImage with image.ID). With force, that
	// untags every reference pointing at the shared image, so BOTH the secondary
	// tag and the original build tag disappear. Assert that real behavior.
	rmCtx, cancelRm := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelRm()
	require.NoError(t, docker.RemoveImageByName(rmCtx, tagRepo, "retag", map[string]interface{}{"force": true}),
		"RemoveImageByName should remove the underlying image by id")

	_, err = docker.GetImage(context.Background(), tagRepo, "retag")
	assert.Error(t, err, "removed secondary tag should no longer be found")

	// Removing by image id untags all references of the shared image.
	_, err = docker.GetImage(context.Background(), repo, tag)
	assert.Error(t, err, "original build tag is also gone once the shared image id is removed")
}

// TestIntegrationDockerPruneImages exercises PruneDanglingImages and PruneImages
// against the live daemon. Both must succeed; we don't assert on the amount
// reclaimed because a shared daemon's dangling set is non-deterministic.
func TestIntegrationDockerPruneImages(t *testing.T) {
	docker := newIntegrationDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// PruneDanglingImages shells out to `docker image prune -f`. It must return
	// without error on a healthy daemon; output may be empty.
	out, err := docker.PruneDanglingImages(ctx)
	require.NoError(t, err, "PruneDanglingImages should succeed")
	_ = out // output content is non-deterministic; presence of no error is enough.

	// PruneImages uses the API. all=false prunes only dangling images.
	require.NoError(t, docker.PruneImages(ctx, map[string]interface{}{"all": false}),
		"PruneImages(all=false) should succeed")
}

// TestIntegrationDockerGetAndListContainersFiltering creates a single throwaway
// container and asserts it is discoverable via GetContainers and via a filtered
// ListContainers (name filter). This exercises the filter-parsing path of
// ListContainers with a real filters.Args value.
func TestIntegrationDockerGetAndListContainersFiltering(t *testing.T) {
	docker := newIntegrationDocker(t)

	pullImage(t, docker, busyboxImage)

	name := strings.ToLower(uniqueName(t, "reagent-list"))

	createCtx, cancelCreate := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelCreate()
	// A short sleep keeps the container alive long enough to be listed in the
	// running state, then it exits on its own.
	id, err := docker.CreateContainer(
		createCtx,
		bcontainer.Config{Image: busyboxImage, Cmd: []string{"sleep", "30"}},
		bcontainer.HostConfig{},
		network.NetworkingConfig{},
		name,
	)
	require.NoError(t, err, "create container should succeed")
	require.NotEmpty(t, id)
	t.Cleanup(func() { forceRemoveContainer(docker, id) })

	require.NoError(t, docker.StartContainer(context.Background(), id), "start should succeed")

	// GetContainers returns all containers; ours must be present by id.
	getCtx, cancelGet := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelGet()
	all, err := docker.GetContainers(getCtx)
	require.NoError(t, err)
	found := false
	for _, c := range all {
		if c.ID == id {
			found = true
			break
		}
	}
	assert.True(t, found, "GetContainers should include the created container")

	// ListContainers with a name filter must return exactly our container.
	nameFilter := filters.NewArgs()
	nameFilter.Add("name", name)

	listCtx, cancelList := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelList()
	results, err := docker.ListContainers(listCtx, map[string]interface{}{
		"all":     true,
		"filters": nameFilter,
	})
	require.NoError(t, err, "filtered ListContainers should succeed")
	require.NotEmpty(t, results, "name-filtered list should include our container")

	matched := false
	for _, r := range results {
		if strings.HasPrefix(r.ID, id) || r.ID == id {
			matched = true
			for _, n := range r.Names {
				assert.Contains(t, n, name, "listed container name should match the filter")
			}
		}
	}
	assert.True(t, matched, "filtered list should contain the created container id")
}

// TestIntegrationDockerWaitForContainerByName creates a container that exits
// with a known non-zero code and asserts WaitForContainerByName resolves the
// container by name and reports that exit code.
func TestIntegrationDockerWaitForContainerByName(t *testing.T) {
	docker := newIntegrationDocker(t)

	pullImage(t, docker, busyboxImage)

	name := strings.ToLower(uniqueName(t, "reagent-wait"))

	createCtx, cancelCreate := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelCreate()
	// Exit deterministically with code 7 so we can assert the wait result.
	id, err := docker.CreateContainer(
		createCtx,
		bcontainer.Config{Image: busyboxImage, Cmd: []string{"sh", "-c", "exit 7"}},
		bcontainer.HostConfig{},
		network.NetworkingConfig{},
		name,
	)
	require.NoError(t, err)
	require.NotEmpty(t, id)
	t.Cleanup(func() { forceRemoveContainer(docker, id) })

	require.NoError(t, docker.StartContainer(context.Background(), id))

	waitCtx, cancelWait := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelWait()
	code, err := docker.WaitForContainerByName(waitCtx, name, bcontainer.WaitConditionNotRunning)
	require.NoError(t, err, "WaitForContainerByName should resolve and wait")
	assert.Equal(t, int64(7), code, "container should exit with the configured code")
}

// TestIntegrationDockerExecCommand starts a long-running container and runs a
// command inside it via ExecCommand, then demultiplexes the hijacked stream and
// asserts the expected stdout. This exercises the GetContainer -> ExecCreate ->
// ExecAttach path against a live daemon.
func TestIntegrationDockerExecCommand(t *testing.T) {
	docker := newIntegrationDocker(t)

	pullImage(t, docker, busyboxImage)

	name := strings.ToLower(uniqueName(t, "reagent-exec"))

	createCtx, cancelCreate := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelCreate()
	// Keep the container alive long enough to exec into it.
	id, err := docker.CreateContainer(
		createCtx,
		bcontainer.Config{Image: busyboxImage, Cmd: []string{"sleep", "60"}},
		bcontainer.HostConfig{},
		network.NetworkingConfig{},
		name,
	)
	require.NoError(t, err)
	require.NotEmpty(t, id)
	t.Cleanup(func() { forceRemoveContainer(docker, id) })

	require.NoError(t, docker.StartContainer(context.Background(), id))

	// Ensure the container is actually running before we exec into it.
	runCtx, cancelRun := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelRun()
	runningC, errC := docker.WaitForRunning(runCtx, id, 100*time.Millisecond)
	select {
	case <-runningC:
	case err := <-errC:
		require.NoError(t, err, "container should reach running state")
	case <-runCtx.Done():
		t.Fatal("timed out waiting for container to start running")
	}

	marker := "reagent-exec-marker"
	execCtx, cancelExec := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelExec()
	resp, err := docker.ExecCommand(execCtx, name, []string{"sh", "-c", "echo " + marker})
	require.NoError(t, err, "ExecCommand should attach successfully")
	require.NotNil(t, resp.Conn)
	require.NotEmpty(t, resp.ExecID, "exec should return a non-empty exec id")
	defer resp.Conn.Close()

	// ExecCommand uses Tty=false, so the stream is multiplexed; demux it.
	var stdout, stderr bytes.Buffer
	_ = resp.Conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	_, _ = stdcopy.StdCopy(&stdout, &stderr, resp.Reader)

	assert.Contains(t, stdout.String(), marker, "exec stdout should contain the echoed marker")
}
