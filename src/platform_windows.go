//go:build windows

package main

import (
	"fmt"
	"unsafe"

	"github.com/rs/zerolog/log"
	"golang.org/x/sys/windows"
)

// singleInstanceMutex is held for the process lifetime; never closed.
var singleInstanceMutex windows.Handle //nolint:unused // write-only on purpose: documents that the handle is held, never closed

// errAgentAlreadyRunning is the operator-facing message shown when another
// agent already holds the machine-wide lock — most often the installed
// LocalSystem service. This is exactly what blocks FlockFlasher's
// "Test Device" console run on a machine that already runs the service.
func errAgentAlreadyRunning() error {
	return fmt.Errorf("another reagent instance is already running on this machine (if it runs as a service, stop it with: reagent service stop)")
}

// acquireSingleInstanceLock refuses a second agent on the machine. The mutex
// lives in the Global\ namespace so it spans session 0 (the service) and
// interactive sessions: a leftover console agent and the service would
// otherwise treat each other's containers as orphans and delete them
// (apps.CleanupOrphanedContainers). This deliberately blocks FlockFlasher's
// "Test Device" console agent on machines that run the service — intended.
func acquireSingleInstanceLock() error {
	name, err := windows.UTF16PtrFromString(`Global\ironflock-reagent`)
	if err != nil {
		return err
	}

	// The x/sys wrapper reports the create outcome in err even when it also
	// returns a valid handle (ERROR_ALREADY_EXISTS).
	handle, err := windows.CreateMutex(nil, false, name)
	switch err {
	case nil:
		singleInstanceMutex = handle
		return nil

	case windows.ERROR_ALREADY_EXISTS:
		// The mutex exists and we could open it — another instance owns it.
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
		return errAgentAlreadyRunning()

	case windows.ERROR_ACCESS_DENIED:
		// CreateMutex denies access in two situations: (a) the mutex already
		// exists but was created by a more-privileged principal (the
		// LocalSystem service in session 0) whose object DACL does not grant
		// this — even elevated — session MUTEX_ALL_ACCESS; or (b) this
		// process genuinely lacks SeCreateGlobalPrivilege to create a Global\
		// object at all (a non-elevated console run with no service present).
		// Probe with OpenMutex to tell them apart: only ERROR_FILE_NOT_FOUND
		// proves the object is truly absent. This is the Test Device case —
		// map (a) to the same clear "already running" guidance instead of
		// leaking a raw "access is denied".
		probe, oerr := windows.OpenMutex(windows.SYNCHRONIZE, false, name)
		if oerr != windows.ERROR_FILE_NOT_FOUND {
			if probe != 0 {
				_ = windows.CloseHandle(probe)
			}
			return errAgentAlreadyRunning()
		}
		return fmt.Errorf("cannot create the machine-wide agent lock: access denied — run the agent from an elevated (Administrator) context")

	default:
		return fmt.Errorf("failed to create single-instance mutex: %w", err)
	}
}

// setupProcessJobObject puts the agent into a kill-on-close job object so
// every child process (docker compose CLI invocations) dies with the agent —
// the Windows equivalent of the Pdeathsig used on Linux
// (container/compose_pdeathsig_linux.go). Failures are logged, not fatal:
// the agent works without it, children just may outlive a hard kill.
func setupProcessJobObject() {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		log.Error().Err(err).Msg("failed to create job object for child process cleanup")
		return
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	_, err = windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), //nolint:gosec // G103: Win32 API call
		uint32(unsafe.Sizeof(info)),
	)
	if err != nil {
		log.Error().Err(err).Msg("failed to configure job object for child process cleanup")
		_ = windows.CloseHandle(job)
		return
	}

	err = windows.AssignProcessToJobObject(job, windows.CurrentProcess())
	if err != nil {
		log.Error().Err(err).Msg("failed to assign the agent to its job object")
		_ = windows.CloseHandle(job)
		return
	}
	// The job handle is deliberately kept open for the process lifetime;
	// closing it would kill the whole job (including this process).
}
