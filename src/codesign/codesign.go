// Package codesign verifies that a binary was Authenticode-signed by
// IronFlock's own code-signing key, independent of whatever certificate
// authorities the machine happens to trust.
//
// It is used to authenticate self-update payloads the Windows service
// downloads and executes as SYSTEM. The SHA-256 manifest checked in
// system.downloadBinary is NOT an authenticity anchor — it is fetched from the
// same origin as the binary and skipped when absent — so the pinned signature
// is the real defense against a compromised distribution server.
//
// Pinning is chainless with respect to the machine store: the signer must
// chain to OUR embedded root, so a device that trusts many enterprise CAs
// still won't accept a binary signed by a different one.
package codesign

import (
	"crypto/x509"
	"embed"
	"encoding/pem"
	"errors"
	"path"
	"strings"

	"github.com/rs/zerolog/log"
)

//go:embed roots
var rootsFS embed.FS

var (
	// ErrUnsigned: the file carries no Authenticode signature.
	ErrUnsigned = errors.New("binary is not signed")
	// ErrTampered: the signature is present but the file's digest does not match.
	ErrTampered = errors.New("binary signature does not match its contents")
	// ErrWrongSigner: validly signed, but not by any pinned root.
	ErrWrongSigner = errors.New("binary is not signed by a pinned IronFlock key")
)

// EmbeddedRoot is one pinned root the installer imports into the device trust
// stores. During a root rotation there is more than one (the overlap window).
type EmbeddedRoot struct {
	FileName   string // basename under roots/, e.g. "ironflock-root.crt"
	CommonName string // for certutil -delstore at uninstall
	PEM        []byte
}

// pinnedRoots are the IronFlock root CAs a signer may chain to. Empty when only
// the placeholder is present (pre-signing transition) — verification no-ops.
var pinnedRoots []*x509.Certificate

// embeddedRoots mirrors pinnedRoots with the metadata the installer needs.
var embeddedRoots []EmbeddedRoot

func init() {
	entries, err := rootsFS.ReadDir("roots")
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".crt") {
			continue
		}
		data, err := rootsFS.ReadFile(path.Join("roots", entry.Name()))
		if err != nil {
			continue
		}
		// A .crt file may hold more than one PEM block.
		rest := data
		for {
			var block *pem.Block
			block, rest = pem.Decode(rest)
			if block == nil {
				break
			}
			cert, perr := x509.ParseCertificate(block.Bytes)
			if perr != nil {
				log.Warn().Err(perr).Msgf("codesign: skipping invalid cert in roots/%s", entry.Name())
				continue
			}
			pinnedRoots = append(pinnedRoots, cert)
			embeddedRoots = append(embeddedRoots, EmbeddedRoot{
				FileName:   entry.Name(),
				CommonName: cert.Subject.CommonName,
				PEM:        pem.EncodeToMemory(block),
			})
		}
	}
}

// Configured reports whether at least one real pinned root is embedded (i.e.
// signing has been set up). Callers can use this to decide whether to enforce.
func Configured() bool {
	return len(pinnedRoots) > 0
}

// enforce makes a failed signature check fatal: an unsigned binary, or one
// signed by anyone but IronFlock, is refused. Windows releases have been
// signed since the Windows service shipped (July 2026); the switch stayed off
// until the pin was bound to the certificate that made the signature
// (verifyFile).
var enforce = true

// enforceStr also turns enforcement on at build time
// (-ldflags "-X reagent/codesign.enforceStr=true"); it cannot turn it off.
var enforceStr = ""

// Enforcing reports whether a failed signature check must REJECT the binary.
// True only when both enabled AND a real root is configured, so enforcement
// can never reject everything for lack of a pin to check against.
func Enforcing() bool {
	on := enforce || enforceStr == "true"
	return on && Configured()
}

// EmbeddedRoots returns the pinned roots (with the metadata the installer
// needs to write + import + later remove them). Empty during the pre-signing
// transition. More than one entry during a root rotation's overlap window.
func EmbeddedRoots() []EmbeddedRoot {
	return embeddedRoots
}

// Verify checks that binaryPath is Authenticode-signed by a pinned IronFlock
// key. On non-Windows platforms, and when no root is configured, it returns
// nil (Authenticode is a Windows concept; the transition period ships unsigned).
func Verify(binaryPath string) error {
	if len(pinnedRoots) == 0 {
		log.Debug().Msg("codesign: no pinned root configured; skipping signature verification")
		return nil
	}
	return platformVerify(binaryPath)
}

// verifyChainToPinnedRoots verifies the signer leaf chains to ANY pinned root,
// using ONLY those roots (not the machine trust store). Accepting any of the
// roots is what enables root rotation's overlap window. Cross-platform so it is
// unit-testable off Windows.
func verifyChainToPinnedRoots(roots []*x509.Certificate, leaf *x509.Certificate, intermediates []*x509.Certificate) error {
	rootPool := x509.NewCertPool()
	for _, r := range roots {
		rootPool.AddCert(r)
	}

	inter := x509.NewCertPool()
	for _, c := range intermediates {
		inter.AddCert(c)
	}

	_, err := leaf.Verify(x509.VerifyOptions{
		Roots:         rootPool,
		Intermediates: inter,
		// Authenticode leaves carry the code-signing EKU; accept any EKU here
		// and let the pin (chain to one of our roots) be the trust decision.
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	})
	if err != nil {
		return ErrWrongSigner
	}
	return nil
}
