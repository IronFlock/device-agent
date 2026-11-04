package codesign

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mkCert issues a certificate signed by parent (self-signed when parent/parentKey
// are nil), mirroring the two-tier root→leaf PKI used in production.
func mkCert(t *testing.T, cn string, isCA bool, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		BasicConstraintsValid: true,
		IsCA:                  isCA,
	}
	if isCA {
		tmpl.KeyUsage = x509.KeyUsageCertSign
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}
	}

	signer, signerKey := parent, parentKey
	if signer == nil {
		signer, signerKey = tmpl, key // self-signed
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &key.PublicKey, signerKey)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return cert, key
}

func TestVerifyChainToPinnedRootAcceptsOwnLeaf(t *testing.T) {
	root, rootKey := mkCert(t, "IronFlock Root", true, nil, nil)
	leaf, _ := mkCert(t, "IronFlock Codesign", false, root, rootKey)

	err := verifyChainToPinnedRoots([]*x509.Certificate{root}, leaf, nil)
	assert.NoError(t, err, "a leaf chaining to a pinned root must verify")
}

func TestVerifyChainToPinnedRootWithIntermediate(t *testing.T) {
	root, rootKey := mkCert(t, "IronFlock Root", true, nil, nil)
	inter, interKey := mkCert(t, "IronFlock Intermediate", true, root, rootKey)
	leaf, _ := mkCert(t, "IronFlock Codesign", false, inter, interKey)

	err := verifyChainToPinnedRoots([]*x509.Certificate{root}, leaf, []*x509.Certificate{inter})
	assert.NoError(t, err)
}

// The overlap window: with both the old and new root pinned, a leaf under
// EITHER must verify — this is what makes a hard-cutover-free root rotation
// possible.
func TestVerifyChainAcceptsEitherRootDuringRotation(t *testing.T) {
	oldRoot, oldKey := mkCert(t, "IronFlock Root v1", true, nil, nil)
	newRoot, newKey := mkCert(t, "IronFlock Root v2", true, nil, nil)
	roots := []*x509.Certificate{oldRoot, newRoot}

	oldLeaf, _ := mkCert(t, "Codesign", false, oldRoot, oldKey)
	newLeaf, _ := mkCert(t, "Codesign", false, newRoot, newKey)

	assert.NoError(t, verifyChainToPinnedRoots(roots, oldLeaf, nil), "leaf under old root must verify")
	assert.NoError(t, verifyChainToPinnedRoots(roots, newLeaf, nil), "leaf under new root must verify")
}

func TestVerifyChainToPinnedRootRejectsForeignSigner(t *testing.T) {
	// A leaf under a DIFFERENT root — the exact "machine trusts many CAs"
	// substitution attack pinning is meant to stop.
	pinnedRootCert, _ := mkCert(t, "IronFlock Root", true, nil, nil)
	foreignRoot, foreignKey := mkCert(t, "Enterprise CA", true, nil, nil)
	foreignLeaf, _ := mkCert(t, "Attacker Codesign", false, foreignRoot, foreignKey)

	err := verifyChainToPinnedRoots([]*x509.Certificate{pinnedRootCert}, foreignLeaf, nil)
	assert.ErrorIs(t, err, ErrWrongSigner, "a leaf under another root must be rejected")
}

func TestVerifyChainRejectsSelfSignedImposter(t *testing.T) {
	pinnedRootCert, _ := mkCert(t, "IronFlock Root", true, nil, nil)
	imposter, _ := mkCert(t, "IronFlock Codesign", false, nil, nil) // self-signed, same CN

	err := verifyChainToPinnedRoots([]*x509.Certificate{pinnedRootCert}, imposter, nil)
	assert.ErrorIs(t, err, ErrWrongSigner, "same-name self-signed cert must not pass the pin")
}

// When no root is configured (pre-signing transition, or a placeholder embed),
// verification no-ops rather than blocking updates.
func TestVerifyNoopsWhenUnconfigured(t *testing.T) {
	saved := pinnedRoots
	defer func() { pinnedRoots = saved }()

	pinnedRoots = nil
	assert.False(t, Configured())
	assert.NoError(t, Verify("/any/path"), "Verify must no-op when no root is configured")
}

func TestEnforcingRequiresConfiguredRoot(t *testing.T) {
	savedRoots, savedEnforce := pinnedRoots, enforce
	defer func() { pinnedRoots, enforce = savedRoots, savedEnforce }()

	// Even with the switch on, no configured root => never enforce (would
	// otherwise reject everything for lack of a pin to check against).
	pinnedRoots = nil
	enforce = true
	assert.False(t, Enforcing(), "enforcement must require a configured root")

	// With a root configured AND the switch on, enforcement is active.
	root, _ := mkCert(t, "IronFlock Root", true, nil, nil)
	pinnedRoots = []*x509.Certificate{root}
	assert.True(t, Enforcing())
}

// The shipped build enforces: the embedded root is configured and the switch
// is on, so an unsigned or foreign-signed download is refused.
func TestShippedBuildEnforces(t *testing.T) {
	require.True(t, Configured(), "the IronFlock root must be embedded (roots/*.crt)")
	assert.True(t, Enforcing(), "Authenticode enforcement must be on")
}
