package osupdate

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"reagent/filesystem"
	"reagent/updatesig"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- os-release ----------------------------------------------------------------

func TestParseOSRelease(t *testing.T) {
	fields := ParseOSRelease([]byte(`# IronFlockOS
NAME="IronFlockOS"
ID=ironflockos
ID_LIKE=debian
PRETTY_NAME="IronFlockOS 1.0.0 (amd64-efi, base)"
VERSION="v1.0.0-g1a2b3c4-20261120T095512"
IMAGE_ID=ironflockos-amd64-efi-base
IMAGE_VERSION=1.0.0
BUILD_ID=20261120T095512Z-g1a2b3c4
QUOTED='single quoted'
ESCAPED="a \"quote\" and a \\ backslash"
EQUALS=a=b
not a line
 =novalue
`))
	assert.Equal(t, "ironflockos", fields["ID"])
	assert.Equal(t, "IronFlockOS 1.0.0 (amd64-efi, base)", fields["PRETTY_NAME"])
	assert.Equal(t, "ironflockos-amd64-efi-base", fields["IMAGE_ID"])
	assert.Equal(t, "single quoted", fields["QUOTED"])
	assert.Equal(t, `a "quote" and a \ backslash`, fields["ESCAPED"])
	assert.Equal(t, "a=b", fields["EQUALS"])
	assert.NotContains(t, fields, "not a line")
	assert.NotContains(t, fields, "")
}

func TestRunningIsIronFlockOS(t *testing.T) {
	assert.True(t, Running{ID: "ironflockos", ImageID: "ironflockos-amd64-efi-base", ImageVersion: "1.0.0"}.IsIronFlockOS())
	assert.False(t, Running{ID: "debian"}.IsIronFlockOS(), "Debian with device-init")
	assert.False(t, Running{ID: "ironflockos", ImageID: "ironflockos-amd64-efi-base"}.IsIronFlockOS(), "no version")
}

func TestReadRunning(t *testing.T) {
	dir := t.TempDir()
	etc := filepath.Join(dir, "os-release")
	lib := filepath.Join(dir, "lib-os-release")
	require.NoError(t, os.WriteFile(lib, []byte("ID=ironflockos\nIMAGE_ID=ironflockos-rpi-arm64-base\nIMAGE_VERSION=1.2.3\nBUILD_ID=x\n"), 0o644))
	old := osReleasePaths
	osReleasePaths = []string{etc, lib}
	t.Cleanup(func() { osReleasePaths = old })

	r, err := ReadRunning()
	require.NoError(t, err, "falls back to /usr/lib/os-release")
	assert.Equal(t, "ironflockos-rpi-arm64-base", r.ImageID)
	assert.Equal(t, "1.2.3", r.ImageVersion)

	osReleasePaths = []string{etc}
	_, err = ReadRunning()
	assert.Error(t, err)
}

// --- the catalog ---------------------------------------------------------------

type signer struct {
	priv ed25519.PrivateKey
	keys []updatesig.PublicKey
}

func newSigner(t *testing.T) signer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	keys, err := updatesig.ParsePublicKeys(updatesig.EncodePublicKey(pub))
	require.NoError(t, err)
	return signer{priv: priv, keys: keys}
}

// trust makes the agent trust s for the test.
func trust(t *testing.T, keys []updatesig.PublicKey) {
	t.Helper()
	old := catalogKeys
	catalogKeys = func() ([]updatesig.PublicKey, error) { return keys, nil }
	t.Cleanup(func() { catalogKeys = old })
}

func (s signer) seal(t *testing.T, subject updatesig.Subject, payload []byte) []byte {
	t.Helper()
	sig, err := updatesig.SignBytes(s.priv, subject, payload)
	require.NoError(t, err)
	env, err := updatesig.SealEnvelope(payload, sig)
	require.NoError(t, err)
	return env
}

func bundleFor(id, version string) *Bundle {
	return &Bundle{
		Artifact: Artifact{
			URL:    "https://example.test/dl/ironflockos/" + version + "/" + id + "-" + version + ".raucb",
			SHA256: strings.Repeat("ab", 32),
			Size:   1000,
		},
		Compatible: id,
	}
}

func testCatalog() Catalog {
	return Catalog{
		Schema:   1,
		IssuedAt: "2026-12-01T10:12:00Z",
		Channels: map[string]string{"production": "1.0.0", "test": "1.0.1"},
		Releases: map[string]Release{
			"1.0.0": {BuildID: "20261120T095512Z-g1a2b3c4", MinAgent: "1.6.0", Images: map[string]Image{
				"ironflockos-amd64-efi-base":      {Target: "amd64-efi", Variant: "base", Bundle: bundleFor("ironflockos-amd64-efi-base", "1.0.0")},
				"ironflockos-amd64-efi-installer": {Target: "amd64-efi", Variant: "installer"},
			}},
			"1.0.1": {BuildID: "20261130T141003Z-g5d6e7f8", MinAgent: "1.6.0", Images: map[string]Image{
				"ironflockos-amd64-efi-base": {Target: "amd64-efi", Variant: "base", Bundle: bundleFor("ironflockos-amd64-efi-base", "1.0.1")},
			}},
		},
	}
}

func payloadOf(t *testing.T, c Catalog) []byte {
	t.Helper()
	data, err := json.Marshal(c)
	require.NoError(t, err)
	return data
}

func TestOpenCatalog(t *testing.T) {
	s := newSigner(t)

	t.Run("a catalog signed with a trusted key", func(t *testing.T) {
		trust(t, s.keys)
		c, keyID, err := OpenCatalog(s.seal(t, CatalogSubject, payloadOf(t, testCatalog())))
		require.NoError(t, err)
		assert.Equal(t, s.keys[0].ID, keyID)
		assert.Equal(t, "1.0.0", c.Channels["production"])
	})

	t.Run("no trusted key: every catalog is refused", func(t *testing.T) {
		trust(t, nil)
		_, _, err := OpenCatalog(s.seal(t, CatalogSubject, payloadOf(t, testCatalog())))
		assert.ErrorIs(t, err, ErrNoCatalogKey)
	})

	t.Run("the shipped key list is IronFlockOS's catalog key", func(t *testing.T) {
		keys, err := catalogKeys()
		require.NoError(t, err)
		require.Len(t, keys, 1)
		assert.Equal(t, "f5e1308e", keys[0].ID)
		// a catalog of another key is still refused
		_, _, err = OpenCatalog(s.seal(t, CatalogSubject, payloadOf(t, testCatalog())))
		assert.Error(t, err)
		assert.NotErrorIs(t, err, ErrNoCatalogKey)
	})

	t.Run("an unknown key is refused", func(t *testing.T) {
		trust(t, newSigner(t).keys)
		_, _, err := OpenCatalog(s.seal(t, CatalogSubject, payloadOf(t, testCatalog())))
		assert.Error(t, err)
	})

	t.Run("a document signed as another subject is refused", func(t *testing.T) {
		trust(t, s.keys)
		agentManifest := updatesig.Subject{Context: updatesig.ContextAgentManifest, Line: "device-agent/manifest-v2.json"}
		_, _, err := OpenCatalog(s.seal(t, agentManifest, payloadOf(t, testCatalog())))
		assert.Error(t, err)
	})

	t.Run("a payload changed after signing is refused", func(t *testing.T) {
		trust(t, s.keys)
		env := s.seal(t, CatalogSubject, payloadOf(t, testCatalog()))
		var e updatesig.Envelope
		require.NoError(t, json.Unmarshal(env, &e))
		changed := testCatalog()
		changed.Channels["production"] = "1.0.1"
		e.Payload = payloadOf(t, changed)
		tampered, err := json.Marshal(e)
		require.NoError(t, err)
		_, _, err = OpenCatalog(tampered)
		assert.Error(t, err)
	})

	t.Run("another schema, no production, a channel without its release", func(t *testing.T) {
		trust(t, s.keys)
		for name, mutate := range map[string]func(*Catalog){
			"schema 2":        func(c *Catalog) { c.Schema = 2 },
			"no production":   func(c *Catalog) { delete(c.Channels, "production") },
			"missing release": func(c *Catalog) { c.Channels["test"] = "9.9.9" },
		} {
			c := testCatalog()
			mutate(&c)
			_, _, err := OpenCatalog(s.seal(t, CatalogSubject, payloadOf(t, c)))
			assert.Error(t, err, name)
		}
	})
}

func TestOffer(t *testing.T) {
	running := Running{ID: "ironflockos", ImageID: "ironflockos-amd64-efi-base", ImageVersion: "1.0.0"}

	t.Run("each channel offers its release", func(t *testing.T) {
		o, err := testCatalog().Offer(running, "production")
		require.NoError(t, err)
		assert.Equal(t, "1.0.0", o.Version)
		assert.False(t, o.Newer, "the device runs it already")

		o, err = testCatalog().Offer(running, "test")
		require.NoError(t, err)
		assert.Equal(t, "1.0.1", o.Version)
		assert.True(t, o.Newer)
		assert.Equal(t, "ironflockos-amd64-efi-base", o.Bundle.Compatible)
	})

	t.Run("a channel the catalog lacks follows production", func(t *testing.T) {
		o, err := testCatalog().Offer(running, "local")
		require.NoError(t, err)
		assert.Equal(t, "production", o.Channel)
		assert.Equal(t, "1.0.0", o.Version)
	})

	t.Run("a newer device is not offered an older release", func(t *testing.T) {
		o, err := testCatalog().Offer(Running{ID: "ironflockos", ImageID: "ironflockos-amd64-efi-base", ImageVersion: "1.1.0"}, "test")
		require.NoError(t, err)
		assert.False(t, o.Newer)
	})

	t.Run("an image the release lacks, or without a bundle", func(t *testing.T) {
		_, err := testCatalog().Offer(Running{ID: "ironflockos", ImageID: "ironflockos-rpi-arm64-base", ImageVersion: "1.0.0"}, "production")
		assert.ErrorIs(t, err, ErrNotOffered)
		_, err = testCatalog().Offer(Running{ID: "ironflockos", ImageID: "ironflockos-amd64-efi-installer", ImageVersion: "1.0.0"}, "production")
		assert.ErrorIs(t, err, ErrNotOffered)
	})

	t.Run("a bundle for another image is refused", func(t *testing.T) {
		c := testCatalog()
		img := c.Releases["1.0.1"].Images["ironflockos-amd64-efi-base"]
		img.Bundle.Compatible = "ironflockos-amd64-efi-nvidia"
		_, err := c.Offer(running, "test")
		assert.Error(t, err)
	})

	t.Run("min_agent", func(t *testing.T) {
		o, err := testCatalog().Offer(running, "test")
		require.NoError(t, err)
		assert.NoError(t, o.CheckAgent("1.6.0"))
		assert.NoError(t, o.CheckAgent("1.7.2"))
		assert.ErrorIs(t, o.CheckAgent("1.5.7"), ErrAgentTooOld)
		assert.NoError(t, o.CheckAgent("development"), "a development agent is not held back")
	})
}

// --- the download --------------------------------------------------------------

func serveBundle(t *testing.T, body []byte) (*httptest.Server, Bundle) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ironflockos/1.0.1/ironflockos-amd64-efi-base-1.0.1.raucb" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(body)
	return srv, Bundle{
		Artifact: Artifact{
			URL:    srv.URL + "/ironflockos/1.0.1/ironflockos-amd64-efi-base-1.0.1.raucb",
			SHA256: hex.EncodeToString(sum[:]),
			Size:   int64(len(body)),
		},
		Compatible: "ironflockos-amd64-efi-base",
	}
}

func TestDownload(t *testing.T) {
	body := make([]byte, 3<<20)
	_, _ = rand.Read(body)

	t.Run("the bundle, checked, with progress, replacing an older one", func(t *testing.T) {
		_, b := serveBundle(t, body)
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "ironflockos-amd64-efi-base-1.0.0.raucb"), []byte("old"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "x.raucb.part"), []byte("partial"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("not ours"), 0o644))
		var last filesystem.DownloadProgress
		path, err := Download(context.Background(), b, dir, func(p filesystem.DownloadProgress) { last = p })
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(dir, "ironflockos-amd64-efi-base-1.0.1.raucb"), path)
		assert.Equal(t, uint64(len(body)), last.CurrentBytes)
		assert.Equal(t, uint64(len(body)), last.TotalFileSize)
		entries, _ := os.ReadDir(dir)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		assert.ElementsMatch(t, []string{"ironflockos-amd64-efi-base-1.0.1.raucb", "keep.txt"}, names)
		assert.Equal(t, path, Downloaded(b, dir))
	})

	t.Run("a wrong checksum, size or status leaves nothing behind", func(t *testing.T) {
		_, good := serveBundle(t, body)
		for name, b := range map[string]Bundle{
			"sha256":  {Artifact: Artifact{URL: good.URL, SHA256: strings.Repeat("00", 32), Size: good.Size}},
			"shorter": {Artifact: Artifact{URL: good.URL, SHA256: good.SHA256, Size: good.Size + 1}},
			"longer":  {Artifact: Artifact{URL: good.URL, SHA256: good.SHA256, Size: good.Size - 1}},
			"404":     {Artifact: Artifact{URL: strings.Replace(good.URL, "1.0.1.raucb", "9.9.9.raucb", 1), SHA256: good.SHA256, Size: good.Size}},
		} {
			dir := t.TempDir()
			_, err := Download(context.Background(), b, dir, nil)
			assert.Error(t, err, name)
			entries, _ := os.ReadDir(dir)
			assert.Empty(t, entries, name)
		}
	})

	t.Run("a url that is no bundle is refused before anything is fetched", func(t *testing.T) {
		_, good := serveBundle(t, body)
		b := good
		b.URL = strings.TrimSuffix(good.URL, ".raucb") + ".img.gz"
		_, err := Download(context.Background(), b, t.TempDir(), nil)
		assert.Error(t, err)
	})

	t.Run("a cancelled context stops the download", func(t *testing.T) {
		_, b := serveBundle(t, body)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		dir := t.TempDir()
		_, err := Download(ctx, b, dir, nil)
		assert.Error(t, err)
		entries, _ := os.ReadDir(dir)
		assert.Empty(t, entries)
	})

	t.Run("a server that stops sending is given up", func(t *testing.T) {
		old := stallTimeout
		stallTimeout = 300 * time.Millisecond
		t.Cleanup(func() { stallTimeout = old })
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "1000")
			_, _ = w.Write([]byte("partial"))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
		t.Cleanup(srv.Close)
		b := Bundle{Artifact: Artifact{URL: srv.URL + "/x.raucb", SHA256: strings.Repeat("ab", 32), Size: 1000}}
		start := time.Now()
		_, err := Download(context.Background(), b, t.TempDir(), nil)
		assert.Error(t, err)
		assert.Less(t, time.Since(start), 5*time.Second)
	})
}

func TestBundleURL(t *testing.T) {
	b := Bundle{Artifact: Artifact{URL: "https://instance-registry.ironflock.com/dl/ironflockos/1.0.1/ironflockos-amd64-efi-base-1.0.1.raucb"}}

	t.Run("below the device's update base, not the catalog's host", func(t *testing.T) {
		got, err := BundleURL("http://appliance.lan:15002/dl", "1.0.1", b)
		require.NoError(t, err)
		assert.Equal(t, "http://appliance.lan:15002/dl/ironflockos/1.0.1/ironflockos-amd64-efi-base-1.0.1.raucb", got)
	})

	t.Run("on the cloud it is the catalog's url", func(t *testing.T) {
		got, err := BundleURL("https://instance-registry.ironflock.com/dl/", "1.0.1", b)
		require.NoError(t, err)
		assert.Equal(t, b.URL, got)
	})

	t.Run("a query does not end up in the file name", func(t *testing.T) {
		q := Bundle{Artifact: Artifact{URL: b.URL + "?x=1"}}
		got, err := BundleURL("https://dl.example", "1.0.1", q)
		require.NoError(t, err)
		assert.Equal(t, "https://dl.example/ironflockos/1.0.1/ironflockos-amd64-efi-base-1.0.1.raucb", got)
	})

	for name, c := range map[string]struct {
		base, version, url string
	}{
		"no update base":       {"", "1.0.1", b.URL},
		"a version that walks": {"https://dl.example", "../1.0.1", b.URL},
		"not a version":        {"https://dl.example", "latest", b.URL},
		"not a bundle":         {"https://dl.example", "1.0.1", "https://cloud.example/dl/ironflockos/1.0.1/ironflockos.img.gz"},
		"no file name":         {"https://dl.example", "1.0.1", "https://cloud.example/dl/ironflockos/1.0.1/.raucb"},
	} {
		t.Run(name+" is refused", func(t *testing.T) {
			_, err := BundleURL(c.base, c.version, Bundle{Artifact: Artifact{URL: c.url}})
			assert.Error(t, err)
		})
	}
}

func TestImageIDReport(t *testing.T) {
	id, ok := ImageIDReport(Running{ID: "ironflockos", ImageID: "ironflockos-amd64-efi-base", ImageVersion: "1.0.0"}, nil)
	assert.True(t, ok)
	assert.Equal(t, "ironflockos-amd64-efi-base", id)

	id, ok = ImageIDReport(Running{ID: "debian"}, nil)
	assert.True(t, ok, "another OS is reported, so a reflashed device loses its image id")
	assert.Empty(t, id)

	_, ok = ImageIDReport(Running{}, os.ErrNotExist)
	assert.False(t, ok, "no os-release (Windows): left out of the report")
}
