//go:build windows

package winmigrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reagent/winservice"
	"strings"
	"time"
	"unsafe"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

// SystemEnv is the Env of the running machine: %ProgramData%\IronFlock, the
// real service control manager, registry, processes, Docker (through the
// environment this process inherited: a service's Environment value), and
// the test hooks from the environment.
func SystemEnv(version string, logf func(format string, args ...any)) Env {
	return Env{
		Layout:  DefaultLayout(),
		Sys:     realSystem{},
		Version: version,
		Fault:   os.Getenv(FaultEnv),
		Pause:   os.Getenv(PauseEnv),
		Logf:    logf,
	}
}

// Every external command runs under a timeout: a hung tool must not hang
// the migration (the watchdog would count it as a crash).
const (
	commandTimeout  = 2 * time.Minute
	defenderTimeout = 5 * time.Minute
	versionTimeout  = 30 * time.Second
	dockerTimeout   = 2 * time.Minute
	scmStopTimeout  = 2 * time.Minute
)

type realSystem struct{}

// stillActive is GetExitCodeProcess's answer for a running process
// (STILL_ACTIVE, not in x/sys).
const stillActive = 259

// --- files ---

func (realSystem) Stat(path string) (os.FileInfo, error)      { return os.Lstat(path) }
func (realSystem) ReadFile(path string) ([]byte, error)       { return os.ReadFile(path) }
func (realSystem) ReadDir(path string) ([]os.DirEntry, error) { return os.ReadDir(path) }
func (realSystem) Remove(path string) error                   { return os.Remove(path) }
func (realSystem) RemoveAll(path string) error                { return os.RemoveAll(path) }
func (realSystem) MkdirAll(path string, mode os.FileMode) error {
	return os.MkdirAll(path, mode)
}

// syncFile flushes a written file to the disk (the contract test counts
// its calls).
var syncFile = (*os.File).Sync

// WriteFile writes next to the file, flushes, and renames into place, so a
// reader never sees a partial file and a power cut never an empty one: the
// journal and its backup are rewritten back to back, and NTFS journals the
// rename but not the data, so without the flush a cut within the lazy
// writer's interval could leave both of them unreadable. The temporary
// file has a unique name: the agent and the migrator both write the
// journal, and two writers of one temporary file would garble it.
func (realSystem) WriteFile(path string, data []byte, mode os.FileMode) error {
	out, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := out.Name()
	_, err = out.Write(data)
	if err == nil {
		err = syncFile(out)
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(tmp, mode)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := replaceFile(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// replaceRetries: a replacing rename is refused while another process reads
// the file (the agent reads the journal for its heartbeat; Go opens without
// FILE_SHARE_DELETE); such a read takes a millisecond, the retry waits it out.
const (
	replaceRetries    = 20
	replaceRetryDelay = 50 * time.Millisecond
)

// replaceFile puts tmp in place of path, written through to the disk.
func replaceFile(tmp, path string) error {
	src, err := windows.UTF16PtrFromString(tmp)
	if err != nil {
		return err
	}
	dst, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		err := windows.MoveFileEx(src, dst, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
		if err == nil {
			return nil
		}
		busy := errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED)
		if !busy || attempt >= replaceRetries {
			return &os.LinkError{Op: "rename", Old: tmp, New: path, Err: err}
		}
		time.Sleep(replaceRetryDelay)
	}
}

// Rename never replaces: MoveFileEx without MOVEFILE_REPLACE_EXISTING fails
// with ERROR_ALREADY_EXISTS when the destination exists (os.Rename would
// overwrite a file). Files and folders alike.
func (realSystem) Rename(from, to string) error {
	src, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	dst, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	err = windows.MoveFileEx(src, dst, windows.MOVEFILE_WRITE_THROUGH)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, windows.ERROR_ALREADY_EXISTS), errors.Is(err, windows.ERROR_FILE_EXISTS):
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: os.ErrExist}
	case errors.Is(err, windows.ERROR_FILE_NOT_FOUND), errors.Is(err, windows.ERROR_PATH_NOT_FOUND):
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: os.ErrNotExist}
	default:
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
	}
}

func (s realSystem) CopyFile(from, to string, mode os.FileMode) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.CreateTemp(filepath.Dir(to), filepath.Base(to)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := out.Name()
	_, err = io.Copy(out, in)
	if err == nil {
		err = syncFile(out) // the journal backup must be on disk before the journal is rewritten
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(tmp, mode)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := replaceFile(tmp, to); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// SameVolume compares the volumes the paths' nearest existing ancestors
// are on: a rename across volumes is a copy, not a move.
func (realSystem) SameVolume(a, b string) (bool, error) {
	va, err := volumeOf(a)
	if err != nil {
		return false, err
	}
	vb, err := volumeOf(b)
	if err != nil {
		return false, err
	}
	return strings.EqualFold(va, vb), nil
}

func volumeOf(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Lstat(path); err == nil {
			break
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", fmt.Errorf("no existing folder above %s", path)
		}
		path = parent
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	buf := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumePathName(name, &buf[0], uint32(len(buf))); err != nil {
		return "", fmt.Errorf("GetVolumePathName(%s): %w", path, err)
	}
	return windows.UTF16ToString(buf), nil
}

// --- the Restart Manager (rstrtmgr.dll, no bindings in x/sys) ---

var (
	rstrtmgr                = windows.NewLazySystemDLL("rstrtmgr.dll")
	procRmStartSession      = rstrtmgr.NewProc("RmStartSession")
	procRmRegisterResources = rstrtmgr.NewProc("RmRegisterResources")
	procRmGetList           = rstrtmgr.NewProc("RmGetList")
	procRmEndSession        = rstrtmgr.NewProc("RmEndSession")
)

const (
	rmSessionKeyLen = 32 + 1 // CCH_RM_SESSION_KEY + 1
	rmMaxAppName    = 255 + 1
	rmMaxSvcName    = 63 + 1
	rmMaxFiles      = 2000 // enough for an agent folder with its apps' data
	rmErrorMoreData = 234
)

type rmUniqueProcess struct {
	ProcessID uint32
	StartTime windows.Filetime
}

type rmProcessInfo struct {
	Process          rmUniqueProcess
	AppName          [rmMaxAppName]uint16
	ServiceShortName [rmMaxSvcName]uint16
	ApplicationType  int32
	AppStatus        uint32
	TSSessionID      uint32
	Restartable      int32
}

// HoldersOf names the processes holding files below path open, for the
// error that tells an operator what blocked a rename. Best effort.
func (realSystem) HoldersOf(path string) []string {
	if err := rstrtmgr.Load(); err != nil {
		return nil
	}
	var files []string
	_ = filepath.WalkDir(path, func(p string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		if len(files) >= rmMaxFiles {
			return filepath.SkipAll
		}
		files = append(files, p)
		return nil
	})
	if len(files) == 0 {
		return nil
	}
	var session uint32
	key := make([]uint16, rmSessionKeyLen)
	if r, _, _ := procRmStartSession.Call(uintptr(unsafe.Pointer(&session)), 0, uintptr(unsafe.Pointer(&key[0]))); r != 0 { //nolint:gosec // G103: the Restart Manager's C signature, buffers sized by its constants
		return nil
	}
	defer func() { _, _, _ = procRmEndSession.Call(uintptr(session)) }()
	names := make([]*uint16, 0, len(files))
	for _, f := range files {
		p, err := windows.UTF16PtrFromString(f)
		if err != nil {
			continue
		}
		names = append(names, p)
	}
	if r, _, _ := procRmRegisterResources.Call(uintptr(session), uintptr(len(names)), uintptr(unsafe.Pointer(&names[0])), 0, 0, 0, 0); r != 0 { //nolint:gosec // G103: the Restart Manager's C signature
		return nil
	}
	var needed, count, reasons uint32
	r, _, _ := procRmGetList.Call(uintptr(session), uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&count)), 0, uintptr(unsafe.Pointer(&reasons))) //nolint:gosec // G103: the Restart Manager's C signature
	if r != rmErrorMoreData || needed == 0 {
		return nil
	}
	infos := make([]rmProcessInfo, needed)
	count = needed
	r, _, _ = procRmGetList.Call(uintptr(session), uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&infos[0])), uintptr(unsafe.Pointer(&reasons))) //nolint:gosec // G103: the Restart Manager's C signature, the array sized by its own count
	if r != 0 {
		return nil
	}
	var holders []string
	for _, info := range infos[:count] {
		name := windows.UTF16ToString(info.AppName[:])
		if svcName := windows.UTF16ToString(info.ServiceShortName[:]); svcName != "" {
			name += " (service " + svcName + ")"
		}
		holders = append(holders, fmt.Sprintf("%s (pid %d)", name, info.Process.ProcessID))
	}
	return holders
}

// --- processes ---

func (realSystem) ExecVersion(exe string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), versionTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, exe, "-version").Output() //nolint:gosec // G204: the agent's own copies
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// ProcessesUnder walks the process list (Toolhelp) and keeps the processes
// whose image lies below dir.
func (realSystem) ProcessesUnder(dir string) ([]Process, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("listing the processes: %w", err)
	}
	defer func() { _ = windows.CloseHandle(snapshot) }()
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	var out []Process
	own := uint32(os.Getpid()) //nolint:gosec // G115: a pid fits
	for err := windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		if entry.ProcessID == 0 || entry.ProcessID == own {
			continue
		}
		exe := processImagePath(entry.ProcessID)
		if exe != "" && underDir(exe, dir) {
			out = append(out, Process{PID: int(entry.ProcessID), Exe: exe})
		}
	}
	return out, nil
}

func processImagePath(pid uint32) string {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(handle, 0, &buf[0], &size); err != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:size])
}

func (realSystem) ProcessAlive(pid int) bool {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid)) //nolint:gosec // G115: a pid fits
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	var code uint32
	if err := windows.GetExitCodeProcess(handle, &code); err != nil {
		return false
	}
	return code == stillActive
}

// KillProcess terminates pid and waits on its handle until it is gone: the
// service control manager reports a service stopped before its process has
// exited, and a process that is gone releases the files it held.
func (realSystem) KillProcess(pid int) error {
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid)) //nolint:gosec // G115: a pid fits
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return nil // gone already
		}
		return fmt.Errorf("opening process %d: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	if err := windows.TerminateProcess(handle, 1); err != nil {
		var code uint32
		if windows.GetExitCodeProcess(handle, &code) == nil && code != stillActive {
			return nil // exited by itself
		}
		return fmt.Errorf("terminating process %d: %w", pid, err)
	}
	event, err := windows.WaitForSingleObject(handle, uint32(killWait/time.Millisecond))
	if err != nil {
		return err
	}
	if event == uint32(windows.WAIT_TIMEOUT) {
		return fmt.Errorf("process %d did not exit within %s of its termination", pid, killWait)
	}
	return nil
}

// --- the service control manager ---

func notExist(err error) bool {
	return errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST)
}

func openService(name string) (*mgr.Mgr, *mgr.Service, error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to the service control manager: %w", err)
	}
	s, err := m.OpenService(name)
	if err != nil {
		_ = m.Disconnect()
		if notExist(err) {
			return nil, nil, fmt.Errorf("service %s: %w", name, os.ErrNotExist)
		}
		return nil, nil, fmt.Errorf("opening service %s: %w", name, err)
	}
	return m, s, nil
}

func closeService(m *mgr.Mgr, s *mgr.Service) {
	_ = s.Close()
	_ = m.Disconnect()
}

func (realSystem) Service(name string) (ServiceConfig, error) {
	m, s, err := openService(name)
	if err != nil {
		return ServiceConfig{}, err
	}
	defer closeService(m, s)
	cfg, err := s.Config()
	if err != nil {
		return ServiceConfig{}, fmt.Errorf("reading the configuration of service %s: %w", name, err)
	}
	args, err := windows.DecomposeCommandLine(cfg.BinaryPathName)
	if err != nil || len(args) == 0 {
		return ServiceConfig{}, fmt.Errorf("the ImagePath of service %s (%q) cannot be parsed: %v", name, cfg.BinaryPathName, err)
	}
	env, err := winservice.Environment(name)
	if err != nil {
		return ServiceConfig{}, fmt.Errorf("reading the environment of service %s: %w", name, err)
	}
	return ServiceConfig{
		Exe:         args[0],
		Args:        args[1:],
		StartType:   winservice.StartTypeOf(cfg.StartType),
		Delayed:     cfg.DelayedAutoStart,
		Env:         env,
		Description: cfg.Description,
	}, nil
}

func (realSystem) ServiceStatus(name string) (ServiceStatus, error) {
	m, s, err := openService(name)
	if err != nil {
		return ServiceStatus{}, err
	}
	defer closeService(m, s)
	status, err := s.Query()
	if err != nil {
		return ServiceStatus{}, fmt.Errorf("querying service %s: %w", name, err)
	}
	return ServiceStatus{State: stateName(status.State), PID: int(status.ProcessId)}, nil
}

func stateName(state svc.State) string {
	switch state {
	case svc.Running:
		return StateRunning
	case svc.Stopped:
		return StateStopped
	case svc.StartPending:
		return StateStartPending
	case svc.StopPending:
		return StateStopPending
	case svc.ContinuePending:
		return "continue-pending"
	case svc.PausePending:
		return "pause-pending"
	case svc.Paused:
		return "paused"
	}
	return fmt.Sprintf("state-%d", state)
}

func (realSystem) CreateOrUpdateService(spec winservice.Spec) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connecting to the service control manager: %w", err)
	}
	defer func() { _ = m.Disconnect() }()
	s, err := winservice.CreateOrUpdateService(m, spec)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
			return fmt.Errorf("%s: %w", spec.Name, ErrServiceMarkedForDelete)
		}
		return err
	}
	_ = s.Close()
	return nil
}

func (realSystem) SetStartType(name string, start winservice.StartType, delayed bool) error {
	m, s, err := openService(name)
	if err != nil {
		return err
	}
	defer closeService(m, s)
	cfg, err := s.Config()
	if err != nil {
		return fmt.Errorf("reading the configuration of service %s: %w", name, err)
	}
	switch start {
	case winservice.StartAutomatic:
		cfg.StartType = mgr.StartAutomatic
	case winservice.StartDisabled:
		cfg.StartType = mgr.StartDisabled
	default:
		cfg.StartType = mgr.StartManual
	}
	cfg.DelayedAutoStart = delayed
	if err := s.UpdateConfig(cfg); err != nil {
		return fmt.Errorf("setting the start type of service %s: %w", name, err)
	}
	winservice.FlushRegistry(winservice.ServicesKey)
	return nil
}

func (realSystem) SetDisplayName(name, display string) error {
	m, s, err := openService(name)
	if err != nil {
		return err
	}
	defer closeService(m, s)
	cfg, err := s.Config()
	if err != nil {
		return fmt.Errorf("reading the configuration of service %s: %w", name, err)
	}
	if cfg.DisplayName == display {
		return nil
	}
	cfg.DisplayName = display
	if err := s.UpdateConfig(cfg); err != nil {
		return fmt.Errorf("setting the display name of service %s: %w", name, err)
	}
	winservice.FlushRegistry(winservice.ServicesKey)
	return nil
}

func (realSystem) StartService(name string) error {
	m, s, err := openService(name)
	if err != nil {
		return err
	}
	defer closeService(m, s)
	if err := s.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return fmt.Errorf("starting service %s: %w", name, err)
	}
	return nil
}

// StopService asks the service to stop and waits for the service control
// manager to report it stopped. Its process may still be exiting.
func (realSystem) StopService(name string) error {
	m, s, err := openService(name)
	if err != nil {
		return err
	}
	defer closeService(m, s)
	status, err := s.Query()
	if err != nil {
		return fmt.Errorf("querying service %s: %w", name, err)
	}
	if status.State == svc.Stopped {
		return nil
	}
	if status.State != svc.StopPending {
		if _, err := s.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return fmt.Errorf("stopping service %s: %w", name, err)
		}
	}
	deadline := time.Now().Add(scmStopTimeout)
	for {
		status, err := s.Query()
		if err != nil {
			return fmt.Errorf("querying service %s: %w", name, err)
		}
		if status.State == svc.Stopped {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("service %s did not stop within %s", name, scmStopTimeout)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (realSystem) DeleteService(name string) error {
	m, s, err := openService(name)
	if err != nil {
		return err
	}
	defer closeService(m, s)
	if err := s.Delete(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
		return fmt.Errorf("deleting service %s: %w", name, err)
	}
	winservice.FlushRegistry(winservice.ServicesKey)
	return nil
}

func (realSystem) InstallEventSource(name string) error {
	err := eventlog.InstallAsEventCreate(name, eventlog.Error|eventlog.Warning|eventlog.Info)
	if err != nil && strings.Contains(err.Error(), "already exists") {
		return nil
	}
	return err
}

func (realSystem) RemoveEventSource(name string) error {
	err := eventlog.Remove(name)
	if errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("event source %s: %w", name, os.ErrNotExist)
	}
	return err
}

// --- the repair scheduled task ---

func runCommand(timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput() //nolint:gosec // G204: fixed Windows tools with the agent's own paths
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (realSystem) ExportTask(name string) ([]byte, error) {
	out, err := runCommand(commandTimeout, "schtasks", "/Query", "/TN", name, "/XML")
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			// schtasks says "the system cannot find the file specified"
			// (localized) for a task that does not exist
			return nil, fmt.Errorf("task %s: %w: %v", name, os.ErrNotExist, err)
		}
		return nil, err
	}
	return out, nil
}

func (realSystem) CreateTask(name, script string) error {
	_, err := runCommand(commandTimeout, "schtasks", "/Create", "/F", "/TN", name, "/SC", "ONSTART", "/RU", "SYSTEM", "/TR", "\""+script+"\"")
	winservice.FlushRegistry(winservice.TaskCacheKey)
	return err
}

func (realSystem) ImportTask(name string, xml []byte) error {
	tmp, err := os.CreateTemp("", "ironflock-task-*.xml")
	if err != nil {
		return err
	}
	path := tmp.Name()
	defer func() { _ = os.Remove(path) }()
	_, err = tmp.Write(xml)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	_, err = runCommand(commandTimeout, "schtasks", "/Create", "/F", "/TN", name, "/XML", path)
	winservice.FlushRegistry(winservice.TaskCacheKey)
	return err
}

// --- Windows Defender ---

func psPath(path string) string { return "'" + strings.ReplaceAll(path, "'", "''") + "'" }

func (realSystem) AddDefenderExclusion(path string) error {
	_, err := runCommand(defenderTimeout, "powershell", "-NonInteractive", "-Command", "Add-MpPreference -ExclusionPath "+psPath(path))
	return err
}

func (realSystem) RemoveDefenderExclusion(path string) error {
	_, err := runCommand(defenderTimeout, "powershell", "-NonInteractive", "-Command", "Remove-MpPreference -ExclusionPath "+psPath(path))
	return err
}

// --- Docker (the engine this process's environment names: the service's
// Environment carries a WSL engine's DOCKER_HOST and certificates) ---

func dockerClient() (*client.Client, error) {
	return client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
}

func (realSystem) Containers() ([]Container, error) {
	cli, err := dockerClient()
	if err != nil {
		return nil, err
	}
	defer func() { _ = cli.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), dockerTimeout)
	defer cancel()
	list, err := cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(list))
	for _, c := range list {
		name := c.ID
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		policy := ""
		if inspected, err := cli.ContainerInspect(ctx, c.ID); err == nil && inspected.HostConfig != nil {
			policy = restartPolicyString(string(inspected.HostConfig.RestartPolicy.Name), inspected.HostConfig.RestartPolicy.MaximumRetryCount)
		}
		var binds []string
		for _, m := range c.Mounts {
			if m.Type == "bind" || m.Type == "" {
				binds = append(binds, m.Source)
			}
		}
		out = append(out, Container{ID: c.ID, Name: name, RestartPolicy: policy, Binds: binds, Running: c.State == "running"})
	}
	return out, nil
}

func (realSystem) SetRestartPolicy(id, policy string) error {
	cli, err := dockerClient()
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), dockerTimeout)
	defer cancel()
	name, retries := parseRestartPolicy(policy)
	_, err = cli.ContainerUpdate(ctx, id, container.UpdateConfig{RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyMode(name), MaximumRetryCount: retries}})
	if cerrdefs.IsNotFound(err) {
		return fmt.Errorf("container %s: %w", id, os.ErrNotExist)
	}
	return err
}

func (realSystem) StopContainer(id string) error {
	cli, err := dockerClient()
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), dockerTimeout)
	defer cancel()
	timeout := 30
	err = cli.ContainerStop(ctx, id, container.StopOptions{Timeout: &timeout})
	if cerrdefs.IsNotFound(err) {
		return fmt.Errorf("container %s: %w", id, os.ErrNotExist)
	}
	return err
}

// --- the machine ---

func (realSystem) Harden(root string) error { return winservice.HardenRoot(root) }

func (realSystem) OwnedByAdministrators(path string) (bool, error) {
	return winservice.OwnedByAdministrators(path)
}

// DockerDesktopSharing reads Docker Desktop's per-user settings: with the
// Hyper-V backend (wslEngineEnabled false) only the folders of its file
// sharing list reach the containers, so the agent folder must stay under
// one of them.
func (realSystem) DockerDesktopSharing() (bool, []string) {
	drive := os.Getenv("SystemDrive")
	if drive == "" {
		drive = "C:"
	}
	for _, name := range []string{"settings-store.json", "settings.json"} {
		matches, _ := filepath.Glob(filepath.Join(drive+`\`, "Users", "*", "AppData", "Roaming", "Docker", name))
		for _, path := range matches {
			data, err := os.ReadFile(path) //nolint:gosec // G703: Docker Desktop's settings below the user profiles, not remote input
			if err != nil {
				continue
			}
			var settings struct {
				WSLEngineEnabled       *bool    `json:"wslEngineEnabled"`
				FilesharingDirectories []string `json:"filesharingDirectories"`
			}
			if json.Unmarshal(data, &settings) != nil {
				continue
			}
			if settings.WSLEngineEnabled != nil && !*settings.WSLEngineEnabled {
				return true, settings.FilesharingDirectories
			}
		}
	}
	return false, nil
}

func (realSystem) Elevated() bool        { return windows.GetCurrentProcessToken().IsElevated() }
func (realSystem) Now() time.Time        { return time.Now() }
func (realSystem) Sleep(d time.Duration) { time.Sleep(d) }

// migrationMutexName is the machine-wide lock of the migration engine.
const migrationMutexName = `Global\ironflock-device-agent-migrate`

// LockMigration holds a named mutex for the life of the engine run; a
// second engine (the migrate service next to a foreground run) finds it
// held. The handle is closed when the run ends, or by the process's exit.
func (realSystem) LockMigration() (func(), error) {
	name, err := windows.UTF16PtrFromString(migrationMutexName)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateMutex(nil, false, name)
	if err != nil {
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return nil, fmt.Errorf("%s is held", migrationMutexName)
		}
		return nil, fmt.Errorf("creating %s: %w", migrationMutexName, err)
	}
	return func() { _ = windows.CloseHandle(handle) }, nil
}
