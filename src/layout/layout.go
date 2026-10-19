// Package layout decides where the agent keeps its files on this machine.
//
// Linux has two layouts:
//
//	/opt/ironflock/device-agent   current: written by ironflock-init >= 1.4.0, or
//	                              moved there in place by `device-agent migrate-layout`
//	/opt/reagent                  legacy: ironflock-init < 1.4.0, and OS images
//	                              (FlockOS, ReswarmOS, cubic, the Jetson script)
//
// The legacy layout runs the agent from units that pass only -config and
// -appsDir, so the built-in defaults have to follow the layout that is
// actually on disk. A binary that opened the current layout's database under a
// legacy unit would find it empty and remove every app container as an orphan.
//
// Off Linux the same split exists below the home directory ($HOME/reagent
// before, $HOME/ironflock/device-agent now). Windows keeps its legacy paths
// until its own migration; its service passes every path explicitly anyway.
package layout

import (
	"os"
	"path/filepath"
	"strings"
)

// Kind names the layout the agent runs in; it is reported in the heartbeat
// stats as "layout".
type Kind string

const (
	// Current is /opt/ironflock/device-agent.
	Current Kind = "ironflock"
	// Legacy is an /opt/reagent install made by ironflock-init < 1.4.0: its
	// units are links into /opt/reagent/reswarmify/services/, so
	// migrate-layout can move it.
	Legacy Kind = "legacy"
	// LegacyImage is an /opt/reagent layout that an OS image owns (FlockOS,
	// ReswarmOS, cubic, Jetson) or that predates /opt/reagent/reswarmify.
	// It stays as it is: an OS update or the image's own boot scripts would
	// bring the old units back.
	LegacyImage Kind = "legacy-image"
)

// Paths are the defaults of one layout. Flags override every field.
type Paths struct {
	Kind     Kind
	AgentDir string
	Database string
	LogFile  string
	FrpcLog  string
	// BinaryName is what downloaded agent binaries are called in AgentDir:
	// <BinaryName>-v<version>. The legacy supervisor (reagent-manager.sh)
	// activates only files named reagent-v<version>.
	BinaryName string
}

// The Linux layouts.
const (
	AgentDir         = "/opt/ironflock/device-agent"
	LogDir           = "/var/log/ironflock"
	LegacyAgentDir   = "/opt/reagent"
	legacyLogFile    = "/var/log/reagent.log"
	legacyFrpcLog    = "/var/log/frpc.log"
	binaryName       = "device-agent"
	legacyBinaryName = "reagent"
)

// Files that identify a legacy install made by ironflock-init < 1.4.0.
const (
	LegacyUnitLink      = "/etc/systemd/system/reagent.service"
	LegacyUnitFile      = LegacyAgentDir + "/reswarmify/services/reagent.service"
	LegacyManagerLink   = "/etc/systemd/system/reagent-manager.service"
	LegacyEnrolled      = LegacyAgentDir + "/reswarm-mode"
	legacyFlockOSMarker = LegacyAgentDir + "/flock-mode"
	legacyFlockOSUnits  = LegacyAgentDir + "/ironflock/services"
)

// CurrentPaths returns the Linux defaults of the current layout.
func CurrentPaths() Paths {
	return Paths{
		Kind:       Current,
		AgentDir:   AgentDir,
		Database:   filepath.Join(AgentDir, "device-agent.db"),
		LogFile:    filepath.Join(LogDir, "device-agent.log"),
		FrpcLog:    filepath.Join(LogDir, "frpc.log"),
		BinaryName: binaryName,
	}
}

func legacyPaths(kind Kind) Paths {
	return Paths{
		Kind:       kind,
		AgentDir:   LegacyAgentDir,
		Database:   filepath.Join(LegacyAgentDir, "reagent.db"),
		LogFile:    legacyLogFile,
		FrpcLog:    legacyFrpcLog,
		BinaryName: legacyBinaryName,
	}
}

// Detect returns the Linux layout found below root ("" or "/" for the real
// filesystem; tests pass a temporary directory):
//
//   - /opt/ironflock/device-agent is a directory: Current. That includes a
//     migrated device, whose /opt/reagent is then a symlink to it.
//   - /opt/reagent is a real directory: Legacy when ironflock-init < 1.4.0
//     made it, otherwise LegacyImage.
//   - neither: Current, so a fresh install starts in the current layout.
func Detect(root string) Paths {
	if isDir(join(root, AgentDir)) {
		return CurrentPaths()
	}
	info, err := os.Lstat(join(root, LegacyAgentDir))
	if err == nil && info.IsDir() {
		if IsIronflockInitInstall(root) {
			return legacyPaths(Legacy)
		}
		return legacyPaths(LegacyImage)
	}
	return CurrentPaths()
}

// IsIronflockInitInstall reports whether the /opt/reagent below root was laid
// down by ironflock-init < 1.4.0: reagent.service is a link to the unit file in
// /opt/reagent/reswarmify/services/ (images ship real unit files), the device
// is enrolled, and it is not FlockOS (which re-enables its units at every
// boot).
func IsIronflockInitInstall(root string) bool {
	if exists(join(root, legacyFlockOSMarker)) || isDir(join(root, legacyFlockOSUnits)) {
		return false
	}
	if !exists(join(root, LegacyEnrolled)) || !isRegular(join(root, LegacyUnitFile)) {
		return false
	}
	target, err := os.Readlink(join(root, LegacyUnitLink))
	if err != nil {
		return false
	}
	return filepath.Clean(strings.TrimPrefix(target, root)) == LegacyUnitFile
}

// DetectHome returns the layout below a home directory, for the agent off
// Linux: $HOME/reagent when that exists (installs before 1.3.0), else
// $HOME/ironflock/device-agent. On Windows it stays $HOME/reagent until the
// Windows migration.
func DetectHome(home string, goos string) Paths {
	legacyDir := filepath.Join(home, "reagent")
	if goos == "windows" || isDir(legacyDir) {
		return Paths{
			Kind:       Legacy,
			AgentDir:   legacyDir,
			Database:   filepath.Join(legacyDir, "reagent.db"),
			LogFile:    filepath.Join(legacyDir, "reagent.log"),
			FrpcLog:    filepath.Join(legacyDir, "frpc.log"),
			BinaryName: legacyBinaryName,
		}
	}
	dir := filepath.Join(home, "ironflock", "device-agent")
	return Paths{
		Kind:       Current,
		AgentDir:   dir,
		Database:   filepath.Join(dir, "device-agent.db"),
		LogFile:    filepath.Join(dir, "device-agent.log"),
		FrpcLog:    filepath.Join(dir, "frpc.log"),
		BinaryName: binaryName,
	}
}

func join(root, path string) string {
	if root == "" || root == "/" {
		return path
	}
	return filepath.Join(root, path)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func isRegular(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}
