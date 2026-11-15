package apps

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"reagent/common"
	"reagent/errdefs"
	"reagent/testutil/fakes"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// A failed build must tell the developer why on the build console. Before,
// a failure ahead of the image build (seen in the field: a Python .venv in
// the app folder, whose symlinks the unpacker refuses) left the console at
// "File transfer has finished, starting build..." and the app FAILED, with
// the reason only in the agent's journal.

// consoleLines returns the build-console lines the agent published for a
// container (LogManager.Write publishes {"type":"build","chunk":text}).
func consoleLines(msg *fakes.Messenger, containerName string) []string {
	lines := make([]string, 0)
	for _, call := range msg.GetPublishCalls() {
		if !strings.HasSuffix(string(call.Topic), "."+containerName) || len(call.Args) == 0 {
			continue
		}
		payload, ok := call.Args[0].(common.Dict)
		if !ok {
			continue
		}
		if chunk, ok := payload["chunk"].(string); ok {
			lines = append(lines, chunk)
		}
	}
	return lines
}

// writeAppArchiveWithSymlink writes the app archive the transfer leaves in the
// build dir: a docker-compose app whose folder holds a virtualenv symlink, as
// the backend's tar packs it ("./" prefixed, the link kept as a link).
func writeAppArchiveWithSymlink(t *testing.T, buildDir, appName string) {
	t.Helper()

	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	body := "services:\n  app:\n    build: .\n"
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0755}))
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "./docker-compose.yml", Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(body))}))
	_, err := tw.Write([]byte(body))
	require.NoError(t, err)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "./.venv/", Typeflag: tar.TypeDir, Mode: 0755}))
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "./.venv/bin/", Typeflag: tar.TypeDir, Mode: 0755}))
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "./.venv/bin/python", Typeflag: tar.TypeSymlink, Linkname: "python3.13", Mode: 0777}))
	require.NoError(t, tw.Close())
	require.NoError(t, gzw.Close())

	require.NoError(t, os.MkdirAll(buildDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(buildDir, appName+".tgz"), buf.Bytes(), 0644))
}

func TestBuildFailureReachesBuildConsole(t *testing.T) {
	t.Run("compose app whose folder holds a symlink: the console names it", func(t *testing.T) {
		sm, _, st, msg, cfg := wiredRunBuildSM(t)

		app := seedApp(t, st, "plc-collector", common.REMOVED, common.DEV)
		payload := execPayload("plc-collector", common.BUILT, common.DEV)
		payload.DockerCompose = map[string]interface{}{
			"services": map[string]interface{}{"app": map[string]interface{}{"build": "."}},
		}
		writeAppArchiveWithSymlink(t, cfg.CommandLineArguments.AppsBuildDir, payload.AppName)

		err := sm.buildApp(payload, app)
		require.Error(t, err)

		lines := consoleLines(msg, payload.ContainerName.Dev)
		require.NotEmpty(t, lines, "the failure must be written to the build console")
		assert.Equal(t,
			"Build failed: could not unpack the app: .venv/bin/python is a symbolic link; only regular files and folders can be sent to the device. Delete it or add it to .dockerignore",
			lines[len(lines)-1])

		// It failed while unpacking, before the image build began.
		assert.NotContains(t, remoteStatesFor(msg), common.BUILDING)
	})

	t.Run("missing Dockerfile keeps its friendly message, written once", func(t *testing.T) {
		sm, mc, st, msg, _ := wiredRunBuildSM(t)

		app := seedApp(t, st, "no-dockerfile", common.REMOVED, common.DEV)
		payload := execPayload("no-dockerfile", common.BUILT, common.DEV)

		mc.EXPECT().
			Build(mock.Anything, mock.Anything, mock.Anything).
			Return(nil, errdefs.DockerfileIsMissing(errors.New("Cannot locate specified Dockerfile"))).
			Once()
		fwdAllowLogs(mc)

		err := sm.buildApp(payload, app)
		require.Error(t, err)
		assert.True(t, errdefs.IsDockerfileIsMissing(err), "the error must reach the caller unwrapped, got %T", err)

		failures := 0
		for _, line := range consoleLines(msg, payload.ContainerName.Dev) {
			if strings.HasPrefix(line, "Build failed:") {
				failures++
				assert.Equal(t, "Build failed: could not find a Dockerfile, please create a Dockerfile in the root of your project", line)
			}
		}
		assert.Equal(t, 1, failures, "exactly one failure line")
	})

	t.Run("a release build writes to the publish console", func(t *testing.T) {
		sm, mc, st, msg, _ := wiredRunBuildSM(t)

		app := seedApp(t, st, "publish-fail", common.REMOVED, common.DEV)
		payload := execPayload("publish-fail", common.PUBLISHED, common.DEV)
		payload.PublishContainerName = "publish_1_publish-fail"

		mc.EXPECT().
			Build(mock.Anything, mock.Anything, mock.Anything).
			Return(fwdErrorStream("compilation error"), nil).
			Once()
		fwdAllowLogs(mc)

		err := sm.buildDevApp(payload, app, true)
		require.Error(t, err)

		lines := consoleLines(msg, payload.PublishContainerName)
		require.NotEmpty(t, lines)
		assert.Equal(t, "Build failed: compilation error", lines[len(lines)-1])
	})

	t.Run("a canceled build is not reported as failed", func(t *testing.T) {
		sm, mc, st, msg, _ := wiredRunBuildSM(t)

		app := seedApp(t, st, "build-cancel", common.REMOVED, common.DEV)
		payload := execPayload("build-cancel", common.BUILT, common.DEV)

		mc.EXPECT().
			Build(mock.Anything, mock.Anything, mock.Anything).
			Return(fwdCanceledStream(), nil).
			Once()
		fwdAllowLogs(mc)

		err := sm.buildApp(payload, app)
		require.Error(t, err)
		assert.True(t, errdefs.IsDockerStreamCanceled(err))

		for _, line := range consoleLines(msg, payload.ContainerName.Dev) {
			assert.NotContains(t, line, "Build failed")
		}
	})
}
