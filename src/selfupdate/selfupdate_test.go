package selfupdate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestManager returns a Manager with the Windows names (two-rename swap)
// over a temp dir with an existing active binary (content "old-binary") and
// injected exec/currentExe so no real process is ever spawned.
func newTestManager(t *testing.T) *Manager {
	t.Helper()
	return newTestManagerWith(t, WindowsNames)
}

func newTestManagerWith(t *testing.T, names Names) *Manager {
	t.Helper()
	dir := t.TempDir()

	m := New(dir)
	m.names = names
	m.retryDelay = time.Millisecond
	m.currentExe = func() (string, error) { return m.activePath(), nil }
	m.execVersion = func(exePath string) (string, error) {
		// The fake downloaded exe contains its version as file content.
		data, err := os.ReadFile(exePath)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(data)), nil
	}

	require.NoError(t, os.WriteFile(m.activePath(), []byte("old-binary"), 0755))
	return m
}

func writeDownload(t *testing.T, m *Manager, version string) {
	t.Helper()
	require.NoError(t, os.WriteFile(m.versionedPath(version), []byte(version), 0755))
}

func readMarkerFile(t *testing.T, m *Manager) *state {
	t.Helper()
	data, err := os.ReadFile(m.markerPath())
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var s state
	require.NoError(t, json.Unmarshal(data, &s))
	return &s
}

func fileContent(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

func TestActivateHappyPath(t *testing.T) {
	m := newTestManager(t)
	writeDownload(t, m, "1.1.0")

	require.NoError(t, m.Activate("1.1.0", "1.0.0"))

	assert.Equal(t, "1.1.0", fileContent(t, m.activePath()), "new binary must be active")
	assert.Equal(t, "old-binary", fileContent(t, m.prevPath()), "old binary must be kept as rollback target")
	assert.NoFileExists(t, m.versionedPath("1.1.0"))

	marker := readMarkerFile(t, m)
	require.NotNil(t, marker)
	assert.Equal(t, phaseProbation, marker.Phase)
	assert.Equal(t, "1.1.0", marker.NewVersion)
	assert.Equal(t, "1.0.0", marker.OldVersion)
	assert.Equal(t, 0, marker.Attempts)
}

func TestActivateReplacesStalePrev(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, os.WriteFile(m.prevPath(), []byte("ancient"), 0755))
	writeDownload(t, m, "1.1.0")

	require.NoError(t, m.Activate("1.1.0", "1.0.0"))
	assert.Equal(t, "old-binary", fileContent(t, m.prevPath()))
}

func TestActivateRefusesBlockedVersion(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, m.writeMarker(&state{Phase: phaseRolledBack, NewVersion: "1.1.0", OldVersion: "1.0.0"}))
	writeDownload(t, m, "1.1.0")

	err := m.Activate("1.1.0", "1.0.0")
	require.ErrorIs(t, err, ErrVersionBlocked)
	assert.Equal(t, "old-binary", fileContent(t, m.activePath()))
	assert.True(t, m.IsVersionBlocked("1.1.0"))
}

func TestActivateNewerVersionClearsBlacklist(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, m.writeMarker(&state{Phase: phaseRolledBack, NewVersion: "1.1.0", OldVersion: "1.0.0"}))
	writeDownload(t, m, "1.2.0")

	require.NoError(t, m.Activate("1.2.0", "1.0.0"))
	assert.Equal(t, "1.2.0", fileContent(t, m.activePath()))
	assert.False(t, m.IsVersionBlocked("1.1.0"), "superseded blacklist entry must be gone")
}

func TestActivateRefusesWrongProcessPath(t *testing.T) {
	m := newTestManager(t)
	m.currentExe = func() (string, error) { return filepath.Join(m.agentDir, "somewhere-else.exe"), nil }
	writeDownload(t, m, "1.1.0")

	err := m.Activate("1.1.0", "1.0.0")
	require.ErrorContains(t, err, "refusing to self-update")
	assert.Equal(t, "old-binary", fileContent(t, m.activePath()))
}

// A layout migration that could not move /opt/reagent links
// /opt/ironflock/device-agent to it: the service starts the agent through the
// link, and the kernel reports the running binary by its resolved path.
func TestActivateThroughSymlinkedAgentDir(t *testing.T) {
	m := newTestManagerWith(t, LinuxNames)
	realDir, err := filepath.EvalSymlinks(m.agentDir)
	require.NoError(t, err)
	link := filepath.Join(t.TempDir(), "device-agent")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	m.agentDir = link
	m.currentExe = func() (string, error) { return filepath.Join(realDir, LinuxNames.Active), nil }
	writeDownload(t, m, "1.1.0")

	require.NoError(t, m.Activate("1.1.0", "1.0.0"))
	assert.Equal(t, "1.1.0", fileContent(t, filepath.Join(realDir, LinuxNames.Active)))
}

func TestActivateMissingOrEmptyDownload(t *testing.T) {
	m := newTestManager(t)

	err := m.Activate("1.1.0", "1.0.0")
	require.ErrorContains(t, err, "not found")

	require.NoError(t, os.WriteFile(m.versionedPath("1.1.0"), []byte{}, 0755))
	err = m.Activate("1.1.0", "1.0.0")
	require.ErrorContains(t, err, "is empty")

	assert.Equal(t, "old-binary", fileContent(t, m.activePath()))
	assert.Nil(t, readMarkerFile(t, m))
}

func TestActivateVersionMismatch(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, os.WriteFile(m.versionedPath("1.1.0"), []byte("9.9.9"), 0755))

	err := m.Activate("1.1.0", "1.0.0")
	require.ErrorContains(t, err, "reports version")
	assert.Equal(t, "old-binary", fileContent(t, m.activePath()))
}

func TestActivateFirstRenameFails(t *testing.T) {
	m := newTestManager(t)
	writeDownload(t, m, "1.1.0")

	realRename := m.rename
	m.rename = func(oldPath, newPath string) error {
		if oldPath == m.activePath() && newPath == m.prevPath() {
			return errors.New("sharing violation")
		}
		return realRename(oldPath, newPath)
	}

	err := m.Activate("1.1.0", "1.0.0")
	require.ErrorContains(t, err, "move the running binary aside")
	assert.Equal(t, "old-binary", fileContent(t, m.activePath()), "active binary untouched")
	assert.Nil(t, readMarkerFile(t, m), "marker cleared on abort")
}

func TestActivateSecondRenameFailsCompensates(t *testing.T) {
	m := newTestManager(t)
	writeDownload(t, m, "1.1.0")

	realRename := m.rename
	m.rename = func(oldPath, newPath string) error {
		if oldPath == m.versionedPath("1.1.0") {
			return errors.New("sharing violation")
		}
		return realRename(oldPath, newPath)
	}

	err := m.Activate("1.1.0", "1.0.0")
	require.ErrorContains(t, err, "move the update into place")
	assert.Equal(t, "old-binary", fileContent(t, m.activePath()), "compensation restored the old binary")
	assert.Nil(t, readMarkerFile(t, m))
}

func TestActivateSecondRenameAndCompensationFail(t *testing.T) {
	m := newTestManager(t)
	writeDownload(t, m, "1.1.0")

	realRename := m.rename
	m.rename = func(oldPath, newPath string) error {
		// Fail moving the update in AND moving prev back: worst case.
		if newPath == m.activePath() {
			return errors.New("sharing violation")
		}
		return realRename(oldPath, newPath)
	}

	err := m.Activate("1.1.0", "1.0.0")
	require.ErrorContains(t, err, "rollback failed")

	marker := readMarkerFile(t, m)
	require.NotNil(t, marker, "swapping marker must survive for boot-time recovery")
	assert.Equal(t, phaseSwapping, marker.Phase)
	assert.NoFileExists(t, m.activePath(), "ImagePath vacant — exactly what the repair task handles")
}

func TestOnServiceStartNoMarker(t *testing.T) {
	m := newTestManager(t)
	result := m.OnServiceStart("1.0.0")
	assert.False(t, result.RollbackPerformed)
}

func TestOnServiceStartInterruptedSwapOfTheNewVersionStartsProbation(t *testing.T) {
	// The process died after the swap but before the probation marker: the
	// new version must still be on probation, or a broken one would stay.
	m := newTestManager(t)
	require.NoError(t, m.writeMarker(&state{Phase: phaseSwapping, NewVersion: "1.1.0", OldVersion: "1.0.0"}))

	result := m.OnServiceStart("1.1.0")
	assert.False(t, result.RollbackPerformed)
	marker := readMarkerFile(t, m)
	require.NotNil(t, marker)
	assert.Equal(t, phaseProbation, marker.Phase)
	assert.Equal(t, 1, marker.Attempts)
}

func TestOnServiceStartInterruptedSwapOfTheOldVersionClears(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, m.writeMarker(&state{Phase: phaseSwapping, NewVersion: "1.1.0", OldVersion: "1.0.0"}))

	result := m.OnServiceStart("1.0.0")
	assert.False(t, result.RollbackPerformed)
	assert.Nil(t, readMarkerFile(t, m))
}

func TestActivateRefusedWhilePreviousUpdateIsOnProbation(t *testing.T) {
	for name, names := range map[string]Names{"windows": WindowsNames, "linux": LinuxNames} {
		t.Run(name, func(t *testing.T) {
			m := newTestManagerWith(t, names)
			require.NoError(t, m.writeMarker(&state{Phase: phaseProbation, NewVersion: "1.1.0", OldVersion: "1.0.0", Attempts: 1}))
			writeDownload(t, m, "1.2.0")

			err := m.Activate("1.2.0", "1.1.0")
			require.ErrorContains(t, err, "still in probation")
			assert.Equal(t, "old-binary", fileContent(t, m.activePath()))
		})
	}
}

func TestNewPicksThisOSNames(t *testing.T) {
	m := New(t.TempDir())
	if runtime.GOOS == "windows" {
		assert.Equal(t, WindowsNames, m.names)
	} else {
		assert.Equal(t, LinuxNames, m.names)
	}
}

func TestLinuxActivateNeverLeavesTheActivePathVacant(t *testing.T) {
	m := newTestManagerWith(t, LinuxNames)
	writeDownload(t, m, "1.1.0")

	realRename, realLink := m.rename, m.link
	m.rename = func(oldPath, newPath string) error {
		if newPath == m.activePath() {
			assert.FileExists(t, m.activePath(), "the update replaces the active binary in one step")
		}
		return realRename(oldPath, newPath)
	}
	m.link = func(oldPath, newPath string) error {
		assert.FileExists(t, m.activePath())
		return realLink(oldPath, newPath)
	}

	require.NoError(t, m.Activate("1.1.0", "1.0.0"))
	assert.Equal(t, "1.1.0", fileContent(t, m.activePath()))
	assert.Equal(t, "old-binary", fileContent(t, m.prevPath()), "the running binary is the rollback target")
	assert.NoFileExists(t, m.versionedPath("1.1.0"))
	marker := readMarkerFile(t, m)
	require.NotNil(t, marker)
	assert.Equal(t, phaseProbation, marker.Phase)
}

func TestLinuxActivateFailuresLeaveTheActiveBinary(t *testing.T) {
	cases := map[string]func(m *Manager){
		"link fails": func(m *Manager) {
			m.link = func(string, string) error { return errors.New("read-only file system") }
		},
		"rename fails": func(m *Manager) {
			real := m.rename
			m.rename = func(oldPath, newPath string) error {
				if newPath == m.activePath() {
					return errors.New("read-only file system")
				}
				return real(oldPath, newPath)
			}
		},
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			m := newTestManagerWith(t, LinuxNames)
			writeDownload(t, m, "1.1.0")
			breakIt(m)

			require.Error(t, m.Activate("1.1.0", "1.0.0"))
			assert.Equal(t, "old-binary", fileContent(t, m.activePath()))
			assert.Nil(t, readMarkerFile(t, m), "no marker left behind")
		})
	}
}

func TestLinuxRollbackRestoresThePreviousBinary(t *testing.T) {
	m := newTestManagerWith(t, LinuxNames)
	require.NoError(t, os.WriteFile(m.prevPath(), []byte("old-binary"), 0755))
	require.NoError(t, os.WriteFile(m.activePath(), []byte("1.1.0"), 0755))
	require.NoError(t, m.writeMarker(&state{Phase: phaseProbation, NewVersion: "1.1.0", OldVersion: "1.0.0", Attempts: maxProbationAttempts}))

	result := m.OnServiceStart("1.1.0")
	assert.True(t, result.RollbackPerformed)
	assert.Equal(t, "old-binary", fileContent(t, m.activePath()))
	assert.Equal(t, "1.1.0", fileContent(t, m.failedPath("1.1.0")))
	assert.True(t, m.IsVersionBlocked("1.1.0"))
}

func TestOnServiceStartProbationCountsAttempts(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, m.writeMarker(&state{Phase: phaseProbation, NewVersion: "1.1.0", OldVersion: "1.0.0"}))

	for attempt := 1; attempt <= maxProbationAttempts; attempt++ {
		result := m.OnServiceStart("1.1.0")
		assert.False(t, result.RollbackPerformed, "attempt %d must not roll back", attempt)
		marker := readMarkerFile(t, m)
		require.NotNil(t, marker)
		assert.Equal(t, attempt, marker.Attempts)
	}
}

func TestOnServiceStartProbationRollsBackAfterMaxAttempts(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, os.WriteFile(m.prevPath(), []byte("old-binary"), 0755))
	// active is the (crash-looping) new version
	require.NoError(t, os.WriteFile(m.activePath(), []byte("1.1.0"), 0755))
	require.NoError(t, m.writeMarker(&state{Phase: phaseProbation, NewVersion: "1.1.0", OldVersion: "1.0.0", Attempts: maxProbationAttempts}))

	result := m.OnServiceStart("1.1.0")
	assert.True(t, result.RollbackPerformed)

	assert.Equal(t, "old-binary", fileContent(t, m.activePath()), "previous binary restored")
	assert.Equal(t, "1.1.0", fileContent(t, m.failedPath("1.1.0")), "failed binary quarantined")

	// Suspect until the old version connects (see TestJudge*): held back
	// meanwhile, like a blocked one.
	marker := readMarkerFile(t, m)
	require.NotNil(t, marker)
	assert.Equal(t, phaseSuspect, marker.Phase)
	assert.True(t, m.IsVersionBlocked("1.1.0"))
	assert.False(t, m.IsVersionBlocked("1.2.0"))
}

func TestOnServiceStartProbationRunningOldVersionBlocksNew(t *testing.T) {
	// The repair task already restored the previous binary at boot.
	m := newTestManager(t)
	require.NoError(t, m.writeMarker(&state{Phase: phaseProbation, NewVersion: "1.1.0", OldVersion: "1.0.0", Attempts: 1}))

	result := m.OnServiceStart("1.0.0")
	assert.False(t, result.RollbackPerformed)
	assert.True(t, m.IsVersionBlocked("1.1.0"))
}

func TestOnServiceStartProbationUnknownVersionClearsMarker(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, m.writeMarker(&state{Phase: phaseProbation, NewVersion: "1.1.0", OldVersion: "1.0.0"}))

	result := m.OnServiceStart("2.0.0")
	assert.False(t, result.RollbackPerformed)
	assert.Nil(t, readMarkerFile(t, m))
}

func TestOnServiceStartRollbackQuarantineFailureKeepsProbation(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, os.WriteFile(m.prevPath(), []byte("old-binary"), 0755))
	require.NoError(t, m.writeMarker(&state{Phase: phaseProbation, NewVersion: "1.1.0", OldVersion: "1.0.0", Attempts: maxProbationAttempts}))

	realRename := m.rename
	m.rename = func(oldPath, newPath string) error {
		if newPath == m.failedPath("1.1.0") {
			return errors.New("access denied")
		}
		return realRename(oldPath, newPath)
	}

	result := m.OnServiceStart("1.1.0")
	assert.False(t, result.RollbackPerformed, "must keep running the new binary when quarantine fails")

	marker := readMarkerFile(t, m)
	require.NotNil(t, marker)
	assert.Equal(t, phaseProbation, marker.Phase, "probation persists so the next start retries the rollback")
}

func TestMarkHealthy(t *testing.T) {
	m := newTestManager(t)

	// no marker: no-op
	m.MarkHealthy()

	require.NoError(t, m.writeMarker(&state{Phase: phaseProbation, NewVersion: "1.1.0", OldVersion: "1.0.0", Attempts: 1}))
	m.MarkHealthy()
	assert.Nil(t, readMarkerFile(t, m), "probation marker cleared")

	require.NoError(t, m.writeMarker(&state{Phase: phaseRolledBack, NewVersion: "1.1.0", OldVersion: "1.0.0"}))
	m.MarkHealthy()
	require.NotNil(t, readMarkerFile(t, m), "blacklist must survive MarkHealthy")
}

func TestCorruptMarkerIsDiscarded(t *testing.T) {
	m := newTestManager(t)
	require.NoError(t, os.WriteFile(m.markerPath(), []byte("{not-json"), 0644))

	assert.False(t, m.IsVersionBlocked("1.1.0"))
	result := m.OnServiceStart("1.0.0")
	assert.False(t, result.RollbackPerformed)
}

func TestActivateWarnsButProceedsWhenSignatureFailsPreCutover(t *testing.T) {
	m := newTestManager(t)
	writeDownload(t, m, "1.1.0")
	m.enforceSignature = false
	m.verifySignature = func(string) error { return errors.New("not signed by pinned key") }

	require.NoError(t, m.Activate("1.1.0", "1.0.0"), "pre-cutover must proceed despite a bad signature")
	assert.Equal(t, "1.1.0", fileContent(t, m.activePath()))
}

func TestActivateRejectsBadSignatureWhenEnforced(t *testing.T) {
	m := newTestManager(t)
	writeDownload(t, m, "1.1.0")
	m.enforceSignature = true
	m.verifySignature = func(string) error { return errors.New("not signed by pinned key") }

	err := m.Activate("1.1.0", "1.0.0")
	require.ErrorContains(t, err, "improperly signed")
	assert.Equal(t, "old-binary", fileContent(t, m.activePath()), "enforced bad signature must not swap")
	assert.Nil(t, readMarkerFile(t, m), "no marker left behind on rejected update")
}

func TestActivateAcceptsGoodSignatureWhenEnforced(t *testing.T) {
	m := newTestManager(t)
	writeDownload(t, m, "1.1.0")
	m.enforceSignature = true
	verified := ""
	m.verifySignature = func(p string) error { verified = p; return nil }

	require.NoError(t, m.Activate("1.1.0", "1.0.0"))
	assert.Equal(t, m.versionedPath("1.1.0"), verified, "the downloaded binary is the one verified")
	assert.Equal(t, "1.1.0", fileContent(t, m.activePath()))
}
