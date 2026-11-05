package codesign

import "fmt"

// Authenticode result codes (HRESULTs from WinVerifyTrust).
const (
	trustENoSignature   = 0x800B0100 // TRUST_E_NOSIGNATURE
	trustEBadDigest     = 0x80096010 // TRUST_E_BAD_DIGEST
	certEUntrustedRoot  = 0x800B0109 // CERT_E_UNTRUSTEDROOT
	certEChaining       = 0x800B010A // CERT_E_CHAINING
	trustESubjectFormUn = 0x800B0003 // TRUST_E_SUBJECT_FORM_UNKNOWN
)

// windowsTrustResult turns WinVerifyTrust's answer (hres, and the error it
// came as) into the result of the Windows half of the check (platformVerify).
// Pure, so it is tested on every platform.
//
// Two answers mean "Windows does not trust our root", and both pass: the
// machine's trust store is not the trust decision, the pin is (verifyFile
// checks the digests and the signature again and requires a chain to an
// embedded root):
//   - CERT_E_UNTRUSTEDROOT: the chain reaches our root, which the machine
//     does not trust.
//   - CERT_E_CHAINING: no chain can be built at all. This is the answer on a
//     machine without our root in its store whenever the signature does not
//     carry the root, and jsign (7.6) never puts a self-signed root into a
//     signature. Every machine installed before the installer imported the
//     root is such a machine, and a self-update never imports it. Refusing
//     this answer would make those devices refuse every update once
//     enforcement is on.
func windowsTrustResult(hres uint32, verifyErr error) error {
	switch hres {
	case trustENoSignature, trustESubjectFormUn:
		return ErrUnsigned
	case trustEBadDigest:
		return ErrTampered
	case certEUntrustedRoot, certEChaining:
		return nil
	default:
		return fmt.Errorf("WinVerifyTrust failed: %w", verifyErr)
	}
}
