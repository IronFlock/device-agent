package linuxservice

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The agent must stay reachable when apps use up the device's CPU or memory.
// Its unit therefore runs in its own top-level slice with a raised CPU weight
// and a memory protection (docs/AGENT-RESOURCE-PROTECTION.md). The settings
// live in two files the agent writes at every start, so that a device gets
// them with an update, not only with a fresh install: the slice, and a drop-in
// of the unit it runs under.
const (
	// LegacyUnitName is the unit of installs before 1.3.0.
	LegacyUnitName = "reagent.service"

	SliceName = "ironflock.slice"
	// ResourcesDropInName sorts before any drop-in an operator adds by hand
	// (systemctl edit writes override.conf), so a local change wins.
	ResourcesDropInName = "10-ironflock-resources.conf"

	// CPUWeight against the default 100 of system.slice, user.slice and every
	// app container: under contention the agent gets what it needs.
	CPUWeight = 2048
	// MemoryMinMiB is the memory the kernel never reclaims from the slice:
	// the agent and its tunnel client, measured at 59 MB peak, with headroom.
	MemoryMinMiB = 96
)

// SlicePath is where the slice lives.
var SlicePath = filepath.Join(unitDir, SliceName)

// IsAgentUnit reports whether unit is one the agent runs under.
func IsAgentUnit(unit string) bool {
	return unit == UnitName || unit == LegacyUnitName
}

// ResourcesDropInPath is the drop-in of the given unit.
func ResourcesDropInPath(unit string) string {
	return filepath.Join(unitDir, unit+".d", ResourcesDropInName)
}

// SliceUnit renders the slice. The protection sits here, not on the service:
// the agent's program code is charged to the cgroup that first loaded it,
// which is an earlier run of the unit (each start gets a new cgroup, the
// self-update writes the binary from the old one). With the cgroup mount
// option memory_recursiveprot the slice's protection covers those too.
func SliceUnit() string {
	return strings.Join([]string{
		"[Unit]",
		"Description=IronFlock device agent",
		"",
		"[Slice]",
		fmt.Sprintf("CPUWeight=%d", CPUWeight),
		fmt.Sprintf("MemoryMin=%dM", MemoryMinMiB),
		"",
	}, "\n")
}

// ResourcesDropIn renders the unit's drop-in. unitProtectsMemory adds the
// protection to the unit itself, for kernels without memory_recursiveprot
// (before 5.7), where a slice's protection does not reach its children.
//
//   - Nice=-16 and CPUWeight= repeat the unit's own lines, so the legacy unit
//     gets them too.
//   - ManagedOOMPreference=avoid keeps systemd-oomd (desktop Ubuntu) from
//     picking the agent's cgroup under pressure.
func ResourcesDropIn(unitProtectsMemory bool) string {
	lines := []string{
		"# Written by the IronFlock device agent at every start. Local changes go",
		"# into a drop-in that sorts after this one.",
		"[Service]",
		"Slice=" + SliceName,
		fmt.Sprintf("CPUWeight=%d", CPUWeight),
		"Nice=-16",
		"ManagedOOMPreference=avoid",
	}
	if unitProtectsMemory {
		lines = append(lines, fmt.Sprintf("MemoryMin=%dM", MemoryMinMiB))
	}
	return strings.Join(append(lines, ""), "\n")
}

// CurrentUnit returns the systemd service the process runs under, from the
// content of /proc/self/cgroup: the "0::" line on cgroup v2, the name=systemd
// hierarchy on v1. "" when the leaf is not a service (a console run in a
// session scope, a container).
func CurrentUnit(cgroupContent string) string {
	var v1 string
	for _, line := range strings.Split(cgroupContent, "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		leaf := filepath.Base(parts[2])
		if !strings.HasSuffix(leaf, ".service") {
			continue
		}
		switch parts[1] {
		case "":
			return leaf
		case "name=systemd":
			v1 = leaf
		}
	}
	return v1
}

// ReadCurrentUnit is CurrentUnit for this process.
func ReadCurrentUnit() string {
	content, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	return CurrentUnit(string(content))
}

// MemoryRecursiveProt reports, from the content of /proc/mounts, whether the
// cgroup v2 hierarchy at /sys/fs/cgroup is mounted with memory_recursiveprot.
// A hybrid mount (cgroup2 at /sys/fs/cgroup/unified, memory on v1) has no
// memory.min at all and reports false.
func MemoryRecursiveProt(mountsContent string) bool {
	for _, line := range strings.Split(mountsContent, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[1] != "/sys/fs/cgroup" || fields[2] != "cgroup2" {
			continue
		}
		for _, option := range strings.Split(fields[3], ",") {
			if option == "memory_recursiveprot" {
				return true
			}
		}
	}
	return false
}

// ReadMemoryRecursiveProt is MemoryRecursiveProt for this host.
func ReadMemoryRecursiveProt() bool {
	content, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}
	return MemoryRecursiveProt(string(content))
}

// WriteResourceFiles writes the slice and the unit's drop-in below root (""
// for the real filesystem), the slice first. A file whose content is already
// current is left alone, so the caller reloads systemd only when the result
// says something changed.
func WriteResourceFiles(root, unit string, unitProtectsMemory bool) (changed bool, err error) {
	files := []struct {
		path    string
		content string
	}{
		{SlicePath, SliceUnit()},
		{ResourcesDropInPath(unit), ResourcesDropIn(unitProtectsMemory)},
	}
	for _, file := range files {
		dst := under(root, file.path)
		current, readErr := os.ReadFile(dst)
		if readErr == nil && string(current) == file.content {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return changed, err
		}
		if err := WriteFileAtomic(dst, []byte(file.content), 0o644); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}

// RemoveResourceFiles removes what WriteResourceFiles wrote. The drop-in
// directory goes too when nothing else is in it.
func RemoveResourceFiles(root, unit string) error {
	for _, path := range []string{SlicePath, ResourcesDropInPath(unit)} {
		if err := os.Remove(under(root, path)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	_ = os.Remove(under(root, filepath.Dir(ResourcesDropInPath(unit))))
	return nil
}
