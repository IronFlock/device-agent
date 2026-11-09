// Package diskguard keeps a device's disks from filling up, which would take
// it offline and unreachable. It watches every filesystem the device depends
// on, not only Docker's: the root filesystem fills from logs as readily as the
// data partition from images. It runs as a background loop in the agent and
// has four jobs:
//
//   - EnsurePreventionConfig: a one-time, idempotent pass that caps the two
//     unbounded log sinks that most often fill a device — Docker container logs
//     (daemon.json log-opts) and the systemd journal.
//   - The watch set (measure): the filesystems holding /, Docker's data-root,
//     the agent's folder and, where they exist, /var/log, /var/log/journal,
//     and the separate mounts /data, /boot/efi and /var/lib/docker —
//     deduplicated by device, read-only and pseudo filesystems left out. Each
//     carries the roles it serves (root, docker, agent, data, boot, logs,
//     journal) and a level (ok, warn, emergency). App space (docker, agent or
//     data: /data on IronFlockOS) has absolute floors, 3 GiB and 1 GiB whatever its
//     size, so when the apps run out of room never depends on the disk size;
//     a system filesystem (a root slot without app data, the ESP, a separate
//     /var/log) has floors relative to its size, so a 4 GiB root slot and a
//     256 MiB ESP get bands of their own (see thresholds). Inodes count too
//     where the filesystem has them.
//   - Run: a periodic loop that, while a filesystem is below its warn floor,
//     reclaims space safely on that filesystem — the Docker steps (prune
//     dangling images + build cache + certainly-unused volumes, truncate
//     runaway container logs) only for one holding Docker's data, the journal
//     vacuum for the journal's, apt clean for the apt cache's, and at
//     emergency the rotated log archives and core dumps; and while a
//     filesystem apps can fill (role root, data, docker or agent) is below
//     its emergency floor, holds a device-wide EMERGENCY state. A filesystem
//     serving only boot, logs or journal (the ESP, a zram /var/log) is
//     reported and cleaned at every level, but never stops the apps (see
//     emergencyRoles).
//   - The EMERGENCY state (IsEmergency) is exported so the rest of the agent can
//     react: it is reported to the cloud in the device status, and the app state
//     machine fails any transition to RUNNING/BUILDING/DOWNLOADING so apps can't
//     pull, build, or start and grow the disk further. While in the state the
//     guard also stops every running container that is not part of the
//     ironflock-appliance compose stack, halting current disk growth without
//     touching that stack (which carries remote reachability). The
//     filesystems and their levels ride in the device stats (StatsFields), so
//     the platform can warn users before it comes to that.
//
// SAFE: it never removes in-use images, and never removes a tagged image whose
// registry the device may be unable to reach (offline devices can't re-pull).
// The one exception is an app image from a registry local to the device — an
// appliance's on-box app registry — where a re-pull costs a loopback copy; see
// reclaimSupersededAppImages. Volumes are only removed when they are positively
// known to be unused (see pruneOrphanedVolumes); anything attributable to an
// installed app — stopped or running — or not attributable at all is left
// alone. App containers are ephemeral — their state lives in volumes/bind
// mounts — so stopping them is non-destructive. Files are only removed when
// nothing writes them any more: rotated log archives and core dumps, never an
// active log (see removeRotatedLogs).
package diskguard

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reagent/common"
	"reagent/container"
	"reagent/safe"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
)

// Docker is the subset of the container layer the guard needs.
// *container.Docker satisfies it.
type Docker interface {
	PruneDanglingImages(ctx context.Context) (string, error)
	PruneBuildCache(ctx context.Context) (string, error)
	ListDanglingVolumes(ctx context.Context) ([]container.VolumeResult, error)
	RemoveVolume(ctx context.Context, name string) error
	ListContainers(ctx context.Context, options common.Dict) ([]container.ContainerResult, error)
	StopContainerByID(ctx context.Context, containerID string, timeout time.Duration) error
	ListImages(ctx context.Context, options map[string]interface{}) ([]container.ImageResult, error)
	RemoveImage(ctx context.Context, imageID string, options map[string]interface{}) error
}

// Every daemon touch is deadline-bounded: on a disk-full box the Docker daemon
// itself is often wedged, and an unbounded call would hang the guard exactly
// when it is needed most (observed live: a hanging prune kept a device
// reporting healthy CONNECTED at ~300 MB free).
const (
	pruneTimeout    = 5 * time.Minute // image/build-cache prunes; slow disks take a while
	dockerOpTimeout = 2 * time.Minute // list operations
	removeTimeout   = 1 * time.Minute // per-volume remove
	stopTimeout     = 30 * time.Second
	cmdTimeout      = 2 * time.Minute // journalctl vacuum, apt-get clean

	// Registry GC drains in-flight image transfers and walks the whole blob
	// tree, so it gets a generous deadline — and a floor between runs: while
	// in EMERGENCY the cleanup re-runs every few seconds, and hammering the
	// sweep would starve app syncs without freeing anything new.
	registryGCTimeout     = 5 * time.Minute
	registryGCMinInterval = 15 * time.Minute

	// checkNowTimeout bounds the startup check, which runs before the agent
	// connects: a statfs hung on a dead disk must not keep it from connecting.
	checkNowTimeout = 30 * time.Second
	// A report (heartbeat, get_storage_data) re-reads the filesystems when its
	// snapshot is older than reportMaxAge, and waits reportTimeout for that.
	reportMaxAge  = 10 * time.Second
	reportTimeout = 2 * time.Second
)

// emergency is the device-wide disk-emergency flag. It is read by the app state
// machine (to fail RUNNING/BUILDING/DOWNLOADING transitions) and by the device
// status report (to inform the cloud).
var emergency atomic.Bool

// IsEmergency reports whether the device is currently in a disk-emergency.
func IsEmergency() bool { return emergency.Load() }

// setEmergency stores v and returns true if the value changed.
func setEmergency(v bool) bool { return emergency.Swap(v) != v }

const (
	mib int64 = 1 << 20
	gib int64 = 1 << 30
)

const (
	composeProjectLabel     = "com.docker.compose.project"
	applianceComposeProject = "ironflock-appliance"

	daemonJSONPath = "/etc/docker/daemon.json"
	journaldDropin = "/etc/systemd/journald.conf.d/10-ironflock.conf"
)

// Config tunes the guard. Zero values fall back to sensible defaults. Each
// watched filesystem gets its own floors (see thresholds): app space the
// absolute ones below, whatever its size; a system filesystem the absolute
// ones lowered to a share of its size.
type Config struct {
	DataRoot          string        // Docker's data-root, watched with role docker (default /var/lib/docker)
	AgentDir          string        // the agent's folder, watched with role agent (empty: not watched)
	Interval          time.Duration // poll cadence, and the floor between cleanups below emergency (default 5m)
	WarnInterval      time.Duration // poll cadence while a filesystem is below its warn floor, to catch it nearing emergency (default 30s)
	EmergencyInterval time.Duration // poll cadence while in EMERGENCY, for fast recovery (default 5s)
	// WarnFreeBytes: warn and run safe cleanup on app space below this; an
	// EMERGENCY of it also clears only at/above it (default 3 GiB).
	// EmergencyFreeBytes: enter EMERGENCY below this on app space (default
	// 1 GiB). On a system filesystem both are caps.
	WarnFreeBytes      int64
	EmergencyFreeBytes int64
	// WarnFreePercent/EmergencyFreePercent lower those floors to this share
	// of a system filesystem's size (defaults 15 and 5), and
	// MinWarnFreeBytes/MinEmergencyFreeBytes keep them from shrinking below
	// this on a tiny one (defaults 8 MiB and 2 MiB).
	WarnFreePercent       int
	EmergencyFreePercent  int
	MinWarnFreeBytes      int64
	MinEmergencyFreeBytes int64
	// WarnFreeInodesPercent/EmergencyFreeInodesPercent: a filesystem with an
	// inode limit is at warn/emergency below this share of free inodes too
	// (defaults 10 and 3).
	WarnFreeInodesPercent      int
	EmergencyFreeInodesPercent int
	LogMaxBytes                int64 // truncate container logs larger than this (default 50 MiB)
	// AppsComposeDir/AppsBuildDir are the roots holding one directory per
	// compose app (PROD/DEV respectively). A dangling compose volume is only
	// attributed to the agent — and thus deletable when its app is gone — if a
	// directory matching its project name exists under one of these.
	AppsComposeDir string
	AppsBuildDir   string
	// InstalledAppNames returns the names of ALL locally known apps (stopped
	// ones included); their compose volumes are never removed. An error means
	// the installed set is unknown — compose volumes are then not touched at
	// all rather than treated as orphaned.
	InstalledAppNames func() ([]string, error)
	// AppImageRegistry is the registry app images are pulled from (the
	// device's docker_registry_url, e.g. "localhost:15001/"). It gates the
	// superseded-app-image reclaim, which only runs when this registry is
	// local to the device — see reclaimSupersededAppImages. Empty disables it.
	AppImageRegistry string
	// WantedAppImages returns every image reference (repo:tag) the device still
	// needs: for each locally known app, its present and newest version in both
	// stages. Anything else under AppImageRegistry is a superseded leftover. An
	// error means the wanted set is unknown — no app image is then touched.
	WantedAppImages func() (map[string]bool, error)
	// OnRecover is called once when the device leaves EMERGENCY (every
	// filesystem apps can fill back at its warn floor), to reinstate the
	// apps' previous requested states (which were stopped/blocked during it).
	OnRecover func()
}

func (c *Config) withDefaults() {
	if c.DataRoot == "" {
		c.DataRoot = "/var/lib/docker"
	}
	if c.Interval == 0 {
		c.Interval = 5 * time.Minute
	}
	if c.WarnInterval == 0 {
		c.WarnInterval = 30 * time.Second
	}
	if c.EmergencyInterval == 0 {
		c.EmergencyInterval = 5 * time.Second
	}
	if c.WarnFreeBytes == 0 {
		c.WarnFreeBytes = 3 * gib
	}
	if c.EmergencyFreeBytes == 0 {
		c.EmergencyFreeBytes = 1 * gib
	}
	if c.WarnFreePercent == 0 {
		c.WarnFreePercent = 15
	}
	if c.EmergencyFreePercent == 0 {
		c.EmergencyFreePercent = 5
	}
	if c.MinWarnFreeBytes == 0 {
		c.MinWarnFreeBytes = 8 * mib
	}
	if c.MinEmergencyFreeBytes == 0 {
		c.MinEmergencyFreeBytes = 2 * mib
	}
	if c.WarnFreeInodesPercent == 0 {
		c.WarnFreeInodesPercent = 10
	}
	if c.EmergencyFreeInodesPercent == 0 {
		c.EmergencyFreeInodesPercent = 3
	}
	if c.LogMaxBytes == 0 {
		c.LogMaxBytes = 50 * mib
	}
}

// Guard runs the periodic disk cleanup and maintains the emergency state.
type Guard struct {
	cfg    Config
	docker Docker

	// The host the guard reads and acts on; tests replace them.
	stat     func(path string) (reading, error)
	runCmd   func(ctx context.Context, name string, args ...string) error
	lookPath func(file string) (string, error)
	paths    hostPaths

	reportMaxAge  time.Duration
	reportTimeout time.Duration
	// wake cuts the Run loop's wait short when a report finds a filesystem
	// worse off than the last pass did.
	wake chan struct{}

	mu          sync.Mutex
	levels      map[string]Level // per filesystem (device ID), as the last pass left them: the hysteresis
	passLevel   Level            // the worst of levels
	snapshot    []Filesystem     // the latest reading, served by Report
	snapshotAt  time.Time
	refreshing  chan struct{} // closed when the report refresh in flight ends; nil when none is
	statErrs    map[string]bool
	lastCleanup time.Time

	// registryGC asks the appliance-local appstore registry for an immediate
	// garbage collection. Wired via SetRegistryGC after the WAMP session
	// exists (the guard itself is built and first run before any connection);
	// nil = disabled. Mutex-guarded because the Run loop reads it while the
	// startup goroutine may still be wiring it.
	registryGCMu   sync.Mutex
	registryGC     func(ctx context.Context)
	lastRegistryGC time.Time
}

// New builds a Guard and makes it the one StatsFields reports on (an agent
// builds one). docker may be nil (the container-dependent steps are then
// skipped). New only builds: nothing is read or changed until CheckNow, Run or
// a report.
func New(docker Docker, cfg Config) *Guard {
	cfg.withDefaults()
	g := &Guard{
		cfg:           cfg,
		docker:        docker,
		stat:          statPath,
		runCmd:        runCommand,
		lookPath:      exec.LookPath,
		paths:         defaultHostPaths(),
		reportMaxAge:  reportMaxAge,
		reportTimeout: reportTimeout,
		wake:          make(chan struct{}, 1),
		levels:        map[string]Level{},
		passLevel:     LevelOK,
		statErrs:      map[string]bool{},
	}
	reporter.Store(g)
	return g
}

// SetRegistryGC wires the immediate-registry-GC trigger once the WAMP session
// exists (the SetMessenger pattern of the other subsystems, kept as a plain
// function so this package stays free of the messenger dependency). fn is
// called from the guard's background loop with a deadline-bounded context and
// must be best-effort: log and return, never block beyond its context.
func (g *Guard) SetRegistryGC(fn func(ctx context.Context)) {
	g.registryGCMu.Lock()
	defer g.registryGCMu.Unlock()
	g.registryGC = fn
}

// CheckNow evaluates every watched filesystem synchronously and sets the
// emergency state from it (stopping non-platform containers when it is on).
// Call it once at startup BEFORE starting apps so the emergency gate is active
// before any container start, instead of racing the background Run loop. It
// runs before the agent connects, so it does no cleanup (Run's first pass, right
// after, does) and waits at most checkNowTimeout: past that the check goes on
// in the background and the start goes on without it.
func (g *Guard) CheckNow() {
	done := make(chan struct{})
	safe.Go(func() {
		defer close(done)
		at := time.Now()
		fss := g.measure()
		g.commit(fss, at)
		g.updateEmergency(fss)
	})
	timer := time.NewTimer(checkNowTimeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		log.Warn().Dur("timeout", checkNowTimeout).Msg("diskguard: startup disk check still running; going on without it")
	}
}

// Run executes one pass immediately, then repeats until ctx is done. While in
// EMERGENCY it polls at EmergencyInterval (a few seconds) so it recovers — and
// reinstates the apps — promptly once space is freed; while a filesystem is
// below its warn floor at WarnInterval, so it catches one nearing emergency;
// otherwise at Interval. A report that finds a filesystem worse off wakes it
// at once.
func (g *Guard) Run(ctx context.Context) {
	for {
		g.runOnce()

		interval := g.cfg.Interval
		switch {
		case IsEmergency():
			interval = g.cfg.EmergencyInterval
		case g.lastPassLevel() != LevelOK:
			interval = g.cfg.WarnInterval
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-g.wake:
			timer.Stop()
		}
	}
}

func (g *Guard) lastPassLevel() Level {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.passLevel
}

func (g *Guard) runOnce() {
	at := time.Now()
	fss := g.measure()
	// Latch a critical reading BEFORE attempting any cleanup: the emergency
	// flag (app-start gate + EMERGENCY heartbeat status) must never wait on
	// cleanup steps, which all talk to a daemon that may be wedged. The steps
	// are deadline-bounded too, but minutes of accumulated timeouts would
	// still delay the report; the flag itself needs none of them.
	if worstLevel(fss) == LevelEmergency {
		g.commit(fss, at)
		g.updateEmergency(fss)
	}
	if g.cleanupDue(fss) {
		for _, f := range fss {
			if f.Level != LevelOK {
				log.Warn().Str("mount", f.Mount).Strs("roles", f.Roles).Str("level", string(f.Level)).
					Uint64("free_mb", f.Free>>20).Msg("diskguard: low disk, running safe cleanup")
			}
		}
		before := fss
		g.cleanup(fss)
		at = time.Now()
		fss = g.measure()
		logFreed(before, fss)
	}
	g.commit(fss, at)
	g.updateEmergency(fss)
}

// cleanupDue reports whether this pass cleans up: while a filesystem is low,
// every pass in an emergency (as fast as the emergency poll) and otherwise at
// most once per Interval, however often the warn poll looks.
func (g *Guard) cleanupDue(fss []Filesystem) bool {
	worst := worstLevel(fss)
	if worst == LevelOK {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if worst != LevelEmergency && !g.lastCleanup.IsZero() && time.Since(g.lastCleanup) < g.cfg.Interval {
		return false
	}
	g.lastCleanup = time.Now()
	return true
}

// logFreed logs what a cleanup freed on each filesystem that was low.
func logFreed(before, after []Filesystem) {
	for _, b := range before {
		if b.Level == LevelOK {
			continue
		}
		for _, a := range after {
			if a.key != b.key {
				continue
			}
			var freed uint64
			if a.Free > b.Free {
				freed = a.Free - b.Free
			}
			log.Info().Str("mount", a.Mount).Uint64("freed_mb", freed>>20).Uint64("free_mb", a.Free>>20).
				Str("level", string(a.Level)).Msg("diskguard: cleanup done")
		}
	}
}

// updateEmergency sets the device-wide emergency from the filesystems' levels:
// on while a filesystem apps can fill (see emergencyRoles) is at emergency,
// each with its own hysteresis (see level), off once every one of them has
// recovered. A boot- or log-only filesystem at emergency is reported and
// cleaned, but leaves the device out of it.
func (g *Guard) updateEmergency(fss []Filesystem) {
	var causes []string
	for _, f := range fss {
		if f.Level == LevelEmergency && f.stopsApps() {
			causes = append(causes, f.Mount)
		}
	}
	if len(causes) > 0 {
		if setEmergency(true) {
			log.Error().Strs("filesystems", causes).
				Msg("diskguard: ENTERING disk-emergency — failing new app start/build/download and stopping non-platform containers")
		}
		g.stopForeignContainers()
		return
	}
	if setEmergency(false) {
		log.Info().Msg("diskguard: cleared disk-emergency state, every filesystem apps can fill has recovered; reinstating app states")
		if g.cfg.OnRecover != nil {
			safe.Go(g.cfg.OnRecover)
		}
	}
}

// cleanup reclaims space on the filesystems that are low, each step aimed at
// what it frees: the Docker steps only for a filesystem holding Docker's data,
// the journal vacuum for the journal's, apt's cache for its own, and at
// emergency only the rotated log archives and core dumps. A low filesystem no
// step can free (a full ESP) gets none.
func (g *Guard) cleanup(fss []Filesystem) {
	dockerLow := false
	for _, f := range fss {
		if f.Level != LevelOK && slices.Contains(f.Roles, RoleDocker) {
			dockerLow = true
		}
	}
	if dockerLow {
		g.cleanupDocker()
	}
	if f, ok := g.holding(fss, g.paths.journal); ok && f.Level != LevelOK {
		g.run("journalctl", "--vacuum-size=100M")
	}
	if f, ok := g.holding(fss, g.paths.aptCache); ok && f.Level != LevelOK {
		if _, err := g.lookPath("apt-get"); err == nil {
			g.run("apt-get", "clean")
		}
	}
	if f, ok := g.holding(fss, g.paths.logs); ok && f.Level == LevelEmergency {
		g.removeRotatedLogs(g.paths.logs)
	}
	if f, ok := g.holding(fss, g.paths.coredumps); ok && f.Level == LevelEmergency {
		g.removeCoreDumps(g.paths.coredumps)
	}
}

// cleanupDocker runs the steps that free a filesystem holding Docker's data.
func (g *Guard) cleanupDocker() {
	// Registry blobs of removed/superseded apps are often the largest
	// reclaimable block on an appliance, and outside this path they wait on
	// the registry's debounced sweep. First: it needs no Docker daemon (which
	// may be wedged on a full disk) and frees space the prunes below can't.
	g.triggerRegistryGC()
	if g.docker != nil {
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), pruneTimeout)
			defer cancel()
			if _, err := g.docker.PruneDanglingImages(ctx); err != nil {
				log.Error().Err(err).Msg("diskguard: prune dangling images failed")
			}
		}()
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), pruneTimeout)
			defer cancel()
			if _, err := g.docker.PruneBuildCache(ctx); err != nil {
				log.Error().Err(err).Msg("diskguard: prune build cache failed")
			}
		}()
		g.pruneOrphanedVolumes()
		g.reclaimSupersededAppImages()
	}
	g.truncateOversizedLogs()
}

// triggerRegistryGC runs the appstore registry's garbage collection now,
// bypassing its debounce. No-op unless wired (SetRegistryGC — appliance host
// only) and throttled to registryGCMinInterval between attempts. The throttle
// stamps before the call: a failed sweep on a struggling box must not be
// retried every emergency tick either.
func (g *Guard) triggerRegistryGC() {
	g.registryGCMu.Lock()
	fn := g.registryGC
	if fn == nil || time.Since(g.lastRegistryGC) < registryGCMinInterval {
		g.registryGCMu.Unlock()
		return
	}
	g.lastRegistryGC = time.Now()
	g.registryGCMu.Unlock()

	log.Info().Msg("diskguard: triggering immediate registry garbage collection")
	ctx, cancel := context.WithTimeout(context.Background(), registryGCTimeout)
	defer cancel()
	fn(ctx)
}

// pruneOrphanedVolumes removes volumes that are positively known to be unused.
// Knowing that takes three facts, because "dangling" alone is not enough — a
// stopped compose app has no containers (compose `down` removes them) yet its
// named volumes hold app data it needs on the next start:
//
//  1. Docker's dangling filter: no container — running or stopped — references
//     the volume.
//  2. The agent's local app database lists every installed app, stopped ones
//     included; a compose volume is owned by the app whose normalized name is
//     in its com.docker.compose.project label, so any volume of a listed app
//     is kept.
//  3. Attribution: a dangling compose volume of a NOT-listed project is only
//     deleted when the agent can prove the project was its own: uninstall runs
//     `compose down -v` BEFORE removing the app's compose directory, so an
//     interrupted teardown always leaves that directory behind. A project with
//     no such directory may be someone else's (a user compose stack on an
//     appliance host) and is left alone.
//
// Anonymous volumes (unlabeled 64-hex names, auto-created by Dockerfile VOLUME
// directives) are certainly unused once dangling: a recreated container gets
// fresh ones, nothing ever re-attaches these. Named unlabeled volumes (user
// `docker volume create`) and the appliance stack's project are never touched.
func (g *Guard) pruneOrphanedVolumes() {
	ctx, cancel := context.WithTimeout(context.Background(), dockerOpTimeout)
	defer cancel()
	volumes, err := g.docker.ListDanglingVolumes(ctx)
	if err != nil {
		log.Error().Err(err).Msg("diskguard: list dangling volumes failed")
		return
	}
	if len(volumes) == 0 {
		return
	}

	installed := map[string]bool{}
	installedKnown := false
	if g.cfg.InstalledAppNames != nil {
		if names, err := g.cfg.InstalledAppNames(); err != nil {
			log.Error().Err(err).Msg("diskguard: cannot read installed apps; leaving compose volumes alone")
		} else {
			installedKnown = true
			for _, n := range names {
				installed[normalizeProjectName(n)] = true
			}
		}
	}

	for _, v := range volumes {
		project := v.Labels[composeProjectLabel]
		if project == "" {
			if len(v.Labels) == 0 && isAnonymousVolumeName(v.Name) {
				g.removeVolume(v.Name, "anonymous, unreferenced")
			}
			continue
		}
		if project == applianceComposeProject || !installedKnown || installed[project] {
			continue
		}
		if !g.hasAppDirForProject(project) {
			continue
		}
		g.removeVolume(v.Name, "leaked by removed app "+project)
	}
}

// reclaimSupersededAppImages removes app image tags the device no longer needs.
//
// Removing a TAGGED image is otherwise off-limits for the guard (see the SAFE
// note in the package doc): a device that cannot re-pull would be left unable
// to restart its own apps. That objection disappears when app images come from
// a registry local to the device — an appliance carries its own on-box app
// registry, so a re-pull there is a loopback copy needing no network, no cloud
// and no credentials beyond what the device already has. AppImageRegistry is
// therefore the gate: only a loopback or private-network registry qualifies,
// and a cloud device (whose docker_registry_url is a WAN host) skips this step
// entirely.
//
// Within that registry a tag is removed only when it is BOTH not wanted (not a
// present/newest version of a locally known app) AND not referenced by any
// container, running or stopped. Removal is per-tag rather than per image ID,
// so an image carrying one wanted and one superseded tag only loses the
// superseded one. force is deliberately not set: if the daemon still sees a
// reference the removal fails and the tag stays.
//
// This is the sweeper for leftovers that the normal paths missed — the
// best-effort image removal on app update is bounded and its error discarded,
// so a wedged or slow daemon silently strands the previous version's image.
func (g *Guard) reclaimSupersededAppImages() {
	host, ok := localRegistryHost(g.cfg.AppImageRegistry)
	if !ok {
		return
	}
	if g.cfg.WantedAppImages == nil {
		return
	}
	wanted, err := g.cfg.WantedAppImages()
	if err != nil {
		log.Error().Err(err).Msg("diskguard: cannot read wanted app images; leaving app images alone")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), dockerOpTimeout)
	defer cancel()

	// all=true: a stopped container still pins its image, and its app is meant
	// to start again.
	containers, err := g.docker.ListContainers(ctx, common.Dict{"all": true})
	if err != nil {
		log.Error().Err(err).Msg("diskguard: list containers failed; leaving app images alone")
		return
	}
	inUse := make(map[string]bool, len(containers))
	for _, c := range containers {
		inUse[c.ImageID] = true
	}

	images, err := g.docker.ListImages(ctx, map[string]interface{}{})
	if err != nil {
		log.Error().Err(err).Msg("diskguard: list images failed; leaving app images alone")
		return
	}
	for _, img := range images {
		if inUse[img.ID] {
			continue
		}
		for _, tag := range img.RepoTags {
			if registryHostOf(tag) != host || wanted[tag] {
				continue
			}
			g.removeImageTag(tag)
		}
	}
}

func (g *Guard) removeImageTag(tag string) {
	ctx, cancel := context.WithTimeout(context.Background(), removeTimeout)
	defer cancel()
	if err := g.docker.RemoveImage(ctx, tag, map[string]interface{}{"force": false, "pruneChildren": true}); err != nil {
		log.Warn().Err(err).Str("image", tag).Msg("diskguard: failed to remove superseded app image")
	} else {
		log.Info().Str("image", tag).Msg("diskguard: removed superseded app image")
	}
}

// registryHostOf returns the registry host of an image reference, or "" when
// the reference carries none. Docker's own rule: the first path segment is a
// registry host if it contains a "." or a ":", or is exactly "localhost";
// otherwise it is an implicit Docker Hub namespace, as in "library/postgres".
func registryHostOf(ref string) string {
	slash := strings.Index(ref, "/")
	if slash < 0 {
		return ""
	}
	host := ref[:slash]
	if !strings.ContainsAny(host, ".:") && !strings.EqualFold(host, "localhost") {
		return ""
	}
	return host
}

// localRegistryHost reports the host of registryURL (a docker_registry_url such
// as "localhost:15001/") when that registry lives on the device or its own
// network, which is what makes its images cheaply re-pullable. Loopback and
// RFC1918/link-local literals qualify; a DNS name other than localhost does
// not, since it cannot be shown to be local without resolving it.
func localRegistryHost(registryURL string) (string, bool) {
	host := registryHostOf(strings.TrimSuffix(registryURL, "/") + "/")
	if host == "" {
		return "", false
	}
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	name = strings.Trim(name, "[]")
	if strings.EqualFold(name, "localhost") {
		return host, true
	}
	ip := net.ParseIP(name)
	if ip == nil {
		return "", false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return host, true
	}
	return "", false
}

func (g *Guard) removeVolume(name string, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), removeTimeout)
	defer cancel()
	if err := g.docker.RemoveVolume(ctx, name); err != nil {
		log.Error().Err(err).Str("volume", name).Msg("diskguard: failed to remove volume")
	} else {
		log.Info().Str("volume", name).Str("reason", reason).Msg("diskguard: removed unused volume")
	}
}

// hasAppDirForProject reports whether a per-app directory matching the compose
// project name exists under the agent's compose or build roots. Compose derives
// a project name by normalizing the compose file's directory name, so the same
// normalization is applied to each candidate directory before comparing.
func (g *Guard) hasAppDirForProject(project string) bool {
	for _, root := range []string{g.cfg.AppsComposeDir, g.cfg.AppsBuildDir} {
		if root == "" {
			continue
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() && normalizeProjectName(e.Name()) == project {
				return true
			}
		}
	}
	return false
}

// normalizeProjectName is the compose default-project-name derivation, shared
// with the state observer's label-based container lookups.
func normalizeProjectName(name string) string {
	return common.NormalizeComposeProjectName(name)
}

// isAnonymousVolumeName reports whether name looks like a Docker-generated
// anonymous volume ID (64 hex characters).
func isAnonymousVolumeName(name string) bool {
	if len(name) != 64 {
		return false
	}
	for _, r := range name {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// stopForeignContainers stops every running container that is NOT part of the
// ironflock-appliance compose stack, halting apps from accumulating more disk.
// The compose stack is preserved because it carries the appliance's remote
// reachability. On non-appliance devices nothing carries that label, so all app
// containers are stopped — the native agent keeps the device reachable.
func (g *Guard) stopForeignContainers() {
	if g.docker == nil {
		return
	}
	listCtx, cancelList := context.WithTimeout(context.Background(), dockerOpTimeout)
	containers, err := g.docker.ListContainers(listCtx, common.Dict{})
	cancelList()
	if err != nil {
		log.Error().Err(err).Msg("diskguard: list containers failed")
		return
	}
	for _, c := range containers {
		if c.State != "running" {
			continue
		}
		if c.Labels[composeProjectLabel] == applianceComposeProject {
			continue // part of the appliance stack — keep it reachable
		}
		name := c.ID
		if len(c.Names) > 0 {
			name = c.Names[0]
		}
		stopCtx, cancelStop := context.WithTimeout(context.Background(), stopTimeout)
		err := g.docker.StopContainerByID(stopCtx, c.ID, 10*time.Second)
		cancelStop()
		if err != nil {
			log.Error().Err(err).Str("container", name).Msg("diskguard: failed to stop container")
		} else {
			log.Warn().Str("container", name).Msg("diskguard: stopped non-platform container to halt disk growth")
		}
	}
}

// truncateOversizedLogs zeroes any container json-file log larger than the
// configured cap. Truncating keeps Docker's open fd valid (it keeps appending)
// while freeing the blocks — covers legacy containers created before rotation.
func (g *Guard) truncateOversizedLogs() {
	root := filepath.Join(g.cfg.DataRoot, "containers")
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".log" {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() <= g.cfg.LogMaxBytes {
			return nil
		}
		if err := os.Truncate(path, 0); err != nil {
			log.Error().Err(err).Str("file", path).Msg("diskguard: failed to truncate oversized log")
		} else {
			log.Info().Str("file", path).Msg("diskguard: truncated oversized container log")
		}
		return nil
	})
}

func (g *Guard) run(name string, args ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()
	if err := g.runCmd(ctx, name, args...); err != nil {
		log.Warn().Err(err).Str("cmd", name).Msg("diskguard: command failed")
	}
}

func runCommand(ctx context.Context, name string, args ...string) error {
	return exec.CommandContext(ctx, name, args...).Run() //nolint:gosec // G204: fixed system commands (journalctl/apt-get) chosen by the agent
}

// EnsurePreventionConfig caps Docker container logs and the systemd journal so
// the disk is far less likely to fill in the first place. Idempotent and safe to
// call on every startup. Docker's log defaults only take full effect for
// containers created after the next daemon restart, so this does not force a
// disruptive restart — it makes the config correct for the next natural restart
// while Run's truncation handles runaway logs in the meantime.
func EnsurePreventionConfig() {
	ensureDockerLogRotation()
	ensureJournaldCap()
}

func ensureDockerLogRotation() {
	cfg := map[string]interface{}{}
	if data, err := os.ReadFile(daemonJSONPath); err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, &cfg); err != nil {
			log.Error().Err(err).Msg("diskguard: daemon.json is not valid JSON; leaving it unchanged")
			return
		}
	}

	changed := false
	if cfg["log-driver"] == nil {
		cfg["log-driver"] = "json-file"
		changed = true
	}
	if cfg["log-opts"] == nil {
		cfg["log-opts"] = map[string]interface{}{"max-size": "10m", "max-file": "3"}
		changed = true
	}
	if !changed {
		return
	}

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		log.Error().Err(err).Msg("diskguard: failed to marshal daemon.json")
		return
	}
	if err := os.MkdirAll(filepath.Dir(daemonJSONPath), 0o755); err != nil {
		log.Error().Err(err).Msg("diskguard: failed to ensure /etc/docker")
		return
	}
	if err := os.WriteFile(daemonJSONPath, append(out, '\n'), 0o644); err != nil { //nolint:gosec // G306: Docker daemon.json / journald drop-in: system configuration, no secrets
		log.Error().Err(err).Msg("diskguard: failed to write daemon.json")
		return
	}
	log.Info().Msg("diskguard: set Docker container-log rotation in daemon.json (effective for containers created after the next Docker restart)")
}

func ensureJournaldCap() {
	const want = "[Journal]\nSystemMaxUse=200M\n"
	if data, err := os.ReadFile(journaldDropin); err == nil && string(data) == want {
		return
	}
	if err := os.MkdirAll(filepath.Dir(journaldDropin), 0o755); err != nil {
		log.Error().Err(err).Msg("diskguard: failed to ensure journald.conf.d")
		return
	}
	if err := os.WriteFile(journaldDropin, []byte(want), 0o644); err != nil { //nolint:gosec // G306: Docker daemon.json / journald drop-in: system configuration, no secrets
		log.Error().Err(err).Msg("diskguard: failed to write journald cap")
		return
	}
	// Best-effort reload so the cap applies without waiting for a reboot.
	if err := exec.Command("systemctl", "restart", "systemd-journald").Run(); err != nil {
		log.Warn().Err(err).Msg("diskguard: wrote journald cap but reload failed; applies on next boot")
	}
	log.Info().Msg("diskguard: capped systemd journal size (SystemMaxUse=200M)")
}
