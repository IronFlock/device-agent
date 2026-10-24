// Package selfupdate activates downloaded agent updates in-process, on
// Windows (service) and on Linux (systemd unit with IRONFLOCK_AGENT_SUPERVISOR
// set). The active binary path must never be left vacant, so the swap is
// atomic where the OS allows it (Linux: link the running binary as the
// rollback target, then rename the update over it) and two renames with a
// compensating rollback where it does not (Windows), with a persisted marker
// for crash recovery and a probation phase: a new version must start, become
// ready and connect to IronFlock within its deadline (Watch), or it is rolled
// back. The version it replaced then judges it: if the old one connects, the
// new one is blocked until a newer release appears; if the old one cannot
// connect either, the network was at fault and the new one may be tried
// again. Either way the device ends up on a version that connects whenever
// any can, which is what keeps it repairable from IronFlock.
//
// Files in agentDir (Windows names / Linux names):
//
//	device-agent.exe / device-agent                  the active binary (service ImagePath / ExecStart)
//	device-agent-v<v>.exe / device-agent-v<v>        downloaded by system.downloadBinary
//	device-agent-prev.exe / device-agent-prev        the previously active binary (rollback target)
//	device-agent-failed-v<v>.exe / device-agent-failed-v<v>  a version that failed probation
//	update-state.json                                the marker persisting swap/probation/rollback state
//	last-connected                                   when the device was last connected to IronFlock
//
// A Windows install from before 1.4.0 runs reagent.exe (reagent-prev.exe,
// reagent-v<v>.exe, …) until the Windows layout migration renames it; the
// names follow the layout (NamesFor), never the OS alone, so such a service
// keeps updating and rolling back under the names its ImagePath points at.
//
// All operations are same-directory renames and links, so the state machine
// is unit-testable on any OS.
package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reagent/codesign"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// Names are the agent's file names in agentDir.
type Names struct {
	Active string // the binary the supervisor starts
	Prev   string // the rollback target
	Prefix string // downloads are <Prefix>-v<v><Ext>, failed versions <Prefix>-failed-v<v><Ext>
	Ext    string
	// LinkSwap: the OS lets a running binary be linked and replaced (Linux).
	// The swap then links active as prev and renames the update over active,
	// so the active path is never vacant; Windows can only rename a running
	// exe away, which leaves it vacant for an instant.
	LinkSwap bool
}

var (
	WindowsNames = Names{Active: "device-agent.exe", Prev: "device-agent-prev.exe", Prefix: "device-agent", Ext: ".exe"}
	// WindowsLegacyNames are the names of a Windows install from before
	// 1.4.0 (service ImagePath …\Reagent\reagent.exe), kept until the layout
	// migration renames the files.
	WindowsLegacyNames = Names{Active: "reagent.exe", Prev: "reagent-prev.exe", Prefix: "reagent", Ext: ".exe"}
	LinuxNames         = Names{Active: "device-agent", Prev: "device-agent-prev", Prefix: "device-agent", LinkSwap: true}
)

// legacyBinaryName is layout.Paths.BinaryName of the legacy layouts.
const legacyBinaryName = "reagent"

// NamesFor returns this OS's names for the layout the agent runs in, given
// the layout's binary name (layout.Paths.BinaryName, config.CommandLineArguments.BinaryName):
// on Windows the legacy names for "reagent", the current ones for anything
// else; Linux has one set.
func NamesFor(binaryName string) Names {
	return namesFor(runtime.GOOS, binaryName)
}

func namesFor(goos string, binaryName string) Names {
	if goos != "windows" {
		return LinuxNames
	}
	if binaryName == legacyBinaryName {
		return WindowsLegacyNames
	}
	return WindowsNames
}

const (
	markerName        = "update-state.json"
	lastConnectedName = "last-connected"

	// maxProbationAttempts mirrors the Linux manager's rollback window: a new
	// binary that fails to stay up this many consecutive starts is rolled
	// back.
	maxProbationAttempts = 3

	renameRetries    = 5
	renameRetryDelay = 300 * time.Millisecond

	// versionProbeTimeout bounds `<update> -version`: a binary that hangs
	// there must not hang the activation.
	versionProbeTimeout = 30 * time.Second
)

// ErrVersionBlocked marks a version that already failed probation on this
// device; it must not be activated again until a newer version supersedes it.
var ErrVersionBlocked = errors.New("this version previously failed on this device and is blocked")

type phase string

const (
	phaseSwapping   phase = "swapping"
	phaseProbation  phase = "probation"
	phaseRolledBack phase = "rolledback" // NewVersion is blocked
	// phaseSuspect: NewVersion was rolled back; the old version, running
	// again, decides whether it is blocked (the old one connects) or may be
	// tried again (the old one cannot connect either).
	phaseSuspect phase = "suspect"
	// phaseStruck: NewVersion failed once while the old version proved it
	// can connect. It is tried again at the next update (the agent's next
	// start, or one a user starts); a second such
	// failure blocks it (strikesToBlock). One failure can be bad luck — an
	// outage that ended just in time for the old version.
	phaseStruck phase = "struck"
)

// strikesToBlock: a version is blocked after failing this often while the
// version it replaced connected each time.
const strikesToBlock = 2

// activateMu serialises the swaps in one process: the periodic update check
// and a platform-requested update may activate at the same time, and an
// interleaving would delete the rollback target.
var activateMu sync.Mutex

// JudgeTimeout is how long the old version gets, after a rollback, to
// connect and so prove the new one broken.
const JudgeTimeout = 10 * time.Minute

type state struct {
	Phase      phase  `json:"phase"`
	NewVersion string `json:"new_version"`
	OldVersion string `json:"old_version"`
	Attempts   int    `json:"attempts"`
	// Since: when the rollback to OldVersion happened (phaseSuspect).
	Since string `json:"since,omitempty"`
	// Strikes: how often NewVersion failed while OldVersion then connected.
	Strikes   int    `json:"strikes,omitempty"`
	Reason    string `json:"reason,omitempty"`
	UpdatedAt string `json:"updated_at"`
}

// StartResult tells the service control loop what OnServiceStart decided.
type StartResult struct {
	// RollbackPerformed: the active binary was swapped back to the previous
	// version; the caller must exit so the supervisor restarts into it.
	RollbackPerformed bool
}

// Manager runs the swap/probation state machine for one agentDir. The file
// operations are injectable for tests; zero-value fields fall back to the os
// implementations.
type Manager struct {
	agentDir   string
	names      Names
	retryDelay time.Duration

	rename      func(oldPath, newPath string) error
	link        func(oldPath, newPath string) error
	remove      func(path string) error
	stat        func(path string) (os.FileInfo, error)
	now         func() time.Time
	currentExe  func() (string, error)
	execVersion func(exePath string) (string, error)
	// verifySignature authenticates the downloaded binary before it becomes
	// the service exe. Injected for tests; defaults to codesign.Verify.
	verifySignature func(exePath string) error
	// enforceSignature rejects an update whose signature fails. False during
	// the pre-cutover transition (warn-and-proceed); flipped true afterwards.
	enforceSignature bool
}

// New returns the Manager for agentDir with the file names of the layout the
// agent runs in (NamesFor).
func New(agentDir string, names Names) *Manager {
	return &Manager{
		agentDir:   agentDir,
		names:      names,
		retryDelay: renameRetryDelay,
		rename:     os.Rename,
		link:       os.Link,
		remove:     os.Remove,
		stat:       os.Stat,
		now:        time.Now,
		currentExe: os.Executable,
		execVersion: func(exePath string) (string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), versionProbeTimeout)
			defer cancel()
			output, err := exec.CommandContext(ctx, exePath, "-version").Output()
			if err != nil {
				return "", err
			}
			return strings.TrimSpace(string(output)), nil
		},
		verifySignature: codesign.Verify,
		// Follows the shared cutover switch: warn-and-proceed pre-cutover (so
		// devices can update TO the first signed release), reject afterwards.
		enforceSignature: codesign.Enforcing(),
	}
}

func (m *Manager) activePath() string { return filepath.Join(m.agentDir, m.names.Active) }
func (m *Manager) prevPath() string   { return filepath.Join(m.agentDir, m.names.Prev) }
func (m *Manager) markerPath() string { return filepath.Join(m.agentDir, markerName) }
func (m *Manager) versionedPath(version string) string {
	return filepath.Join(m.agentDir, fmt.Sprintf("%s-v%s%s", m.names.Prefix, version, m.names.Ext))
}
func (m *Manager) failedPath(version string) string {
	return filepath.Join(m.agentDir, fmt.Sprintf("%s-failed-v%s%s", m.names.Prefix, version, m.names.Ext))
}

// ActivePath is the binary the supervisor starts.
func (m *Manager) ActivePath() string { return m.activePath() }

// syncDir makes the renames and links in agentDir durable: a power cut right
// after an activation must not bring back a directory without the new
// binary, or with neither.
func (m *Manager) syncDir() {
	dir, err := os.Open(m.agentDir)
	if err != nil {
		return
	}
	_ = dir.Sync()
	_ = dir.Close()
}

// retryRename tolerates the transient sharing violations Defender causes by
// holding scan handles on freshly written executables.
func (m *Manager) retryRename(oldPath, newPath string) error {
	var err error
	for attempt := 0; attempt < renameRetries; attempt++ {
		err = m.rename(oldPath, newPath)
		if err == nil {
			return nil
		}
		time.Sleep(m.retryDelay)
	}
	return err
}

func (m *Manager) readMarker() (*state, error) {
	data, err := os.ReadFile(m.markerPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var s state
	err = json.Unmarshal(data, &s)
	if err != nil {
		// An unreadable marker must never brick updates; treat it as absent.
		log.Error().Err(err).Msg("selfupdate: discarding corrupt update-state marker")
		return nil, nil
	}
	return &s, nil
}

// writeMarker persists atomically (temp file + rename) so a crash mid-write
// cannot leave a half marker.
func (m *Manager) writeMarker(s *state) error {
	s.UpdatedAt = m.now().UTC().Format(time.RFC3339)
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}

	tmpPath := m.markerPath() + ".tmp"
	file, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644) //nolint:gosec // G302: staged agent binary, made executable before use; contents are verified by signature
	if err != nil {
		return err
	}
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	err = m.rename(tmpPath, m.markerPath())
	if err == nil {
		m.syncDir()
	}
	return err
}

func (m *Manager) clearMarker() {
	err := m.remove(m.markerPath())
	if err != nil && !os.IsNotExist(err) {
		log.Error().Err(err).Msg("selfupdate: failed to remove update-state marker")
	}
}

// IsVersionBlocked reports whether version failed probation earlier and must
// not be re-activated. Without this, the update check would re-download and
// re-activate the same broken version right after every rollback (the semver
// guard only checks "newer than running").
func (m *Manager) IsVersionBlocked(version string) bool {
	s, err := m.readMarker()
	if err != nil || s == nil {
		return false
	}
	return (s.Phase == phaseRolledBack || s.Phase == phaseSuspect) && s.NewVersion == version
}

// samePath reports whether two paths lead to the same location once symlinks
// are resolved, ignoring case (Windows). A path that cannot be resolved is
// compared as it is.
func samePath(a, b string) bool {
	return strings.EqualFold(resolvedPath(a), resolvedPath(b))
}

func resolvedPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

// Activate swaps the downloaded <Prefix>-v<newVersion><Ext> into the active
// path. It runs inside the OLD process; on success the caller must trigger a
// process restart so the supervisor launches the new file.
func (m *Manager) Activate(newVersion string, currentVersion string) error {
	activateMu.Lock()
	defer activateMu.Unlock()
	marker, err := m.readMarker()
	if err != nil {
		return err
	}
	strikes := 0
	if marker != nil && marker.Phase == phaseSwapping {
		return fmt.Errorf("an activation of v%s is under way", marker.NewVersion)
	}
	if marker != nil && marker.Phase == phaseStruck && marker.NewVersion == newVersion {
		strikes = marker.Strikes // its second chance
	}
	if marker != nil && (marker.Phase == phaseRolledBack || marker.Phase == phaseSuspect) {
		if marker.NewVersion == newVersion {
			return ErrVersionBlocked
		}
		// a newer version supersedes the blocked (or suspect) one
		m.clearMarker()
	}
	if marker != nil && marker.Phase == phaseProbation {
		// The previous update has not proven itself yet: activating now would
		// make the unproven version the rollback target.
		return fmt.Errorf("v%s is still in probation; not activating v%s yet", marker.NewVersion, newVersion)
	}

	// Never swap when the process does not run from the expected service
	// location — someone started the exe from a download folder. The kernel
	// names the running binary by its resolved path, while the active path
	// may lead through a symlink (a layout migration that could not move
	// /opt/reagent links /opt/ironflock/device-agent to it), so both are
	// compared resolved.
	runningExe, err := m.currentExe()
	if err != nil {
		return err
	}
	if !samePath(runningExe, m.activePath()) {
		return fmt.Errorf("running from %s instead of %s; refusing to self-update", runningExe, m.activePath())
	}

	newExe := m.versionedPath(newVersion)
	info, err := m.stat(newExe)
	if err != nil {
		return fmt.Errorf("downloaded update not found: %w", err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("downloaded update %s is empty", newExe)
	}

	// Prove the download actually executes before making it the service
	// binary (also forces a Defender scan now rather than mid-swap).
	reportedVersion, err := m.execVersion(newExe)
	if err != nil {
		return fmt.Errorf("downloaded update failed to execute: %w", err)
	}
	if reportedVersion != newVersion {
		return fmt.Errorf("downloaded update reports version %q, expected %q", reportedVersion, newVersion)
	}

	// Authenticate the binary that is about to become the service exe.
	// Pre-cutover this only warns; post-cutover a bad/absent signature aborts
	// the swap so a compromised distribution server can't push a replacement.
	if m.verifySignature != nil {
		if sigErr := m.verifySignature(newExe); sigErr != nil {
			if m.enforceSignature {
				return fmt.Errorf("refusing to activate an improperly signed update: %w", sigErr)
			}
			log.Warn().Err(sigErr).Msgf("update v%s failed signature verification (proceeding: pre-cutover)", newVersion)
		}
	}

	// Drop the stale rollback target; the current binary becomes the new one.
	err = m.remove(m.prevPath())
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove stale previous binary: %w", err)
	}

	err = m.writeMarker(&state{Phase: phaseSwapping, NewVersion: newVersion, OldVersion: currentVersion, Strikes: strikes})
	if err != nil {
		return err
	}

	if m.names.LinkSwap {
		err = m.linkSwap(newExe)
	} else {
		err = m.renameSwap(newExe)
	}
	if err != nil {
		return err
	}
	m.syncDir()

	err = m.writeMarker(&state{Phase: phaseProbation, NewVersion: newVersion, OldVersion: currentVersion, Strikes: strikes})
	if err != nil {
		log.Error().Err(err).Msg("selfupdate: swap succeeded but writing the probation marker failed")
	}

	log.Info().Msgf("selfupdate: activated v%s (previous v%s kept for rollback)", newVersion, currentVersion)
	return nil
}

// linkSwap (Linux): link the running binary as the rollback target, then
// rename the update over the active path. The active path is occupied at
// every instant, and the running process keeps its own inode.
func (m *Manager) linkSwap(newExe string) error {
	err := m.link(m.activePath(), m.prevPath())
	if err != nil {
		m.clearMarker()
		return fmt.Errorf("failed to keep the running binary as the rollback target: %w", err)
	}
	err = m.rename(newExe, m.activePath())
	if err != nil {
		// active is untouched; prev is just a second name for it.
		m.clearMarker()
		return fmt.Errorf("failed to move the update into place: %w", err)
	}
	return nil
}

// renameSwap (Windows): renaming a running executable is legal on NTFS
// (deleting/overwriting is not); the two renames leave the ImagePath vacant
// only for an instant, and the boot-time repair task covers a crash inside
// that window.
func (m *Manager) renameSwap(newExe string) error {
	err := m.retryRename(m.activePath(), m.prevPath())
	if err != nil {
		m.clearMarker()
		return fmt.Errorf("failed to move the running binary aside: %w", err)
	}

	err = m.retryRename(newExe, m.activePath())
	if err != nil {
		compErr := m.retryRename(m.prevPath(), m.activePath())
		if compErr != nil {
			// ImagePath is vacant; keep the swapping marker so the repair
			// task / next start reconciles. The running process is unaffected.
			log.Error().Err(compErr).Msg("selfupdate: failed to restore the previous binary after a failed swap — repair task will recover at next boot")
			return fmt.Errorf("swap failed and rollback failed: %v (swap error: %w)", compErr, err)
		}
		m.clearMarker()
		return fmt.Errorf("failed to move the update into place: %w", err)
	}
	return nil
}

// OnServiceStart reconciles the marker state. Call it at service start with
// the running binary's version, BEFORE starting the agent.
func (m *Manager) OnServiceStart(runningVersion string) StartResult {
	marker, err := m.readMarker()
	if err != nil || marker == nil {
		return StartResult{}
	}

	switch marker.Phase {
	case phaseSwapping:
		// The previous process died mid-swap. Since we ARE running, the active
		// path is occupied again. If it is the new version, the swap went
		// through and only the probation marker is missing: probation starts
		// now, so a broken update can still be rolled back.
		if runningVersion == marker.NewVersion {
			log.Warn().Msgf("selfupdate: v%s was swapped in by an interrupted activation; starting its probation", marker.NewVersion)
			marker.Phase = phaseProbation
			marker.Attempts = 1
			if err := m.writeMarker(marker); err != nil {
				log.Error().Err(err).Msg("selfupdate: failed to persist probation attempt")
			}
			return StartResult{}
		}
		log.Warn().Msg("selfupdate: recovered from an interrupted swap")
		m.clearMarker()
		return StartResult{}

	case phaseProbation:
		if runningVersion == marker.NewVersion {
			marker.Attempts++
			if marker.Attempts <= maxProbationAttempts {
				err = m.writeMarker(marker)
				if err != nil {
					log.Error().Err(err).Msg("selfupdate: failed to persist probation attempt")
				}
				return StartResult{}
			}

			// The new version keeps dying before MarkHealthy: roll back.
			log.Error().Msgf("selfupdate: v%s failed %d consecutive starts, rolling back to v%s", marker.NewVersion, marker.Attempts-1, marker.OldVersion)
			return m.rollbackFrom(marker, fmt.Sprintf("failed %d consecutive starts", marker.Attempts-1))
		}

		if runningVersion == marker.OldVersion {
			// The repair task (or a failed swap compensation) already put the
			// previous version back; it judges the new one.
			log.Warn().Msgf("selfupdate: running the previous v%s again; v%s is suspect", marker.OldVersion, marker.NewVersion)
			m.markSuspect(marker, "the previous version was put back")
			return StartResult{}
		}

		// Neither the new nor the old version: manual intervention happened.
		m.clearMarker()
		return StartResult{}

	case phaseRolledBack:
		// Keep the blacklist; it is cleared when a newer version activates.
		return StartResult{}

	case phaseSuspect:
		if runningVersion == marker.OldVersion {
			// The old version keeps dying before it proves anything: it is
			// no better than the new one. Alternate, until one connects.
			marker.Attempts++
			if marker.Attempts > maxProbationAttempts {
				log.Error().Msgf("selfupdate: v%s, rolled back to, failed %d starts as well; trying v%s again", marker.OldVersion, marker.Attempts-1, marker.NewVersion)
				if result := m.reactivateFailed(marker); result.RollbackPerformed {
					return result
				}
			}
			if err := m.writeMarker(marker); err != nil {
				log.Error().Err(err).Msg("selfupdate: failed to persist the start of the old version")
			}
			// Watch decides while it runs; the deadline also passes across
			// restarts.
			m.Judge(runningVersion, false, 0)
			return StartResult{}
		}
		m.clearMarker()
		return StartResult{}

	case phaseStruck:
		// Tried again at the next update (Activate); nothing to do.
		return StartResult{}
	}

	m.clearMarker()
	return StartResult{}
}

// MarkHealthy ends probation: the new binary stayed up long enough. The
// previous binary is kept as the rollback candidate for the NEXT update.
func (m *Manager) MarkHealthy() {
	marker, err := m.readMarker()
	if err != nil || marker == nil {
		return
	}
	if marker.Phase == phaseProbation {
		log.Info().Msgf("selfupdate: v%s passed probation", marker.NewVersion)
		m.clearMarker()
	}
}

// rollbackFrom puts marker.OldVersion back over the active path and leaves
// marker.NewVersion suspect. The caller must exit for a restart when
// RollbackPerformed is set.
func (m *Manager) rollbackFrom(marker *state, reason string) StartResult {
	activateMu.Lock()
	defer activateMu.Unlock()
	if m.names.LinkSwap {
		// Keep the failed binary under its quarantine name, then put the
		// previous one back over the active path in one rename.
		err := m.link(m.activePath(), m.failedPath(marker.NewVersion))
		if err == nil || os.IsExist(err) {
			err = m.rename(m.prevPath(), m.activePath())
		}
		if err != nil {
			log.Error().Err(err).Msg("selfupdate: rollback failed; keeping probation, the next start retries")
			if writeErr := m.writeMarker(marker); writeErr != nil {
				log.Error().Err(writeErr).Msg("selfupdate: failed to persist probation attempt")
			}
			return StartResult{}
		}
		m.syncDir()
		m.markSuspect(marker, reason)
		return StartResult{RollbackPerformed: true}
	}
	err := m.retryRename(m.activePath(), m.failedPath(marker.NewVersion))
	if err != nil {
		// Can't quarantine the running binary; keep probation and let the
		// next start retry the rollback.
		log.Error().Err(err).Msg("selfupdate: rollback failed to quarantine the active binary")
		if writeErr := m.writeMarker(marker); writeErr != nil {
			log.Error().Err(writeErr).Msg("selfupdate: failed to persist probation attempt")
		}
		return StartResult{}
	}
	err = m.retryRename(m.prevPath(), m.activePath())
	if err != nil {
		log.Error().Err(err).Msg("selfupdate: rollback failed to restore the previous binary — repair task will recover at next boot")
		return StartResult{RollbackPerformed: true}
	}
	m.markSuspect(marker, reason)
	return StartResult{RollbackPerformed: true}
}

func (m *Manager) markSuspect(marker *state, reason string) {
	err := m.writeMarker(&state{
		Phase:      phaseSuspect,
		NewVersion: marker.NewVersion,
		OldVersion: marker.OldVersion,
		Since:      m.now().UTC().Format(time.RFC3339),
		Reason:     reason,
		Strikes:    marker.Strikes,
	})
	if err != nil {
		log.Error().Err(err).Msg("selfupdate: failed to persist rollback marker")
	}
}

// RollBackUnconnected rolls back version, still in probation, because it did
// not become ready and connect within after.
func (m *Manager) RollBackUnconnected(version string, after time.Duration) StartResult {
	marker, err := m.readMarker()
	if err != nil || marker == nil || marker.Phase != phaseProbation || marker.NewVersion != version {
		return StartResult{}
	}
	log.Error().Msgf("selfupdate: v%s did not connect to IronFlock within %s, rolling back to v%s", version, after, marker.OldVersion)
	return m.rollbackFrom(marker, fmt.Sprintf("did not connect within %s", after))
}

// Judge settles a suspect version, called with the running version.
//
// proven: the old version met the bar a new one must meet (ready,
// connected, up HealthyUptime) — so the new one failed where the old one
// works: a strike; strikesToBlock of them block it until a newer release.
//
// Without that proof, once JudgeTimeout passed since the rollback (by the
// wall clock, or by unprovenFor: how long this process has run without it,
// which holds when the clock is wrong), the network is blamed and the new
// version may be tried again (its strikes kept).
func (m *Manager) Judge(version string, proven bool, unprovenFor time.Duration) {
	marker, err := m.readMarker()
	if err != nil || marker == nil || marker.Phase != phaseSuspect || marker.OldVersion != version {
		return
	}
	if proven {
		marker.Strikes++
		marker.Attempts = 0
		if marker.Strikes >= strikesToBlock {
			log.Warn().Msgf("selfupdate: v%s connects, and v%s failed %d times (%s): blocked until a newer release", version, marker.NewVersion, marker.Strikes, marker.Reason)
			marker.Phase = phaseRolledBack
		} else {
			log.Warn().Msgf("selfupdate: v%s connects, v%s did not (%s): it gets one more try at the next update", version, marker.NewVersion, marker.Reason)
			marker.Phase = phaseStruck
		}
		if err := m.writeMarker(marker); err != nil {
			log.Error().Err(err).Msg("selfupdate: failed to persist the judgement")
		}
		return
	}
	since, err := time.Parse(time.RFC3339, marker.Since)
	elapsed := m.now().Sub(since)
	if err != nil || elapsed >= JudgeTimeout || elapsed < 0 || unprovenFor >= JudgeTimeout {
		log.Warn().Msgf("selfupdate: v%s did not prove itself either since the rollback: the network, not v%s, was at fault; it may be tried again", version, marker.NewVersion)
		if marker.Strikes > 0 {
			marker.Phase = phaseStruck
			if err := m.writeMarker(marker); err != nil {
				log.Error().Err(err).Msg("selfupdate: failed to persist the judgement")
			}
			return
		}
		m.clearMarker()
	}
}

// reactivateFailed puts marker.NewVersion's quarantined binary back as the
// active one and starts its probation again: the old version did not run
// either. The caller must exit for a restart when RollbackPerformed is set.
func (m *Manager) reactivateFailed(marker *state) StartResult {
	activateMu.Lock()
	defer activateMu.Unlock()
	failed := m.failedPath(marker.NewVersion)
	if _, err := m.stat(failed); err != nil {
		return StartResult{}
	}
	if err := m.remove(m.prevPath()); err != nil && !os.IsNotExist(err) {
		return StartResult{}
	}
	if err := m.writeMarker(&state{Phase: phaseSwapping, NewVersion: marker.NewVersion, OldVersion: marker.OldVersion, Strikes: marker.Strikes}); err != nil {
		return StartResult{}
	}
	var err error
	if m.names.LinkSwap {
		err = m.linkSwap(failed)
	} else {
		err = m.renameSwap(failed)
	}
	if err != nil {
		log.Error().Err(err).Msgf("selfupdate: putting v%s back failed", marker.NewVersion)
		m.markSuspect(marker, marker.Reason)
		return StartResult{}
	}
	m.syncDir()
	if err := m.writeMarker(&state{Phase: phaseProbation, NewVersion: marker.NewVersion, OldVersion: marker.OldVersion, Strikes: marker.Strikes}); err != nil {
		log.Error().Err(err).Msg("selfupdate: failed to persist the probation marker")
	}
	return StartResult{RollbackPerformed: true}
}

// InProbation reports whether version is a new version still in probation.
func (m *Manager) InProbation(version string) bool {
	marker, err := m.readMarker()
	return err == nil && marker != nil && marker.Phase == phaseProbation && marker.NewVersion == version
}

// RecordConnected stores when the device was last connected to IronFlock.
func (m *Manager) RecordConnected(at time.Time) {
	path := filepath.Join(m.agentDir, lastConnectedName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(at.UTC().Format(time.RFC3339)+"\n"), 0o644); err != nil { //nolint:gosec // G306: update timestamp, no secrets
		return
	}
	_ = m.rename(tmp, path)
}

// LastConnected is when the device was last connected (false: never
// recorded).
func (m *Manager) LastConnected() (time.Time, bool) {
	data, err := os.ReadFile(filepath.Join(m.agentDir, lastConnectedName))
	if err != nil {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data)))
	return at, err == nil
}
