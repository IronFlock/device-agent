//go:build windows

package diskguard

import (
	"io/fs"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

// rootPath is the system drive's root (C:\ as a rule).
func rootPath() string {
	if drive := os.Getenv("SystemDrive"); drive != "" {
		return drive + `\`
	}
	return `C:\`
}

// statPath reads the volume holding path. Windows has no device IDs and no
// inodes: the volume's GUID path is the dedup key, and the inode fields stay 0.
// On Windows the guard only reports (see agent.go); it never cleans or enters
// an emergency there.
func statPath(path string) (reading, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return reading{}, err
	}
	var avail, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &totalFree); err != nil {
		return reading{}, err
	}
	buf := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumePathName(p, &buf[0], uint32(len(buf))); err != nil {
		return reading{}, err
	}
	mount := windows.UTF16ToString(buf)
	r := reading{
		key:   strings.ToLower(mount),
		mount: mount,
		total: total,
		used:  total - totalFree,
		free:  avail,
	}
	m, err := windows.UTF16PtrFromString(mount)
	if err != nil {
		return r, nil
	}
	volume := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumeNameForVolumeMountPoint(m, &volume[0], uint32(len(volume))); err == nil {
		r.key = windows.UTF16ToString(volume)
	}
	var flags uint32
	if err := windows.GetVolumeInformation(m, nil, 0, nil, nil, &flags, nil, 0); err == nil {
		r.readOnly = flags&windows.FILE_READ_ONLY_VOLUME != 0
	}
	return r, nil
}

// sameDevice is never asked on Windows: the file cleanups are Linux-only.
func sameDevice(a, b fs.FileInfo) bool { return true }
