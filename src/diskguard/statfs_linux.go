//go:build linux

package diskguard

import (
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

// rootPath is the root filesystem's path.
func rootPath() string { return "/" }

// pseudoMagics are the f_type values of filesystems that hold no disk space:
// kernel interfaces and memory-backed mounts. How full they are says nothing
// about the disk, and nothing the guard does frees them.
var pseudoMagics = map[uint32]bool{
	unix.PROC_SUPER_MAGIC:    true,
	unix.SYSFS_MAGIC:         true,
	unix.TMPFS_MAGIC:         true, // tmpfs, and devtmpfs on current kernels
	unix.RAMFS_MAGIC:         true,
	unix.DEVPTS_SUPER_MAGIC:  true,
	unix.CGROUP_SUPER_MAGIC:  true,
	unix.CGROUP2_SUPER_MAGIC: true,
	unix.DEBUGFS_MAGIC:       true,
	unix.TRACEFS_MAGIC:       true,
	unix.SECURITYFS_MAGIC:    true,
	unix.PSTOREFS_MAGIC:      true,
	unix.BPF_FS_MAGIC:        true,
	unix.EFIVARFS_MAGIC:      true,
	unix.NSFS_MAGIC:          true,
	unix.AUTOFS_SUPER_MAGIC:  true,
	unix.HUGETLBFS_MAGIC:     true,
}

// statPath reads the filesystem holding path: statfs for its size, stat for
// its device ID (st_dev), and the mount point found by walking up from path
// while the device stays the same.
func statPath(path string) (reading, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return reading{}, err
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		return reading{}, err
	}
	// uint64 casts keep this correct across arches (field types differ).
	bsize := uint64(fs.Bsize)
	r := reading{
		key:      strconv.FormatUint(st.Dev, 10),
		mount:    mountPoint(path, st.Dev),
		readOnly: fs.Flags&unix.ST_RDONLY != 0,
		pseudo:   pseudoMagics[uint32(fs.Type)],
		total:    uint64(fs.Blocks) * bsize,
		used:     (uint64(fs.Blocks) - uint64(fs.Bfree)) * bsize,
		free:     uint64(fs.Bavail) * bsize,
	}
	// FAT and btrfs report no inode limit (0); a free count above the total
	// is no reading either.
	if fs.Files > 0 && fs.Ffree <= fs.Files {
		r.inodesTotal = fs.Files
		r.inodesFree = fs.Ffree
	}
	return r, nil
}

// mountPoint returns the topmost directory above path (path included) on
// device dev: the mount point of the filesystem holding path, or of the bind
// mount path came through (/opt/ironflock on IronFlockOS, a bind of /data/ironflock).
func mountPoint(path string, dev uint64) string {
	p := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		p = resolved
	}
	p = filepath.Clean(p)
	for p != "/" {
		parent := filepath.Dir(p)
		var st unix.Stat_t
		if err := unix.Stat(parent, &st); err != nil || st.Dev != dev {
			return p
		}
		p = parent
	}
	return p
}
