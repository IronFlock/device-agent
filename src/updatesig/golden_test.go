package updatesig

import (
	"crypto/ed25519"
	"testing"
)

// TestGoldenVector pins the signature format. The same file is in
// device-agent/src/updatesig and device-init/cli/updatesig: the agent and
// ironflock-init must accept exactly the same signatures, so when this test
// needs a change in one repository, the other changes with it.
func TestGoldenVector(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	if got := EncodePublicKey(pub); got != "A6EHv/POEL4dcN0Y50vAmWfk1jCbpQ1fHdyGZBJVMbg=" {
		t.Fatalf("public key %s", got)
	}
	keys := []PublicKey{{ID: KeyID(pub), Key: pub}}
	data := []byte("ironflock golden vector\n")

	target := Target{Context: ContextAgentUpdate, OS: "linux", Arch: "armv7", Version: "1.3.0", FileName: "device-agent"}
	targetSig := "hk8wlCMIWuPV3tXx4Mr/s5bW0zxViyPpWyt2Gtu1X2i1G5AodchSKYL0PcxPhy7hqcP8bm/iMPwsNCA6evMMCA==\n"
	sig, err := SignBytes(priv, Subject{Context: target.Context, Line: target.Path()}, data)
	if err != nil || string(sig) != targetSig {
		t.Fatalf("binary signature %q, %v", sig, err)
	}
	if _, err := VerifyBytes(keys, Subject{Context: target.Context, Line: target.Path()}, data, []byte(targetSig)); err != nil {
		t.Fatalf("binary signature does not verify: %v", err)
	}

	manifest := Subject{Context: ContextAgentManifest, Line: "device-agent/manifest.json"}
	envelope := "{\n  \"payload\": \"aXJvbmZsb2NrIGdvbGRlbiB2ZWN0b3IK\",\n  \"signature\": \"vEM0S0eilMgEVEI+VezYPr1LtR9V3E/g8KC0EGfpRnh/kHtEI61mwsfXq9aOPlFsYr4LM8sbnW4h8ADvO59fDg==\"\n}"
	payload, _, err := OpenEnvelope(keys, manifest, []byte(envelope))
	if err != nil || string(payload) != string(data) {
		t.Fatalf("envelope: %q, %v", payload, err)
	}
	sig, err = SignBytes(priv, manifest, data)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := SealEnvelope(data, sig)
	if err != nil || string(sealed) != envelope {
		t.Fatalf("sealed %q, %v", sealed, err)
	}
}
