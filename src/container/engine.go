package container

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
)

// EngineInfo is what the device reports about the Docker engine that runs its
// apps. An empty field is not known.
type EngineInfo struct {
	// Platform is the engine's default platform as an OCI platform string:
	// linux/amd64, linux/arm64, linux/arm/v7, linux/arm/v6, linux/arm/v8 (a
	// 32-bit engine on an ARMv8 CPU), or linux/arm when the variant cannot be
	// told. What an app image must match is this, not the architecture the
	// agent was built for.
	Platform string
	// Version is the engine's version, e.g. 27.0.3.
	Version string
}

// readCPUInfo reads the CPU description the engine reads for its variant:
// /proc/cpuinfo of the kernel the agent shares with a Linux engine. A seam
// for the tests.
var readCPUInfo = func() ([]byte, error) {
	if runtime.GOOS != "linux" {
		return nil, errors.New("no /proc/cpuinfo on " + runtime.GOOS)
	}
	return os.ReadFile("/proc/cpuinfo")
}

// EngineInfo asks the engine for its platform and version. On Windows and
// macOS the engine runs in a VM, and what it answers is what counts, never
// the host's architecture. The caller treats an error as "not known yet".
//
// Its own requests end with ctx. It is not strictly bounded by ctx, though:
// the Docker client negotiates its API version once, under a lock it holds
// while it pings with the context of the call that negotiates. While another
// call without a deadline is stuck in that ping (a daemon whose /_ping
// stalls), EngineInfo waits for that lock beyond its own timeout. Callers run
// it in the background, where that costs one waiting goroutine.
func (docker *Docker) EngineInfo(ctx context.Context) (EngineInfo, error) {
	version, err := docker.client.ServerVersion(ctx)
	if err != nil {
		return EngineInfo{}, err
	}

	var machine string
	var cpuinfo []byte
	if strings.EqualFold(version.Arch, "arm") {
		// The Engine API names no ARM variant. The engine takes it from the
		// CPU (/proc/cpuinfo), and from its kernel's machine name (/info)
		// where that says nothing. This machine's CPU is the engine's only
		// when the engine runs here (a unix socket); for an engine elsewhere
		// (DOCKER_HOST=tcp://, ssh://) only its own answer counts.
		if engineIsLocal(docker.client.DaemonHost()) {
			cpuinfo, _ = readCPUInfo()
		}
		if info, err := docker.client.Info(ctx); err == nil {
			machine = info.Architecture
		}
	}

	return EngineInfo{
		Platform: enginePlatform(version.Os, version.Arch, machine, cpuinfo),
		Version:  strings.TrimSpace(version.Version),
	}, nil
}

// engineIsLocal reports whether the engine at host (DOCKER_HOST's form) runs
// on this machine: a unix socket or a Windows named pipe.
func engineIsLocal(host string) bool {
	return strings.HasPrefix(host, "unix://") || strings.HasPrefix(host, "npipe://")
}

// enginePlatform is the engine's default platform from its OS and GOARCH
// (/version), and for 32-bit ARM the variant the way the engine derives it
// for itself (containerd's platforms package, getCPUVariant): the "CPU
// architecture" of /proc/cpuinfo, with the Raspberry Pi ARMv6 that reports 7,
// else the machine name (/info Architecture: armv7l, armv6l, aarch64). An
// unknown variant is left out rather than guessed; a 32-bit engine on an
// ARMv8 CPU stays v8, which the platform maps.
func enginePlatform(osName, arch, machine string, cpuinfo []byte) string {
	osName = strings.ToLower(strings.TrimSpace(osName))
	arch = strings.ToLower(strings.TrimSpace(arch))
	if osName == "" || arch == "" {
		return ""
	}
	if arch != "arm" {
		return osName + "/" + arch
	}
	if variant := armVariant(machine, cpuinfo); variant != "" {
		return osName + "/arm/" + variant
	}
	return osName + "/arm"
}

// armVariant is the ARM variant (v3 to v8) of a 32-bit engine, or "".
func armVariant(machine string, cpuinfo []byte) string {
	variant, found := cpuInfoField(cpuinfo, "CPU architecture")
	if found {
		// A Raspberry Pi ARMv6 reports 7 (a kernel quirk).
		if variant == "7" {
			if model, ok := cpuInfoField(cpuinfo, "model name"); ok && strings.HasPrefix(strings.ToLower(model), "armv6-compatible") {
				variant = "6"
			}
		}
	} else {
		variant = machineVariant(machine)
	}

	switch strings.ToLower(variant) {
	case "8", "aarch64":
		return "v8"
	case "7", "7m", "?(12)", "?(13)", "?(14)", "?(15)", "?(16)", "?(17)":
		return "v7"
	case "6", "6tej":
		return "v6"
	case "5", "5t", "5te", "5tej":
		return "v5"
	case "4", "4t":
		return "v4"
	case "3":
		return "v3"
	default:
		return ""
	}
}

// machineVariant is the variant a kernel's machine name (uname -m) says.
func machineVariant(machine string) string {
	machine = strings.ToLower(strings.TrimSpace(machine))
	if machine == "aarch64" || machine == "arm64" {
		return "8"
	}
	if strings.HasPrefix(machine, "armv") && len(machine) >= 5 {
		return machine[4:5]
	}
	return ""
}

// cpuInfoField is the value of the first line of /proc/cpuinfo that names
// field (case-insensitive), as the engine reads it: the first core is enough.
func cpuInfoField(cpuinfo []byte, field string) (string, bool) {
	scanner := bufio.NewScanner(bytes.NewReader(cpuinfo))
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), ":")
		if ok && strings.EqualFold(strings.TrimSpace(key), field) {
			return strings.TrimSpace(value), true
		}
	}
	return "", false
}
