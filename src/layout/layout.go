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
// before, $HOME/ironflock/device-agent now).
//
// On Windows the layout follows the name of the running executable
// (DetectWindows): a 1.3.x service install runs %ProgramData%\IronFlock\Reagent\
// reagent.exe and updates itself in place, so the first 1.4.0 on such a device
// runs as reagent.exe and must keep the legacy file names (reagent-prev.exe,
// reagent-v<v>.exe) for its own updates and rollbacks until the Windows
// migration (package migrate/winmigrate) renames it. The service passes every
// path explicitly, so the directory defaults here matter only for console runs.
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
	// LegacyConsole is a reagent.exe on Windows that no service supervises
	// (a console run, FlockFlasher's test device). It cannot move by itself:
	// the Windows migration needs the legacy service to replace.
	LegacyConsole Kind = "legacy-console"
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

// DetectHome returns the layout below a home directory, for the agent on
// macOS: $HOME/reagent when that exists (installs before 1.3.0), else
// $HOME/ironflock/device-agent.
func DetectHome(home string) Paths {
	if isDir(filepath.Join(home, "reagent")) {
		return homePaths(home, Legacy)
	}
	return homePaths(home, Current)
}

// LegacyExeName is the executable of a Windows install before 1.4.0; every
// other name is the current layout.
const LegacyExeName = "reagent.exe"

// DetectWindows returns the Windows layout, decided from the name of the
// running executable (exeName, the base name): reagent.exe is the legacy
// layout, anything else the current one. service says whether the service
// control manager started the process; a legacy executable without one is
// LegacyConsole. The directories are the console defaults below home; the
// service passes every path explicitly.
func DetectWindows(home string, exeName string, service bool) Paths {
	if !strings.EqualFold(exeName, LegacyExeName) {
		return homePaths(home, Current)
	}
	if service {
		return homePaths(home, Legacy)
	}
	return homePaths(home, LegacyConsole)
}

func homePaths(home string, kind Kind) Paths {
	if kind == Current {
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
	legacyDir := filepath.Join(home, "reagent")
	return Paths{
		Kind:       kind,
		AgentDir:   legacyDir,
		Database:   filepath.Join(legacyDir, "reagent.db"),
		LogFile:    filepath.Join(legacyDir, "reagent.log"),
		FrpcLog:    filepath.Join(legacyDir, "frpc.log"),
		BinaryName: legacyBinaryName,
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
