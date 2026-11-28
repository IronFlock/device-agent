package apps

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"reagent/common"
	containerpkg "reagent/container"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const (
	pdDigestA = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	pdDigestB = "sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

func TestPushDigestCapture(t *testing.T) {
	read := func(t *testing.T, stream string) *pushDigestCapture {
		t.Helper()
		reader, capture := capturePushDigest(io.NopCloser(strings.NewReader(stream)))
		passed, err := io.ReadAll(reader)
		require.NoError(t, err)
		assert.Equal(t, stream, string(passed), "the stream passes on unchanged")
		require.NoError(t, reader.Close())
		return capture
	}

	t.Run("the aux message of the tag", func(t *testing.T) {
		capture := read(t, `{"status":"Pushed"}`+"\n"+`{"aux":{"Tag":"1.0.0","Digest":"`+pdDigestA+`","Size":1}}`)
		digest, ok := pushedDigestOf("registry.test/apps/x:1.0.0", capture)
		assert.True(t, ok)
		assert.Equal(t, pdDigestA, digest)
		_, ok = pushedDigestOf("registry.test/apps/x:2.0.0", capture)
		assert.False(t, ok, "another tag")
	})

	t.Run("the status line alone (containerd image store)", func(t *testing.T) {
		capture := read(t, `{"status":"Pushed","id":"abc"}`+"\n"+`{"status":"1.0.0: digest: `+pdDigestA+` size: 856"}`+"\n")
		digest, ok := pushedDigestOf("registry.test/apps/x:1.0.0", capture)
		assert.True(t, ok)
		assert.Equal(t, pdDigestA, digest)
		_, ok = pushedDigestOf("registry.test/apps/x:2.0.0", capture)
		assert.False(t, ok, "another tag")
	})

	t.Run("the status line and the aux message agree", func(t *testing.T) {
		capture := read(t, `{"status":"1.0.0: digest: `+pdDigestA+` size: 856"}`+"\n"+`{"aux":{"Tag":"1.0.0","Digest":"`+pdDigestA+`","Size":856}}`+"\n")
		digest, ok := pushedDigestOf("registry.test/apps/x:1.0.0", capture)
		assert.True(t, ok)
		assert.Equal(t, pdDigestA, digest)
	})

	t.Run("none, two different, or not a digest: unknown", func(t *testing.T) {
		for _, stream := range []string{
			`{"status":"Pushed"}` + "\n",
			`{"aux":{"Tag":"1.0.0","Digest":"` + pdDigestA + `"}}` + "\n" + `{"aux":{"Tag":"1.0.0","Digest":"` + pdDigestB + `"}}` + "\n",
			`{"aux":{"Tag":"1.0.0","Digest":"md5:abc"}}` + "\n",
			`{"aux":{"Tag":"1.0.0","Digest":""}}` + "\n",
			`not json` + "\n",
			`{"status":"1.0.0: digest: ` + pdDigestA + ` size: 1"}` + "\n" + `{"aux":{"Tag":"1.0.0","Digest":"` + pdDigestB + `"}}` + "\n",
			`{"status":"1.0.0: digest: sha256:abc size: 1"}` + "\n",
			`{"status":"1.0.0: digest: ` + pdDigestA + `"}` + "\n",
		} {
			_, ok := pushedDigestOf("registry.test/apps/x:1.0.0", read(t, stream))
			assert.False(t, ok, stream)
		}
	})
}

// After `docker compose push` the digest is read from the image's
// RepoDigests entry of exactly that repository; where it carries two, which
// one this push wrote cannot be told.
func TestRepoDigestOf(t *testing.T) {
	sm, mc, _, _, _ := wiredRunBuildSM(t)
	ref := "registry.test/apps/prod_arm64_12_demo_1:1.0.0"

	mc.EXPECT().GetImage(mock.Anything, "registry.test/apps/prod_arm64_12_demo_1", "1.0.0").
		Return(containerpkg.ImageResult{RepoDigests: []string{
			"registry.test/apps/prod_arm64_12_demo_1@" + pdDigestA,
			"registry.test/apps/prod_arm64_12_demo_10@" + pdDigestB, // another repository
		}}, nil).Once()
	digest, ok := sm.repoDigestOf(ref)
	assert.True(t, ok)
	assert.Equal(t, pdDigestA, digest)

	mc.EXPECT().GetImage(mock.Anything, "registry.test/apps/prod_arm64_12_demo_1", "1.0.0").
		Return(containerpkg.ImageResult{RepoDigests: []string{
			"registry.test/apps/prod_arm64_12_demo_1@" + pdDigestA,
			"registry.test/apps/prod_arm64_12_demo_1@" + pdDigestB,
		}}, nil).Once()
	_, ok = sm.repoDigestOf(ref)
	assert.False(t, ok, "two digests of the repository")

	mc.EXPECT().GetImage(mock.Anything, "registry.test/apps/prod_arm64_12_demo_1", "1.0.0").
		Return(containerpkg.ImageResult{}, imageNotFound()).Once()
	_, ok = sm.repoDigestOf(ref)
	assert.False(t, ok, "no image")
}

func TestComposePushedImages(t *testing.T) {
	path := filepath.Join(t.TempDir(), DockerFileName)
	require.NoError(t, os.WriteFile(path, []byte(`{"services":{
		"web":{"build":".","image":"registry.test/apps/prod_arm64_12_demo_1:1.0.0"},
		"db":{"image":"registry.test/apps/prod_arm64_12_demo_2:1.0.0","x-source-image":"postgres:16"},
		"tool":{"build":"."}
	}}`), 0o600))
	assert.Equal(t, []string{"registry.test/apps/prod_arm64_12_demo_1:1.0.0"}, composePushedImages(path))
	assert.Empty(t, composePushedImages(filepath.Join(t.TempDir(), "missing.json")))
}

// The image-only services of a Compose release build are pushed one by one;
// each push's digest comes from its stream.
func TestRehostSourceImagesAnswersThePushedDigests(t *testing.T) {
	sm, mc, _, _, _ := wiredRunBuildSM(t)
	payload := execPayload("demo", common.PUBLISHED, common.DEV)
	known := "registry.test/apps/prod_arm64_1_demo_1:1.0.0"
	unknown := "registry.test/apps/prod_arm64_1_demo_2:1.0.0"
	payload.DockerCompose = map[string]interface{}{"services": map[string]interface{}{
		"db":    map[string]interface{}{"image": known, "x-source-image": "postgres:16"},
		"cache": map[string]interface{}{"image": unknown, "x-source-image": "redis:7"},
		"web":   map[string]interface{}{"image": "registry.test/apps/prod_arm64_1_demo_3:1.0.0", "build": "."},
	}}

	mc.EXPECT().Pull(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, _ string, _ containerpkg.PullOptions) (io.ReadCloser, error) {
			return fwdDockerStream(), nil
		}).Twice()
	mc.EXPECT().Tag(mock.Anything, "postgres:16", known).Return(nil).Once()
	mc.EXPECT().Tag(mock.Anything, "redis:7", unknown).Return(nil).Once()
	mc.EXPECT().Push(mock.Anything, known, mock.Anything).Return(fwdDockerStream(
		`{"status":"1.0.0: digest: `+pdDigestA+` size: 1"}`,
		`{"aux":{"Tag":"1.0.0","Digest":"`+pdDigestA+`","Size":1}}`), nil).Once()
	mc.EXPECT().Push(mock.Anything, unknown, mock.Anything).Return(fwdDockerStream(), nil).Once()
	fwdAllowLogs(mc)

	digests, err := sm.rehostSourceImages(payload)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{known: pdDigestA}, digests)
}
