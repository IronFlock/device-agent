package system

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"reagent/config"
	"reagent/filesystem"
	"reagent/osupdate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cloudBundleURL is where the signed catalog says the bundle is: the cloud
// copy, which a device downloads only when the cloud is its update base.
const cloudBundleURL = "https://cloud.example/dl/flockos/1.0.1/flockos-amd64-efi-base-1.0.1.raucb"

// flockOSDevice sets the seams: a FlockOS device running version whose update
// base is base, a catalog there whose test channel offers 1.0.1 with the
// bundle at cloudBundleURL, the cache in a temporary directory, and RAUC
// replaced by install.
func flockOSDevice(t *testing.T, version, base string, bundle []byte, install func(string, func(string, uint64)) error) *System {
	t.Helper()
	sum := sha256.Sum256(bundle)
	catalog := osupdate.Catalog{
		Schema:   1,
		Channels: map[string]string{"production": "1.0.0", "test": "1.0.1"},
		Releases: map[string]osupdate.Release{
			"1.0.0": {Images: map[string]osupdate.Image{}},
			"1.0.1": {BuildID: "20261130T141003Z-g5d6e7f8", MinAgent: "0.0.1", Images: map[string]osupdate.Image{
				"flockos-amd64-efi-base": {Bundle: &osupdate.Bundle{
					Artifact:   osupdate.Artifact{URL: cloudBundleURL, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(bundle))},
					Compatible: "flockos-amd64-efi-base",
				}},
			}},
		},
	}
	old := struct {
		read    func() (osupdate.Running, error)
		open    func([]byte) (osupdate.Catalog, string, error)
		fetch   func(string) ([]byte, int, error)
		cache   string
		install func(string, func(string, uint64)) error
	}{readRunning, openCatalog, fetchSmallC, cacheDir, installRauc}
	t.Cleanup(func() {
		readRunning, openCatalog, fetchSmallC, cacheDir, installRauc = old.read, old.open, old.fetch, old.cache, old.install
	})
	readRunning = func() (osupdate.Running, error) {
		return osupdate.Running{ID: "flockos", ImageID: "flockos-amd64-efi-base", ImageVersion: version}, nil
	}
	fetchSmallC = func(url string) ([]byte, int, error) {
		if url != base+"/flockos/manifest.json" {
			return nil, http.StatusNotFound, nil
		}
		return []byte("signed catalog"), http.StatusOK, nil
	}
	openCatalog = func(data []byte) (osupdate.Catalog, string, error) {
		if string(data) != "signed catalog" {
			return osupdate.Catalog{}, "", errors.New("bad signature")
		}
		return catalog, "key", nil
	}
	cacheDir = t.TempDir()
	installRauc = install
	return &System{config: &config.Config{ReswarmConfig: &config.ReswarmConfig{Environment: "test", UpdateURL: base}}}
}

// serve is an update base (an appliance's mirror) that serves the bundle at
// <base>/flockos/1.0.1/<file> and nothing else.
func serve(t *testing.T, body []byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/flockos/1.0.1/flockos-amd64-efi-base-1.0.1.raucb" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestFlockOSOffer(t *testing.T) {
	t.Run("the device's channel offers its release", func(t *testing.T) {
		sys := flockOSDevice(t, "1.0.0", "https://dl.example", []byte("x"), nil)
		running, offer, err := sys.FlockOSOffer()
		require.NoError(t, err)
		assert.Equal(t, "1.0.0", running.ImageVersion)
		assert.Equal(t, "test", offer.Channel)
		assert.Equal(t, "1.0.1", offer.Version)
		assert.True(t, offer.Newer)
	})

	t.Run("not FlockOS", func(t *testing.T) {
		sys := flockOSDevice(t, "1.0.0", "https://dl.example", nil, nil)
		readRunning = func() (osupdate.Running, error) { return osupdate.Running{ID: "debian"}, nil }
		_, _, err := sys.FlockOSOffer()
		assert.ErrorIs(t, err, ErrNotFlockOS)
		assert.False(t, RunsFlockOS())
	})

	t.Run("a catalog that does not verify is refused", func(t *testing.T) {
		sys := flockOSDevice(t, "1.0.0", "https://dl.example", nil, nil)
		fetchSmallC = func(string) ([]byte, int, error) { return []byte("forged"), http.StatusOK, nil }
		_, _, err := sys.FlockOSOffer()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "bad signature")
	})

	t.Run("no catalog at the update base", func(t *testing.T) {
		sys := flockOSDevice(t, "1.0.0", "https://dl.example", nil, nil)
		sys.config.ReswarmConfig.UpdateURL = "https://elsewhere.example"
		_, _, err := sys.FlockOSOffer()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "404")
	})
}

func TestDownloadAndInstallFlockOSUpdate(t *testing.T) {
	bundle := []byte("a bundle of FlockOS 1.0.1")

	t.Run("download, then install the downloaded bundle", func(t *testing.T) {
		var installed string
		sys := flockOSDevice(t, "1.0.0", serve(t, bundle), bundle, func(path string, _ func(string, uint64)) error {
			installed = path
			return nil
		})
		var progress []filesystem.DownloadProgress
		path, err := sys.DownloadFlockOSUpdate(context.Background(), func(p filesystem.DownloadProgress) { progress = append(progress, p) })
		require.NoError(t, err)
		assert.NotEmpty(t, progress)

		again, err := sys.DownloadFlockOSUpdate(context.Background(), nil)
		require.NoError(t, err)
		assert.Equal(t, path, again, "a downloaded bundle is not fetched again")

		require.NoError(t, sys.InstallFlockOSUpdate(nil))
		assert.Equal(t, path, installed)
	})

	t.Run("RAUC's error is returned, not swallowed", func(t *testing.T) {
		sys := flockOSDevice(t, "1.0.0", serve(t, bundle), bundle, func(string, func(string, uint64)) error {
			return errors.New("Installation error: Compatible mismatch")
		})
		_, err := sys.DownloadFlockOSUpdate(context.Background(), nil)
		require.NoError(t, err)
		err = sys.InstallFlockOSUpdate(nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Compatible mismatch")
	})

	t.Run("install before download is refused", func(t *testing.T) {
		called := false
		sys := flockOSDevice(t, "1.0.0", serve(t, bundle), bundle, func(string, func(string, uint64)) error { called = true; return nil })
		err := sys.InstallFlockOSUpdate(nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "download_os_update first")
		assert.False(t, called)
	})

	t.Run("nothing newer: nothing is downloaded", func(t *testing.T) {
		sys := flockOSDevice(t, "1.0.1", serve(t, bundle), bundle, nil)
		_, err := sys.DownloadFlockOSUpdate(context.Background(), nil)
		assert.ErrorIs(t, err, ErrNoNewerRelease)
	})

	t.Run("one OS update at a time", func(t *testing.T) {
		sys := flockOSDevice(t, "1.0.0", serve(t, bundle), bundle, nil)
		osUpdateMu.Lock()
		defer osUpdateMu.Unlock()
		_, err := sys.DownloadFlockOSUpdate(context.Background(), nil)
		assert.ErrorIs(t, err, ErrOSUpdateRunning)
		assert.ErrorIs(t, sys.InstallFlockOSUpdate(nil), ErrOSUpdateRunning)
	})
}

func TestMarkFlockOSSlotGoodSkipsOtherSystems(t *testing.T) {
	old := readRunning
	t.Cleanup(func() { readRunning = old })
	readRunning = func() (osupdate.Running, error) { return osupdate.Running{ID: "debian"}, nil }
	assert.NoError(t, MarkFlockOSSlotGood(context.Background()), "no RAUC is asked on a device that is not FlockOS")
}
