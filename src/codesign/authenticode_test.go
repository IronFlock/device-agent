package codesign

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"debug/pe"
	"encoding/asn1"
	"encoding/binary"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests sign a minimal PE32+ file the way osslsigncode and jsign do
// (PKCS #7 SignedData over SpcIndirectDataContent, signed attributes, one
// WIN_CERTIFICATE at the end of the file). The digest rules were checked
// against a Microsoft-signed signtool.exe; CI checks the published IronFlock
// binaries (TestSignedFiles).

const optHeaderSize = 240 // pe.OptionalHeader64

func minimalPE(t *testing.T, payload []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	dos := make([]byte, 64)
	dos[0], dos[1] = 'M', 'Z'
	binary.LittleEndian.PutUint32(dos[0x3c:], 64)
	b.Write(dos)
	b.WriteString("PE\x00\x00")
	require.NoError(t, binary.Write(&b, binary.LittleEndian, pe.FileHeader{Machine: pe.IMAGE_FILE_MACHINE_AMD64, SizeOfOptionalHeader: optHeaderSize}))
	require.NoError(t, binary.Write(&b, binary.LittleEndian, pe.OptionalHeader64{Magic: 0x20b, NumberOfRvaAndSizes: 16, SizeOfHeaders: 512}))
	b.Write(payload)
	for b.Len()%8 != 0 {
		b.WriteByte(0)
	}
	return b.Bytes()
}

const (
	testChecksumOff = 64 + 24 + 64
	testCertDirOff  = 64 + 24 + 112 + imageDirectorySecurity*8
)

type signOpts struct {
	key        *ecdsa.PrivateKey   // signs the signed attributes
	sid        *x509.Certificate   // the signer the SignerInfo names
	certs      []*x509.Certificate // the certificates the signature carries
	signers    int                 // SignerInfos (default 1)
	badDigest  bool                // a wrong image digest in the indirect data
	trailing   []byte              // bytes appended after the signature
	extraEntry bool                // a second WIN_CERTIFICATE entry
}

func mustMarshal(t *testing.T, v any, params ...string) []byte {
	t.Helper()
	var der []byte
	var err error
	if len(params) > 0 {
		der, err = asn1.MarshalWithParams(v, params[0])
	} else {
		der, err = asn1.Marshal(v)
	}
	require.NoError(t, err)
	return der
}

func raw(der []byte) asn1.RawValue { return asn1.RawValue{FullBytes: der} }

// explicit0 wraps der in [0] EXPLICIT (asn1.Marshal writes a RawValue's
// FullBytes as they are, ignoring an explicit tag).
func explicit0(der []byte) asn1.RawValue {
	return asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: der}
}

func authenticodeSign(t *testing.T, img []byte, o signOpts) []byte {
	t.Helper()
	certOff := len(img)
	sha256Alg := pkix.AlgorithmIdentifier{Algorithm: oidSHA256, Parameters: asn1.NullRawValue}

	// The image digest leaves out the checksum, the certificate table entry
	// and the table itself (which is not there yet).
	h := sha256.New()
	h.Write(img[:testChecksumOff])
	h.Write(img[testChecksumOff+4 : testCertDirOff])
	h.Write(img[testCertDirOff+8:])
	imageDigest := h.Sum(nil)
	if o.badDigest {
		imageDigest[0] ^= 0xff
	}

	type typeAndValue struct {
		Type  asn1.ObjectIdentifier
		Value asn1.RawValue
	}
	type digestInfo struct {
		Algorithm pkix.AlgorithmIdentifier
		Digest    []byte
	}
	spc := mustMarshal(t, struct {
		Data          typeAndValue
		MessageDigest digestInfo
	}{
		Data:          typeAndValue{Type: oidSpcPEImageData, Value: raw(mustMarshal(t, []asn1.RawValue{}))},
		MessageDigest: digestInfo{Algorithm: sha256Alg, Digest: imageDigest},
	})
	var spcRaw asn1.RawValue
	_, err := asn1.Unmarshal(spc, &spcRaw)
	require.NoError(t, err)
	md := sha256.Sum256(spcRaw.Bytes)

	attr := func(oid asn1.ObjectIdentifier, value []byte) []byte {
		return mustMarshal(t, struct {
			Type   asn1.ObjectIdentifier
			Values []asn1.RawValue `asn1:"set"`
		}{oid, []asn1.RawValue{raw(value)}})
	}
	attrs := append(attr(oidContentType, mustMarshal(t, oidSpcIndirectData)), attr(oidMessageDigest, mustMarshal(t, md[:]))...)
	setOf := append([]byte{0x31}, mustMarshal(t, asn1.RawValue{Tag: asn1.TagSet, IsCompound: true, Bytes: attrs})[1:]...)
	sum := sha256.Sum256(setOf)
	signature, err := o.key.Sign(rand.Reader, sum[:], crypto.SHA256)
	require.NoError(t, err)

	type signerInfo struct {
		Version         int
		IssuerAndSerial struct {
			Issuer asn1.RawValue
			Serial *big.Int
		}
		DigestAlgorithm           pkix.AlgorithmIdentifier
		SignedAttributes          asn1.RawValue
		DigestEncryptionAlgorithm pkix.AlgorithmIdentifier
		EncryptedDigest           []byte
	}
	si := signerInfo{Version: 1, DigestAlgorithm: sha256Alg, EncryptedDigest: signature,
		SignedAttributes:          asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: attrs},
		DigestEncryptionAlgorithm: pkix.AlgorithmIdentifier{Algorithm: oidECPublicKey}}
	si.IssuerAndSerial.Issuer = raw(o.sid.RawIssuer)
	si.IssuerAndSerial.Serial = o.sid.SerialNumber
	n := o.signers
	if n == 0 {
		n = 1
	}
	var infos []asn1.RawValue
	for i := 0; i < n; i++ {
		infos = append(infos, raw(mustMarshal(t, si)))
	}

	var bag []byte
	for _, c := range o.certs {
		bag = append(bag, c.Raw...)
	}
	sd := mustMarshal(t, struct {
		Version          int
		DigestAlgorithms []pkix.AlgorithmIdentifier `asn1:"set"`
		ContentInfo      struct {
			ContentType asn1.ObjectIdentifier
			Content     asn1.RawValue
		}
		Certificates asn1.RawValue
		SignerInfos  []asn1.RawValue `asn1:"set"`
	}{
		Version:          1,
		DigestAlgorithms: []pkix.AlgorithmIdentifier{sha256Alg},
		ContentInfo: struct {
			ContentType asn1.ObjectIdentifier
			Content     asn1.RawValue
		}{oidSpcIndirectData, explicit0(spc)},
		Certificates: asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: bag},
		SignerInfos:  infos,
	})
	p7 := mustMarshal(t, struct {
		ContentType asn1.ObjectIdentifier
		Content     asn1.RawValue
	}{oidSignedData, explicit0(sd)})

	entry := func(blob []byte) []byte {
		e := make([]byte, 8, 8+len(blob)+8)
		binary.LittleEndian.PutUint32(e[0:], uint32(8+len(blob)))
		binary.LittleEndian.PutUint16(e[4:], winCertRevision2)
		binary.LittleEndian.PutUint16(e[6:], winCertTypePKCSSigned)
		e = append(e, blob...)
		for len(e)%8 != 0 {
			e = append(e, 0)
		}
		return e
	}
	table := entry(p7)
	if o.extraEntry {
		table = append(table, entry(p7)...)
	}

	out := append(append([]byte(nil), img...), table...)
	binary.LittleEndian.PutUint32(out[testCertDirOff:], uint32(certOff))
	binary.LittleEndian.PutUint32(out[testCertDirOff+4:], uint32(len(table)))
	return append(out, o.trailing...)
}

func writeFile(t *testing.T, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "agent.exe")
	require.NoError(t, os.WriteFile(p, b, 0o600))
	return p
}

type pki struct {
	root, leaf *x509.Certificate
	leafKey    *ecdsa.PrivateKey
}

func newPKI(t *testing.T) pki {
	root, rootKey := mkCert(t, "IronFlock Code Signing Root", true, nil, nil)
	leaf, leafKey := mkCert(t, "IronFlock Device Agent", false, root, rootKey)
	return pki{root: root, leaf: leaf, leafKey: leafKey}
}

func TestVerifyFileAcceptsOurSignature(t *testing.T) {
	p := newPKI(t)
	img := authenticodeSign(t, minimalPE(t, []byte("agent code")), signOpts{key: p.leafKey, sid: p.leaf, certs: []*x509.Certificate{p.leaf, p.root}})
	assert.NoError(t, verifyFile(writeFile(t, img), []*x509.Certificate{p.root}))
}

// The attack the pin exists for: someone signs with their own certificate and
// adds our (public) leaf to the certificates the signature carries. Before the
// pin was bound to the signer, it took our leaf from that bag and passed.
func TestVerifyFileRejectsOurLeafCarriedByAnotherSigner(t *testing.T) {
	p := newPKI(t)
	attacker, attackerKey := mkCert(t, "Attacker", false, nil, nil)
	img := authenticodeSign(t, minimalPE(t, []byte("malware")), signOpts{key: attackerKey, sid: attacker, certs: []*x509.Certificate{attacker, p.leaf, p.root}})
	assert.ErrorIs(t, verifyFile(writeFile(t, img), []*x509.Certificate{p.root}), ErrWrongSigner)
}

// The SignerInfo names our leaf, but another key made the signature.
func TestVerifyFileRejectsSignatureByAnotherKeyNamingOurLeaf(t *testing.T) {
	p := newPKI(t)
	_, otherKey := mkCert(t, "Attacker", false, nil, nil)
	img := authenticodeSign(t, minimalPE(t, []byte("malware")), signOpts{key: otherKey, sid: p.leaf, certs: []*x509.Certificate{p.leaf, p.root}})
	assert.ErrorIs(t, verifyFile(writeFile(t, img), []*x509.Certificate{p.root}), ErrTampered)
}

func TestVerifyFileRejectsForeignRoot(t *testing.T) {
	p := newPKI(t)
	other := newPKI(t)
	img := authenticodeSign(t, minimalPE(t, []byte("agent code")), signOpts{key: other.leafKey, sid: other.leaf, certs: []*x509.Certificate{other.leaf, other.root}})
	assert.ErrorIs(t, verifyFile(writeFile(t, img), []*x509.Certificate{p.root}), ErrWrongSigner)
}

func TestVerifyFileRejectsChangedContent(t *testing.T) {
	p := newPKI(t)
	img := authenticodeSign(t, minimalPE(t, []byte("agent code")), signOpts{key: p.leafKey, sid: p.leaf, certs: []*x509.Certificate{p.leaf}})
	img[64+24+optHeaderSize] ^= 1 // the first payload byte
	assert.ErrorIs(t, verifyFile(writeFile(t, img), []*x509.Certificate{p.root}), ErrTampered)

	bad := authenticodeSign(t, minimalPE(t, []byte("agent code")), signOpts{key: p.leafKey, sid: p.leaf, certs: []*x509.Certificate{p.leaf}, badDigest: true})
	assert.ErrorIs(t, verifyFile(writeFile(t, bad), []*x509.Certificate{p.root}), ErrTampered)
}

// The checksum field is outside the digest (signing tools rewrite it).
func TestVerifyFileIgnoresChecksumField(t *testing.T) {
	p := newPKI(t)
	img := authenticodeSign(t, minimalPE(t, []byte("agent code")), signOpts{key: p.leafKey, sid: p.leaf, certs: []*x509.Certificate{p.leaf}})
	binary.LittleEndian.PutUint32(img[testChecksumOff:], 0xdeadbeef)
	assert.NoError(t, verifyFile(writeFile(t, img), []*x509.Certificate{p.root}))
}

func TestVerifyFileRejectsDataAfterTheSignature(t *testing.T) {
	p := newPKI(t)
	img := authenticodeSign(t, minimalPE(t, []byte("agent code")), signOpts{key: p.leafKey, sid: p.leaf, certs: []*x509.Certificate{p.leaf}, trailing: []byte("appended")})
	assert.ErrorIs(t, verifyFile(writeFile(t, img), []*x509.Certificate{p.root}), ErrTampered)
}

func TestVerifyFileRejectsAmbiguousSignatures(t *testing.T) {
	p := newPKI(t)
	roots := []*x509.Certificate{p.root}
	base := minimalPE(t, []byte("agent code"))

	twoSigners := authenticodeSign(t, base, signOpts{key: p.leafKey, sid: p.leaf, certs: []*x509.Certificate{p.leaf}, signers: 2})
	assert.ErrorIs(t, verifyFile(writeFile(t, twoSigners), roots), ErrWrongSigner, "two SignerInfos")

	twoEntries := authenticodeSign(t, base, signOpts{key: p.leafKey, sid: p.leaf, certs: []*x509.Certificate{p.leaf}, extraEntry: true})
	assert.ErrorIs(t, verifyFile(writeFile(t, twoEntries), roots), ErrWrongSigner, "two WIN_CERTIFICATE entries")

	// Two carried certificates with the signer's issuer and serial number.
	twin := *p.leaf
	dup := authenticodeSign(t, base, signOpts{key: p.leafKey, sid: p.leaf, certs: []*x509.Certificate{p.leaf, &twin}})
	assert.ErrorIs(t, verifyFile(writeFile(t, dup), roots), ErrWrongSigner, "two certificates match the signer")

	// The signer's certificate is not carried at all.
	missing := authenticodeSign(t, base, signOpts{key: p.leafKey, sid: p.leaf, certs: []*x509.Certificate{p.root}})
	assert.ErrorIs(t, verifyFile(writeFile(t, missing), roots), ErrWrongSigner, "signer not carried")
}

func TestVerifyFileUnsigned(t *testing.T) {
	p := newPKI(t)
	assert.ErrorIs(t, verifyFile(writeFile(t, minimalPE(t, []byte("agent code"))), []*x509.Certificate{p.root}), ErrUnsigned)
	assert.ErrorIs(t, verifyFile(writeFile(t, []byte("#!/bin/sh\necho not a PE file, long enough for a DOS header..........\n")), []*x509.Certificate{p.root}), ErrUnsigned)
}
