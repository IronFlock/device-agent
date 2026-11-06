//go:build !windows

package diskguard

import (
	"io/fs"
	"syscall"
)

// sameDevice reports whether two files lie on the same filesystem (st_dev),
// so the file cleanups never cross into another mount.
func sameDevice(a, b fs.FileInfo) bool {
	sa, okA := a.Sys().(*syscall.Stat_t)
	sb, okB := b.Sys().(*syscall.Stat_t)
	return okA && okB && sa.Dev == sb.Dev
}
