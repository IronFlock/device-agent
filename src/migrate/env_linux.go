//go:build linux

package migrate

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// SystemEnv is the Env of the running machine.
func SystemEnv(version string, logf func(format string, args ...any)) Env {
	return Env{
		Systemctl:      systemctl,
		MainPID:        mainPID,
		UnitLoaded:     unitLoaded,
		MountPoints:    mountPoints,
		SameFilesystem: sameFilesystem,
		Unmount:        func(path string) error { return syscall.Unmount(path, syscall.MNT_DETACH) },
		Now:            time.Now,
		Sleep:          time.Sleep,
		Version:        version,
		Fault:          os.Getenv(FaultEnv),
		Pause:          os.Getenv(PauseEnv),
		Logf:           logf,
	}
}

func systemctl(args ...string) error {
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func mainPID(unit string) (int, error) {
	out, err := exec.Command("systemctl", "show", "--property=MainPID", "--value", unit).Output()
	if err != nil {
		return 0, fmt.Errorf("systemctl show %s: %w", unit, err)
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

func unitLoaded(unit string) bool {
	out, err := exec.Command("systemctl", "show", "--property=LoadState", "--value", unit).Output()
	return err == nil && strings.TrimSpace(string(out)) == "loaded"
}

// mountPoints reads the mount points from /proc/self/mountinfo (field 5,
// with the kernel's octal escapes for spaces and the like).
func mountPoints() ([]string, error) {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var mounts []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			continue
		}
		mounts = append(mounts, unescapeMount(fields[4]))
	}
	return mounts, scanner.Err()
}

func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func sameFilesystem(a, b string) (bool, error) {
	var sa, sb syscall.Stat_t
	if err := syscall.Stat(a, &sa); err != nil {
		return false, err
	}
	if err := syscall.Stat(b, &sb); err != nil {
		return false, err
	}
	return sa.Dev == sb.Dev, nil
}
