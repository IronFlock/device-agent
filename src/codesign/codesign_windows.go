//go:build windows

package codesign

import (
	"fmt"
	"os"
	"time"
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

// ReportSelf runs both checks once on the agent's own executable, the binary
// an update or the installer downloaded, and logs each result. It never
// refuses or changes anything. It shows, on every device and right after an
// update, whether the check passes there (no internet, the machine's trust
// store, its clock) before enforcement is turned on. Call it in the
// background: WinVerifyTrust can wait on the network.
func ReportSelf() {
	if len(pinnedRoots) == 0 {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Warn().Msgf("codesign: self-check panicked: %v", r)
		}
	}()
	exe, err := os.Executable()
	if err != nil {
		log.Warn().Err(err).Msg("codesign: self-check skipped: the agent's executable is unknown")
		return
	}

	start := time.Now()
	winErr := winVerifyTrust(exe)
	winTook := time.Since(start)
	signer, pinErr := checkFile(exe, pinnedRoots)

	event := log.Info()
	if winErr != nil || pinErr != nil {
		event = log.Warn()
	}
	event = event.Str("file", exe).Bool("enforcing", Enforcing()).
		Str("windows_check", resultOf(winErr)).Dur("windows_check_took", winTook).
		Str("pin_check", resultOf(pinErr))
	if signer != nil {
		event = event.Str("signer", signer.Subject.CommonName).
			Str("signer_serial", signer.SerialNumber.Text(16)).
			Time("signer_valid_until", signer.NotAfter)
	}
	if winErr != nil || pinErr != nil {
		event.Msg("codesign: self-check FAILED: with enforcement on, this device would refuse updates signed like this agent")
		return
	}
	event.Msg("codesign: self-check passed")
}

func resultOf(err error) string {
	if err != nil {
		return err.Error()
	}
	return "ok"
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
