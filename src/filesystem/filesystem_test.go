package filesystem

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reagent/config"
	"reagent/errdefs"
	"reagent/testutil/builders"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testFsConfig returns a fresh config seeded from the shared builder, so this
// package's config fixtures stay consistent with the rest of the module.
func testFsConfig(t *testing.T) *config.Config {
	t.Helper()
	return builders.DefaultTestConfig()
}

// tmpScriptCount counts the generated *.sh script files in os.TempDir(), used
// to assert ExecuteAsScript cleans up after itself.
func tmpScriptCount(t *testing.T) int {
	t.Helper()
	entries, err := filepath.Glob(filepath.Join(os.TempDir(), "*.sh"))
	require.NoError(t, err)
	return len(entries)
}

// tarEntry describes one member of a tar archive used in tests.
type tarEntry struct {
	name    string
	body    string // ignored for directories
	isDir   bool
	typflag byte // when non-zero, overrides the auto-derived typeflag
}

// buildTarGz writes a gzip-compressed tar containing the given entries to
// dir/<name> and returns the full path. Files written to disk; the archive
// itself is built fully in memory.
func buildTarGz(t *testing.T, dir, name string, entries []tarEntry) string {
	t.Helper()

	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)

	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0644}
		switch {
		case e.typflag != 0:
			hdr.Typeflag = e.typflag
			if e.typflag == tar.TypeReg {
				hdr.Size = int64(len(e.body))
			}
		case e.isDir:
			hdr.Typeflag = tar.TypeDir
			hdr.Mode = 0755
		default:
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(e.body))
		}

		require.NoError(t, tw.WriteHeader(hdr), "write header for %s", e.name)
		if hdr.Typeflag == tar.TypeReg && len(e.body) > 0 {
			_, err := tw.Write([]byte(e.body))
			require.NoError(t, err, "write body for %s", e.name)
		}
	}

	require.NoError(t, tw.Close(), "close tar writer")
	require.NoError(t, gzw.Close(), "close gzip writer")

	archivePath := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(archivePath, buf.Bytes(), 0644), "write archive")
	return archivePath
}

func TestExtractTarGz(t *testing.T) {
	srcDir := t.TempDir()
	archive := buildTarGz(t, srcDir, "bundle.tgz", []tarEntry{
		{name: "subdir/", isDir: true},
		{name: "subdir/nested.txt", body: "nested content"},
		{name: "root.txt", body: "root content"},
	})

	dest := t.TempDir()
	err := ExtractTarGz(archive, dest)
	require.NoError(t, err, "ExtractTarGz should succeed")

	// Regular files are extracted with their contents.
	rootBytes, err := os.ReadFile(filepath.Join(dest, "root.txt"))
	require.NoError(t, err, "root.txt should exist")
	assert.Equal(t, "root content", string(rootBytes))

	nestedBytes, err := os.ReadFile(filepath.Join(dest, "subdir", "nested.txt"))
	require.NoError(t, err, "subdir/nested.txt should exist")
	assert.Equal(t, "nested content", string(nestedBytes))

	// The explicit directory entry was created.
	dirInfo, err := os.Stat(filepath.Join(dest, "subdir"))
	require.NoError(t, err, "subdir should exist")
	assert.True(t, dirInfo.IsDir(), "subdir should be a directory")
}

func TestExtractTarGzFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits are not meaningful on windows")
	}

	srcDir := t.TempDir()
	archive := buildTarGz(t, srcDir, "perm.tgz", []tarEntry{
		{name: "binary", body: "#!/bin/sh\n"},
	})

	dest := t.TempDir()
	require.NoError(t, ExtractTarGz(archive, dest))

	info, err := os.Stat(filepath.Join(dest, "binary"))
	require.NoError(t, err)
	// ExtractTarGz chmods regular files to 0755 regardless of the archive mode.
	assert.Equal(t, os.FileMode(0755), info.Mode().Perm(), "extracted file should be 0755")
}

func TestExtractTarGzDirPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits are not meaningful on windows")
	}

	srcDir := t.TempDir()
	archive := buildTarGz(t, srcDir, "dir.tgz", []tarEntry{
		{name: "data/", isDir: true},
	})

	dest := t.TempDir()
	require.NoError(t, ExtractTarGz(archive, dest))

	info, err := os.Stat(filepath.Join(dest, "data"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0755), info.Mode().Perm(), "extracted dir should be 0755")
}

func TestExtractTarGzUnknownType(t *testing.T) {
	srcDir := t.TempDir()
	// A symlink entry is neither TypeDir nor TypeReg, so extraction must fail.
	archive := buildTarGz(t, srcDir, "weird.tgz", []tarEntry{
		{name: "link", typflag: tar.TypeSymlink},
	})

	dest := t.TempDir()
	err := ExtractTarGz(archive, dest)
	require.Error(t, err, "unsupported entry type should error")
	assert.Contains(t, err.Error(), "unknown type")
}

func TestExtractTarGzMissingSource(t *testing.T) {
	err := ExtractTarGz(filepath.Join(t.TempDir(), "does-not-exist.tgz"), t.TempDir())
	require.Error(t, err, "missing source archive should error")
}

func TestExtractTarGzNotGzip(t *testing.T) {
	srcDir := t.TempDir()
	plain := filepath.Join(srcDir, "plain.tgz")
	require.NoError(t, os.WriteFile(plain, []byte("this is not gzip data"), 0644))

	err := ExtractTarGz(plain, t.TempDir())
	require.Error(t, err, "non-gzip source should error")
}

func TestPathExists(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "present.txt")
	require.NoError(t, os.WriteFile(existing, []byte("x"), 0644))

	t.Run("existing file", func(t *testing.T) {
		ok, err := PathExists(existing)
		require.NoError(t, err)
		assert.True(t, ok)
	})

	t.Run("existing directory", func(t *testing.T) {
		ok, err := PathExists(dir)
		require.NoError(t, err)
		assert.True(t, ok)
	})

	t.Run("missing path", func(t *testing.T) {
		ok, err := PathExists(filepath.Join(dir, "missing.txt"))
		require.NoError(t, err, "a missing path is not an error")
		assert.False(t, ok)
	})
}

func TestOverwriteFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "conf")

	// OverwriteFile uses O_TRUNC|O_WRONLY with no O_CREATE, so the file must
	// already exist. Pre-create it with longer content to verify truncation.
	require.NoError(t, os.WriteFile(target, []byte("original-longer-content"), 0644))

	err := OverwriteFile(target, "short")
	require.NoError(t, err)

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "short", string(got), "old content should be fully replaced, not appended")
}

func TestOverwriteFileMissingTarget(t *testing.T) {
	err := OverwriteFile(filepath.Join(t.TempDir(), "nope"), "data")
	require.Error(t, err, "overwriting a non-existent file should error (no O_CREATE)")
}

func TestReadFileInTgz(t *testing.T) {
	srcDir := t.TempDir()
	archive := buildTarGz(t, srcDir, "search.tgz", []tarEntry{
		{name: "ignored/", isDir: true},
		{name: "ignored/other.txt", body: "other"},
		{name: "nested/dir/target.json", body: `{"found":true}`},
	})

	t.Run("found by base name", func(t *testing.T) {
		reader, err := ReadFileInTgz(archive, "target.json")
		require.NoError(t, err)
		require.NotNil(t, reader)

		content, err := io.ReadAll(reader)
		require.NoError(t, err)
		assert.Equal(t, `{"found":true}`, string(content))
	})

	t.Run("not found", func(t *testing.T) {
		reader, err := ReadFileInTgz(archive, "missing.json")
		require.ErrorIs(t, err, errdefs.ErrNotFound)
		assert.Nil(t, reader)
	})

	t.Run("missing archive", func(t *testing.T) {
		_, err := ReadFileInTgz(filepath.Join(srcDir, "absent.tgz"), "target.json")
		require.Error(t, err)
	})
}

func TestGetTunnelBinaryPath(t *testing.T) {
	cfg := testFsConfig(t)
	cfg.CommandLineArguments.AgentDir = "/opt/reagent"

	got := GetTunnelBinaryPath(cfg, "frpc")

	expected := filepath.Join("/opt/reagent", "frpc")
	if runtime.GOOS == "windows" {
		expected += ".exe"
	}
	assert.Equal(t, expected, got)
}

func TestWriteCounter(t *testing.T) {
	var progress []DownloadProgress
	wc := &WriteCounter{
		FilePath: "/tmp/file",
		Size:     10,
		callback: func(dp DownloadProgress) { progress = append(progress, dp) },
	}

	n, err := wc.Write([]byte("abcd"))
	require.NoError(t, err)
	assert.Equal(t, 4, n, "Write reports the number of bytes consumed")

	n, err = wc.Write([]byte("ef"))
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	assert.Equal(t, uint64(6), wc.Total, "Total accumulates across writes")

	require.Len(t, progress, 2, "callback fires once per Write")
	assert.Equal(t, uint64(4), progress[0].Increment)
	assert.Equal(t, uint64(4), progress[0].CurrentBytes)
	assert.Equal(t, uint64(10), progress[0].TotalFileSize)
	assert.Equal(t, "/tmp/file", progress[0].FilePath)
	assert.Equal(t, uint64(2), progress[1].Increment)
	assert.Equal(t, uint64(6), progress[1].CurrentBytes)
}

func TestWriteCounterNilCallback(t *testing.T) {
	wc := &WriteCounter{Size: 4}
	n, err := wc.Write([]byte("data"))
	require.NoError(t, err, "a nil callback must not panic")
	assert.Equal(t, 4, n)
	assert.Equal(t, uint64(4), wc.Total)
}

func TestInitDirectories(t *testing.T) {
	root := t.TempDir()
	cfg := testFsConfig(t)
	cli := cfg.CommandLineArguments
	cli.AppsDirectory = filepath.Join(root, "apps")
	cli.AppsBuildDir = filepath.Join(root, "apps", "build")
	cli.AppsComposeDir = filepath.Join(root, "apps", "compose")
	cli.AppsSharedDir = filepath.Join(root, "apps", "shared")
	cli.DownloadDir = filepath.Join(root, "downloads")

	err := InitDirectories(cli)
	require.NoError(t, err)

	for _, dir := range []string{cli.AppsDirectory, cli.AppsBuildDir, cli.AppsComposeDir, cli.AppsSharedDir, cli.DownloadDir} {
		info, err := os.Stat(dir)
		require.NoErrorf(t, err, "expected %s to be created", dir)
		assert.Truef(t, info.IsDir(), "%s should be a directory", dir)
	}
}

func TestInitDirectoriesIdempotent(t *testing.T) {
	root := t.TempDir()
	cfg := testFsConfig(t)
	cli := cfg.CommandLineArguments
	cli.AppsDirectory = filepath.Join(root, "apps")
	cli.AppsBuildDir = filepath.Join(root, "apps", "build")
	cli.AppsComposeDir = filepath.Join(root, "apps", "compose")
	cli.AppsSharedDir = filepath.Join(root, "apps", "shared")
	cli.DownloadDir = filepath.Join(root, "downloads")

	require.NoError(t, InitDirectories(cli))
	// Calling again on already-existing directories must not error.
	require.NoError(t, InitDirectories(cli), "InitDirectories should be idempotent")
}

// The dirs holding app secrets are private to the agent, also where an older
// agent created them open; the apps dir (Docker's data-root on FlockOS) and
// the shared dir are left as they are.
func TestInitDirectoriesPrivateAppDirs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes")
	}

	root := t.TempDir()
	cfg := testFsConfig(t)
	cli := cfg.CommandLineArguments
	cli.AppsDirectory = filepath.Join(root, "apps")
	cli.AppsBuildDir = filepath.Join(root, "apps", "build")
	cli.AppsComposeDir = filepath.Join(root, "apps", "compose")
	cli.AppsSharedDir = filepath.Join(root, "apps", "shared")
	cli.DownloadDir = filepath.Join(root, "downloads")

	prodDir := filepath.Join(cli.AppsDirectory, "prod")
	require.NoError(t, os.MkdirAll(prodDir, 0755))
	require.NoError(t, os.Chmod(prodDir, 0755))

	require.NoError(t, InitDirectories(cli))

	for _, dir := range []string{cli.AppsBuildDir, cli.AppsComposeDir, prodDir, filepath.Join(cli.AppsDirectory, "dev")} {
		info, err := os.Stat(dir)
		require.NoError(t, err)
		assert.Equalf(t, os.FileMode(0700), info.Mode().Perm(), "%s", dir)
	}
	for _, dir := range []string{cli.AppsDirectory, cli.AppsSharedDir} {
		info, err := os.Stat(dir)
		require.NoError(t, err)
		assert.NotEqualf(t, os.FileMode(0700), info.Mode().Perm(), "%s", dir)
	}
}

func TestExecuteAsScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ExecuteAsScript shells out to /bin/bash")
	}

	out, err := ExecuteAsScript(`printf "hello-%s" script`)
	require.NoError(t, err)
	assert.Equal(t, "hello-script", string(out), "stdout of the script is returned")
}

func TestExecuteAsScriptCleansUpScriptFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ExecuteAsScript shells out to /bin/bash")
	}

	// Capture the temp dir before/after to confirm the generated *.sh is removed.
	before := tmpScriptCount(t)
	_, err := ExecuteAsScript(`true`)
	require.NoError(t, err)
	after := tmpScriptCount(t)

	assert.Equal(t, before, after, "the generated script file should be removed after execution")
}

func TestExecuteAsScriptCommandError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ExecuteAsScript shells out to /bin/bash")
	}

	// The generated script ends with "exit 0", so a normal failing command
	// still exits 0. Force a non-zero exit before that trailer is reached.
	_, err := ExecuteAsScript(`exit 7`)
	require.Error(t, err, "a non-zero script exit should surface as an error")
}

func TestDownloadURL(t *testing.T) {
	payload := []byte("downloaded-bytes-content")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "out.bin")

	var lastProgress DownloadProgress
	err := DownloadURL(dest, srv.URL, func(dp DownloadProgress) { lastProgress = dp })
	require.NoError(t, err)

	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, payload, got, "downloaded file content should match the server body")
	assert.Equal(t, uint64(len(payload)), lastProgress.CurrentBytes, "progress callback tracks total bytes")
	assert.Equal(t, uint64(len(payload)), lastProgress.TotalFileSize)

	// The per-path lock must be released so a follow-up download succeeds.
	_, locked := DownloadLocks[dest]
	assert.False(t, locked, "download lock should be released after completion")
}

func TestDownloadURLStatusHandling(t *testing.T) {
	payload := []byte("binary-bytes")

	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "200 OK is written to disk", status: http.StatusOK},
		{name: "206 Partial Content is accepted", status: http.StatusPartialContent},
		{name: "404 Not Found is rejected", status: http.StatusNotFound, wantErr: true},
		{name: "500 Internal Server Error is rejected", status: http.StatusInternalServerError, wantErr: true},
		{name: "503 Service Unavailable is rejected", status: http.StatusServiceUnavailable, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Error pages carry a Content-Length too — that is exactly how a
				// 404 body used to end up on disk as the "binary".
				w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
				w.WriteHeader(tt.status)
				_, _ = w.Write(payload)
			}))
			defer srv.Close()

			dest := filepath.Join(t.TempDir(), "out.bin")
			err := DownloadURL(dest, srv.URL, nil)

			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), strconv.Itoa(tt.status), "error names the status")
				assert.Contains(t, err.Error(), srv.URL, "error names the URL")
				_, statErr := os.Stat(dest)
				assert.True(t, os.IsNotExist(statErr), "a rejected download must leave no file behind")
			} else {
				require.NoError(t, err)
				got, readErr := os.ReadFile(dest)
				require.NoError(t, readErr)
				assert.Equal(t, payload, got)
			}

			_, locked := DownloadLocks[dest]
			assert.False(t, locked, "download lock should be released")
		})
	}
}

func TestDownloadURLRemovesPartialFileOnTruncatedBody(t *testing.T) {
	// Hijack the connection to send a raw response that promises more bytes
	// than it delivers, then close: the client sees an unexpected EOF mid-copy.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		require.True(t, ok, "test server must support hijacking")
		conn, _, err := hj.Hijack()
		require.NoError(t, err)
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\npartial")
		_ = conn.Close()
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "out.bin")
	err := DownloadURL(dest, srv.URL, nil)
	require.Error(t, err, "a truncated body should error")

	_, statErr := os.Stat(dest)
	assert.True(t, os.IsNotExist(statErr), "the partial file must be removed")

	_, locked := DownloadLocks[dest]
	assert.False(t, locked, "download lock should be released even on error")
}

func TestDownloadURLMissingContentLength(t *testing.T) {
	// Stream the body in chunks without ever setting Content-Length. The Go
	// server then uses chunked transfer-encoding, leaving Content-Length empty,
	// which DownloadURL's strconv.Atoi must reject.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		require.True(t, ok, "test server must support flushing")
		_, _ = w.Write([]byte("chunk-one"))
		flusher.Flush()
		_, _ = w.Write([]byte("chunk-two"))
		flusher.Flush()
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "out.bin")
	err := DownloadURL(dest, srv.URL, nil)
	require.Error(t, err, "missing Content-Length should error")

	// The lock must still be released on the error path.
	_, locked := DownloadLocks[dest]
	assert.False(t, locked, "download lock should be released even on error")
}

func TestGetRemoteFile(t *testing.T) {
	payload := []byte("remote-file-body")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	body, err := GetRemoteFile(srv.URL)
	require.NoError(t, err)
	require.NotNil(t, body)
	defer body.Close()

	got, err := io.ReadAll(body)
	require.NoError(t, err)
	assert.Equal(t, payload, got)
}

// connClosedServer serves payload and signals on the returned channel once the
// server side of a client connection is closed.
func connClosedServer(t *testing.T, payload []byte) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	closed := make(chan struct{}, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, closed
}

// requireConnClosed fails unless the connection closes promptly. A kept-alive
// connection stays open here until the test server shuts down; behind a
// firewall that forgets idle sessions, the server's late close gets dropped.
func requireConnClosed(t *testing.T, closed <-chan struct{}) {
	t.Helper()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("connection still open after the response: the client left it idle instead of closing it")
	}
}

func TestGetRemoteFileClosesConnection(t *testing.T) {
	srv, closed := connClosedServer(t, []byte(`{"all":"1.2.3"}`))

	body, err := GetRemoteFile(srv.URL)
	require.NoError(t, err)
	_, err = io.ReadAll(body)
	require.NoError(t, err)
	require.NoError(t, body.Close())

	requireConnClosed(t, closed)
}

func TestDownloadURLClosesConnection(t *testing.T) {
	srv, closed := connClosedServer(t, []byte("binary-bytes"))

	dest := filepath.Join(t.TempDir(), "out.bin")
	require.NoError(t, DownloadURL(dest, srv.URL, nil))

	requireConnClosed(t, closed)
}

func TestDecompressTgz(t *testing.T) {
	srcDir := t.TempDir()
	archive := buildTarGz(t, srcDir, "single.tgz", []tarEntry{
		{name: "payload.bin", body: "decompressed-payload"},
	})

	destDir := t.TempDir()
	target := filepath.Join(destDir, "extracted.out")
	err := decompressTgz(archive, destDir, "extracted.out")
	require.NoError(t, err)

	// decompressTgz copies from a tar.Reader without first calling Next(), so a
	// reader positioned at the archive start yields no entry bytes: the target
	// file is created but empty. Assert that real behavior so the test guards
	// against silent regressions (e.g. accidentally writing raw tar bytes).
	info, err := os.Stat(target)
	require.NoError(t, err, "target file should be created")
	assert.Zero(t, info.Size(), "no entry bytes are copied without a preceding Next() call")
}

func TestDecompressTgzMissingSource(t *testing.T) {
	err := decompressTgz(filepath.Join(t.TempDir(), "missing.tgz"), t.TempDir(), "out")
	require.Error(t, err)
}
