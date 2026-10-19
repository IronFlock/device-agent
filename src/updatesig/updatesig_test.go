package updatesig

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// placeholderOKEnv lets a local run skip TestProductionUpdateKeysConfigured
// while AGENT_UPDATE_PUBLIC_KEYS still holds the placeholder. CI must never set
// it: the failing test is what stops a release without the production key.
const placeholderOKEnv = "AGENT_SIGNING_KEY_PLACEHOLDER_OK"

// TestProductionUpdateKeysConfigured FAILS ON PURPOSE until the maintainer has
// put the production public key into AGENT_UPDATE_PUBLIC_KEYS (`just
// agent-update-pubkeys` prints it from OpenBao). A build that still holds the
// placeholder trusts no key and would refuse every OTA update, so it must not
// get through the release gate. With the real key in, it passes and
// AGENT_SIGNING_KEY_PLACEHOLDER_OK no longer matters.
func TestProductionUpdateKeysConfigured(t *testing.T) {
	keys, err := TrustedKeys()
	if errors.Is(err, ErrPlaceholderKey) && os.Getenv(placeholderOKEnv) == "1" {
		t.Skipf("AGENT_UPDATE_PUBLIC_KEYS still holds the placeholder (%s=1); this tree cannot be released", placeholderOKEnv)
	}
	require.NoError(t, err, "put the production update key into AGENT_UPDATE_PUBLIC_KEYS before releasing")
	require.NotEmpty(t, keys)
	assert.NotContains(t, AGENT_UPDATE_PUBLIC_KEYS, "TODO")
}

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return pub, priv
}

func trusted(pubs ...ed25519.PublicKey) []PublicKey {
	var keys []PublicKey
	for _, pub := range pubs {
		keys = append(keys, PublicKey{ID: KeyID(pub), Key: pub})
	}
	return keys
}

func writeBinary(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "reagent")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o755))
	return path
}

// agentTarget is a linux/arm64 agent update to version.
func agentTarget(version string) Target {
	return Target{Context: ContextAgentUpdate, OS: "linux", Arch: "arm64", Version: version, FileName: "reagent"}
}

func sign(t *testing.T, priv ed25519.PrivateKey, target Target, binary string) []byte {
	t.Helper()
	sigFile, err := SignFile(priv, target, binary)
	require.NoError(t, err)
	return sigFile
}

// The signed message, spelled out byte for byte.
func TestTargetMessage(t *testing.T) {
	digest := sha256.Sum256([]byte("reagent"))
	digestHex := hex.EncodeToString(digest[:])

	message, err := Target{Context: ContextAgentUpdate, OS: "linux", Arch: "armv7", Version: "1.2.3", FileName: "reagent"}.Message(digest[:])
	require.NoError(t, err)
	assert.Equal(t, "reagent-update:v1\nlinux/armv7/1.2.3/reagent\n"+digestHex, string(message))

	message, err = Target{Context: ContextFrpcDownload, OS: "windows", Arch: "amd64", Version: "0.71.0", FileName: "frpc.exe"}.Message(digest[:])
	require.NoError(t, err)
	assert.Equal(t, "frpc-download:v1\nwindows/amd64/0.71.0/frpc.exe\n"+digestHex, string(message))

	// Nothing may shift text between the fields.
	for _, target := range []Target{
		{OS: "linux", Arch: "arm64", Version: "1.2.3", FileName: "reagent"},
		{Context: ContextAgentUpdate, OS: "linux", Arch: "arm64", Version: "", FileName: "reagent"},
		{Context: ContextAgentUpdate, OS: "linux", Arch: "arm64", Version: "1.2.3/..", FileName: "reagent"},
		{Context: ContextAgentUpdate, OS: "linux", Arch: "arm64\nlinux", Version: "1.2.3", FileName: "reagent"},
	} {
		_, err := target.Message(digest[:])
		assert.ErrorIs(t, err, ErrInvalidTarget, "%q", target.String())
	}
}

// The .sig is the standard, padded base64 of a raw Ed25519 signature over the
// signed message. Checked with the standard library alone, so the format
// cannot drift together with the package's own code.
func TestSignatureFileFormat(t *testing.T) {
	pub, priv := newKey(t)
	content := "agent binary bytes"
	binary := writeBinary(t, content)

	sigFile := sign(t, priv, agentTarget("1.2.3"), binary)

	text := string(sigFile)
	require.True(t, strings.HasSuffix(text, "\n"))
	require.Len(t, text, 89, "88 base64 characters and a newline")
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(text, "\n"))
	require.NoError(t, err)
	require.Len(t, raw, ed25519.SignatureSize)

	digest := sha256.Sum256([]byte(content))
	message := "reagent-update:v1\nlinux/arm64/1.2.3/reagent\n" + hex.EncodeToString(digest[:])
	assert.True(t, ed25519.Verify(pub, []byte(message), raw))
}

func TestVerifyFileAcceptsValidSignature(t *testing.T) {
	pub, priv := newKey(t)
	binary := writeBinary(t, "release 1.2.3")
	sigFile := sign(t, priv, agentTarget("1.2.3"), binary)

	keyID, err := VerifyFile(trusted(pub), agentTarget("1.2.3"), binary, sigFile)
	require.NoError(t, err)
	assert.Equal(t, KeyID(pub), keyID)

	// Tolerant of how the file was written: no newline, CRLF, stray blanks.
	bare := strings.TrimSpace(string(sigFile))
	for _, variant := range []string{bare, bare + "\r\n", " " + bare + "\t \n"} {
		_, err = VerifyFile(trusted(pub), agentTarget("1.2.3"), binary, []byte(variant))
		assert.NoError(t, err)
	}
}

// Any listed key may have signed: what makes a rotation overlap.
func TestVerifyFileAcceptsAnyTrustedKey(t *testing.T) {
	oldPub, _ := newKey(t)
	newPub, newPriv := newKey(t)
	binary := writeBinary(t, "release 1.2.4")
	sigFile := sign(t, newPriv, agentTarget("1.2.4"), binary)

	keyID, err := VerifyFile(trusted(oldPub, newPub), agentTarget("1.2.4"), binary, sigFile)
	require.NoError(t, err)
	assert.Equal(t, KeyID(newPub), keyID)
}

func TestVerifyFileRefuses(t *testing.T) {
	pub, priv := newKey(t)
	_, otherPriv := newKey(t)
	installing := agentTarget("1.2.5")

	signedAs := func(key ed25519.PrivateKey, target Target) func(*testing.T, string) []byte {
		return func(t *testing.T, binary string) []byte { return sign(t, key, target, binary) }
	}
	withVersion := func(version string) Target { target := installing; target.Version = version; return target }
	withArch := func(arch string) Target { target := installing; target.Arch = arch; return target }

	tests := []struct {
		name      string
		keys      []PublicKey
		sig       func(t *testing.T, binary string) []byte
		tamper    bool
		installAs Target // zero: installing
		wantErr   error
	}{
		{
			name:    "signature by an untrusted key",
			keys:    trusted(pub),
			sig:     signedAs(otherPriv, installing),
			wantErr: ErrBadSignature,
		},
		{
			name:    "binary changed after signing",
			keys:    trusted(pub),
			sig:     signedAs(priv, installing),
			tamper:  true,
			wantErr: ErrBadSignature,
		},
		{
			name:    "signed as an older version (downgrade)",
			keys:    trusted(pub),
			sig:     signedAs(priv, withVersion("1.2.4")),
			wantErr: ErrBadSignature,
		},
		{
			name:      "linux/arm64 signature on linux/amd64",
			keys:      trusted(pub),
			sig:       signedAs(priv, installing),
			installAs: withArch("amd64"),
			wantErr:   ErrBadSignature,
		},
		{
			name:    "signed for windows",
			keys:    trusted(pub),
			sig:     signedAs(priv, Target{Context: ContextAgentUpdate, OS: "windows", Arch: "arm64", Version: "1.2.5", FileName: "reagent"}),
			wantErr: ErrBadSignature,
		},
		{
			name:    "frpc signature presented as an agent update",
			keys:    trusted(pub),
			sig:     signedAs(priv, Target{Context: ContextFrpcDownload, OS: "linux", Arch: "arm64", Version: "1.2.5", FileName: "reagent"}),
			wantErr: ErrBadSignature,
		},
		{
			name:    "empty signature file",
			keys:    trusted(pub),
			sig:     func(*testing.T, string) []byte { return nil },
			wantErr: ErrMalformedSignature,
		},
		{
			name:    "not base64",
			keys:    trusted(pub),
			sig:     func(*testing.T, string) []byte { return []byte("not a signature!\n") },
			wantErr: ErrMalformedSignature,
		},
		{
			name: "base64 of the wrong length",
			keys: trusted(pub),
			sig: func(*testing.T, string) []byte {
				return []byte(base64.StdEncoding.EncodeToString(make([]byte, 32)) + "\n")
			},
			wantErr: ErrMalformedSignature,
		},
		{
			name: "hex instead of base64",
			keys: trusted(pub),
			sig: func(t *testing.T, binary string) []byte {
				raw, err := ParseSignature(sign(t, priv, installing, binary))
				require.NoError(t, err)
				return []byte(hex.EncodeToString(raw))
			},
			wantErr: ErrMalformedSignature,
		},
		{
			name: "valid signature padded past the size limit",
			keys: trusted(pub),
			sig: func(t *testing.T, binary string) []byte {
				return append(sign(t, priv, installing, binary), strings.Repeat(" ", maxSignatureFileSize)...)
			},
			wantErr: ErrMalformedSignature,
		},
		{
			name:    "no trusted key at all",
			keys:    nil,
			sig:     signedAs(priv, installing),
			wantErr: ErrNoTrustedKeys,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			binary := writeBinary(t, "release 1.2.5")
			sigFile := tt.sig(t, binary)
			if tt.tamper {
				require.NoError(t, os.WriteFile(binary, []byte("release 1.2.5 with a backdoor"), 0o755))
			}

			target := tt.installAs
			if target == (Target{}) {
				target = installing
			}
			keyID, err := VerifyFile(tt.keys, target, binary, sigFile)
			assert.ErrorIs(t, err, tt.wantErr)
			assert.Empty(t, keyID)
		})
	}
}

// Key ID = first 8 hex characters of SHA-256 over the raw 32-byte key. The
// expected value for the RFC 8032 test 1 public key was computed outside Go.
func TestKeyID(t *testing.T) {
	raw, err := hex.DecodeString("d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a")
	require.NoError(t, err)
	assert.Equal(t, "21fe31df", KeyID(raw))
	assert.Equal(t, "11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo=", EncodePublicKey(raw))
}

func TestParsePublicKeys(t *testing.T) {
	first, _ := newKey(t)
	second, _ := newKey(t)

	t.Run("one key per line, comments and blank lines ignored", func(t *testing.T) {
		list := "\n# production keys\n" +
			EncodePublicKey(first) + " # key " + KeyID(first) + "\n\n" +
			"  " + EncodePublicKey(second) + "\n"
		keys, err := ParsePublicKeys(list)
		require.NoError(t, err)
		require.Len(t, keys, 2)
		assert.Equal(t, PublicKey{ID: KeyID(first), Key: first}, keys[0])
		assert.Equal(t, PublicKey{ID: KeyID(second), Key: second}, keys[1])
	})

	t.Run("the placeholder trusts nothing", func(t *testing.T) {
		keys, err := ParsePublicKeys(EncodePublicKey(first) + "\n" + publicKeyPlaceholder + "\n")
		assert.ErrorIs(t, err, ErrPlaceholderKey)
		assert.Empty(t, keys)
	})

	t.Run("one bad line rejects the whole list", func(t *testing.T) {
		truncated := EncodePublicKey(second)[:20]
		keys, err := ParsePublicKeys(EncodePublicKey(first) + "\n" + truncated + "\n")
		assert.ErrorContains(t, err, "line 2")
		assert.Empty(t, keys)
	})

	t.Run("an empty list trusts nothing", func(t *testing.T) {
		_, err := ParsePublicKeys("\n# nothing yet\n")
		assert.ErrorIs(t, err, ErrNoTrustedKeys)
	})
}

func TestAllowUnsigned(t *testing.T) {
	for value, want := range map[string]bool{
		"":       false,
		"true":   true,
		"1":      true,
		"TRUE":   true,
		"false":  false,
		"please": false,
	} {
		t.Setenv(AGENT_UPDATE_ALLOW_UNSIGNED, value)
		assert.Equal(t, want, AllowUnsigned(), "%s=%q", AGENT_UPDATE_ALLOW_UNSIGNED, value)
	}
}

func TestSubjectMessageMatchesTargetMessage(t *testing.T) {
	digest := make([]byte, 32)
	target := agentTarget("1.3.0")
	fromTarget, err := target.Message(digest)
	require.NoError(t, err)
	fromSubject, err := Subject{Context: target.Context, Line: target.Path()}.Message(digest)
	require.NoError(t, err)
	assert.Equal(t, fromTarget, fromSubject, "a target is a subject whose line is its download path")

	for _, bad := range []Subject{{}, {Context: ContextAgentManifest}, {Context: ContextAgentManifest, Line: "a\nb"}} {
		_, err := bad.Message(digest)
		assert.ErrorIs(t, err, ErrInvalidTarget, "%+v", bad)
	}
}

func TestEnvelopeRoundTripAndRefusals(t *testing.T) {
	pub, priv := newKey(t)
	keys := trusted(pub)
	subject := Subject{Context: ContextAgentManifest, Line: "device-agent/manifest.json"}
	payload := []byte(`{"channels":{"production":"1.3.0"}}`)

	sig, err := SignBytes(priv, subject, payload)
	require.NoError(t, err)
	file, err := SealEnvelope(payload, sig)
	require.NoError(t, err)

	got, keyID, err := OpenEnvelope(keys, subject, file)
	require.NoError(t, err)
	assert.Equal(t, payload, got)
	assert.Equal(t, KeyID(pub), keyID)

	t.Run("tampered payload", func(t *testing.T) {
		var envelope Envelope
		require.NoError(t, json.Unmarshal(file, &envelope))
		envelope.Payload = []byte(`{"channels":{"production":"9.9.9"}}`)
		tampered, _ := json.Marshal(envelope)
		_, _, err := OpenEnvelope(keys, subject, tampered)
		assert.ErrorIs(t, err, ErrBadSignature)
	})
	t.Run("another subject", func(t *testing.T) {
		_, _, err := OpenEnvelope(keys, Subject{Context: ContextAgentUpdate, Line: "device-agent/manifest.json"}, file)
		assert.ErrorIs(t, err, ErrBadSignature)
	})
	t.Run("untrusted key", func(t *testing.T) {
		other, _ := newKey(t)
		_, _, err := OpenEnvelope(trusted(other), subject, file)
		assert.ErrorIs(t, err, ErrBadSignature)
	})
	t.Run("no signature", func(t *testing.T) {
		unsigned, _ := json.Marshal(Envelope{Payload: payload})
		_, _, err := OpenEnvelope(keys, subject, unsigned)
		assert.ErrorIs(t, err, ErrMissingSignature)
	})
	t.Run("not an envelope", func(t *testing.T) {
		_, _, err := OpenEnvelope(keys, subject, payload)
		assert.Error(t, err)
	})
}
