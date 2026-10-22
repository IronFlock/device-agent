package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"reagent/updatesig"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// keygenForTest runs keygen into a temp dir and returns the PEM path, the
// private key read back from it and keygen's output. The key exists for this
// test only.
func keygenForTest(t *testing.T) (string, ed25519.PrivateKey, string) {
	t.Helper()
	keyPath := filepath.Join(t.TempDir(), "agent-update-signing.pem")
	var stdout bytes.Buffer
	require.NoError(t, run([]string{"keygen", "-out", keyPath}, nil, &stdout))

	data, err := os.ReadFile(keyPath)
	require.NoError(t, err)
	block, _ := pem.Decode(data)
	require.NotNil(t, block)
	require.Equal(t, "PRIVATE KEY", block.Type)
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	require.NoError(t, err)
	privateKey, ok := parsed.(ed25519.PrivateKey)
	require.True(t, ok)
	return keyPath, privateKey, stdout.String()
}

// trustOnly makes the helper's notion of "compiled-in keys" exactly pubs.
func trustOnly(t *testing.T, pubs ...ed25519.PublicKey) {
	t.Helper()
	previous := trustedKeys
	trustedKeys = func() ([]updatesig.PublicKey, error) { return publicKeys(pubs...), nil }
	t.Cleanup(func() { trustedKeys = previous })
}

func publicKeys(pubs ...ed25519.PublicKey) []updatesig.PublicKey {
	var keys []updatesig.PublicKey
	for _, pub := range pubs {
		keys = append(keys, updatesig.PublicKey{ID: updatesig.KeyID(pub), Key: pub})
	}
	return keys
}

func writeTestBinary(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o755))
	return path
}

// transitSign does what OpenBao transit/sign does with an ed25519 key: sign
// the base64-decoded input as is and answer "vault:v<N>:<base64>".
func transitSign(t *testing.T, priv ed25519.PrivateKey, keyVersion int, input string) string {
	t.Helper()
	signed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(input))
	require.NoError(t, err)
	return "vault:v" + strconv.Itoa(keyVersion) + ":" + base64.StdEncoding.EncodeToString(ed25519.Sign(priv, signed))
}

// keygen prints the public key, its ID and the AGENT_UPDATE_PUBLIC_KEYS line,
// and nothing of the private key.
func TestKeygenPrintsOnlyPublicMaterial(t *testing.T) {
	keyPath, privateKey, out := keygenForTest(t)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	encoded := updatesig.EncodePublicKey(publicKey)

	assert.Contains(t, out, "DEVELOPMENT key written to "+keyPath)
	assert.Contains(t, out, "Key ID:     "+updatesig.KeyID(publicKey)+"\n")
	assert.Contains(t, out, "Public key: "+encoded+"\n")
	line := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(encoded) + ` # DEVELOPMENT key [0-9a-f]{8}, created \d{4}-\d{2}-\d{2}$`).FindString(out)
	require.NotEmpty(t, line, out)
	keys, err := updatesig.ParsePublicKeys(line)
	require.NoError(t, err, "the printed line must parse as AGENT_UPDATE_PUBLIC_KEYS")
	assert.True(t, keys[0].Key.Equal(publicKey))

	assert.NotContains(t, out, "PRIVATE KEY")
	assert.NotContains(t, out, base64.StdEncoding.EncodeToString(privateKey.Seed()))
	assert.NotContains(t, out, base64.StdEncoding.EncodeToString(privateKey))
	pemData, err := os.ReadFile(keyPath)
	require.NoError(t, err)
	block, _ := pem.Decode(pemData)
	assert.NotContains(t, out, base64.StdEncoding.EncodeToString(block.Bytes))

	if runtime.GOOS != "windows" {
		info, err := os.Stat(keyPath)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
}

func TestKeygenNeverOverwritesAKey(t *testing.T) {
	keyPath, _, _ := keygenForTest(t)
	before, err := os.ReadFile(keyPath)
	require.NoError(t, err)

	var stdout bytes.Buffer
	err = run([]string{"keygen", "-out", keyPath}, nil, &stdout)
	require.Error(t, err)

	after, err := os.ReadFile(keyPath)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	assert.Empty(t, stdout.String())
}

// `message` prints the base64 of exactly the bytes the agent verifies: the
// documented format, spelled out here independently of updatesig.
func TestMessageMatchesAgentBytes(t *testing.T) {
	_, privateKey, _ := keygenForTest(t)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	content := "agent build for armv7"
	binary := writeTestBinary(t, "reagent-linux-armv7", content)

	var stdout bytes.Buffer
	require.NoError(t, run([]string{"message", "-version", "1.2.6", binary}, nil, &stdout))
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(stdout.String()))
	require.NoError(t, err)
	digest := sha256.Sum256([]byte(content))
	assert.Equal(t, "reagent-update:v1\nlinux/armv7/1.2.6/reagent\n"+hex.EncodeToString(digest[:]), string(decoded))

	// A signature over those bytes, as transit makes it, is what the agent's
	// verifier accepts for this target.
	signature := updatesig.EncodeSignature(ed25519.Sign(privateKey, decoded))
	target := updatesig.Target{Context: updatesig.ContextAgentUpdate, OS: "linux", Arch: "armv7", Version: "1.2.6", FileName: "reagent"}
	_, err = updatesig.VerifyFile(publicKeys(publicKey), target, binary, signature)
	require.NoError(t, err)

	stdout.Reset()
	frpc := writeTestBinary(t, "frpc-windows-amd64", "frpc")
	require.NoError(t, run([]string{"message", "-version", "0.71.0", frpc}, nil, &stdout))
	decoded, err = base64.StdEncoding.DecodeString(strings.TrimSpace(stdout.String()))
	require.NoError(t, err)
	digest = sha256.Sum256([]byte("frpc"))
	assert.Equal(t, "frpc-download:v1\nwindows/amd64/0.71.0/frpc.exe\n"+hex.EncodeToString(digest[:]), string(decoded))
}

func TestParseTransitSignature(t *testing.T) {
	raw := make([]byte, ed25519.SignatureSize)
	_, err := rand.Read(raw)
	require.NoError(t, err)
	encoded := base64.StdEncoding.EncodeToString(raw)

	for _, text := range []string{"vault:v1:" + encoded, "vault:v12:" + encoded + "\n", "  vault:v3:" + encoded + "\r\n"} {
		version, signature, err := parseTransitSignature(text)
		require.NoError(t, err, "%q", text)
		assert.Equal(t, raw, signature)
		assert.Contains(t, []int{1, 12, 3}, version)
	}
	version, _, err := parseTransitSignature("vault:v12:" + encoded)
	require.NoError(t, err)
	assert.Equal(t, 12, version)

	for _, text := range []string{
		"",
		encoded,
		"vault:" + encoded,
		"vault:v:" + encoded,
		"vault:v0:" + encoded,
		"vault:v-1:" + encoded,
		"vault:v+1:" + encoded,
		"vault:v01:" + encoded,
		"vault:vX:" + encoded,
		"vault:v1",
		"vault:v1:" + base64.StdEncoding.EncodeToString(raw[:32]),
		"vault:v1:" + hex.EncodeToString(raw),
		"vault:v1:" + encoded + "AAAA",
		"vault:v1:" + encoded + ":extra",
	} {
		_, _, err := parseTransitSignature(text)
		assert.Error(t, err, "%q", text)
	}
}

// sig-from-openbao writes a .sig only when OpenBao's answer verifies for the
// target against the trusted keys.
func TestSigFromOpenBao(t *testing.T) {
	_, privateKey, _ := keygenForTest(t)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	_, otherKey, _ := keygenForTest(t)
	trustOnly(t, publicKey)

	binary := writeTestBinary(t, "reagent-windows-amd64", "authenticode-signed exe")
	var stdout bytes.Buffer
	require.NoError(t, run([]string{"message", "-version", "1.2.6", binary}, nil, &stdout))
	input := stdout.String()

	t.Run("a valid answer becomes a .sig the agent accepts", func(t *testing.T) {
		var out bytes.Buffer
		answer := transitSign(t, privateKey, 3, input)
		require.NoError(t, run([]string{"sig-from-openbao", "-version", "1.2.6", binary}, strings.NewReader(answer+"\n"), &out))
		assert.Contains(t, out.String(), "by transit key version 3 (key "+updatesig.KeyID(publicKey)+")")

		signature, err := os.ReadFile(binary + ".sig")
		require.NoError(t, err)
		target := updatesig.Target{Context: updatesig.ContextAgentUpdate, OS: "windows", Arch: "amd64", Version: "1.2.6", FileName: "reagent.exe"}
		_, err = updatesig.VerifyFile(publicKeys(publicKey), target, binary, signature)
		require.NoError(t, err)

		require.NoError(t, run([]string{"verify", "-version", "1.2.6", binary}, nil, &out))
		require.NoError(t, os.Remove(binary+".sig"))
	})

	for name, tc := range map[string]struct {
		args   []string
		answer string
	}{
		"signed by a key this build does not trust": {[]string{"-version", "1.2.6"}, transitSign(t, otherKey, 1, input)},
		"signed for another version":                {[]string{"-version", "1.2.7"}, transitSign(t, privateKey, 1, input)},
		"signed for another arch":                   {[]string{"-version", "1.2.6", "-arch", "arm64"}, transitSign(t, privateKey, 1, input)},
		"not a transit answer":                      {[]string{"-version", "1.2.6"}, "Error writing data to transit/sign/agent-update: permission denied"},
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			args := append(append([]string{"sig-from-openbao"}, tc.args...), binary)
			assert.Error(t, run(args, strings.NewReader(tc.answer), &bytes.Buffer{}))
			assert.NoFileExists(t, binary+".sig")
		})
	}
}

// kmsSign answers a `message` request like `gcloud kms asymmetric-sign` does
// for an Ed25519 key, base64-encoded the way sign-release.sh hands it over.
func kmsSign(t *testing.T, priv ed25519.PrivateKey, input string) string {
	t.Helper()
	signed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(input))
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, signed))
}

func TestSigFromKMS(t *testing.T) {
	_, privateKey, _ := keygenForTest(t)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	_, otherKey, _ := keygenForTest(t)
	trustOnly(t, publicKey)

	binary := writeTestBinary(t, "device-agent-linux-amd64", "agent build")
	var stdout bytes.Buffer
	require.NoError(t, run([]string{"message", "-version", "1.4.0", binary}, nil, &stdout))
	input := stdout.String()

	t.Run("a valid KMS signature becomes a .sig the agent accepts", func(t *testing.T) {
		var out bytes.Buffer
		require.NoError(t, run([]string{"sig-from-kms", "-version", "1.4.0", binary}, strings.NewReader(kmsSign(t, privateKey, input)+"\n"), &out))
		assert.Contains(t, out.String(), "by Cloud KMS (key "+updatesig.KeyID(publicKey)+")")
		require.NoError(t, run([]string{"verify", "-version", "1.4.0", binary}, nil, &out))
		require.NoError(t, os.Remove(binary+".sig"))
	})

	for name, tc := range map[string]struct {
		args   []string
		answer string
	}{
		"signed by a key this build does not trust": {[]string{"-version", "1.4.0"}, kmsSign(t, otherKey, input)},
		"signed for another version":                {[]string{"-version", "1.4.1"}, kmsSign(t, privateKey, input)},
		"not a signature":                           {[]string{"-version", "1.4.0"}, "PERMISSION_DENIED: Permission 'cloudkms.cryptoKeyVersions.useToSign' denied"},
		"an OpenBao answer":                         {[]string{"-version", "1.4.0"}, transitSign(t, privateKey, 1, input)},
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			args := append(append([]string{"sig-from-kms"}, tc.args...), binary)
			assert.Error(t, run(args, strings.NewReader(tc.answer), &bytes.Buffer{}))
			assert.NoFileExists(t, binary+".sig")
		})
	}
}

// The fixture is real `bao read -format=json transit/keys/agent-update`
// output of an OpenBao 2.7.1 dev server after one rotation; the key IDs were
// computed outside Go.
func TestPubkeyFromOpenBao(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "transit-key-agent-update.json"))
	require.NoError(t, err)

	var stdout bytes.Buffer
	require.NoError(t, run([]string{"pubkey-from-openbao"}, bytes.NewReader(fixture), &stdout))
	assert.Equal(t,
		"CYJjWyOf1b1SAJXGHBQJoGcetaXDApxl0mDYkJ0qWhk= # key 12a3711e, OpenBao transit agent-update v1, created 2026-10-12\n"+
			"HWxenIQYOyLYEgrBGRf6BhQNnBEmdKl5XFScrmUCy/E= # key a4681beb, OpenBao transit agent-update v2, created 2026-10-12\n",
		stdout.String())

	keys, err := updatesig.ParsePublicKeys(stdout.String())
	require.NoError(t, err, "the output must paste into AGENT_UPDATE_PUBLIC_KEYS as is")
	require.Len(t, keys, 2)
	assert.Equal(t, "12a3711e", keys[0].ID)
	assert.Equal(t, "a4681beb", keys[1].ID)

	for name, json := range map[string]string{
		"not JSON":       "Error reading transit/keys/agent-update: permission denied",
		"an RSA key":     strings.Replace(string(fixture), `"type": "ed25519"`, `"type": "rsa-2048"`, 1),
		"no versions":    `{"data": {"name": "agent-update", "type": "ed25519", "keys": {}}}`,
		"a bad key":      `{"data": {"name": "agent-update", "type": "ed25519", "keys": {"1": {"public_key": "AAAA"}}}}`,
		"a bad version":  `{"data": {"name": "agent-update", "type": "ed25519", "keys": {"one": {"public_key": "CYJjWyOf1b1SAJXGHBQJoGcetaXDApxl0mDYkJ0qWhk="}}}}`,
		"a missing data": `{}`,
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			var out bytes.Buffer
			assert.Error(t, run([]string{"pubkey-from-openbao"}, strings.NewReader(json), &out))
			assert.Empty(t, out.String())
		})
	}
}

// sign with a development key produces .sig files the agent's own
// verification accepts for exactly the path each binary is published at, and
// verify agrees.
func TestSignRoundTrip(t *testing.T) {
	keyPath, privateKey, _ := keygenForTest(t)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	keys := publicKeys(publicKey)
	trustOnly(t, publicKey)

	linux := writeTestBinary(t, "reagent-linux-armv7", "linux build")
	windows := writeTestBinary(t, "reagent-windows-amd64", "authenticode-signed exe")
	frpc := writeTestBinary(t, "frpc-windows-amd64", "authenticode-signed frpc")

	var stdout bytes.Buffer
	require.NoError(t, run([]string{"sign", "-key-file", keyPath, "-version", "1.2.6", linux, windows}, nil, &stdout))
	require.NoError(t, run([]string{"sign", "-key-file", keyPath, "--version", "0.71.0", frpc}, nil, &stdout))
	assert.NotContains(t, stdout.String(), "PRIVATE KEY")
	assert.Contains(t, stdout.String(), "as reagent-update:v1 linux/armv7/1.2.6/reagent with key "+updatesig.KeyID(publicKey))

	for binary, target := range map[string]updatesig.Target{
		linux:   {Context: updatesig.ContextAgentUpdate, OS: "linux", Arch: "armv7", Version: "1.2.6", FileName: "reagent"},
		windows: {Context: updatesig.ContextAgentUpdate, OS: "windows", Arch: "amd64", Version: "1.2.6", FileName: "reagent.exe"},
		frpc:    {Context: updatesig.ContextFrpcDownload, OS: "windows", Arch: "amd64", Version: "0.71.0", FileName: "frpc.exe"},
	} {
		signature, err := os.ReadFile(binary + ".sig")
		require.NoError(t, err)
		keyID, err := updatesig.VerifyFile(keys, target, binary, signature)
		require.NoError(t, err, target.String())
		assert.Equal(t, updatesig.KeyID(publicKey), keyID)

		// The same signature names nothing else.
		otherVersion := target
		otherVersion.Version = "1.0.0"
		_, err = updatesig.VerifyFile(keys, otherVersion, binary, signature)
		assert.ErrorIs(t, err, updatesig.ErrBadSignature)
	}

	stdout.Reset()
	require.NoError(t, run([]string{"verify", "-version", "1.2.6", linux, windows}, nil, &stdout))
	assert.Contains(t, stdout.String(), "signature OK as reagent-update:v1 windows/amd64/1.2.6/reagent.exe (key "+updatesig.KeyID(publicKey)+")")

	// verify against an explicit public key, as a mirror operator would.
	stdout.Reset()
	require.NoError(t, run([]string{"verify", "-version", "0.71.0", "-public-key", updatesig.EncodePublicKey(publicKey), frpc}, nil, &stdout))

	// verify for another version, or after the binary changed, fails.
	assert.ErrorIs(t, run([]string{"verify", "-version", "1.2.5", linux}, nil, &stdout), updatesig.ErrBadSignature)
	require.NoError(t, os.WriteFile(linux, []byte("linux build, patched"), 0o755))
	assert.ErrorIs(t, run([]string{"verify", "-version", "1.2.6", linux}, nil, &stdout), updatesig.ErrBadSignature)
}

// -kind/-os/-arch name the target when the file name does not.
func TestSignWithExplicitTarget(t *testing.T) {
	keyPath, privateKey, _ := keygenForTest(t)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	trustOnly(t, publicKey)

	binary := writeTestBinary(t, "downloaded.bin", "linux build")
	var stdout bytes.Buffer
	require.NoError(t, run([]string{"sign", "-key-file", keyPath, "-kind", "reagent", "-os", "linux", "-arch", "arm64", "-version", "2.0.0", binary}, nil, &stdout))

	signature, err := os.ReadFile(binary + ".sig")
	require.NoError(t, err)
	target := updatesig.Target{Context: updatesig.ContextAgentUpdate, OS: "linux", Arch: "arm64", Version: "2.0.0", FileName: "reagent"}
	_, err = updatesig.VerifyFile(publicKeys(publicKey), target, binary, signature)
	require.NoError(t, err)

	// The flags override what the name says.
	named := writeTestBinary(t, "reagent-linux-amd64", "linux build")
	require.NoError(t, run([]string{"sign", "-key-file", keyPath, "-arch", "arm64", "-version", "2.0.0", named}, nil, &stdout))
	signature, err = os.ReadFile(named + ".sig")
	require.NoError(t, err)
	_, err = updatesig.VerifyFile(publicKeys(publicKey), target, named, signature)
	require.NoError(t, err)
}

func TestSignRefuses(t *testing.T) {
	keyPath, privateKey, _ := keygenForTest(t)

	t.Run("without a key file", func(t *testing.T) {
		trustOnly(t, privateKey.Public().(ed25519.PublicKey))
		binary := writeTestBinary(t, "reagent-linux-amd64", "build")

		err := run([]string{"sign", "-version", "1.2.6", binary}, nil, &bytes.Buffer{})
		assert.ErrorContains(t, err, "sign needs -key-file")
		assert.NoFileExists(t, binary+".sig")
	})

	t.Run("a key the build does not trust", func(t *testing.T) {
		_, otherPrivateKey, _ := keygenForTest(t)
		trustOnly(t, otherPrivateKey.Public().(ed25519.PublicKey))
		binary := writeTestBinary(t, "reagent-linux-amd64", "build")

		err := run([]string{"sign", "-key-file", keyPath, "-version", "1.2.6", binary}, nil, &bytes.Buffer{})
		assert.ErrorContains(t, err, "is not in AGENT_UPDATE_PUBLIC_KEYS")
		assert.NoFileExists(t, binary+".sig")
	})

	t.Run("a build that still holds the placeholder", func(t *testing.T) {
		previous := trustedKeys
		trustedKeys = func() ([]updatesig.PublicKey, error) { return nil, updatesig.ErrPlaceholderKey }
		t.Cleanup(func() { trustedKeys = previous })
		binary := writeTestBinary(t, "reagent-linux-amd64", "build")

		err := run([]string{"sign", "-key-file", keyPath, "-version", "1.2.6", binary}, nil, &bytes.Buffer{})
		assert.ErrorIs(t, err, updatesig.ErrPlaceholderKey)
		assert.NoFileExists(t, binary+".sig")
	})

	t.Run("a key file that is not a PEM key, without echoing it", func(t *testing.T) {
		trustOnly(t, privateKey.Public().(ed25519.PublicKey))
		notPEM := writeTestBinary(t, "key.pem", "c2VjcmV0LWJ1dC1ub3QtcGVt")
		binary := writeTestBinary(t, "reagent-linux-amd64", "build")

		err := run([]string{"sign", "-key-file", notPEM, "-version", "1.2.6", binary}, nil, &bytes.Buffer{})
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "c2VjcmV0LWJ1dC1ub3QtcGVt")
	})

	t.Run("without a version", func(t *testing.T) {
		trustOnly(t, privateKey.Public().(ed25519.PublicKey))
		binary := writeTestBinary(t, "reagent-linux-amd64", "build")

		err := run([]string{"sign", "-key-file", keyPath, binary}, nil, &bytes.Buffer{})
		assert.ErrorContains(t, err, "need -version")
		assert.NoFileExists(t, binary+".sig")
	})

	t.Run("a name that says neither kind nor platform, and no flags", func(t *testing.T) {
		trustOnly(t, privateKey.Public().(ed25519.PublicKey))
		named := writeTestBinary(t, "reagent-linux-amd64", "build")
		odd := writeTestBinary(t, "agent.bin", "build")

		err := run([]string{"sign", "-key-file", keyPath, "-version", "1.2.6", named, odd}, nil, &bytes.Buffer{})
		assert.ErrorContains(t, err, "none of device-agent, reagent, frpc")
		assert.NoFileExists(t, named+".sig", "nothing is signed when one target is unknown")

		err = run([]string{"sign", "-key-file", keyPath, "-version", "1.2.6", "-kind", "reagent", odd}, nil, &bytes.Buffer{})
		assert.ErrorContains(t, err, "-os/-arch")
		assert.NoFileExists(t, odd+".sig")
	})
}

// device-agent holds a dash: its build names still give os and arch.
func TestSignDeviceAgentBuildName(t *testing.T) {
	keyPath, privateKey, _ := keygenForTest(t)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	trustOnly(t, publicKey)

	for name, target := range map[string]updatesig.Target{
		"device-agent-linux-armv7":   {Context: updatesig.ContextAgentUpdate, OS: "linux", Arch: "armv7", Version: "1.3.0", FileName: "device-agent"},
		"device-agent-windows-amd64": {Context: updatesig.ContextAgentUpdate, OS: "windows", Arch: "amd64", Version: "1.3.0", FileName: "device-agent.exe"},
		"reagent-linux-arm64":        {Context: updatesig.ContextAgentUpdate, OS: "linux", Arch: "arm64", Version: "1.3.0", FileName: "reagent"},
	} {
		binary := writeTestBinary(t, name, "build of "+name)
		require.NoError(t, run([]string{"sign", "-key-file", keyPath, "-version", "1.3.0", binary}, nil, &bytes.Buffer{}))
		signature, err := os.ReadFile(binary + ".sig")
		require.NoError(t, err)
		_, err = updatesig.VerifyFile(publicKeys(publicKey), target, binary, signature)
		require.NoError(t, err, name)
		require.NoError(t, run([]string{"verify", "-version", "1.3.0", binary}, nil, &bytes.Buffer{}))
	}
}

// The manifest: signed as a subject, sealed into the envelope the agent opens.
func TestSealAndOpenAManifest(t *testing.T) {
	keyPath, privateKey, _ := keygenForTest(t)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	trustOnly(t, publicKey)
	subject := []string{"-context", updatesig.ContextAgentManifest, "-subject", "device-agent/manifest.json"}

	payload := writeTestBinary(t, "manifest.payload.json", `{"channels":{"production":"1.3.0"}}`)
	var out bytes.Buffer
	require.NoError(t, run(append(append([]string{"sign", "-key-file", keyPath}, subject...), payload), nil, &out))

	out.Reset()
	require.NoError(t, run(append(append([]string{"seal"}, subject...), payload), nil, &out))
	envelope := out.Bytes()
	got, _, err := updatesig.OpenEnvelope(publicKeys(publicKey), updatesig.Subject{Context: updatesig.ContextAgentManifest, Line: "device-agent/manifest.json"}, envelope)
	require.NoError(t, err)
	assert.Equal(t, `{"channels":{"production":"1.3.0"}}`, string(got))

	manifest := writeTestBinary(t, "manifest.json", string(envelope))
	out.Reset()
	require.NoError(t, run(append(append([]string{"open"}, subject...), manifest), nil, &out))
	assert.Equal(t, `{"channels":{"production":"1.3.0"}}`, out.String())

	t.Run("another subject does not open it", func(t *testing.T) {
		err := run([]string{"open", "-context", updatesig.ContextAgentManifest, "-subject", "re-agent/manifest.json", manifest}, nil, &bytes.Buffer{})
		assert.ErrorIs(t, err, updatesig.ErrBadSignature)
	})
	t.Run("a changed payload does not seal", func(t *testing.T) {
		require.NoError(t, os.WriteFile(payload, []byte(`{"channels":{"production":"0.0.1"}}`), 0o644))
		err := run(append(append([]string{"seal"}, subject...), payload), nil, &bytes.Buffer{})
		assert.ErrorIs(t, err, updatesig.ErrBadSignature)
	})
	t.Run("subject and binary flags do not mix", func(t *testing.T) {
		err := run(append(append([]string{"verify", "-version", "1.3.0"}, subject...), payload), nil, &bytes.Buffer{})
		assert.ErrorContains(t, err, "go together")
	})
}
