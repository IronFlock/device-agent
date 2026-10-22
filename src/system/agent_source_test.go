package system

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"reagent/updatesig"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// currentTreeAgent is the agent published in the current tree: binaries named
// device-agent below device-agent/, announced by a signed manifest.json.
var currentTreeAgent = signedArtifact{
	name:     "agent update (current tree)",
	bucket:   agentTree,
	fileName: "device-agent",
	context:  updatesig.ContextAgentUpdate,
	version:  signedTestVersion,
}

func sealManifest(t *testing.T, priv ed25519.PrivateKey, channels map[string]string) []byte {
	t.Helper()
	return sealManifestAs(t, priv, agentManifestSubject, channels)
}

func sealManifestAs(t *testing.T, priv ed25519.PrivateKey, subject updatesig.Subject, channels map[string]string) []byte {
	t.Helper()
	payload, err := json.Marshal(agentManifest{Channels: channels, IssuedAt: "2026-10-18T12:00:00Z"})
	require.NoError(t, err)
	sig, err := updatesig.SignBytes(priv, subject, payload)
	require.NoError(t, err)
	file, err := updatesig.SealEnvelope(payload, sig)
	require.NoError(t, err)
	return file
}

func TestAgentUpdateFromTheSignedManifest(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	trustUpdateKey(t, pub)

	origin := newUpdateOrigin(t)
	origin.files["/"+agentTree+"/"+agentManifestName] = sealManifest(t, priv, map[string]string{"production": signedTestVersion})
	binary := []byte("current-tree agent")
	origin.publish(currentTreeAgent, binary, signBytes(t, priv, currentTreeAgent.target(signedTestVersion), binary))

	sys, cfg := newUpdateSystem(t, origin)
	cfg.CommandLineArguments.BinaryName = "device-agent"

	result, err := sys.updateAgentIfRequired(nil)
	require.NoError(t, err)
	assert.True(t, result.DidUpdate)
	installed := filepath.Join(cfg.CommandLineArguments.AgentDir, "device-agent-v"+signedTestVersion+exeSuffix())
	data, err := os.ReadFile(installed)
	require.NoError(t, err)
	assert.Equal(t, binary, data)
}

// A manifest that exists but does not verify must be refused, and must NOT
// send the agent to the unsigned legacy tree: a hostile mirror could
// otherwise force the downgrade path by serving garbage.
func TestBadManifestNeverFallsBackToTheLegacyTree(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	trustUpdateKey(t, pub)
	_, attacker, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	origin := newUpdateOrigin(t)
	origin.files["/"+agentTree+"/"+agentManifestName] = sealManifest(t, attacker, map[string]string{"production": signedTestVersion})
	// A perfectly signed legacy binary is right there; it must stay unused.
	legacy := signedArtifacts[0]
	binary := []byte("legacy agent")
	origin.publish(legacy, binary, signBytes(t, priv, legacy.target(signedTestVersion), binary))

	sys, cfg := newUpdateSystem(t, origin)
	_, err = sys.updateAgentIfRequired(nil)
	require.ErrorIs(t, err, updatesig.ErrBadSignature)
	assert.NoFileExists(t, legacy.installed(cfg))
}

func TestMissingManifestFallsBackToTheLegacyTree(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	trustUpdateKey(t, pub)

	origin := newUpdateOrigin(t) // serves only the legacy tree
	legacy := signedArtifacts[0]
	binary := []byte("legacy agent")
	origin.publish(legacy, binary, signBytes(t, priv, legacy.target(signedTestVersion), binary))

	sys, cfg := newUpdateSystem(t, origin)
	channels, source, err := sys.resolveAgentVersions()
	require.NoError(t, err)
	assert.True(t, source.legacy())
	assert.Equal(t, "reagent", source.publishedName)
	assert.Equal(t, signedTestVersion, channels["production"])

	result, err := sys.updateAgentIfRequired(nil)
	require.NoError(t, err)
	assert.True(t, result.DidUpdate)
	assert.FileExists(t, legacy.installed(cfg))
}

// manifest-v2.json (signed in CI) wins over the frozen manifest.json.
func TestManifestV2IsReadFirst(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	trustUpdateKey(t, pub)

	origin := newUpdateOrigin(t)
	origin.files["/"+agentTree+"/"+agentManifestV2Name] = sealManifestAs(t, priv, agentManifestV2Subject, map[string]string{"production": "1.4.0"})
	origin.files["/"+agentTree+"/"+agentManifestName] = sealManifest(t, priv, map[string]string{"production": "1.3.5"})

	sys, _ := newUpdateSystem(t, origin)
	channels, source, err := sys.resolveAgentVersions()
	require.NoError(t, err)
	assert.False(t, source.legacy())
	assert.Equal(t, "1.4.0", channels["production"])
}

// A mirror without manifest-v2.json (not synced yet) still serves the frozen
// manifest.json, which keeps the agent on the bridge release.
func TestMissingManifestV2FallsBackToManifest(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	trustUpdateKey(t, pub)

	origin := newUpdateOrigin(t)
	origin.files["/"+agentTree+"/"+agentManifestName] = sealManifest(t, priv, map[string]string{"production": "1.3.5"})

	sys, _ := newUpdateSystem(t, origin)
	channels, source, err := sys.resolveAgentVersions()
	require.NoError(t, err)
	assert.False(t, source.legacy())
	assert.Equal(t, "1.3.5", channels["production"])
}

// A manifest-v2.json that does not verify is refused; it is never a reason to
// read the older manifest.json.
func TestBadManifestV2NeverFallsBack(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	trustUpdateKey(t, pub)
	_, attacker, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	origin := newUpdateOrigin(t)
	origin.files["/"+agentTree+"/"+agentManifestV2Name] = sealManifestAs(t, attacker, agentManifestV2Subject, map[string]string{"production": "6.6.6"})
	origin.files["/"+agentTree+"/"+agentManifestName] = sealManifest(t, priv, map[string]string{"production": "1.3.5"})

	sys, _ := newUpdateSystem(t, origin)
	_, _, err = sys.resolveAgentVersions()
	require.ErrorIs(t, err, updatesig.ErrBadSignature)
}

// The frozen manifest.json, signed as device-agent/manifest.json, cannot be
// served as manifest-v2.json: the subject is part of the signature.
func TestManifestCannotStandInForManifestV2(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	trustUpdateKey(t, pub)

	origin := newUpdateOrigin(t)
	origin.files["/"+agentTree+"/"+agentManifestV2Name] = sealManifest(t, priv, map[string]string{"production": "1.3.5"})

	sys, _ := newUpdateSystem(t, origin)
	_, _, err = sys.resolveAgentVersions()
	require.ErrorIs(t, err, updatesig.ErrBadSignature)
}
