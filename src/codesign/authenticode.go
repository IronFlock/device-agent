package codesign

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"

	_ "crypto/sha256" // register SHA-256 for crypto.Hash.New
	_ "crypto/sha512" // register SHA-384 and SHA-512
)

// verifyFile is the complete Authenticode check of one PE file, in Go, so it
// runs (and is tested) on every platform, and the release checks the bytes it
// is about to publish with the code the device runs. It binds the pin to the
// certificate that made the signature:
//
//  1. the file has exactly one signature, and it is the last thing in the file;
//  2. the signature has exactly one signer, and exactly one of the carried
//     certificates is that signer (issuer and serial number);
//  3. that certificate's key made the signature over the signed attributes;
//  4. the signed message digest is the digest of the signed content, and the
//     signed content holds the digest of this file's image;
//  5. that certificate chains to one of roots, using no other trust store.
//
// Certificates a signature merely carries prove nothing: anyone can add our
// public leaf to their own signature. Only the signer counts.
func verifyFile(path string, roots []*x509.Certificate) error {
	_, err := checkFile(path, roots)
	return err
}

// CheckFile is verifyFile for the release tooling (cmd/sign-update
// authenticode): it checks path against roots, or against the embedded roots
// when roots is nil, and returns the certificate that made the signature.
func CheckFile(path string, roots []*x509.Certificate) (*x509.Certificate, error) {
	if roots == nil {
		roots = pinnedRoots
	}
	if len(roots) == 0 {
		return nil, errors.New("codesign: no root to check against")
	}
	return checkFile(path, roots)
}

func checkFile(path string, roots []*x509.Certificate) (*x509.Certificate, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the file being verified
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}

	img, err := parsePE(f, info.Size())
	if err != nil {
		return nil, err
	}
	sig, err := img.signature()
	if err != nil {
		return nil, err
	}
	signed, err := parseSignedData(sig)
	if err != nil {
		return nil, err
	}

	digest, err := img.digest(signed.imageHash)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(digest, signed.imageDigest) {
		return nil, fmt.Errorf("%w: the image digest differs", ErrTampered)
	}
	if err := verifyChainToPinnedRoots(roots, signed.signer, signed.others); err != nil {
		return nil, err
	}
	return signed.signer, nil
}

// peImage locates the fields Authenticode leaves out of the image digest.
type peImage struct {
	r           io.ReaderAt
	size        int64
	checksumOff int64 // OptionalHeader.CheckSum
	certDirOff  int64 // DataDirectory[IMAGE_DIRECTORY_ENTRY_SECURITY]
	certOff     int64 // the attribute certificate table (a file offset)
	certSize    int64
}

const (
	winCertRevision2       = 0x0200
	winCertTypePKCSSigned  = 0x0002
	imageDirectorySecurity = 4
)

func parsePE(r io.ReaderAt, size int64) (*peImage, error) {
	read := func(off int64, n int) ([]byte, error) {
		if off < 0 || off+int64(n) > size {
			return nil, fmt.Errorf("%w: not a PE file", ErrUnsigned)
		}
		b := make([]byte, n)
		if _, err := r.ReadAt(b, off); err != nil {
			return nil, err
		}
		return b, nil
	}

	dos, err := read(0, 64)
	if err != nil {
		return nil, err
	}
	if dos[0] != 'M' || dos[1] != 'Z' {
		return nil, fmt.Errorf("%w: not a PE file", ErrUnsigned)
	}
	peOff := int64(binary.LittleEndian.Uint32(dos[0x3c:]))
	hdr, err := read(peOff, 24)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(hdr[:4], []byte("PE\x00\x00")) {
		return nil, fmt.Errorf("%w: not a PE file", ErrUnsigned)
	}
	optSize := int64(binary.LittleEndian.Uint16(hdr[20:]))
	opt := peOff + 24
	magic, err := read(opt, 2)
	if err != nil {
		return nil, err
	}

	var countOff, dirsOff int64
	switch binary.LittleEndian.Uint16(magic) {
	case 0x10b: // PE32
		countOff, dirsOff = opt+92, opt+96
	case 0x20b: // PE32+
		countOff, dirsOff = opt+108, opt+112
	default:
		return nil, fmt.Errorf("%w: unknown optional header", ErrUnsigned)
	}
	count, err := read(countOff, 4)
	if err != nil {
		return nil, err
	}
	certDirOff := dirsOff + imageDirectorySecurity*8
	if binary.LittleEndian.Uint32(count) <= imageDirectorySecurity || certDirOff+8 > opt+optSize {
		return nil, ErrUnsigned
	}
	dir, err := read(certDirOff, 8)
	if err != nil {
		return nil, err
	}
	img := &peImage{
		r:           r,
		size:        size,
		checksumOff: opt + 64,
		certDirOff:  certDirOff,
		certOff:     int64(binary.LittleEndian.Uint32(dir[0:])),
		certSize:    int64(binary.LittleEndian.Uint32(dir[4:])),
	}
	if img.certSize == 0 {
		return nil, ErrUnsigned
	}
	// The signature is the last thing in a signed file: nothing may follow it
	// (it would not be covered by the digest), and it must lie after the
	// headers it is located from.
	if img.certOff < certDirOff+8 || img.certOff+img.certSize != size {
		return nil, fmt.Errorf("%w: the signature is not at the end of the file", ErrTampered)
	}
	return img, nil
}

// signature returns the PKCS #7 SignedData of the one signature the file
// carries (WIN_CERTIFICATE, revision 2, PKCS signed data).
func (img *peImage) signature() ([]byte, error) {
	table := make([]byte, img.certSize)
	if _, err := img.r.ReadAt(table, img.certOff); err != nil {
		return nil, err
	}
	if len(table) < 8 {
		return nil, fmt.Errorf("%w: truncated signature", ErrTampered)
	}
	length := int64(binary.LittleEndian.Uint32(table[0:]))
	revision := binary.LittleEndian.Uint16(table[4:])
	kind := binary.LittleEndian.Uint16(table[6:])
	if length < 8 || length > int64(len(table)) {
		return nil, fmt.Errorf("%w: truncated signature", ErrTampered)
	}
	if revision != winCertRevision2 || kind != winCertTypePKCSSigned {
		return nil, fmt.Errorf("%w: unsupported signature type", ErrWrongSigner)
	}
	// Only padding may follow: a second signature entry is not one we make.
	if !allZero(table[length:]) {
		return nil, fmt.Errorf("%w: more than one signature", ErrWrongSigner)
	}
	return table[8:length], nil
}

// digest is the Authenticode image digest: the whole file without the
// checksum field, the certificate table entry and the certificate table.
func (img *peImage) digest(h crypto.Hash) ([]byte, error) {
	hash := h.New()
	ranges := [][2]int64{
		{0, img.checksumOff},
		{img.checksumOff + 4, img.certDirOff},
		{img.certDirOff + 8, img.certOff},
	}
	for _, rg := range ranges {
		if rg[1] < rg[0] {
			return nil, fmt.Errorf("%w: overlapping headers", ErrTampered)
		}
		if _, err := io.Copy(hash, io.NewSectionReader(img.r, rg[0], rg[1]-rg[0])); err != nil {
			return nil, err
		}
	}
	return hash.Sum(nil), nil
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

var (
	oidSignedData      = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}
	oidSpcIndirectData = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 311, 2, 1, 4}
	oidSpcPEImageData  = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 311, 2, 1, 15}
	oidContentType     = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 3}
	oidMessageDigest   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 4}

	oidSHA256 = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}
	oidSHA384 = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 2}
	oidSHA512 = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 3}

	oidRSAEncryption   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 1}
	oidSHA256WithRSA   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 11}
	oidSHA384WithRSA   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 12}
	oidSHA512WithRSA   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 13}
	oidECPublicKey     = asn1.ObjectIdentifier{1, 2, 840, 10045, 2, 1}
	oidECDSAWithSHA256 = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 2}
	oidECDSAWithSHA384 = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 3}
	oidECDSAWithSHA512 = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 4}
)

type contentInfo struct {
	ContentType asn1.ObjectIdentifier
	Content     asn1.RawValue `asn1:"explicit,tag:0"`
}

type signedDataASN struct {
	Version          int
	DigestAlgorithms asn1.RawValue
	ContentInfo      contentInfo
	Certificates     asn1.RawValue   `asn1:"optional,tag:0"`
	CRLs             asn1.RawValue   `asn1:"optional,tag:1"`
	SignerInfos      []signerInfoASN `asn1:"set"`
}

type issuerAndSerial struct {
	Issuer       asn1.RawValue
	SerialNumber *big.Int
}

type signerInfoASN struct {
	Version                   int
	IssuerAndSerial           issuerAndSerial
	DigestAlgorithm           pkix.AlgorithmIdentifier
	SignedAttributes          asn1.RawValue `asn1:"optional,tag:0"`
	DigestEncryptionAlgorithm pkix.AlgorithmIdentifier
	EncryptedDigest           []byte
	UnsignedAttributes        asn1.RawValue `asn1:"optional,tag:1"`
}

type attributeASN struct {
	Type   asn1.ObjectIdentifier
	Values asn1.RawValue
}

type spcIndirectDataContent struct {
	Data struct {
		Type  asn1.ObjectIdentifier
		Value asn1.RawValue `asn1:"optional"`
	}
	MessageDigest struct {
		Algorithm pkix.AlgorithmIdentifier
		Digest    []byte
	}
}

type signedData struct {
	signer      *x509.Certificate
	others      []*x509.Certificate
	imageHash   crypto.Hash
	imageDigest []byte
}

func malformed(what string) error {
	return fmt.Errorf("%w: malformed signature (%s)", ErrTampered, what)
}

// parseSignedData checks steps 2 to 4 of verifyFile on the PKCS #7 blob.
func parseSignedData(der []byte) (*signedData, error) {
	var ci contentInfo
	rest, err := asn1.Unmarshal(der, &ci)
	if err != nil || !allZero(rest) || !ci.ContentType.Equal(oidSignedData) {
		return nil, malformed("content info")
	}
	// An explicitly tagged RawValue keeps its [0] wrapper: the element is
	// in .Bytes.
	var sd signedDataASN
	if rest, err = asn1.Unmarshal(ci.Content.Bytes, &sd); err != nil || len(rest) != 0 {
		return nil, malformed("signed data")
	}
	if !sd.ContentInfo.ContentType.Equal(oidSpcIndirectData) {
		return nil, malformed("content type")
	}
	spcDER := sd.ContentInfo.Content.Bytes
	var spc spcIndirectDataContent
	if rest, err = asn1.Unmarshal(spcDER, &spc); err != nil || len(rest) != 0 {
		return nil, malformed("indirect data")
	}
	var spcRaw asn1.RawValue
	if _, err = asn1.Unmarshal(spcDER, &spcRaw); err != nil {
		return nil, malformed("indirect data")
	}
	if !spc.Data.Type.Equal(oidSpcPEImageData) {
		return nil, malformed("not a PE image signature")
	}
	imageHash, ok := hashFor(spc.MessageDigest.Algorithm.Algorithm)
	if !ok {
		return nil, fmt.Errorf("%w: unsupported image digest", ErrWrongSigner)
	}

	if len(sd.SignerInfos) != 1 {
		return nil, fmt.Errorf("%w: %d signers", ErrWrongSigner, len(sd.SignerInfos))
	}
	si := sd.SignerInfos[0]

	certs, err := x509.ParseCertificates(sd.Certificates.Bytes)
	if err != nil || len(certs) == 0 {
		return nil, ErrUnsigned
	}
	var signer *x509.Certificate
	var others []*x509.Certificate
	matches := 0
	for _, c := range certs {
		if bytes.Equal(c.RawIssuer, si.IssuerAndSerial.Issuer.FullBytes) && si.IssuerAndSerial.SerialNumber != nil &&
			c.SerialNumber.Cmp(si.IssuerAndSerial.SerialNumber) == 0 {
			signer = c
			matches++
			continue
		}
		others = append(others, c)
	}
	if matches != 1 {
		return nil, fmt.Errorf("%w: %d certificates match the signer", ErrWrongSigner, matches)
	}

	// Signed attributes: the signature is over their DER as a SET OF.
	if len(si.SignedAttributes.FullBytes) == 0 {
		return nil, malformed("no signed attributes")
	}
	attrs, err := parseAttributes(si.SignedAttributes.Bytes)
	if err != nil {
		return nil, err
	}
	var contentType asn1.ObjectIdentifier
	if v, ok := attrs[oidContentType.String()]; !ok {
		return nil, malformed("no content type attribute")
	} else if _, err := asn1.Unmarshal(v, &contentType); err != nil || !contentType.Equal(oidSpcIndirectData) {
		return nil, malformed("content type attribute")
	}
	var messageDigest []byte
	if v, ok := attrs[oidMessageDigest.String()]; !ok {
		return nil, malformed("no message digest attribute")
	} else if _, err := asn1.Unmarshal(v, &messageDigest); err != nil {
		return nil, malformed("message digest attribute")
	}

	signerHash, ok := hashFor(si.DigestAlgorithm.Algorithm)
	if !ok {
		return nil, fmt.Errorf("%w: unsupported signer digest", ErrWrongSigner)
	}
	// Authenticode digests the indirect data's content octets, without the
	// SEQUENCE tag and length.
	h := signerHash.New()
	h.Write(spcRaw.Bytes)
	if !bytes.Equal(h.Sum(nil), messageDigest) {
		return nil, fmt.Errorf("%w: the message digest differs", ErrTampered)
	}

	algo, err := signatureAlgorithm(signer, signerHash, si.DigestEncryptionAlgorithm.Algorithm)
	if err != nil {
		return nil, err
	}
	signedAttrs := append([]byte(nil), si.SignedAttributes.FullBytes...)
	signedAttrs[0] = 0x31 // [0] IMPLICIT -> SET OF
	if err := signer.CheckSignature(algo, signedAttrs, si.EncryptedDigest); err != nil {
		return nil, fmt.Errorf("%w: the signature does not verify", ErrTampered)
	}

	return &signedData{signer: signer, others: others, imageHash: imageHash, imageDigest: spc.MessageDigest.Digest}, nil
}

// parseAttributes returns each attribute's single value; an attribute given
// twice, or with other than one value, is malformed.
func parseAttributes(b []byte) (map[string][]byte, error) {
	out := map[string][]byte{}
	for len(b) > 0 {
		var a attributeASN
		var err error
		if b, err = asn1.Unmarshal(b, &a); err != nil {
			return nil, malformed("attribute")
		}
		if a.Values.Class != asn1.ClassUniversal || a.Values.Tag != asn1.TagSet {
			return nil, malformed("attribute values")
		}
		var v asn1.RawValue
		rest, err := asn1.Unmarshal(a.Values.Bytes, &v)
		if err != nil || len(rest) != 0 {
			return nil, malformed("attribute value")
		}
		key := a.Type.String()
		if _, dup := out[key]; dup {
			return nil, malformed("duplicate attribute")
		}
		out[key] = v.FullBytes
	}
	return out, nil
}

func hashFor(oid asn1.ObjectIdentifier) (crypto.Hash, bool) {
	switch {
	case oid.Equal(oidSHA256):
		return crypto.SHA256, true
	case oid.Equal(oidSHA384):
		return crypto.SHA384, true
	case oid.Equal(oidSHA512):
		return crypto.SHA512, true
	}
	return 0, false
}

var errUnsupportedSignature = errors.New("unsupported signature algorithm")

func signatureAlgorithm(signer *x509.Certificate, h crypto.Hash, oid asn1.ObjectIdentifier) (x509.SignatureAlgorithm, error) {
	switch signer.PublicKey.(type) {
	case *rsa.PublicKey:
		ok := oid.Equal(oidRSAEncryption) ||
			(h == crypto.SHA256 && oid.Equal(oidSHA256WithRSA)) ||
			(h == crypto.SHA384 && oid.Equal(oidSHA384WithRSA)) ||
			(h == crypto.SHA512 && oid.Equal(oidSHA512WithRSA))
		if ok {
			return map[crypto.Hash]x509.SignatureAlgorithm{
				crypto.SHA256: x509.SHA256WithRSA, crypto.SHA384: x509.SHA384WithRSA, crypto.SHA512: x509.SHA512WithRSA,
			}[h], nil
		}
	case *ecdsa.PublicKey:
		ok := oid.Equal(oidECPublicKey) ||
			(h == crypto.SHA256 && oid.Equal(oidECDSAWithSHA256)) ||
			(h == crypto.SHA384 && oid.Equal(oidECDSAWithSHA384)) ||
			(h == crypto.SHA512 && oid.Equal(oidECDSAWithSHA512))
		if ok {
			return map[crypto.Hash]x509.SignatureAlgorithm{
				crypto.SHA256: x509.ECDSAWithSHA256, crypto.SHA384: x509.ECDSAWithSHA384, crypto.SHA512: x509.ECDSAWithSHA512,
			}[h], nil
		}
	}
	return 0, fmt.Errorf("%w: %w", ErrWrongSigner, errUnsupportedSignature)
}
