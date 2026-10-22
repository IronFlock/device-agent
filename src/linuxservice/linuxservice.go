// Package linuxservice holds the systemd unit the agent runs under on Linux
// and the repair script it starts with, and writes them. `device-agent service
// install` (used by ironflock-init) and `device-agent migrate-layout` both go
// through it, so a fresh install and a migrated device run the same unit.
package linuxservice

import (
	"fmt"
	"os"
	"path/filepath"
	"reagent/layout"
	"strings"
)

const (
	UnitName = "ironflock-device-agent.service"
	unitDir  = "/etc/systemd/system"
)

// UnitPath is where the unit file lives; DropInDir holds its drop-ins (the
// proxy environment, for instance).
var (
	UnitPath  = filepath.Join(unitDir, UnitName)
	DropInDir = UnitPath + ".d"
)

// Paths name the files the unit refers to.
type Paths struct {
	AgentDir string
	AppsDir  string
	LogFile  string
	// ExtraArgs follow the paths on the command line (a migrated install
	// keeps the flags its legacy unit passed, e.g. -env or -nmw).
	ExtraArgs []string
}

// DefaultPaths are the current layout's.
func DefaultPaths() Paths {
	return Paths{AgentDir: layout.AgentDir, AppsDir: "/apps", LogFile: layout.CurrentPaths().LogFile}
}

func (p Paths) Binary() string   { return filepath.Join(p.AgentDir, "device-agent") }
func (p Paths) Config() string   { return filepath.Join(p.AgentDir, "device-config.flock") }
func (p Paths) Database() string { return filepath.Join(p.AgentDir, "device-agent.db") }
func (p Paths) Enrolled() string { return filepath.Join(p.AgentDir, "enrolled") }
func (p Paths) Repair() string   { return filepath.Join(p.AgentDir, "repair.sh") }

// Unit renders the unit file.
//
//   - StartLimitIntervalSec=0: the start limit would otherwise leave the unit
//     dead for good after a few quick restarts — exactly what a rollback after
//     a crashing update looks like.
//   - SuccessExitStatus=75: the agent's deliberate exit-for-restart
//     (activation, remote restart) is not logged as a failure.
//   - RestartSteps/RestartMaxDelaySec need systemd >= 254 and are ignored
//     (with a warning) by older ones.
//   - No PrivateTmp: the single-instance locks live in /tmp.
func (p Paths) Unit() string {
	return strings.Join([]string{
		"[Unit]",
		"Description=IronFlock Device Agent",
		"Wants=network-online.target",
		"After=network-online.target docker.service",
		"ConditionPathExists=" + p.Enrolled(),
		"StartLimitIntervalSec=0",
		"",
		"[Service]",
		"Type=simple",
		"Environment=IRONFLOCK_AGENT_SUPERVISOR=systemd",
		"RuntimeDirectory=ironflock-device-agent",
		"ExecStartPre=-" + p.Repair(),
		strings.TrimSpace(fmt.Sprintf("ExecStart=%s -config %s -agentDir %s -appsDir %s -dbFileName %s -logFile %s %s",
			p.Binary(), p.Config(), p.AgentDir, p.AppsDir, p.Database(), p.LogFile, strings.Join(p.ExtraArgs, " "))),
		"Restart=always",
		"RestartSec=5",
		"RestartSteps=3",
		"RestartMaxDelaySec=120",
		"SuccessExitStatus=75",
		"TimeoutStopSec=30",
		"CPUWeight=2048",
		"Nice=-16",
		"",
		"[Install]",
		"WantedBy=multi-user.target",
		"",
	}, "\n")
}

// RepairScript renders the ExecStartPre script. A power cut during an update
// can leave the active binary missing or truncated; the agent's own rollback
// cannot help then, because it never runs. The script puts the newest binary
// that starts back in place and never fails the unit itself.
func (p Paths) RepairScript() string {
	return fmt.Sprintf(`#!/bin/sh
# ExecStartPre of %[1]s: make sure the agent binary starts.
dir=%[2]s
active="$dir/device-agent"
works() { [ -x "$1" ] && "$1" -version >/dev/null 2>&1; }
works "$active" && exit 0
# Last resort, a version that failed its probation: one that starts beats none.
for candidate in "$dir/device-agent-prev" $(ls -t "$dir"/device-agent-v* 2>/dev/null) $(ls -t "$dir"/device-agent-failed-v* 2>/dev/null); do
	if works "$candidate"; then
		cp -f "$candidate" "$active.repair" && mv -f "$active.repair" "$active" && sync
		echo "repair: restored $active from $candidate" >&2
		exit 0
	fi
done
echo "repair: no working agent binary in $dir" >&2
exit 0
`, UnitName, p.AgentDir)
}

// WriteFiles writes the unit and the repair script below root ("" for the
// real filesystem). Each file is written to a temporary name and renamed, so
// a power cut leaves the old or the new file, never half of one.
func WriteFiles(root string, p Paths) error {
	if err := os.MkdirAll(under(root, p.AgentDir), 0o755); err != nil {
		return err
	}
	if err := WriteFileAtomic(under(root, p.Repair()), []byte(p.RepairScript()), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(under(root, unitDir), 0o755); err != nil {
		return err
	}
	return WriteFileAtomic(under(root, UnitPath), []byte(p.Unit()), 0o644)
}

// WriteFileAtomic writes data to path through a synced temporary file.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
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
	if err == nil {
		err = os.Chmod(tmp, mode)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func under(root, path string) string {
	if root == "" || root == "/" {
		return path
	}
	return filepath.Join(root, path)
}
