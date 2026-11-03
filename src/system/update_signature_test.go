package system

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"reagent/config"
	"reagent/embedded"
	"reagent/release"
	"reagent/testutil/builders"
	"reagent/updatesig"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// signedTestVersion is the agent version availableVersions.json announces;
// newer than any real release, so updateAgentIfRequired goes for it.
const signedTestVersion = "99.0.0"

// updateOrigin is a download server the test fully controls, as an attacker
// controlling the bucket or a mirror would: it serves whatever bytes, .sha256
// and .sig it is given, and 404 for everything else.
type updateOrigin struct {
	server *httptest.Server
	files  map[string][]byte
}

func newUpdateOrigin(t *testing.T) *updateOrigin {
	t.Helper()
	origin := &updateOrigin{files: map[string][]byte{
		"/re-agent/availableVersions.json": []byte(`{"production":"` + signedTestVersion + `"}`),
	}}
	origin.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := origin.files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data)
	}))
	t.Cleanup(origin.server.Close)
	return origin
}

// signedArtifact is one kind of binary the agent downloads and runs, reached
// through the agent's real entry point for it.
type signedArtifact struct {
	name      string
	bucket    string
	fileName  string
	context   string
	version   string // the version the agent resolves and downloads
	download  func(sys *System) error
	installed func(cfg *config.Config) string
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

var signedArtifacts = []signedArtifact{
	{
		name:     "agent update",
		bucket:   "re-agent",
		fileName: "reagent",
		context:  updatesig.ContextAgentUpdate,
		version:  signedTestVersion,
		download: func(sys *System) error {
			_, err := sys.updateAgentIfRequired(nil)
			return err
		},
		installed: func(cfg *config.Config) string {
			return filepath.Join(cfg.CommandLineArguments.AgentDir, "reagent-v"+signedTestVersion+exeSuffix())
		},
	},
	{
		name:     "frpc download",
		bucket:   "re-agent/frpc",
		fileName: "frpc",
		context:  updatesig.ContextFrpcDownload,
		version:  embedded.FRP_VERSION,
		download: func(sys *System) error { return sys.downloadFrpc() },
		installed: func(cfg *config.Config) string {
			return filepath.Join(cfg.CommandLineArguments.AgentDir, "frpc"+exeSuffix())
		},
	},
}

// target is what this host installs artifact a as, at version.
func (a signedArtifact) target(version string) updatesig.Target {
	return updatesig.Target{Context: a.context, OS: runtime.GOOS, Arch: release.GetBuildArch(), Version: version, FileName: a.fileName + exeSuffix()}
}

// publish serves binary where this host downloads artifact a from, with a
// .sha256 that matches whatever is served (an attacker publishes a consistent
// one) and sig as .sig when it is not nil.
func (o *updateOrigin) publish(a signedArtifact, binary, sig []byte) {
	path := buildBinaryDownloadURL("", a.bucket, runtime.GOOS, release.GetBuildArch(), a.version, a.fileName)
	sum := sha256.Sum256(binary)
	o.files[path] = binary
	o.files[path+".sha256"] = []byte(hex.EncodeToString(sum[:]) + "\n")
	if sig != nil {
		o.files[path+".sig"] = sig
	}
}

func newUpdateSystem(t *testing.T, origin *updateOrigin) (*System, *config.Config) {
	t.Helper()
	cfg := builders.DefaultTestConfig()
	cfg.CommandLineArguments.AgentDir = t.TempDir()
	cfg.CommandLineArguments.DownloadDir = t.TempDir()
	cfg.CommandLineArguments.RemoteUpdateURL = origin.server.URL
	sys := New(cfg, nil)
	return &sys, cfg
}

// trustUpdateKey makes pub the only key downloads verify against.
func trustUpdateKey(t *testing.T, pub ed25519.PublicKey) {
	t.Helper()
	previous := trustedUpdateKeys
	trustedUpdateKeys = func() ([]updatesig.PublicKey, error) {
		return []updatesig.PublicKey{{ID: updatesig.KeyID(pub), Key: pub}}, nil
	}
	t.Cleanup(func() { trustedUpdateKeys = previous })
}

func signBytes(t *testing.T, priv ed25519.PrivateKey, target updatesig.Target, binary []byte) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "to-sign")
	require.NoError(t, os.WriteFile(path, binary, 0o644))
	sig, err := updatesig.SignFile(priv, target, path)
	require.NoError(t, err)
	return sig
}

// otherArch is an arch path segment this host does not have.
func otherArch() string {
	if release.GetBuildArch() == "arm64" {
		return "amd64"
	}
	return "arm64"
}

// assertNothingInstalled: a refused binary must neither reach AgentDir
// (reagent-manager.sh activates whatever reagent-v* it finds there, the tunnel
// manager runs frpc from there) nor stay behind in the download directory.
func assertNothingInstalled(t *testing.T, a signedArtifact, cfg *config.Config) {
	t.Helper()
	assert.NoFileExists(t, a.installed(cfg))
	leftovers, err := os.ReadDir(cfg.CommandLineArguments.DownloadDir)
	require.NoError(t, err)
	assert.Empty(t, leftovers, "the refused download must be removed")
}

func TestDownloadSignatureVerification(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	for _, a := range signedArtifacts {
		t.Run(a.name, func(t *testing.T) {
			genuine := []byte("genuine " + a.fileName + " " + a.version)
			forged := []byte("forged " + a.fileName + " " + a.version)
			installing := a.target(a.version)

			otherVersion := installing
			otherVersion.Version = "98.0.0"
			otherPlatform := installing
			otherPlatform.Arch = otherArch()
			otherOS := installing
			otherOS.OS = "plan9"
			otherKind := installing
			otherKind.Context = updatesig.ContextFrpcDownload
			if a.context == updatesig.ContextFrpcDownload {
				otherKind.Context = updatesig.ContextAgentUpdate
			}

			tests := []struct {
				name    string
				binary  []byte
				sig     []byte
				wantErr error
			}{
				{"signed by a key from another pipeline", genuine, signBytes(t, otherPriv, installing, genuine), updatesig.ErrBadSignature},
				{"binary swapped after signing, with a matching .sha256", forged, signBytes(t, priv, installing, genuine), updatesig.ErrBadSignature},
				{"valid signature for another version served as this one", genuine, signBytes(t, priv, otherVersion, genuine), updatesig.ErrBadSignature},
				{"valid signature for another arch", genuine, signBytes(t, priv, otherPlatform, genuine), updatesig.ErrBadSignature},
				{"valid signature for another os", genuine, signBytes(t, priv, otherOS, genuine), updatesig.ErrBadSignature},
				{"valid signature for the other kind of binary", genuine, signBytes(t, priv, otherKind, genuine), updatesig.ErrBadSignature},
				{"no .sig published", genuine, nil, updatesig.ErrMissingSignature},
				{"malformed .sig", genuine, []byte("-----BEGIN SIGNATURE-----\n"), updatesig.ErrMalformedSignature},
			}

			for _, tt := range tests {
				t.Run("refuses "+tt.name, func(t *testing.T) {
					t.Setenv(updatesig.AGENT_UPDATE_ALLOW_UNSIGNED, "")
					trustUpdateKey(t, pub)
					origin := newUpdateOrigin(t)
					origin.publish(a, tt.binary, tt.sig)
					sys, cfg := newUpdateSystem(t, origin)

					assert.ErrorIs(t, a.download(sys), tt.wantErr)
					assertNothingInstalled(t, a, cfg)
				})
			}

			t.Run("accepts a valid signature", func(t *testing.T) {
				t.Setenv(updatesig.AGENT_UPDATE_ALLOW_UNSIGNED, "")
				trustUpdateKey(t, pub)
				origin := newUpdateOrigin(t)
				origin.publish(a, genuine, signBytes(t, priv, installing, genuine))
				sys, cfg := newUpdateSystem(t, origin)

				require.NoError(t, a.download(sys))
				installed, err := os.ReadFile(a.installed(cfg))
				require.NoError(t, err)
				assert.Equal(t, genuine, installed)
			})

			// What OpenBao transit signs: the documented message, spelled out
			// here rather than built by updatesig, signed as raw bytes.
			t.Run("accepts a raw signature over the documented message", func(t *testing.T) {
				t.Setenv(updatesig.AGENT_UPDATE_ALLOW_UNSIGNED, "")
				trustUpdateKey(t, pub)
				digest := sha256.Sum256(genuine)
				documented := a.context + "\n" + runtime.GOOS + "/" + release.GetBuildArch() + "/" + a.version + "/" + a.fileName + exeSuffix() + "\n" + hex.EncodeToString(digest[:])
				origin := newUpdateOrigin(t)
				origin.publish(a, genuine, updatesig.EncodeSignature(ed25519.Sign(priv, []byte(documented))))
				sys, cfg := newUpdateSystem(t, origin)

				require.NoError(t, a.download(sys))
				assert.FileExists(t, a.installed(cfg))
			})

			// A build that still holds the key placeholder trusts nothing, so
			// even a correctly signed binary is refused rather than waved through.
			t.Run("refuses everything without a configured key", func(t *testing.T) {
				t.Setenv(updatesig.AGENT_UPDATE_ALLOW_UNSIGNED, "")
				previous := trustedUpdateKeys
				trustedUpdateKeys = func() ([]updatesig.PublicKey, error) { return nil, updatesig.ErrPlaceholderKey }
				t.Cleanup(func() { trustedUpdateKeys = previous })
				origin := newUpdateOrigin(t)
				origin.publish(a, genuine, signBytes(t, priv, installing, genuine))
				sys, cfg := newUpdateSystem(t, origin)

				assert.ErrorIs(t, a.download(sys), updatesig.ErrPlaceholderKey)
				assertNothingInstalled(t, a, cfg)
			})

			t.Run("the emergency switch installs an unsigned binary until it runs out", func(t *testing.T) {
				t.Setenv(updatesig.AGENT_UPDATE_ALLOW_UNSIGNED, allowUnsignedFor(time.Hour))
				trustUpdateKey(t, pub)
				origin := newUpdateOrigin(t)
				origin.publish(a, genuine, nil)
				sys, cfg := newUpdateSystem(t, origin)

				require.NoError(t, a.download(sys))
				assert.FileExists(t, a.installed(cfg))
			})

			t.Run("the old true and a value without expiry keep verification on", func(t *testing.T) {
				for _, value := range []string{"true", "please", allowUnsignedFor(-time.Minute), allowUnsignedFor(48 * time.Hour)} {
					t.Setenv(updatesig.AGENT_UPDATE_ALLOW_UNSIGNED, value)
					trustUpdateKey(t, pub)
					origin := newUpdateOrigin(t)
					origin.publish(a, genuine, nil)
					sys, cfg := newUpdateSystem(t, origin)

					assert.ErrorIs(t, a.download(sys), updatesig.ErrMissingSignature, "%s=%s", updatesig.AGENT_UPDATE_ALLOW_UNSIGNED, value)
					assertNothingInstalled(t, a, cfg)
				}
			})

			t.Run("a non-boolean escape hatch value keeps verification on", func(t *testing.T) {
				t.Setenv(updatesig.AGENT_UPDATE_ALLOW_UNSIGNED, "please")
				trustUpdateKey(t, pub)
				origin := newUpdateOrigin(t)
				origin.publish(a, genuine, nil)
				sys, cfg := newUpdateSystem(t, origin)

				assert.ErrorIs(t, a.download(sys), updatesig.ErrMissingSignature)
				assertNothingInstalled(t, a, cfg)
			})
		})
	}
}

// allowUnsignedFor is AGENT_UPDATE_ALLOW_UNSIGNED's value running out after d
// (a negative d: already run out).
func allowUnsignedFor(d time.Duration) string {
	return "until=" + time.Now().Add(d).UTC().Format(time.RFC3339)
}

// updateSigLogChild marks the child test binary that
// TestDownloadSignatureLogs runs itself in.
const updateSigLogChild = "AGENT_UPDATE_SIGNATURE_LOG_CHILD"

// A refusal is logged as an error that says why and how to bypass it; a
// binary let through by the escape hatch is logged as a warning, every time.
//
// The log is read from a child test binary whose logger writes JSON to
// stderr: swapping the global logger in this process would race any goroutine
// another test left logging.
func TestDownloadSignatureLogs(t *testing.T) {
	if os.Getenv(updateSigLogChild) != "1" {
		child := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.count=1")
		child.Env = append(os.Environ(), updateSigLogChild+"=1")
		var logged, output bytes.Buffer
		child.Stderr = &logged
		child.Stdout = &output
		require.NoError(t, child.Run(), "%s\n%s", output.String(), logged.String())

		assert.Equal(t, 2, strings.Count(logged.String(), `"level":"error","error":"update signature is missing: nothing published at `),
			"one refusal per binary:\n%s", logged.String())
		assert.Contains(t, logged.String(), "it is not signed by a trusted IronFlock release key as reagent-update:v1 ")
		assert.Contains(t, logged.String(), "it is not signed by a trusted IronFlock release key as frpc-download:v1 ")
		assert.Equal(t, 4, strings.Count(logged.String(), `"level":"warn","error":"update signature is missing: nothing published at `),
			"one warning per binary let through:\n%s", logged.String())
		assert.Contains(t, logged.String(), "WARNING: AGENT_UPDATE_ALLOW_UNSIGNED is set")
		return
	}

	log.Logger = zerolog.New(os.Stderr)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	trustUpdateKey(t, pub)

	for _, a := range signedArtifacts {
		run := func(allow string) error {
			t.Setenv(updatesig.AGENT_UPDATE_ALLOW_UNSIGNED, allow)
			origin := newUpdateOrigin(t)
			origin.publish(a, []byte("unsigned "+a.fileName), nil)
			sys, _ := newUpdateSystem(t, origin)
			return a.download(sys)
		}

		require.Error(t, run(""))
		require.NoError(t, run(allowUnsignedFor(time.Hour)))
		require.NoError(t, run(allowUnsignedFor(time.Hour)))
	}
}
