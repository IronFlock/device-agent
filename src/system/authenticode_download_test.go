package system

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"testing"

	"reagent/codesign"
	"reagent/updatesig"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errNotSignedByIronFlock = errors.New("not signed by IronFlock")

// What downloadBinary does with the Authenticode check's answer, for both
// kinds of download (agent update, frpc): with enforcement on, a download
// that fails it is refused and removed; one that passes is installed; with
// enforcement off, a failure only warns. Every download is checked.
func TestDownloadAuthenticode(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	previousVerify, previousEnforcing := verifyAuthenticode, authenticodeEnforcing
	t.Cleanup(func() { verifyAuthenticode, authenticodeEnforcing = previousVerify, previousEnforcing })

	for _, a := range signedArtifacts {
		t.Run(a.name, func(t *testing.T) {
			binary := []byte("genuine " + a.fileName + " " + a.version)

			tests := []struct {
				name          string
				enforcing     bool
				checkErr      error
				wantInstalled bool
			}{
				{"enforcing refuses a failed check", true, errNotSignedByIronFlock, false},
				{"enforcing installs a passed check", true, nil, true},
				{"not enforcing installs a failed check", false, errNotSignedByIronFlock, true},
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					t.Setenv(updatesig.AGENT_UPDATE_ALLOW_UNSIGNED, "")
					trustUpdateKey(t, pub)
					origin := newUpdateOrigin(t)
					origin.publish(a, binary, signBytes(t, priv, a.target(a.version), binary))
					sys, cfg := newUpdateSystem(t, origin)

					checked := ""
					verifyAuthenticode = func(path string) error { checked = path; return tt.checkErr }
					authenticodeEnforcing = func() bool { return tt.enforcing }

					err := a.download(sys)
					assert.NotEmpty(t, checked, "every download is checked")
					if !tt.wantInstalled {
						require.ErrorIs(t, err, errNotSignedByIronFlock)
						assert.ErrorContains(t, err, "refusing to install an improperly signed binary")
						assertNothingInstalled(t, a, cfg)
						return
					}
					require.NoError(t, err)
					installed, readErr := os.ReadFile(a.installed(cfg))
					require.NoError(t, readErr)
					assert.Equal(t, binary, installed)
				})
			}
		})
	}
}

// The shipped wiring: downloads follow the shared switch, which is on.
func TestDownloadsFollowTheSharedEnforcementSwitch(t *testing.T) {
	assert.Equal(t, codesign.Enforcing(), authenticodeEnforcing())
	assert.True(t, authenticodeEnforcing(), "a download that fails the Authenticode check is refused")
}
