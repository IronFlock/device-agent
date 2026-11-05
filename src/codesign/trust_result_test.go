package codesign

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The machine's trust store is not the trust decision: an untrusted root and a
// root the machine does not have at all both leave the decision to the pin.
// The second is what a jsign signature (signer only, no root) gets on a
// machine without our root in its store.
func TestWindowsTrustResultLeavesAMissingOrUntrustedRootToThePin(t *testing.T) {
	for name, hres := range map[string]uint32{
		"CERT_E_UNTRUSTEDROOT": certEUntrustedRoot,
		"CERT_E_CHAINING":      certEChaining,
	} {
		t.Run(name, func(t *testing.T) {
			assert.NoError(t, windowsTrustResult(hres, errors.New(name)))
		})
	}
}

func TestWindowsTrustResultRefusesUnsignedAndTamperedFiles(t *testing.T) {
	assert.ErrorIs(t, windowsTrustResult(trustENoSignature, errors.New("no signature")), ErrUnsigned)
	assert.ErrorIs(t, windowsTrustResult(trustESubjectFormUn, errors.New("not a PE")), ErrUnsigned)
	assert.ErrorIs(t, windowsTrustResult(trustEBadDigest, errors.New("bad digest")), ErrTampered)
}

func TestWindowsTrustResultRefusesEveryOtherFailure(t *testing.T) {
	cause := errors.New("CERT_E_EXPIRED")
	err := windowsTrustResult(0x800B0101, cause)
	require.Error(t, err)
	assert.ErrorIs(t, err, cause)
	assert.Contains(t, err.Error(), "WinVerifyTrust failed")
}
