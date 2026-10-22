//go:build !windows

package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// singleInstanceLockFiles are held open (and flock'ed) for the process
// lifetime; the kernel releases the locks on any exit, including SIGKILL.
var singleInstanceLockFiles []*os.File

// singleInstanceLockPaths: the current name, and the one agents before 1.3.0
// take. Holding both keeps an old and a new agent from running side by side
// while a device moves between them.
var singleInstanceLockPaths = []string{"/tmp/ironflock-device-agent.lock", "/tmp/ironflock-reagent.lock"}

// errAgentAlreadyRunning mirrors the Windows message: the most likely holder
// of the lock is the installed system service.
func errAgentAlreadyRunning() error {
	return fmt.Errorf("another device agent is already running on this machine (if it runs as a service, stop it with: systemctl stop ironflock-device-agent, or reagent on older installs)")
}

// acquireSingleInstanceLock refuses a second agent on the machine: two agents
// treat each other's containers as orphans and delete them
// (apps.CleanupOrphanedContainers) — the same hazard the Global\ mutex guards
// against on Windows (platform_windows.go). Self-update is unaffected: the old
// process exits — releasing the locks — before systemd starts the new one.
func acquireSingleInstanceLock() error {
	for _, path := range singleInstanceLockPaths {
		f, err := lockFile(path)
		if err != nil {
			for _, held := range singleInstanceLockFiles {
				_ = held.Close()
			}
			singleInstanceLockFiles = nil
			return err
		}
		singleInstanceLockFiles = append(singleInstanceLockFiles, f)
	}
	return nil
}

// lockFile opens and exclusively flocks path, returning the open file.
func lockFile(path string) (*os.File, error) {
	for {
		// O_RDONLY is enough for flock(2) and keeps the file openable by any
		// user regardless of which user created it (root service vs console).
		f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o644) //nolint:gosec // G302: lock file, no content
		if err != nil {
			return nil, fmt.Errorf("cannot open the machine-wide agent lock %s: %w", path, err)
		}

		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == unix.EWOULDBLOCK {
			_ = f.Close()
			return nil, errAgentAlreadyRunning()
		}
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("cannot lock the machine-wide agent lock %s: %w", path, err)
		}

		// The path may have been unlinked or replaced between open and flock
		// (systemd-tmpfiles aging, an operator's rm): a lock on an unlinked
		// inode guards nothing, so verify the locked fd is still what the
		// path names and start over if not.
		var fdStat, pathStat unix.Stat_t
		if unix.Fstat(int(f.Fd()), &fdStat) == nil &&
			unix.Stat(path, &pathStat) == nil &&
			fdStat.Ino == pathStat.Ino && fdStat.Dev == pathStat.Dev {
			return f, nil
		}
		_ = f.Close()
	}
}

// setupProcessJobObject puts the process into a kill-on-close Windows job
// object so children (docker compose CLI) die with the agent. On Linux the
// same is achieved per-child via Pdeathsig (container/compose_pdeathsig_linux.go).
func setupProcessJobObject() {
}
