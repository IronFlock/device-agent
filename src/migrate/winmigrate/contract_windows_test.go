//go:build windows

package winmigrate

// The contract between the fake System the migration's tests run against and
// Windows itself: throwaway services made from this test binary run under the
// real service control manager (elevated runs only: windows-latest in CI), the
// real repair scripts run over the fake's fixture folders through cmd.exe,
// and the file semantics the engine relies on run against NTFS.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reagent/winservice"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	contractModeEnv = "IRONFLOCK_CONTRACT_SERVICE"
	contractNameEnv = "IRONFLOCK_CONTRACT_NAME"
	contractDirEnv  = "IRONFLOCK_CONTRACT_DIR"
	// contractLinger: the throwaway service's process keeps running this
	// long after it reported stopped (what the SCM does not wait for).
	contractLinger = 5 * time.Second
)

// contractStubEnv marks a process the repair-script harness started: in it,
// this binary copied as sc.exe / reg.exe is that tool's stub.
const contractStubEnv = "IRONFLOCK_CONTRACT_STUB"

// TestMain lets the service control manager run this binary as a throwaway
// service: the service's Environment names the behaviour.
func TestMain(m *testing.M) {
	// Copied as sc.exe and reg.exe, this binary is the repair scripts' stub
	// tools (TestContractRepairScripts): an executable, as the real ones are.
	// A batch-file stub would not do: a script that runs another batch file
	// without CALL continues in that file and never comes back.
	// cmd.exe starts it as "sc" (no extension in argv[0]); only a process the
	// repair-script harness started (contractStubEnv) is a stub, so a
	// mismatch can never make the copy run the whole suite again.
	if os.Getenv(contractStubEnv) != "" {
		switch strings.TrimSuffix(strings.ToLower(filepath.Base(os.Args[0])), ".exe") {
		case "sc":
			os.Exit(stubSC(os.Args[1:]))
		case "reg":
			os.Exit(stubReg(os.Args[1:]))
		}
		fmt.Fprintf(os.Stderr, "contract stub: unknown tool %q\n", os.Args[0])
		os.Exit(3)
	}
	if mode := os.Getenv(contractModeEnv); mode != "" {
		os.Exit(contractServiceMain(mode))
	}
	os.Exit(m.Run())
}

// stubSC records "<verb> <service>" in %SC_LOG%.
func stubSC(args []string) int {
	appendLine(os.Getenv("SC_LOG"), strings.Join(args, " "))
	return 0
}

// stubReg answers `reg query HKLM\...\Services\<name> /v Start` from
// %REG_STATES% ("<name>=<0x2>" lines) the way reg.exe prints a value; an
// unknown service is an error, as for reg.exe.
func stubReg(args []string) int {
	if len(args) < 2 || !strings.EqualFold(args[0], "query") {
		return 1
	}
	key := strings.TrimRight(args[1], `\`)
	name := key[strings.LastIndex(key, `\`)+1:]
	data, err := os.ReadFile(os.Getenv("REG_STATES"))
	if err != nil {
		return 1
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r", ""), "\n") {
		if service, code, ok := strings.Cut(line, "="); ok && strings.EqualFold(service, name) {
			fmt.Printf("\r\nHKEY_LOCAL_MACHINE\\SYSTEM\\CurrentControlSet\\Services\\%s\r\n    Start    REG_DWORD    %s\r\n\r\n", name, code)
			return 0
		}
	}
	fmt.Fprintln(os.Stderr, "ERROR: The system was unable to find the specified registry key or value.")
	return 1
}

func contractServiceMain(mode string) int {
	s := &contractService{mode: mode, dir: os.Getenv(contractDirEnv)}
	if err := svc.Run(os.Getenv(contractNameEnv), s); err != nil {
		return 2
	}
	if s.linger {
		time.Sleep(contractLinger)
	}
	return 0
}

// contractService behaves as its mode says once it reported running:
// "crash" dies without reporting stopped, "exit0" stops with 0, "exit1"
// stops with a service-specific exit code, "hold" runs until stopped while
// holding <dir>\held\open.txt open, then lingers after reporting stopped;
// "stopexit1" runs until stopped and then stops with a service-specific
// exit code (what the migrate service does when stopped while no agent
// runs). Each start logs whether the SCM's process id of the service is
// this process already while it is start-pending (runningServiceName).
type contractService struct {
	mode, dir string
	linger    bool
}

func (s *contractService) Execute(args []string, requests <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	changes <- svc.Status{State: svc.StartPending}
	appendLine(filepath.Join(s.dir, "starts.log"), fmt.Sprintf("%d %s pidmatch=%v", os.Getpid(), s.mode, ownPIDMatches(os.Getenv(contractNameEnv))))
	var held *os.File
	if s.mode == "hold" {
		held, _ = os.OpenFile(filepath.Join(s.dir, "held", "open.txt"), os.O_RDWR, 0)
	}
	changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	switch s.mode {
	case "crash":
		time.Sleep(200 * time.Millisecond)
		os.Exit(3)
	case "exit0":
		changes <- svc.Status{State: svc.StopPending}
		return false, 0
	case "exit1":
		changes <- svc.Status{State: svc.StopPending}
		return true, 1
	}
	for r := range requests {
		switch r.Cmd {
		case svc.Interrogate:
			changes <- r.CurrentStatus
		case svc.Stop, svc.Shutdown:
			changes <- svc.Status{State: svc.StopPending}
			if s.mode == "stopexit1" {
				return true, 1
			}
			s.linger = true
			_ = held // closed when the process exits, after the linger
			return false, 0
		}
	}
	return false, 0
}

// ownPIDMatches is the agent's runningServiceName check: the SCM reports
// this process as the service's process while it is still start-pending.
func ownPIDMatches(name string) bool {
	m, err := mgr.Connect()
	if err != nil {
		return false
	}
	defer func() { _ = m.Disconnect() }()
	service, err := m.OpenService(name)
	if err != nil {
		return false
	}
	defer func() { _ = service.Close() }()
	status, err := service.Query()
	return err == nil && status.ProcessId == uint32(os.Getpid()) //nolint:gosec // G115: a pid fits
}

func appendLine(path, line string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(f, line)
	_ = f.Close()
}

// --- the real service control manager --------------------------------------

func contractSetup(t *testing.T) (string, realSystem) {
	t.Helper()
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("creating services needs an elevated process")
	}
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "held"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "held", "open.txt"), []byte("held"), 0o644))
	return dir, realSystem{}
}

// throwawayService registers this test binary as a service of the given
// behaviour, with recovery restarts after delays, and removes it afterwards.
func throwawayService(t *testing.T, sys realSystem, dir, mode string, delays []int) string {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err)
	return throwawayServiceFrom(t, sys, dir, mode, delays, exe)
}

// throwawayServiceFrom is throwawayService running the given executable (a
// copy of the test binary, when the test needs to touch the image).
func throwawayServiceFrom(t *testing.T, sys realSystem, dir, mode string, delays []int, exe string) string {
	t.Helper()
	name := fmt.Sprintf("ironflock-contract-%d-%s", os.Getpid(), mode)
	spec := winservice.Spec{
		Name:             name,
		DisplayName:      "IronFlock contract test (" + mode + ")",
		Description:      "A throwaway service of the device agent's migration tests; delete it if it was left behind.",
		Exe:              exe,
		Args:             []string{"-test.run", "^$"},
		StartType:        winservice.StartManual,
		RestartDelays:    delays,
		ResetAfter:       86400,
		NonCrashRecovery: true,
		Env:              []string{contractModeEnv + "=" + mode, contractNameEnv + "=" + name, contractDirEnv + "=" + dir},
	}
	require.NoError(t, sys.CreateOrUpdateService(spec))
	t.Cleanup(func() {
		_ = sys.SetStartType(name, winservice.StartDisabled, false)
		_ = sys.StopService(name)
		if status, err := sys.ServiceStatus(name); err == nil && status.PID > 0 {
			_ = sys.KillProcess(status.PID)
		}
		_ = sys.DeleteService(name)
	})
	return name
}

func starts(t *testing.T, dir string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "starts.log"))
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func waitStarts(t *testing.T, dir string, n int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if len(starts(t, dir)) >= n {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("not %d starts within %s: %v", n, within, starts(t, dir))
}

func waitRunning(t *testing.T, sys realSystem, name string) int {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if status, err := sys.ServiceStatus(name); err == nil && status.Running() && status.PID > 0 {
			return status.PID
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s did not run within 20s", name)
	return 0
}

// Recovery actions (with the non-crash flag) start a service again after a
// crash and after a non-zero exit, not after a clean exit 0: what the fake's
// restartAt models. And the SCM reports the service's process id as this
// process while it is still start-pending (runningServiceName).
func TestContractRecoveryRestartsCrashesAndFailuresOnly(t *testing.T) {
	for mode, restarted := range map[string]bool{"crash": true, "exit1": true, "exit0": false} {
		t.Run(mode, func(t *testing.T) {
			dir, sys := contractSetup(t)
			name := throwawayService(t, sys, dir, mode, []int{1})
			require.NoError(t, sys.StartService(name))
			waitStarts(t, dir, 1, 20*time.Second)
			if restarted {
				waitStarts(t, dir, 2, 30*time.Second)
			} else {
				time.Sleep(8 * time.Second)
				assert.Len(t, starts(t, dir), 1, "an exit 0 is not a failure")
			}
			for _, line := range starts(t, dir) {
				assert.Contains(t, line, "pidmatch=true", "the SCM names the start-pending process")
			}
		})
	}
}

// A service that is asked to stop and then exits with a service-specific
// error code is started again by its recovery actions (the non-crash flag):
// the migrate service relies on it when a stop lands while no agent runs.
func TestContractAStopWithExitCode1IsRestarted(t *testing.T) {
	dir, sys := contractSetup(t)
	name := throwawayService(t, sys, dir, "stopexit1", []int{1})
	require.NoError(t, sys.StartService(name))
	waitRunning(t, sys, name)
	require.NoError(t, sys.StopService(name))
	waitStarts(t, dir, 2, 30*time.Second)

	// the twin: a stop with exit code 0 is not a failure
	dir2, _ := contractSetup(t)
	hold := throwawayService(t, sys, dir2, "hold", []int{1})
	require.NoError(t, sys.StartService(hold))
	waitRunning(t, sys, hold)
	require.NoError(t, sys.StopService(hold))
	time.Sleep(8 * time.Second)
	assert.Len(t, starts(t, dir2), 1)
}

// A display name another service holds: accepted by Windows (the fake does the
// same), and SetDisplayName renames a service in place.
func TestContractDuplicateDisplayName(t *testing.T) {
	dir, sys := contractSetup(t)
	first := throwawayService(t, sys, dir, "exit0", nil)
	exe, err := os.Executable()
	require.NoError(t, err)
	second := winservice.Spec{Name: first + "-twin", DisplayName: "IronFlock contract test (exit0)", Exe: exe, Args: []string{"-test.run", "^$"}, StartType: winservice.StartManual}
	t.Cleanup(func() { _ = sys.DeleteService(second.Name) })
	// Windows Server 2022 accepts a second service with a display name
	// another one has (seen on the canary device, 2026-10-24): giving the
	// legacy service's name up keeps services.msc unambiguous, nothing more.
	require.NoError(t, sys.CreateOrUpdateService(second))
	require.NoError(t, sys.SetDisplayName(first, "IronFlock contract test (exit0, before)"))
	cfg, err := sys.Service(second.Name)
	require.NoError(t, err)
	assert.Equal(t, second.Exe, cfg.Exe)
}

// CreateOrUpdateService reapplies every setting on an existing service: a
// resumed migration reaches the full configuration whatever a previous run
// managed, and a start type set meanwhile is reset.
func TestContractCreateOrUpdateReappliesEverything(t *testing.T) {
	dir, sys := contractSetup(t)
	exe, err := os.Executable()
	require.NoError(t, err)
	name := fmt.Sprintf("ironflock-contract-%d-reapply", os.Getpid())
	t.Cleanup(func() { _ = sys.DeleteService(name) })
	partial := winservice.Spec{Name: name, DisplayName: "IronFlock contract test (reapply)", Exe: exe, Args: []string{"-test.run", "^$"}, StartType: winservice.StartManual}
	require.NoError(t, sys.CreateOrUpdateService(partial))
	full := partial
	full.Args = []string{"-test.run", "^$", "-contract"}
	full.Description = "the full spec"
	full.StartType, full.Delayed = winservice.StartAutomatic, true
	full.RestartDelays, full.ResetAfter, full.NonCrashRecovery = []int{5, 30, 120}, 86400, true
	full.Env = []string{contractDirEnv + "=" + dir}
	require.NoError(t, sys.CreateOrUpdateService(full))
	cfg, err := sys.Service(name)
	require.NoError(t, err)
	assert.Equal(t, full.Args, cfg.Args)
	assert.Equal(t, winservice.StartAutomatic, cfg.StartType)
	assert.True(t, cfg.Delayed)
	assert.Equal(t, full.Env, cfg.Env)
	assert.Equal(t, "the full spec", cfg.Description)
	m, err := mgr.Connect()
	require.NoError(t, err)
	defer func() { _ = m.Disconnect() }()
	s, err := m.OpenService(name)
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	actions, err := s.RecoveryActions()
	require.NoError(t, err)
	assert.Len(t, actions, 3)
	reset, err := s.ResetPeriod()
	require.NoError(t, err)
	assert.Equal(t, uint32(86400), reset)
	nonCrash, err := s.RecoveryActionsOnNonCrashFailures()
	require.NoError(t, err)
	assert.True(t, nonCrash)

	require.NoError(t, sys.SetStartType(name, winservice.StartDisabled, false))
	require.NoError(t, sys.CreateOrUpdateService(full))
	cfg, err = sys.Service(name)
	require.NoError(t, err)
	assert.Equal(t, winservice.StartAutomatic, cfg.StartType, "a disabled service is enabled again")
}

// The repair task's definition survives an export and import (prepare backs
// the legacy task up, the rollback restores it); a definition with garbage
// in front is refused, which is what the rollback's fallback (a task
// registered anew) covers.
func TestContractRepairTaskRoundTrip(t *testing.T) {
	_, sys := contractSetup(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "a.cmd")
	require.NoError(t, os.WriteFile(script, []byte("@echo off\r\n"), 0o644))
	name := fmt.Sprintf("ironflock-contract-task-%d", os.Getpid())
	t.Cleanup(func() { _, _ = runCommand(commandTimeout, "schtasks", "/Delete", "/F", "/TN", name) })
	require.NoError(t, sys.CreateTask(name, script))
	xml, err := sys.ExportTask(name)
	require.NoError(t, err)
	require.NoError(t, sys.ImportTask(name, xml))
	out, err := runCommand(commandTimeout, "schtasks", "/Query", "/TN", name, "/V", "/FO", "LIST")
	require.NoError(t, err)
	assert.Contains(t, string(out), "a.cmd")
	require.Error(t, sys.ImportTask(name, append([]byte("garbage\r\n"), xml...)))
	_, err = sys.ExportTask(name + "-missing")
	assert.ErrorIs(t, err, os.ErrNotExist)
}

// WriteFile and CopyFile flush the data before the replacing rename: NTFS
// journals the rename, not the data.
func TestContractWriteFileFlushes(t *testing.T) {
	sys := realSystem{}
	dir := t.TempDir()
	synced := 0
	previous := syncFile
	syncFile = func(f *os.File) error { synced++; return f.Sync() }
	defer func() { syncFile = previous }()
	path := filepath.Join(dir, "journal.json")
	require.NoError(t, sys.WriteFile(path, []byte("one"), 0o644))
	assert.Equal(t, 1, synced)
	require.NoError(t, sys.CopyFile(path, path+".bak", 0o644))
	assert.Equal(t, 2, synced)
	data, err := os.ReadFile(path + ".bak")
	require.NoError(t, err)
	assert.Equal(t, "one", string(data))
	leftovers, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	assert.Empty(t, leftovers)
}

// A replacing write that lands while another process reads the file (the
// agent reads the journal for its heartbeat) waits the read out.
func TestContractReplaceWaitsForAReader(t *testing.T) {
	sys := realSystem{}
	path := filepath.Join(t.TempDir(), "journal.json")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o644))
	reader, err := os.Open(path)
	require.NoError(t, err)
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = reader.Close()
	}()
	require.NoError(t, sys.WriteFile(path, []byte("new"), 0o644))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "new", string(data))
}

// The executable of a running service cannot be removed (a mapped image):
// what keeps a Stage from replacing a running migrator, and what the fake's
// Remove models.
func TestContractARunningImageCannotBeRemoved(t *testing.T) {
	dir, sys := contractSetup(t)
	exe, err := os.Executable()
	require.NoError(t, err)
	copy := filepath.Join(dir, "copy.exe")
	require.NoError(t, sys.CopyFile(exe, copy, 0o755))
	name := throwawayServiceFrom(t, sys, dir, "hold", nil, copy)
	require.NoError(t, sys.StartService(name))
	pid := waitRunning(t, sys, name)
	require.Error(t, sys.Remove(copy), "in use")
	require.NoError(t, sys.StopService(name))
	require.NoError(t, sys.KillProcess(pid))
	require.NoError(t, sys.Remove(copy))
}

// The engine's lock is one per machine.
func TestContractLockMigration(t *testing.T) {
	sys := realSystem{}
	release, err := sys.LockMigration()
	require.NoError(t, err)
	_, err = sys.LockMigration()
	require.ErrorContains(t, err, "is held")
	release()
	release, err = sys.LockMigration()
	require.NoError(t, err)
	release()
}

// A disabled service can neither be started nor restarted by its recovery
// actions: the rollback disables the new service before stopping it, and
// stop-legacy disables reagent before stopping it.
func TestContractDisabledServiceNeitherStartsNorRecovers(t *testing.T) {
	dir, sys := contractSetup(t)
	name := throwawayService(t, sys, dir, "hold", []int{1})
	require.NoError(t, sys.SetStartType(name, winservice.StartDisabled, false))
	err := sys.StartService(name)
	require.Error(t, err)
	assert.ErrorIs(t, err, windows.ERROR_SERVICE_DISABLED)

	require.NoError(t, sys.SetStartType(name, winservice.StartManual, false))
	require.NoError(t, sys.StartService(name))
	pid := waitRunning(t, sys, name)
	require.NoError(t, sys.SetStartType(name, winservice.StartDisabled, false))
	require.NoError(t, sys.KillProcess(pid))
	time.Sleep(5 * time.Second)
	assert.Len(t, starts(t, dir), 1, "no recovery for a disabled service")
	status, err := sys.ServiceStatus(name)
	require.NoError(t, err)
	assert.True(t, status.Stopped())
}

// The service control manager reports a service stopped before its process
// has exited; the files it holds are released only then (the engine
// terminates and waits: KillProcess, clearProcessesUnder).
func TestContractStoppedIsReportedBeforeTheProcessExits(t *testing.T) {
	dir, sys := contractSetup(t)
	name := throwawayService(t, sys, dir, "hold", nil)
	require.NoError(t, sys.StartService(name))
	pid := waitRunning(t, sys, name)
	require.NoError(t, sys.StopService(name))
	assert.True(t, sys.ProcessAlive(pid), "stopped, but the process lingers")
	held := filepath.Join(dir, "held")
	require.Error(t, sys.Rename(held, held+"2"), "the open handle blocks the rename")
	require.NoError(t, sys.KillProcess(pid))
	assert.False(t, sys.ProcessAlive(pid))
	require.NoError(t, sys.Rename(held, held+"2"))
}

// Deleting a running service marks it for deletion: it stays until it
// stops, and a service of that name cannot be created meanwhile
// (ErrServiceMarkedForDelete, which register-service waits out).
func TestContractDeleteWhileRunningIsMarkedForDelete(t *testing.T) {
	dir, sys := contractSetup(t)
	name := throwawayService(t, sys, dir, "hold", nil)
	require.NoError(t, sys.StartService(name))
	pid := waitRunning(t, sys, name)
	require.NoError(t, sys.DeleteService(name))
	_, err := sys.Service(name)
	require.NoError(t, err, "still there while it runs")
	cfg, _ := sys.Service(name)
	err = sys.CreateOrUpdateService(winservice.Spec{Name: name, DisplayName: name, Exe: cfg.Exe, Args: cfg.Args, StartType: winservice.StartManual})
	require.ErrorIs(t, err, ErrServiceMarkedForDelete)
	require.NoError(t, sys.StopService(name))
	_ = sys.KillProcess(pid)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := sys.Service(name); err != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	_, err = sys.Service(name)
	assert.ErrorIs(t, err, os.ErrNotExist, "gone once stopped")
}

// A folder with a file held open cannot be renamed; the Restart Manager
// names the holder; once the holder is gone the rename works. And Rename
// never overwrites (a plain os.Rename would).
func TestContractRenameWithOpenHandles(t *testing.T) {
	dir, sys := contractSetup(t)
	name := throwawayService(t, sys, dir, "hold", nil)
	require.NoError(t, sys.StartService(name))
	pid := waitRunning(t, sys, name)
	held := filepath.Join(dir, "held")
	err := sys.Rename(held, held+"-moved")
	require.Error(t, err)
	holders := sys.HoldersOf(held)
	require.NotEmpty(t, holders, "the Restart Manager names the holder")
	assert.Contains(t, strings.Join(holders, ", "), fmt.Sprintf("pid %d", pid))
	require.NoError(t, sys.StopService(name))
	require.NoError(t, sys.KillProcess(pid))
	require.NoError(t, sys.Rename(held, held+"-moved"))
}

func TestContractRenameNeverOverwrites(t *testing.T) {
	sys := realSystem{}
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	require.NoError(t, os.WriteFile(a, []byte("a"), 0o644))
	require.NoError(t, os.WriteFile(b, []byte("b"), 0o644))
	err := sys.Rename(a, b)
	require.ErrorIs(t, err, os.ErrExist)
	data, _ := os.ReadFile(b)
	assert.Equal(t, "b", string(data), "untouched")
	require.ErrorIs(t, sys.Rename(filepath.Join(dir, "missing"), filepath.Join(dir, "c")), os.ErrNotExist)
	require.NoError(t, sys.Rename(a, filepath.Join(dir, "c")))
	assert.NoFileExists(t, a)
}

// RemoveForeign against NTFS: a junction a local user made (no privilege
// needed) at the migrator's folder and at the new folder, pointing at a
// folder Administrators own, is judged by its own owner — the user's — and
// goes, the link and never what it points at. Go reports a junction as a
// link (irregular), which the engine's rule for its own files relies on.
func TestContractAJunctionIsJudgedByItsOwnOwner(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("setting another owner needs an elevated process")
	}
	sys := realSystem{}
	dir := t.TempDir()
	e := Env{Layout: Layout{Root: filepath.Join(dir, "IronFlock")}, Sys: sys, Logf: t.Logf}
	require.NoError(t, os.MkdirAll(e.Layout.Root, 0o755))
	target := filepath.Join(dir, "administrators")
	require.NoError(t, os.MkdirAll(target, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(target, winservice.ExeName), []byte("kept"), 0o644))
	require.NoError(t, winservice.RunCommand("icacls", target, "/setowner", winservice.SidAdministrators))
	owned, err := sys.OwnedByAdministrators(target)
	require.NoError(t, err)
	require.True(t, owned)

	links := []string{e.Layout.MigrateDir(), e.Layout.NewDir()}
	for _, link := range links {
		out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
		require.NoError(t, err, string(out))
		// the owner the user who made it has (/L: the link itself)
		require.NoError(t, winservice.RunCommand("icacls", link, "/setowner", winservice.SidUsers, "/L"))
		info, err := sys.Stat(link)
		require.NoError(t, err)
		assert.NotZero(t, info.Mode().Type()&^os.ModeDir, "%s reported as a link (%v)", link, info.Mode())
		owned, err := sys.OwnedByAdministrators(link)
		require.NoError(t, err)
		assert.False(t, owned, "%s judged by its own owner, not by its target's", link)
	}

	require.NoError(t, RemoveForeign(e))
	for _, link := range links {
		_, err := os.Lstat(link)
		assert.True(t, os.IsNotExist(err), "%s removed", link)
	}
	data, err := os.ReadFile(filepath.Join(target, winservice.ExeName))
	require.NoError(t, err)
	assert.Equal(t, "kept", string(data), "the folder the links pointed at is untouched")
}

// --- the real repair scripts -------------------------------------------------

// The behaviour table of the repair scripts (contract_test.go), run through
// cmd.exe over the fake's fixture folders. `sc` and `reg` are stubbed in the
// script's working directory (cmd.exe looks there first): sc records which
// service the script starts, which is then applied to the fake's service
// control manager; reg answers the Start values the fake's services have.
func TestContractRepairScripts(t *testing.T) {
	for _, c := range repairScriptCases() {
		t.Run(c.name, func(t *testing.T) {
			d := newDevice(t)
			d.powerCut()
			c.setup(d)
			runRealRepairTask(t, d)
			c.check(t, d)
		})
	}
}

func runRealRepairTask(t *testing.T, d *device) {
	t.Helper()
	if d.sys.task == nil {
		return
	}
	if _, err := os.Stat(d.sys.task.script); err != nil {
		return
	}
	stubs := t.TempDir()
	scLog := filepath.Join(stubs, "sc.log")
	states := filepath.Join(stubs, "reg-states.txt")
	var lines []string
	for name, s := range d.sys.services {
		code := "0x3"
		switch s.startType {
		case winservice.StartAutomatic:
			code = "0x2"
		case winservice.StartDisabled:
			code = "0x4"
		}
		lines = append(lines, name+"="+code)
	}
	require.NoError(t, os.WriteFile(states, []byte(strings.Join(lines, "\r\n")+"\r\n"), 0o644))
	self, err := os.Executable()
	require.NoError(t, err)
	for _, tool := range []string{"sc.exe", "reg.exe"} {
		data, err := os.ReadFile(self)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(stubs, tool), data, 0o755))
	}
	// The script's path is quoted by hand: t.TempDir keeps the commas and
	// parentheses of the case names, and `cmd /c "<path>"` strips the quotes
	// of such a line; /s with doubled quotes keeps them (first run on a real
	// machine, 2026-10-24).
	cmd := exec.Command("cmd")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `cmd /s /c ""` + d.sys.task.script + `""`}
	cmd.Dir = stubs
	cmd.Env = append(os.Environ(), "SC_LOG="+scLog, "REG_STATES="+states, contractStubEnv+"=1")
	// The fake's brokenExe marks a file's content; one the script replaced
	// (deleted and copied anew, a new file identity) runs again.
	before := map[string]os.FileInfo{}
	for path := range d.sys.brokenExe {
		if info, err := os.Stat(path); err == nil {
			// Windows loads a FileInfo's file id by path on the first
			// SameFile: load it now, or it is the replacement's id later.
			_ = os.SameFile(info, info)
			before[path] = info
		}
	}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	for path := range d.sys.brokenExe {
		if after, err := os.Stat(path); err != nil || before[path] == nil || !os.SameFile(before[path], after) {
			delete(d.sys.brokenExe, path)
		}
	}
	data, _ := os.ReadFile(scLog)
	t.Logf("script output:\n%s\nsc calls:\n%s", out, data)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 4 && fields[0] == "config" && fields[2] == "start=" && fields[3] == "auto" {
			if s := d.sys.services[fields[1]]; s != nil {
				s.startType = winservice.StartAutomatic
			}
		}
		if len(fields) == 2 && fields[0] == "start" {
			_ = d.sys.startService(fields[1])
		}
	}
}

// HardenRoot as Stage and `service install` run it: icacls refuses
// /setowner next to /grant, so a single call failed on every real machine
// (found on the canary device, 2026-10-24) while the fake's Harden passed.
func TestContractHardenRoot(t *testing.T) {
	_, _ = contractSetup(t)
	root := filepath.Join(t.TempDir(), "IronFlock")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "child"), 0o755))
	require.NoError(t, winservice.HardenRoot(root))
	owned, err := winservice.OwnedByAdministrators(root)
	require.NoError(t, err)
	assert.True(t, owned)
	require.NoError(t, winservice.HardenRoot(root), "idempotent")
}
