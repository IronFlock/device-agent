//go:build windows

package winservice

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc/mgr"
)

// Language-neutral SIDs (localized group names like "Administratoren" would
// break icacls on non-English Windows).
const (
	SidSystem         = "*S-1-5-18"
	SidAdministrators = "*S-1-5-32-544"
	SidUsers          = "*S-1-5-32-545"
)

// CreateOrUpdateService opens the service named by spec or creates it, then
// applies every setting of spec: ImagePath, display name, description, start
// type, recovery actions and the Environment value. A resumed migration and
// a reinstall therefore reach the same state whatever a previous run managed.
// The returned service is open; the caller closes it.
// The registry keys whose hives hold what the agent's services and the repair
// task are: the services (SYSTEM hive) and the task scheduler's cache
// (SOFTWARE hive).
const (
	ServicesKey  = `SYSTEM\CurrentControlSet\Services`
	TaskCacheKey = `SOFTWARE\Microsoft\Windows NT\CurrentVersion\Schedule\TaskCache`
)

var procRegFlushKey = windows.NewLazySystemDLL("advapi32.dll").NewProc("RegFlushKey")

// FlushRegistry writes the hive holding key (below HKEY_LOCAL_MACHINE) to
// disk. The service control manager keeps a service's configuration in the
// registry, which Windows writes back lazily: a power cut seconds after a
// change loses it (seen on the canary device, 2026-10-24) while a flushed
// file survives, so a migration resumed from its journal would meet the
// services as they were before the step. Best effort: an error leaves the
// change as durable as it was without the flush.
func FlushRegistry(key string) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, key, registry.QUERY_VALUE)
	if err != nil {
		return
	}
	defer func() { _ = k.Close() }()
	_, _, _ = procRegFlushKey.Call(uintptr(k))
}

func CreateOrUpdateService(m *mgr.Mgr, spec Spec) (*mgr.Service, error) {
	config := mgr.Config{
		DisplayName:      spec.DisplayName,
		Description:      spec.Description,
		StartType:        startTypeValue(spec.StartType),
		DelayedAutoStart: spec.Delayed,
		ErrorControl:     mgr.ErrorNormal,
	}
	service, err := m.OpenService(spec.Name)
	if err != nil {
		service, err = m.CreateService(spec.Name, spec.Exe, config, spec.Args...)
		if err != nil {
			return nil, fmt.Errorf("creating service %s: %w", spec.Name, err)
		}
	} else {
		current, err := service.Config()
		if err != nil {
			_ = service.Close()
			return nil, fmt.Errorf("reading the configuration of service %s: %w", spec.Name, err)
		}
		current.BinaryPathName = windows.ComposeCommandLine(append([]string{spec.Exe}, spec.Args...))
		current.DisplayName = config.DisplayName
		current.Description = config.Description
		current.StartType = config.StartType
		current.DelayedAutoStart = config.DelayedAutoStart
		current.ErrorControl = config.ErrorControl
		if err := service.UpdateConfig(current); err != nil {
			_ = service.Close()
			return nil, fmt.Errorf("updating service %s: %w", spec.Name, err)
		}
	}

	if len(spec.RestartDelays) > 0 {
		actions := make([]mgr.RecoveryAction, 0, len(spec.RestartDelays))
		for _, seconds := range spec.RestartDelays {
			actions = append(actions, mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: time.Duration(seconds) * time.Second})
		}
		if err := service.SetRecoveryActions(actions, uint32(spec.ResetAfter)); err != nil {
			_ = service.Close()
			return nil, fmt.Errorf("setting the recovery actions of service %s: %w", spec.Name, err)
		}
		if err := service.SetRecoveryActionsOnNonCrashFailures(spec.NonCrashRecovery); err != nil {
			_ = service.Close()
			return nil, fmt.Errorf("setting the recovery flag of service %s: %w", spec.Name, err)
		}
	}

	if spec.Env != nil {
		if err := SetEnvironment(spec.Name, spec.Env); err != nil {
			_ = service.Close()
			return nil, err
		}
	}
	FlushRegistry(ServicesKey)
	return service, nil
}

func startTypeValue(s StartType) uint32 {
	switch s {
	case StartAutomatic:
		return mgr.StartAutomatic
	case StartDisabled:
		return mgr.StartDisabled
	default:
		return mgr.StartManual
	}
}

// StartTypeOf maps an SCM start type back.
func StartTypeOf(value uint32) StartType {
	switch value {
	case mgr.StartAutomatic:
		return StartAutomatic
	case mgr.StartDisabled:
		return StartDisabled
	default:
		return StartManual
	}
}

func serviceKey(name string) string {
	return `SYSTEM\CurrentControlSet\Services\` + name
}

// Environment reads the service's Environment registry value (REG_MULTI_SZ):
// a LocalSystem service only inherits the machine-wide environment, so a
// proxy, or a WSL engine's DOCKER_HOST, lives here. nil when there is none.
func Environment(name string) ([]string, error) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, serviceKey(name), registry.QUERY_VALUE)
	if err != nil {
		return nil, err
	}
	defer func() { _ = key.Close() }()
	values, _, err := key.GetStringsValue("Environment")
	if errors.Is(err, registry.ErrNotExist) {
		return nil, nil
	}
	return values, err
}

// SetEnvironment replaces the service's Environment value (the whole
// REG_MULTI_SZ); an empty list removes it.
func SetEnvironment(name string, env []string) error {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, serviceKey(name), registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("opening the registry key of service %s: %w", name, err)
	}
	defer func() { _ = key.Close() }()
	if len(env) == 0 {
		err = key.DeleteValue("Environment")
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return err
	}
	return key.SetStringsValue("Environment", env)
}

// HardenRoot restricts %ProgramData%\IronFlock (root) to the agent: the
// folder inherits create rights for every user from %ProgramData%, and Go
// ignores 0o700 on Windows, so without this a local user could plant files
// that the service (SYSTEM) executes or renames. Inheritance off; SYSTEM and
// Administrators full control; Users read and execute; owner Administrators.
func HardenRoot(root string) error {
	if err := RunCommand("icacls", root, "/inheritance:r",
		"/grant", SidSystem+":(OI)(CI)F",
		"/grant", SidAdministrators+":(OI)(CI)F",
		"/grant", SidUsers+":(OI)(CI)RX"); err != nil {
		return err
	}
	// A call of its own: icacls refuses /setowner next to /grant
	// ("Invalid parameter").
	return RunCommand("icacls", root, "/setowner", SidAdministrators)
}

// OwnedByAdministrators reports whether SYSTEM or the Administrators group
// owns the entry at path. A pre-existing migration file or agent folder
// owned by anyone else was not made by the agent and is not trusted. A link
// is judged by its own owner, never by its target's: the path is opened
// without following it (FILE_FLAG_OPEN_REPARSE_POINT), so a junction a local
// user made (no privilege needed) is the user's even when it points at a
// folder Administrators own.
func OwnedByAdministrators(path string) (bool, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	// READ_CONTROL reads the owner and takes part in no sharing check;
	// FILE_FLAG_BACKUP_SEMANTICS is what opens a folder.
	handle, err := windows.CreateFile(name, windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return false, fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return false, err
	}
	return owner.IsWellKnown(windows.WinLocalSystemSid) || owner.IsWellKnown(windows.WinBuiltinAdministratorsSid), nil
}

// RunCommand runs one of the fixed Windows tools (icacls, schtasks, certutil,
// powershell) and returns its output in the error.
func RunCommand(name string, args ...string) error {
	output, err := exec.Command(name, args...).CombinedOutput() //nolint:gosec // G204: fixed Windows service/registry tools chosen by the installer
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}
