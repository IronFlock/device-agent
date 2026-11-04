package system

import (
	"os"
	"testing"
)

// TestMain stands in for the Authenticode check of downloads in the whole
// package: the binaries the tests download carry no Authenticode signature,
// and on Windows the real check (codesign.Verify) refuses every one of them
// now that enforcement is on. The real check is tested in package codesign
// and, against the published binaries, in CI (windows-unit, `just
// codesign-check-published`); TestDownloadAuthenticode covers what
// downloadBinary does with its answer. Whether enforcement is on stays real
// (authenticodeEnforcing), except where a test sets it.
func TestMain(m *testing.M) {
	verifyAuthenticode = func(string) error { return nil }
	os.Exit(m.Run())
}
