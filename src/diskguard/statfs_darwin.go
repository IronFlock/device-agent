//go:build darwin

package diskguard

import (
	"strconv"

	"golang.org/x/sys/unix"
)

// rootPath is the root filesystem's path. On macOS it is the sealed,
// read-only system volume, which the watch set leaves out; the agent's folder
// brings in the data volume.
func rootPath() string { return "/" }

// statPath reads the filesystem holding path: statfs for its size, mount
// point and flags, stat for its device ID (st_dev).
func statPath(path string) (reading, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return reading{}, err
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		return reading{}, err
	}
	fsType := unix.ByteSliceToString(fs.Fstypename[:])
	bsize := uint64(fs.Bsize)
	r := reading{
		key:      strconv.FormatInt(int64(st.Dev), 10),
		mount:    unix.ByteSliceToString(fs.Mntonname[:]),
		readOnly: fs.Flags&unix.MNT_RDONLY != 0,
		pseudo:   fsType == "devfs" || fsType == "autofs",
		total:    fs.Blocks * bsize,
		used:     (fs.Blocks - fs.Bfree) * bsize,
		free:     fs.Bavail * bsize,
	}
	if fs.Files > 0 && fs.Ffree <= fs.Files {
		r.inodesTotal = fs.Files
		r.inodesFree = fs.Ffree
	}
	return r, nil
}
