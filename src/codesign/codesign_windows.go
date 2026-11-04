//go:build windows

package codesign

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Authenticode result codes (HRESULTs from WinVerifyTrust).
const (
	trustENoSignature   = 0x800B0100 // TRUST_E_NOSIGNATURE
	trustEBadDigest     = 0x80096010 // TRUST_E_BAD_DIGEST
	certEUntrustedRoot  = 0x800B0109 // CERT_E_UNTRUSTEDROOT
	trustESubjectFormUn = 0x800B0003 // TRUST_E_SUBJECT_FORM_UNKNOWN
)

// platformVerify runs two independent checks, and both must pass:
//  1. Windows' own: the Authenticode signature is well-formed and the file
//     digest matches (WinVerifyTrust). CERT_E_UNTRUSTEDROOT is accepted here:
//     the machine's trust store is not the trust decision, because…
//  2. ours (verifyFile): the certificate that MADE the signature chains to one
//     of our embedded roots, using no other trust store, with the digests and
//     the signature checked again in Go.
//
// A panic in the syscall glue must never crash the agent; recover into an error
// so a verification bug degrades to "treat as unverified", not a crash.
func platformVerify(path string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("codesign: panic during verification: %v", r)
		}
	}()

	if verr := winVerifyTrust(path); verr != nil {
		return verr
	}
	return verifyFile(path, pinnedRoots)
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
