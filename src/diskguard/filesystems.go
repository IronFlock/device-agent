package diskguard

import (
	"errors"
	"io/fs"
	"path/filepath"
	"reagent/common"
	"reagent/safe"
	"slices"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// Level is how full a filesystem is, and (the worst of them) the device.
type Level string

const (
	LevelOK        Level = "ok"        // above its warn floor
	LevelWarn      Level = "warn"      // below its warn floor: the guard cleans up, the platform warns
	LevelEmergency Level = "emergency" // below its emergency floor: the device is in a disk-emergency
)

func (l Level) rank() int {
	switch l {
	case LevelWarn:
		return 1
	case LevelEmergency:
		return 2
	}
	return 0
}

// worse returns the worse of two levels.
func worse(a, b Level) Level {
	if b.rank() > a.rank() {
		return b
	}
	return a
}

// The roles a watched filesystem serves: why it is watched, and which cleanup
// can free it.
const (
	RoleRoot    = "root"    // the root filesystem (/; on FlockOS the RAUC root slot)
	RoleBoot    = "boot"    // the EFI system partition (/boot/efi; FlockOS keeps GRUB's A/B state there)
	RoleData    = "data"    // FlockOS's data partition (/data)
	RoleLogs    = "logs"    // /var/log
	RoleJournal = "journal" // the persistent systemd journal (/var/log/journal)
	RoleDocker  = "docker"  // Docker's data-root, and /var/lib/docker when it is a mount of its own
	RoleAgent   = "agent"   // the agent's folder: database, apps, downloads
)

// emergencyRoles are the roles of the filesystems apps can fill: the root
// filesystem (host bind mounts, the logs apps cause), /data, Docker's
// data-root and the agent's folder. Only their emergency is the device's (app
// gate, stopping non-appliance containers, EMERGENCY status): stopping the
// apps is what halts their growth. A filesystem that serves none of them —
// only boot, logs or journal: the ESP, a separate /var/log on zram or
// log2ram — still gets its level, its report and its cleanup, but stopping the
// apps would not free it, and one that is full by design would hold the device
// in an emergency it can never leave.
var emergencyRoles = []string{RoleRoot, RoleData, RoleDocker, RoleAgent}

// stopsApps reports whether f's emergency is a device-wide one: whether f
// serves one of emergencyRoles.
func (f Filesystem) stopsApps() bool {
	return slices.ContainsFunc(f.Roles, func(role string) bool { return slices.Contains(emergencyRoles, role) })
}

// appSpaceRoles are the roles of the filesystems that hold the apps' storage:
// Docker's data-root, the agent's folder (apps, downloads) and /data. Their
// floors are absolute (see thresholds).
var appSpaceRoles = []string{RoleDocker, RoleAgent, RoleData}

// isAppSpace reports whether f holds the apps' storage: whether it serves one
// of appSpaceRoles. Every other filesystem is a system filesystem.
func (f Filesystem) isAppSpace() bool {
	return slices.ContainsFunc(f.Roles, func(role string) bool { return slices.Contains(appSpaceRoles, role) })
}

// Filesystem is one watched filesystem as the guard last read it. Free is the
// space available to unprivileged writers (f_bavail), as in storage_free. The
// inode fields are 0 on filesystems without an inode limit (FAT, btrfs) and
// on Windows.
type Filesystem struct {
	Mount         string   `json:"mount"`
	Roles         []string `json:"roles"`
	Total         uint64   `json:"total"`
	Used          uint64   `json:"used"`
	Free          uint64   `json:"free"`
	InodesTotal   uint64   `json:"inodes_total"`
	InodesFree    uint64   `json:"inodes_free"`
	WarnFree      uint64   `json:"warn_free"`
	EmergencyFree uint64   `json:"emergency_free"`
	Level         Level    `json:"level"`

	key string // the device ID the watch set is deduplicated by
}

// dict is the filesystem as the device stats carry it.
func (f Filesystem) dict() common.Dict {
	return common.Dict{
		"mount":          f.Mount,
		"roles":          slices.Clone(f.Roles),
		"total":          f.Total,
		"used":           f.Used,
		"free":           f.Free,
		"inodes_total":   f.InodesTotal,
		"inodes_free":    f.InodesFree,
		"warn_free":      f.WarnFree,
		"emergency_free": f.EmergencyFree,
		"level":          string(f.Level),
	}
}

// reading is one look at the filesystem holding a path (see statPath).
type reading struct {
	key         string // device ID (st_dev; on Windows the volume)
	mount       string // mount point of the filesystem holding the path
	readOnly    bool
	pseudo      bool // a kernel or memory-backed filesystem (proc, sysfs, tmpfs, ...)
	total       uint64
	used        uint64
	free        uint64 // available to unprivileged writers (f_bavail)
	inodesTotal uint64 // 0: no inode limit
	inodesFree  uint64
}

// hostPaths are the host paths the guard watches and cleans. A field of the
// Guard so tests can point the cleanups at a temporary tree.
type hostPaths struct {
	root         string
	data         string
	boot         string
	logs         string
	journal      string
	varLibDocker string
	aptCache     string
	coredumps    string
}

func defaultHostPaths() hostPaths {
	return hostPaths{
		root:         rootPath(),
		data:         "/data",
		boot:         "/boot/efi",
		logs:         "/var/log",
		journal:      "/var/log/journal",
		varLibDocker: "/var/lib/docker",
		aptCache:     "/var/cache/apt",
		coredumps:    "/var/lib/systemd/coredump",
	}
}

// candidate is a path whose filesystem the guard watches, and the role that
// filesystem then serves.
type candidate struct {
	path string
	role string
	// mountOnly: watched only when the path is a mount point of its own. A
	// plain /boot/efi directory on the root filesystem holds no ESP, a
	// leftover /var/lib/docker beside a moved data-root no Docker data.
	mountOnly bool
}

// candidates lists the watched paths. Order matters: a filesystem is reported
// under the mount point of the first candidate on it, so the real mount
// points come before the paths that reach a filesystem through a bind mount
// (/opt/ironflock on FlockOS, a bind of /data/ironflock).
func (g *Guard) candidates() []candidate {
	p := g.paths
	return []candidate{
		{path: p.root, role: RoleRoot},
		{path: p.data, role: RoleData, mountOnly: true},
		{path: p.boot, role: RoleBoot, mountOnly: true},
		{path: p.logs, role: RoleLogs},
		{path: p.journal, role: RoleJournal},
		{path: p.varLibDocker, role: RoleDocker, mountOnly: true},
		{path: g.cfg.DataRoot, role: RoleDocker},
		{path: g.cfg.AgentDir, role: RoleAgent},
	}
}

// measure reads every watched filesystem once: each candidate path that
// exists, deduplicated by device, read-only and pseudo filesystems left out,
// with its thresholds and level. It changes no state; the hysteresis it
// applies is what the last guard pass left (see commit).
func (g *Guard) measure() []Filesystem {
	g.mu.Lock()
	prev := g.levels
	g.mu.Unlock()

	var out []Filesystem
	index := map[string]int{}
	for _, c := range g.candidates() {
		if c.path == "" {
			continue
		}
		r, err := g.stat(c.path)
		g.noteStatError(c.path, err)
		if err != nil {
			// Not there, or unreadable: nothing to watch. The guard never
			// acts on a reading it does not have.
			continue
		}
		if c.mountOnly && r.mount != filepath.Clean(c.path) {
			continue
		}
		if r.readOnly || r.pseudo {
			continue
		}
		if i, ok := index[r.key]; ok {
			if !slices.Contains(out[i].Roles, c.role) {
				out[i].Roles = append(out[i].Roles, c.role)
			}
			continue
		}
		index[r.key] = len(out)
		out = append(out, newFilesystem(r, c.role))
	}
	// Thresholds and level last: they depend on every role the filesystem
	// serves (see isAppSpace).
	for i := range out {
		out[i].WarnFree, out[i].EmergencyFree = g.thresholds(out[i].Total, out[i].isAppSpace())
		out[i].Level = g.level(out[i], prev[out[i].key] == LevelEmergency)
	}
	return out
}

// noteStatError logs a failed read of a watched path once, until it reads
// again. A path that does not exist is no error: most candidates are optional.
func (g *Guard) noteStatError(path string, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		delete(g.statErrs, path)
		return
	}
	if g.statErrs[path] {
		return
	}
	g.statErrs[path] = true
	log.Warn().Err(err).Str("path", path).Msg("diskguard: cannot stat filesystem; not watching it")
}

// newFilesystem is a filesystem's entry from its first reading, without
// thresholds and level yet.
func newFilesystem(r reading, role string) Filesystem {
	return Filesystem{
		Mount:       r.mount,
		Roles:       []string{role},
		Total:       r.total,
		Used:        r.used,
		Free:        r.free,
		InodesTotal: r.inodesTotal,
		InodesFree:  r.inodesFree,
		key:         r.key,
	}
}

// thresholds returns a filesystem's warn and emergency floors in free bytes.
//
// App space (isAppSpace: on FlockOS /data, on a single-disk host the root
// filesystem) gets the absolute floors, WarnFreeBytes and EmergencyFreeBytes,
// whatever its size: when the apps run out of room must not depend on how big
// the disk is. App space smaller than a floor is at that level from the start;
// that is meant (too little room for apps).
//
// A system filesystem (a root slot without app data, the ESP, a separate
// /var/log) holds what the OS writes, not what apps do, and the absolute
// floors would keep a 4 GiB root slot or a 256 MiB ESP in permanent
// emergency. There they shrink to a share of its size (WarnFreePercent,
// EmergencyFreePercent), but never below MinWarnFreeBytes /
// MinEmergencyFreeBytes, so a tiny filesystem keeps a band one pass can react
// to.
func (g *Guard) thresholds(total uint64, appSpace bool) (warn, emergency uint64) {
	c := g.cfg
	if appSpace {
		warn, emergency = uint64(c.WarnFreeBytes), uint64(c.EmergencyFreeBytes)
	} else {
		warn = min(uint64(c.WarnFreeBytes), max(total*uint64(c.WarnFreePercent)/100, uint64(c.MinWarnFreeBytes)))
		emergency = min(uint64(c.EmergencyFreeBytes), max(total*uint64(c.EmergencyFreePercent)/100, uint64(c.MinEmergencyFreeBytes)))
	}
	// A band never inverts, whatever the configuration says.
	return max(warn, emergency), emergency
}

// level maps a reading to its level: the worse of the byte and the inode
// level (inodes only where the filesystem has a limit). Hysteresis: a
// filesystem in emergency stays there through its warn band, so the emergency
// clears only once the filesystem is back at its warn floor.
func (g *Guard) level(f Filesystem, wasEmergency bool) Level {
	l := LevelOK
	switch {
	case f.Free < f.EmergencyFree:
		l = LevelEmergency
	case f.Free < f.WarnFree:
		l = LevelWarn
	}
	if f.InodesTotal > 0 {
		switch {
		case f.InodesFree*100 < f.InodesTotal*uint64(g.cfg.EmergencyFreeInodesPercent):
			l = worse(l, LevelEmergency)
		case f.InodesFree*100 < f.InodesTotal*uint64(g.cfg.WarnFreeInodesPercent):
			l = worse(l, LevelWarn)
		}
	}
	if l == LevelWarn && wasEmergency {
		l = LevelEmergency
	}
	return l
}

// worstLevel returns the worst level among fss (ok for none).
func worstLevel(fss []Filesystem) Level {
	worst := LevelOK
	for _, f := range fss {
		worst = worse(worst, f.Level)
	}
	return worst
}

// holding returns the watched filesystem that holds path, if path exists and
// its filesystem is watched.
func (g *Guard) holding(fss []Filesystem, path string) (Filesystem, bool) {
	if path == "" {
		return Filesystem{}, false
	}
	r, err := g.stat(path)
	if err != nil {
		return Filesystem{}, false
	}
	for _, f := range fss {
		if f.key == r.key {
			return f, true
		}
	}
	return Filesystem{}, false
}

// commit makes fss the guard's state after a pass: the per-filesystem levels
// (and with them the hysteresis of the next measure), the snapshot reports
// serve, and a log line for every filesystem whose level changed.
func (g *Guard) commit(fss []Filesystem, at time.Time) {
	g.mu.Lock()
	prev := g.levels
	g.levels = make(map[string]Level, len(fss))
	for _, f := range fss {
		g.levels[f.key] = f.Level
	}
	g.passLevel = worstLevel(fss)
	g.storeLocked(fss, at)
	g.mu.Unlock()

	for _, f := range fss {
		from := prev[f.key]
		if from == "" {
			from = LevelOK
		}
		if from != f.Level {
			logLevelChange(f, from)
		}
	}
}

func logLevelChange(f Filesystem, from Level) {
	var ev *zerolog.Event
	switch f.Level {
	case LevelEmergency:
		ev = log.Error()
	case LevelWarn:
		ev = log.Warn()
	default:
		ev = log.Info()
	}
	ev = ev.Str("mount", f.Mount).Strs("roles", f.Roles).
		Str("from", string(from)).Str("level", string(f.Level)).
		Uint64("free_mb", f.Free>>20).Uint64("total_mb", f.Total>>20).
		Uint64("warn_free_mb", f.WarnFree>>20).Uint64("emergency_free_mb", f.EmergencyFree>>20)
	if f.InodesTotal > 0 {
		ev = ev.Uint64("inodes_free", f.InodesFree).Uint64("inodes_total", f.InodesTotal)
	}
	if f.Level == LevelEmergency {
		ev = ev.Bool("device_emergency", f.stopsApps())
	}
	ev.Msg("diskguard: filesystem level changed")
}

// storeLocked keeps fss as the snapshot unless a newer one is already there
// (a slow report refresh must not overwrite a pass that started after it).
// g.mu must be held.
func (g *Guard) storeLocked(fss []Filesystem, at time.Time) {
	if at.Before(g.snapshotAt) {
		return
	}
	g.snapshot = fss
	g.snapshotAt = at
}

// Report returns the watched filesystems with readings at most reportMaxAge
// old. An older snapshot is refreshed first, but Report waits for that at most
// reportTimeout and otherwise returns what it has: a statfs hung on a dead
// disk must not hold up the heartbeat (one refresh at a time, so a hung one
// does not pile up more). A refresh that finds a filesystem worse off than
// the guard's last pass wakes the guard, so the emergency gate engages within
// a heartbeat instead of at the next poll.
func (g *Guard) Report() []Filesystem {
	g.mu.Lock()
	if time.Since(g.snapshotAt) >= g.reportMaxAge {
		done := g.refreshing
		if done == nil {
			done = make(chan struct{})
			g.refreshing = done
			safe.Go(func() { g.refresh(done) })
		}
		g.mu.Unlock()
		timer := time.NewTimer(g.reportTimeout)
		select {
		case <-done:
		case <-timer.C:
		}
		timer.Stop()
		g.mu.Lock()
	}
	out := make([]Filesystem, len(g.snapshot))
	for i, f := range g.snapshot {
		f.Roles = slices.Clone(f.Roles)
		out[i] = f
	}
	g.mu.Unlock()
	return out
}

func (g *Guard) refresh(done chan struct{}) {
	defer func() {
		g.mu.Lock()
		g.refreshing = nil
		g.mu.Unlock()
		close(done)
	}()
	at := time.Now()
	fss := g.measure()
	g.mu.Lock()
	g.storeLocked(fss, at)
	wake := worstLevel(fss).rank() > g.passLevel.rank()
	g.mu.Unlock()
	if wake {
		select {
		case g.wake <- struct{}{}:
		default:
		}
	}
}

// statsFields is the guard's part of the device stats (see StatsFields).
func (g *Guard) statsFields() common.Dict {
	fss := g.Report()
	if len(fss) == 0 {
		return common.Dict{}
	}
	list := make([]common.Dict, 0, len(fss))
	for _, f := range fss {
		list = append(list, f.dict())
	}
	return common.Dict{
		"filesystems": list,
		"disk_level":  string(worstLevel(fss)),
	}
}

// reporter is the guard StatsFields reports on: the one the agent built.
var reporter atomic.Pointer[Guard]

// StatsFields returns the disk fields of the device stats that the heartbeat
// and get_storage_data carry: "filesystems", one entry per watched filesystem
// (mount, roles, total, used, free, inodes_total, inodes_free, warn_free,
// emergency_free, level), and "disk_level", the worst level. The platform
// warns users from them. Empty while no guard exists or nothing could be
// read: the platform then knows as little as from an agent without them.
func StatsFields() common.Dict {
	g := reporter.Load()
	if g == nil {
		return common.Dict{}
	}
	return g.statsFields()
}
