package diskguard

import (
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog/log"
)

// rotatedLogPatterns are the names logrotate gives archived logs. A file
// under /var/log matching them is no longer written to; the active logs
// (syslog, kern.log, nginx/access.log) never match.
var rotatedLogPatterns = []string{"*.gz", "*.[0-9]", "*.[0-9].gz", "*.old"}

// fileCleanupTimeout bounds each file cleanup's walk: a dying disk can make
// a directory walk crawl, and the guard must get back to its loop.
const fileCleanupTimeout = time.Minute

func isRotatedLog(name string) bool {
	for _, pattern := range rotatedLogPatterns {
		if ok, _ := filepath.Match(pattern, name); ok {
			return true
		}
	}
	return false
}

// removeRotatedLogs deletes the rotated log archives under dir: regular files
// matching rotatedLogPatterns, on dir's own filesystem only (the walk does
// not descend into other mounts; on FlockOS /var/log/journal is /data's) and
// outside the journal, which journald manages. Active logs, symlinks and
// everything else stay.
func (g *Guard) removeRotatedLogs(dir string) {
	g.removeFiles(dir, true, isRotatedLog, "rotated log archives")
}

// removeCoreDumps deletes the core dumps systemd-coredump keeps: the regular
// files directly in dir.
func (g *Guard) removeCoreDumps(dir string) {
	g.removeFiles(dir, false, func(string) bool { return true }, "core dumps")
}

// removeFiles deletes the regular files under dir whose name matches, in dir
// itself or (descend) its subdirectories on the same filesystem, and logs what
// it freed. The walk and the removals go through an os.Root of dir, so a
// symlink swapped into a group-writable log directory cannot steer a removal
// outside it. Deadline-bounded by fileCleanupTimeout.
func (g *Guard) removeFiles(dir string, descend bool, match func(name string) bool, what string) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return
	}
	defer func() { _ = root.Close() }()
	rootInfo, err := root.Stat(".")
	if err != nil {
		return
	}
	deadline := time.Now().Add(fileCleanupTimeout)
	var count int
	var freed int64
	_ = fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if time.Now().After(deadline) {
			log.Warn().Str("dir", dir).Msgf("diskguard: removing %s timed out", what)
			return fs.SkipAll
		}
		if d.IsDir() {
			if rel == "." {
				return nil
			}
			if !descend || filepath.Join(dir, rel) == g.paths.journal {
				return fs.SkipDir
			}
			if info, err := d.Info(); err != nil || !sameDevice(info, rootInfo) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !match(d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if err := root.Remove(rel); err != nil {
			log.Warn().Err(err).Str("file", filepath.Join(dir, rel)).Msg("diskguard: failed to remove file")
			return nil
		}
		count++
		freed += info.Size()
		return nil
	})
	if count > 0 {
		log.Info().Str("dir", dir).Int("files", count).Int64("freed_mb", freed>>20).
			Msgf("diskguard: removed %s", what)
	}
}
