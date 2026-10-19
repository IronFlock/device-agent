//go:build linux

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reagent/layout"
	"reagent/linuxservice"
)

// runServiceCommand handles `device-agent service <verb>` on Linux:
//
//	install    put the binary, unit and repair script in place and enable the
//	           unit (ironflock-init runs it with -config and -start)
//	uninstall  stop and disable the unit and remove its files; the agent's data
//	           (database, apps, Docker data) stays
//	start | stop | restart | status   systemctl on the unit
func runServiceCommand(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: device-agent service install|uninstall|start|stop|restart|status")
		return 2
	}
	var err error
	switch args[0] {
	case "install":
		err = linuxServiceInstall(args[1:])
	case "uninstall":
		err = linuxServiceUninstall()
	case "start", "stop", "restart", "status":
		err = systemctl(args[0], linuxservice.UnitName)
	default:
		fmt.Fprintf(os.Stderr, "unknown service command %q\n", args[0])
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func linuxServiceInstall(args []string) error {
	flags := flag.NewFlagSet("service install", flag.ContinueOnError)
	configPath := flags.String("config", "", "device .flock to enrol with; copied to <agentDir>/device-config.flock (mode 0600)")
	agentDir := flags.String("agentDir", layout.AgentDir, "agent folder")
	appsDir := flags.String("appsDir", "/apps", "apps folder")
	start := flags.Bool("start", false, "start (or restart) the agent once installed")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return errors.New("service install must run as root")
	}
	// A legacy reagent.service on the same machine would run a second agent
	// on the old paths; that device is moved with migrate-layout instead.
	if _, err := os.Lstat(layout.LegacyUnitLink); err == nil {
		return fmt.Errorf("%s exists: this device runs the legacy layout; move it with `device-agent migrate-layout`", layout.LegacyUnitLink)
	}

	paths := linuxservice.Paths{AgentDir: *agentDir, AppsDir: *appsDir, LogFile: layout.CurrentPaths().LogFile}
	if err := os.MkdirAll(paths.AgentDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(paths.LogFile), 0o755); err != nil {
		return err
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}
	if filepath.Clean(self) != paths.Binary() {
		if err := copyFileAtomic(self, paths.Binary(), 0o755); err != nil {
			return fmt.Errorf("installing the binary: %w", err)
		}
	}

	if *configPath != "" {
		source, err := filepath.Abs(*configPath)
		if err != nil {
			return err
		}
		if source != paths.Config() {
			if err := copyFileAtomic(source, paths.Config(), 0o600); err != nil {
				return fmt.Errorf("installing the device configuration: %w", err)
			}
		}
	}

	if err := linuxservice.WriteFiles("", paths); err != nil {
		return err
	}
	// The unit only starts once the device has a configuration to enrol with.
	if _, err := os.Stat(paths.Config()); err == nil {
		if err := linuxservice.WriteFileAtomic(paths.Enrolled(), nil, 0o644); err != nil {
			return err
		}
	}

	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	if err := systemctl("enable", linuxservice.UnitName); err != nil {
		return err
	}
	if *start {
		return systemctl("restart", linuxservice.UnitName)
	}
	return nil
}

func linuxServiceUninstall() error {
	if os.Geteuid() != 0 {
		return errors.New("service uninstall must run as root")
	}
	// Failing to stop or disable a unit that is not loaded must not stop the
	// clean-up of its files.
	_ = systemctl("disable", "--now", linuxservice.UnitName)
	paths := linuxservice.DefaultPaths()
	for _, path := range []string{linuxservice.UnitPath, paths.Repair()} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return systemctl("daemon-reload")
}

func systemctl(args ...string) error {
	cmd := exec.Command("systemctl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("systemctl %v: %w", args, err)
	}
	return nil
}

// copyFileAtomic copies src to dst through a synced temporary file next to
// dst, so dst is either the old or the complete new file.
func copyFileAtomic(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(tmp, mode)
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if dir, err := os.Open(filepath.Dir(dst)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
