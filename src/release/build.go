package release

import (
	"context"
	_ "embed"
	"runtime"
	"strings"

	"github.com/shirou/gopsutil/v4/host"
)

//go:embed version.txt
var version string
var BuildArch string = ""

// Test builds only, set with -ldflags -X (scripts/build.sh EXTRA_LDFLAGS;
// no release build sets them): versionOverride replaces version.txt, and
// TestFault makes a deliberately broken agent for device-init's end-to-end
// test of update rollbacks ("crash": exits at start; "offline": never
// reaches IronFlock).
var (
	versionOverride string
	TestFault       string
)

// SystemInfo holds architecture and detailed OS information about the host system.
type SystemInfo struct {
	OS         string // runtime.GOOS: "linux", "darwin", "windows"
	Arch       string // CPU architecture: "amd64", "arm64", "arm"
	Variant    string // ARM variant: "", "v7", "v6", "v5"
	DetailedOS string // Rich OS string, e.g. "linux debian 13.3 6.1.0-28-amd64"
}

// GetSystemInfo returns architecture details and a rich OS description.
// It never panics — on any failure the DetailedOS falls back to runtime.GOOS.
func GetSystemInfo() SystemInfo {
	info := SystemInfo{
		OS: runtime.GOOS,
	}

	arch := BuildArch
	if arch == "" {
		arch = runtime.GOARCH
	}

	if strings.Contains(arch, "arm") {
		splitArmArch := strings.Split(arch, "v")
		if len(splitArmArch) == 2 {
			info.Variant = "v" + splitArmArch[1]
			arch = "arm"
		}
	}
	info.Arch = arch

	info.DetailedOS = getDetailedOS(info.OS)

	return info
}

// getDetailedOS builds a rich OS description string using gopsutil.
// Returns at minimum runtime.GOOS; never panics.
func getDetailedOS(goos string) (result string) {
	result = goos

	defer func() {
		if r := recover(); r != nil {
			result = goos
		}
	}()

	hostInfo, err := host.InfoWithContext(context.Background())
	if err != nil {
		return goos
	}

	parts := []string{goos}
	if hostInfo.Platform != "" && hostInfo.Platform != goos {
		parts = append(parts, hostInfo.Platform)
	}
	if hostInfo.PlatformVersion != "" {
		parts = append(parts, hostInfo.PlatformVersion)
	}
	if hostInfo.KernelVersion != "" {
		parts = append(parts, hostInfo.KernelVersion)
	}

	return strings.Join(parts, " ")
}

func GetVersion() string {
	if versionOverride != "" {
		return versionOverride
	}
	return strings.TrimSpace(version)
}

func GetBuildArch() string {
	if BuildArch == "" {
		info := GetSystemInfo()
		BuildArch = info.Arch + info.Variant
	}
	return BuildArch
}
