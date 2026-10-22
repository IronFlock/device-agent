//go:build windows

package codesign

import (
	"crypto/x509"
	"fmt"
	"unsafe"

	"github.com/rs/zerolog/log"
	"golang.org/x/sys/windows"
)

// Authenticode result codes (HRESULTs from WinVerifyTrust).
const (
	trustENoSignature   = 0x800B0100 // TRUST_E_NOSIGNATURE
	trustEBadDigest     = 0x80096010 // TRUST_E_BAD_DIGEST
	certEUntrustedRoot  = 0x800B0109 // CERT_E_UNTRUSTEDROOT
	trustESubjectFormUn = 0x800B0003 // TRUST_E_SUBJECT_FORM_UNKNOWN
)

// platformVerify enforces two independent properties:
//  1. Integrity/validity — the Authenticode signature is well-formed and the
//     file digest matches (WinVerifyTrust). We deliberately ACCEPT
//     CERT_E_UNTRUSTEDROOT here: our self-signed root need not be in the
//     machine store for this check, because…
//  2. Pinning — the signer chains to OUR embedded root (verifyChainToPinnedRoot),
//     independent of the machine trust store, so trusting other CAs can't
//     substitute a different signer.
//
// A panic in the syscall glue must never crash the agent; recover into an error
// so a verification bug degrades to "treat as unverified", not a crash.
func platformVerify(path string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("codesign: panic during verification: %v", r)
		}
	}()

	// (1) Integrity + signature validity.
	if verr := winVerifyTrust(path); verr != nil {
		return verr
	}

	// (2) Pin to one of our roots.
	leaf, intermediates, cerr := extractSignerCerts(path)
	if cerr != nil {
		return cerr
	}
	if leaf == nil {
		return ErrUnsigned
	}
	return verifyChainToPinnedRoots(pinnedRoots, leaf, intermediates)
}

func winVerifyTrust(path string) error {
	filePath, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}

	fileInfo := windows.WinTrustFileInfo{
		Size:     uint32(unsafe.Sizeof(windows.WinTrustFileInfo{})),
		FilePath: filePath,
	}

	data := windows.WinTrustData{
		Size:                            uint32(unsafe.Sizeof(windows.WinTrustData{})),
		UIChoice:                        windows.WTD_UI_NONE,
		RevocationChecks:                windows.WTD_REVOKE_NONE,
		UnionChoice:                     windows.WTD_CHOICE_FILE,
		StateAction:                     windows.WTD_STATEACTION_VERIFY,
		FileOrCatalogOrBlobOrSgnrOrCert: unsafe.Pointer(&fileInfo), //nolint:gosec // G103: Win32 WinVerifyTrust / certificate API calls
	}

	action := windows.WINTRUST_ACTION_GENERIC_VERIFY_V2
	verifyErr := windows.WinVerifyTrustEx(windows.InvalidHWND, &action, &data)

	// Always release the state, regardless of the verify result.
	data.StateAction = windows.WTD_STATEACTION_CLOSE
	_ = windows.WinVerifyTrustEx(windows.InvalidHWND, &action, &data)

	if verifyErr == nil {
		return nil
	}

	switch hres := hresultOf(verifyErr); hres {
	case trustENoSignature, trustESubjectFormUn:
		return ErrUnsigned
	case trustEBadDigest:
		return ErrTampered
	case certEUntrustedRoot:
		// Signature is cryptographically valid; the machine just doesn't trust
		// the root. That's fine — pinning (step 2) is our trust decision.
		return nil
	default:
		return fmt.Errorf("WinVerifyTrust failed: %w", verifyErr)
	}
}

func hresultOf(err error) uint32 {
	if errno, ok := err.(windows.Errno); ok {
		return uint32(errno)
	}
	return 0
}

// extractSignerCerts pulls the certificates embedded in the PE's Authenticode
// signature. The first certificate with the code-signing usage (or, failing
// that, the first cert) is treated as the leaf; the rest are intermediates.
func extractSignerCerts(path string) (*x509.Certificate, []*x509.Certificate, error) {
	obj, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, nil, err
	}

	// We only need the certificate store, not the decoded message handle, so
	// pass nil for msg — there is then no HCRYPTMSG to close.
	var certStore windows.Handle
	err = windows.CryptQueryObject(
		windows.CERT_QUERY_OBJECT_FILE,
		unsafe.Pointer(obj), //nolint:gosec // G103: Win32 WinVerifyTrust / certificate API calls
		windows.CERT_QUERY_CONTENT_FLAG_PKCS7_SIGNED_EMBED,
		windows.CERT_QUERY_FORMAT_FLAG_BINARY,
		0,
		nil, nil, nil,
		&certStore, nil, nil,
	)
	if err != nil {
		// No embedded signature at all.
		return nil, nil, ErrUnsigned
	}
	defer func() { _ = windows.CertCloseStore(certStore, 0) }()

	var certs []*x509.Certificate
	var prev *windows.CertContext
	for {
		ctx, enumErr := windows.CertEnumCertificatesInStore(certStore, prev)
		if ctx == nil || enumErr != nil {
			break
		}
		der := unsafe.Slice(ctx.EncodedCert, ctx.Length) //nolint:gosec // G103: Win32 WinVerifyTrust / certificate API calls
		buf := make([]byte, len(der))
		copy(buf, der)
		if c, perr := x509.ParseCertificate(buf); perr == nil {
			certs = append(certs, c)
		}
		prev = ctx
	}

	if len(certs) == 0 {
		return nil, nil, ErrUnsigned
	}

	leafIdx := 0
	for i, c := range certs {
		for _, eku := range c.ExtKeyUsage {
			if eku == x509.ExtKeyUsageCodeSigning {
				leafIdx = i
				break
			}
		}
	}

	leaf := certs[leafIdx]
	intermediates := make([]*x509.Certificate, 0, len(certs)-1)
	for i, c := range certs {
		if i != leafIdx {
			intermediates = append(intermediates, c)
		}
	}

	log.Debug().Msgf("codesign: extracted %d certificate(s) from %s", len(certs), path)
	return leaf, intermediates, nil
}
