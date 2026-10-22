package api

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reagent/common"
	"reagent/config"
	"reagent/container"
	"reagent/errdefs"
	"reagent/filesystem"
	"reagent/logging"
	"reagent/messenger"
	"reagent/messenger/topics"
	"reagent/store"
	"reagent/system"
	"reagent/testutil/builders"
	"reagent/testutil/fakes"
	"reagent/testutil/mocks"
	"testing"

	dockercontainer "github.com/docker/docker/api/types/container"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// newSystem builds a real *system.System wired to a fake messenger. With an
// empty RemoteUpdateURL, System.GetLatestVersion fails fast (the URL has no
// scheme, so net/http rejects it in microseconds without any network I/O), so
// handlers that call it deterministically take their error branch.
func newSystem(cfg *config.Config) *system.System {
	sys := system.New(cfg, fakes.NewMessenger())
	return &sys
}

// =============================================================================
// listContainersHandler - passes the docker container slice straight through
// =============================================================================

func TestListContainersHandler(t *testing.T) {
	t.Run("returns the container slice when privileged", func(t *testing.T) {
		cont := mocks.NewContainer(t)
		containers := []dockercontainer.Summary{
			{ID: "abc123", Names: []string{"/app_one"}, Image: "img:1"},
			{ID: "def456", Names: []string{"/app_two"}, Image: "img:2"},
		}
		cont.EXPECT().GetContainers(mock.Anything).Return(containers, nil).Once()

		ex := &External{Container: cont, Privilege: priv(t, true)}

		res, err := ex.listContainersHandler(context.Background(), messenger.Result{
			Details: systemDetails(),
		})

		require.NoError(t, err)
		require.NotNil(t, res)
		require.Len(t, res.Arguments, 1)

		got, ok := res.Arguments[0].([]dockercontainer.Summary)
		require.True(t, ok)
		assert.Equal(t, containers, got)
	})

	t.Run("propagates container error", func(t *testing.T) {
		cont := mocks.NewContainer(t)
		cont.EXPECT().GetContainers(mock.Anything).Return(nil, errors.New("docker down")).Once()

		ex := &External{Container: cont, Privilege: priv(t, true)}

		res, err := ex.listContainersHandler(context.Background(), messenger.Result{
			Details: systemDetails(),
		})

		require.Error(t, err)
		assert.Nil(t, res)
	})

	t.Run("denies unprivileged caller without touching the container", func(t *testing.T) {
		// Strict mock: GetContainers must NOT be called when access is denied.
		cont := mocks.NewContainer(t)
		details, m := grantPrivilege(false)
		ex := &External{Container: cont, Privilege: newPrivilege(testConfig(), m)}

		res, err := ex.listContainersHandler(context.Background(), messenger.Result{
			Details: details,
		})

		require.Error(t, err)
		assert.Nil(t, res)
		assert.True(t, errdefs.IsInsufficientPrivileges(err))
	})
}

// =============================================================================
// getImagesHandler - flattens []ImageResult into an []interface{} arg list
// =============================================================================

func TestGetImagesHandler(t *testing.T) {
	t.Run("flattens image results into the argument list", func(t *testing.T) {
		cont := mocks.NewContainer(t)
		results := []container.ImageResult{
			{ID: "sha256:aaa", Size: 100, RepoTags: []string{"img:1"}},
			{ID: "sha256:bbb", Size: 200, RepoTags: []string{"img:2"}},
		}
		// The handler builds its own context.WithTimeout and passes nil options.
		cont.EXPECT().ListImages(mock.Anything, mock.Anything).Return(results, nil).Once()

		ex := &External{Container: cont, Privilege: priv(t, true)}

		res, err := ex.getImagesHandler(context.Background(), messenger.Result{
			Details: systemDetails(),
		})

		require.NoError(t, err)
		require.NotNil(t, res)
		require.Len(t, res.Arguments, 2)
		assert.Equal(t, results[0], res.Arguments[0])
		assert.Equal(t, results[1], res.Arguments[1])
	})

	t.Run("returns empty (non-nil) argument list for no images", func(t *testing.T) {
		cont := mocks.NewContainer(t)
		cont.EXPECT().ListImages(mock.Anything, mock.Anything).
			Return([]container.ImageResult{}, nil).Once()

		ex := &External{Container: cont, Privilege: priv(t, true)}

		res, err := ex.getImagesHandler(context.Background(), messenger.Result{
			Details: systemDetails(),
		})

		require.NoError(t, err)
		require.NotNil(t, res)
		assert.NotNil(t, res.Arguments)
		assert.Empty(t, res.Arguments)
	})

	t.Run("propagates list error", func(t *testing.T) {
		cont := mocks.NewContainer(t)
		cont.EXPECT().ListImages(mock.Anything, mock.Anything).
			Return(nil, errors.New("list failed")).Once()

		ex := &External{Container: cont, Privilege: priv(t, true)}

		res, err := ex.getImagesHandler(context.Background(), messenger.Result{
			Details: systemDetails(),
		})

		require.Error(t, err)
		assert.Nil(t, res)
	})

	t.Run("denies unprivileged caller", func(t *testing.T) {
		cont := mocks.NewContainer(t)
		details, m := grantPrivilege(false)
		ex := &External{Container: cont, Privilege: newPrivilege(testConfig(), m)}

		res, err := ex.getImagesHandler(context.Background(), messenger.Result{
			Details: details,
		})

		require.Error(t, err)
		assert.Nil(t, res)
		assert.True(t, errdefs.IsInsufficientPrivileges(err))
	})
}

// =============================================================================
// pruneImageHandler - all=true -> PruneSystem, else PruneDanglingImages
// =============================================================================

func TestPruneImageHandler(t *testing.T) {
	t.Run("all=true runs a full system prune and returns its output", func(t *testing.T) {
		cont := mocks.NewContainer(t)
		cont.EXPECT().PruneSystem().Return("reclaimed 1GB", nil).Once()

		ex := &External{Container: cont, Privilege: priv(t, true)}

		res, err := ex.pruneImageHandler(context.Background(), messenger.Result{
			Details:   systemDetails(),
			Arguments: []interface{}{map[string]interface{}{"all": true}},
		})

		require.NoError(t, err)
		require.NotNil(t, res)
		require.Len(t, res.Arguments, 1)
		assert.Equal(t, "reclaimed 1GB", res.Arguments[0])
	})

	t.Run("all=false prunes only dangling images", func(t *testing.T) {
		cont := mocks.NewContainer(t)
		cont.EXPECT().PruneDanglingImages(mock.Anything).Return("", nil).Once()

		ex := &External{Container: cont, Privilege: priv(t, true)}

		res, err := ex.pruneImageHandler(context.Background(), messenger.Result{
			Details:   systemDetails(),
			Arguments: []interface{}{map[string]interface{}{"all": false}},
		})

		require.NoError(t, err)
		require.NotNil(t, res)
		assert.Empty(t, res.Arguments)
	})

	t.Run("missing all key prunes only dangling images", func(t *testing.T) {
		cont := mocks.NewContainer(t)
		cont.EXPECT().PruneDanglingImages(mock.Anything).Return("", nil).Once()

		ex := &External{Container: cont, Privilege: priv(t, true)}

		res, err := ex.pruneImageHandler(context.Background(), messenger.Result{
			Details:   systemDetails(),
			Arguments: []interface{}{map[string]interface{}{}},
		})

		require.NoError(t, err)
		require.NotNil(t, res)
		assert.Empty(t, res.Arguments)
	})

	t.Run("propagates PruneSystem error", func(t *testing.T) {
		cont := mocks.NewContainer(t)
		cont.EXPECT().PruneSystem().Return("", errors.New("prune boom")).Once()

		ex := &External{Container: cont, Privilege: priv(t, true)}

		res, err := ex.pruneImageHandler(context.Background(), messenger.Result{
			Details:   systemDetails(),
			Arguments: []interface{}{map[string]interface{}{"all": true}},
		})

		require.Error(t, err)
		assert.Nil(t, res)
	})

	t.Run("propagates PruneDanglingImages error", func(t *testing.T) {
		cont := mocks.NewContainer(t)
		cont.EXPECT().PruneDanglingImages(mock.Anything).Return("", errors.New("dangling boom")).Once()

		ex := &External{Container: cont, Privilege: priv(t, true)}

		res, err := ex.pruneImageHandler(context.Background(), messenger.Result{
			Details:   systemDetails(),
			Arguments: []interface{}{map[string]interface{}{}},
		})

		require.Error(t, err)
		assert.Nil(t, res)
	})

	// The options are optional. The check for them was inverted and indexed
	// the argument list whenever it was nil or empty, panicking the client's
	// invocation goroutine, which nothing recovers.
	for _, args := range [][]interface{}{nil, {}, {nil}} {
		t.Run(fmt.Sprintf("absent options %#v prune only dangling images", args), func(t *testing.T) {
			cont := mocks.NewContainer(t)
			cont.EXPECT().PruneDanglingImages(mock.Anything).Return("", nil).Once()

			ex := &External{Container: cont, Privilege: priv(t, true)}

			require.NotPanics(t, func() {
				res, err := ex.pruneImageHandler(context.Background(), messenger.Result{
					Details:   systemDetails(),
					Arguments: args,
				})

				require.NoError(t, err)
				require.NotNil(t, res)
			})
		})
	}

	t.Run("rejects non-dict first argument", func(t *testing.T) {
		// Validation happens before any container call; strict mock stays untouched.
		cont := mocks.NewContainer(t)
		ex := &External{Container: cont, Privilege: priv(t, true)}

		res, err := ex.pruneImageHandler(context.Background(), messenger.Result{
			Details:   systemDetails(),
			Arguments: []interface{}{"not-a-dict"},
		})

		require.Error(t, err)
		assert.Nil(t, res)
	})

	t.Run("rejects non-boolean all value", func(t *testing.T) {
		cont := mocks.NewContainer(t)
		ex := &External{Container: cont, Privilege: priv(t, true)}

		res, err := ex.pruneImageHandler(context.Background(), messenger.Result{
			Details:   systemDetails(),
			Arguments: []interface{}{map[string]interface{}{"all": "yes"}},
		})

		require.Error(t, err)
		assert.Nil(t, res)
	})

	t.Run("denies unprivileged caller without touching the container", func(t *testing.T) {
		cont := mocks.NewContainer(t)
		details, m := grantPrivilege(false)
		ex := &External{Container: cont, Privilege: newPrivilege(testConfig(), m)}

		res, err := ex.pruneImageHandler(context.Background(), messenger.Result{
			Details:   details,
			Arguments: []interface{}{map[string]interface{}{"all": true}},
		})

		require.Error(t, err)
		assert.Nil(t, res)
		assert.True(t, errdefs.IsInsufficientPrivileges(err))
	})
}

// =============================================================================
// getAgentMetadataHandler - base metadata dict shaping + privilege gate
//
// With an empty RemoteUpdateURL, System.GetLatestVersion fails fast, so the
// handler returns the base dict WITHOUT the version-comparison keys.
// =============================================================================

func TestGetAgentMetadataHandler(t *testing.T) {
	t.Run("returns base metadata dict when privileged", func(t *testing.T) {
		cfg := testConfig()
		cfg.ReswarmConfig.SerialNumber = "SN-12345"

		ex := &External{
			Config:    cfg,
			System:    newSystem(cfg),
			Privilege: priv(t, true),
		}

		res, err := ex.getAgentMetadataHandler(context.Background(), messenger.Result{
			Details: systemDetails(),
		})

		require.NoError(t, err)
		require.NotNil(t, res)
		require.Len(t, res.Arguments, 1)

		dict, ok := res.Arguments[0].(common.Dict)
		require.True(t, ok)

		// Base keys are always present.
		for _, key := range []string{
			"os", "arch", "variant", "version",
			"serialNumber", "canUpdate", "OSVersion",
		} {
			assert.Contains(t, dict, key, "missing base key %q", key)
		}
		assert.Equal(t, "SN-12345", dict["serialNumber"])
		assert.Equal(t, true, dict["canUpdate"])

		// GetLatestVersion failed (bad URL), so these are omitted.
		assert.NotContains(t, dict, "latestVersion")
		assert.NotContains(t, dict, "hasLatest")
		assert.NotContains(t, dict, "latestAgentVersion")
	})

	t.Run("denies unprivileged caller", func(t *testing.T) {
		details, m := grantPrivilege(false)
		ex := &External{Config: testConfig(), Privilege: newPrivilege(testConfig(), m)}

		res, err := ex.getAgentMetadataHandler(context.Background(), messenger.Result{
			Details: details,
		})

		require.Error(t, err)
		assert.Nil(t, res)
		assert.True(t, errdefs.IsInsufficientPrivileges(err))
	})

	t.Run("propagates privilege check error", func(t *testing.T) {
		m := fakes.NewMessenger()
		m.SetCallError(string(topics.CheckPrivilege), errors.New("rpc boom"))
		ex := &External{Config: testConfig(), Privilege: newPrivilege(testConfig(), m)}

		res, err := ex.getAgentMetadataHandler(context.Background(), messenger.Result{
			Details: common.Dict{"caller_authid": "999"},
		})

		require.Error(t, err)
		assert.Nil(t, res)
		assert.False(t, errdefs.IsInsufficientPrivileges(err))
	})
}

// =============================================================================
// writeToFileHandler - writes a hex data chunk into the build dir on disk
//
// The handler reads the destination dir from ex.Messenger.GetConfig() and
// writes via the real filesystem. A data chunk only publishes progress when an
// active transfer exists, so the LogManager is wired with a fake messenger to
// keep the async safe.Go progress publish harmless.
// =============================================================================

func TestWriteToFileHandler(t *testing.T) {
	// newWriteEx builds an External whose build dir is a temp dir and whose
	// Filesystem is seeded with an active transfer for `container` so a data
	// chunk is accepted and written.
	newWriteEx := func(t *testing.T, buildDir string) (*External, *filesystem.Filesystem) {
		t.Helper()

		cfg := builders.DefaultTestConfig()
		cfg.CommandLineArguments.AppsBuildDir = buildDir
		fakeMsg := fakes.NewMessengerWithConfig(cfg)

		fs := filesystem.New()

		lm := logging.NewLogManager(nil, fakeMsg, nil, store.AppStore{})

		ex := &External{
			Messenger:  fakeMsg,
			Filesystem: &fs,
			LogManager: &lm,
			Privilege:  priv(t, true),
		}
		return ex, &fs
	}

	t.Run("writes a hex-decoded data chunk to disk", func(t *testing.T) {
		buildDir := t.TempDir()
		ex, fs := newWriteEx(t, buildDir)

		const containerName = "dev_100_myapp"
		const fileName = "payload.bin"

		// Open the transfer directly so the handler's BEGIN safe.Go path (which
		// touches the LogManager DB) is not exercised.
		require.NoError(t, fs.Write(filesystem.FileChunk{
			ID:            "t1",
			FileName:      fileName,
			FilePath:      buildDir,
			Data:          "BEGIN",
			ContainerName: containerName,
			Total:         4,
		}))

		payload := []byte{0xDE, 0xAD, 0xBE, 0xEF}
		hexData := hex.EncodeToString(payload)

		res, err := ex.writeToFileHandler(context.Background(), messenger.Result{
			Details: systemDetails(),
			Arguments: []interface{}{
				hexData,       // chunk
				fileName,      // fileName
				containerName, // containerName
				uint64(4),     // total
				"t1",          // id
			},
		})

		require.NoError(t, err)
		require.NotNil(t, res)

		got, readErr := os.ReadFile(filepath.Join(buildDir, fileName))
		require.NoError(t, readErr)
		assert.Equal(t, payload, got)
	})

	t.Run("stale chunk ID is ignored and writes nothing", func(t *testing.T) {
		// A chunk whose ID does not match the active transfer is silently
		// dropped by Filesystem.Write (returns nil), and the handler reports
		// progress on the still-active transfer via safe.Go(PublishProgress),
		// which only needs the fake messenger. Nothing is written to disk.
		buildDir := t.TempDir()
		ex, fs := newWriteEx(t, buildDir)

		const containerName = "dev_102_thirdapp"
		const fileName = "mismatch.bin"

		require.NoError(t, fs.Write(filesystem.FileChunk{
			ID:            "real-id",
			FileName:      fileName,
			FilePath:      buildDir,
			Data:          "BEGIN",
			ContainerName: containerName,
			Total:         2,
		}))

		// Chunk with a stale/mismatched transfer ID: Write logs + returns nil,
		// the handler then reports progress on the still-active transfer.
		res, err := ex.writeToFileHandler(context.Background(), messenger.Result{
			Details: systemDetails(),
			Arguments: []interface{}{
				hex.EncodeToString([]byte{0x01, 0x02}),
				fileName,
				containerName,
				uint64(2),
				"stale-id",
			},
		})

		require.NoError(t, err)
		require.NotNil(t, res)

		// Nothing was written because the chunk's ID did not match the transfer.
		got, readErr := os.ReadFile(filepath.Join(buildDir, fileName))
		require.NoError(t, readErr)
		assert.Empty(t, got)
	})

	t.Run("denies unprivileged caller", func(t *testing.T) {
		details, m := grantPrivilege(false)
		ex := &External{Privilege: newPrivilege(testConfig(), m)}

		res, err := ex.writeToFileHandler(context.Background(), messenger.Result{
			Details: details,
			Arguments: []interface{}{
				"BEGIN", "f", "c", uint64(0), "id",
			},
		})

		require.Error(t, err)
		assert.Nil(t, res)
		assert.True(t, errdefs.IsInsufficientPrivileges(err))
	})

	parseErrCases := []struct {
		name string
		args []interface{}
	}{
		{
			name: "bad fileName type",
			args: []interface{}{"BEGIN", 123, "c", uint64(0), "id"},
		},
		{
			name: "bad chunk type",
			args: []interface{}{123, "f", "c", uint64(0), "id"},
		},
		{
			name: "bad containerName type",
			args: []interface{}{"BEGIN", "f", 123, uint64(0), "id"},
		},
		{
			name: "bad total type",
			args: []interface{}{"BEGIN", "f", "c", "four", "id"},
		},
		{
			name: "bad id type",
			args: []interface{}{"BEGIN", "f", "c", uint64(0), 123},
		},
	}

	for _, tc := range parseErrCases {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			ex := &External{Privilege: priv(t, true)}

			res, err := ex.writeToFileHandler(context.Background(), messenger.Result{
				Details:   systemDetails(),
				Arguments: tc.args,
			})

			require.Error(t, err)
			assert.Nil(t, res)
		})
	}

	// Indexed, a short list panicked the client's invocation goroutine, which
	// nothing recovers, and took the agent down.
	for _, args := range [][]interface{}{nil, {}, {"BEGIN", "f", "c", uint64(0)}} {
		t.Run(fmt.Sprintf("rejects the short argument list %#v", args), func(t *testing.T) {
			ex := &External{Privilege: priv(t, true)}

			require.NotPanics(t, func() {
				res, err := ex.writeToFileHandler(context.Background(), messenger.Result{
					Details:   systemDetails(),
					Arguments: args,
				})

				require.Error(t, err)
				assert.Nil(t, res)
			})
		})
	}
}
