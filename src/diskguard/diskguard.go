// Package diskguard keeps a device's disk from filling up, which would take it
// offline and unreachable. It runs as a background loop in the agent and has
// three jobs:
//
//   - EnsurePreventionConfig: a one-time, idempotent pass that caps the two
//     unbounded log sinks that most often fill a device — Docker container logs
//     (daemon.json log-opts) and the systemd journal.
//   - Run: a periodic loop that, below a warn threshold, reclaims space safely
//     (prune dangling images + build cache + certainly-unused volumes, vacuum
//     journald, apt clean, truncate runaway container logs); and below a
//     critical threshold, enters a device-wide EMERGENCY state.
//   - The EMERGENCY state (IsEmergency) is exported so the rest of the agent can
//     react: it is reported to the cloud in the device status, and the app state
//     machine fails any transition to RUNNING/BUILDING/DOWNLOADING so apps can't
//     pull, build, or start and grow the disk further. While in the state the
//     guard also stops every running container that is not part of the
//     ironflock-appliance compose stack, halting current disk growth without
//     touching that stack (which carries remote reachability).
//
// SAFE: it never removes in-use images, and never removes a tagged image whose
// registry the device may be unable to reach (offline devices can't re-pull).
// The one exception is an app image from a registry local to the device — an
// appliance's on-box app registry — where a re-pull costs a loopback copy; see
// reclaimSupersededAppImages. Volumes are only removed when they are positively
// known to be unused (see pruneOrphanedVolumes); anything attributable to an
// installed app — stopped or running — or not attributable at all is left
// alone. App containers are ephemeral — their state lives in volumes/bind
// mounts — so stopping them is non-destructive.
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
	// in EMERGENCY safeCleanup re-runs every few seconds, and hammering the
	// sweep would starve app syncs without freeing anything new.
	registryGCTimeout     = 5 * time.Minute
	registryGCMinInterval = 15 * time.Minute
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

type action int

const (
	actNone action = iota
	actWarn
	actEmergency
)

// Config tunes the guard. Zero values fall back to sensible defaults. The
// thresholds are absolute free-byte floors (not percentages) because keeping the
// agent online needs an absolute headroom regardless of disk size.
type Config struct {
	DataRoot           string        // filesystem to watch (default /var/lib/docker)
	Interval           time.Duration // normal poll cadence (default 5m)
	EmergencyInterval  time.Duration // poll cadence while in EMERGENCY, for fast recovery (default 5s)
	WarnFreeBytes      int64         // run safe cleanup below this; also clears EMERGENCY at/above it (default 3 GiB)
	EmergencyFreeBytes int64         // enter EMERGENCY below this (default 1 GiB)
	LogMaxBytes        int64         // truncate container logs larger than this (default 50 MiB)
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
	// OnRecover is called once when the device leaves EMERGENCY, to reinstate the
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
	if c.EmergencyInterval == 0 {
		c.EmergencyInterval = 5 * time.Second
	}
	if c.WarnFreeBytes == 0 {
		c.WarnFreeBytes = 3 * gib
	}
	if c.EmergencyFreeBytes == 0 {
		c.EmergencyFreeBytes = 1 * gib
	}
	if c.LogMaxBytes == 0 {
		c.LogMaxBytes = 50 * mib
	}
}

// Guard runs the periodic disk cleanup and maintains the emergency state.
type Guard struct {
	cfg    Config
	docker Docker

	// registryGC asks the appliance-local appstore registry for an immediate
	// garbage collection. Wired via SetRegistryGC after the WAMP session
	// exists (the guard itself is built and first run before any connection);
	// nil = disabled. Mutex-guarded because the Run loop reads it while the
	// startup goroutine may still be wiring it.
	registryGCMu   sync.Mutex
	registryGC     func(ctx context.Context)
	lastRegistryGC time.Time
}

// New builds a Guard. docker may be nil (the container-dependent steps are then
// skipped).
func New(docker Docker, cfg Config) *Guard {
	cfg.withDefaults()
	return &Guard{cfg: cfg, docker: docker}
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

// decide maps free bytes to an action band. Pure function (testable). The warn
// band (between Emergency and Warn) is hysteresis: an active emergency is held
// there, so it only clears once free rises back to WarnFreeBytes.
func (g *Guard) decide(freeBytes int64) action {
	switch {
	case freeBytes < g.cfg.EmergencyFreeBytes:
		return actEmergency
	case freeBytes < g.cfg.WarnFreeBytes:
		return actWarn
	default:
		return actNone
	}
}

// CheckNow runs a single guard pass synchronously: it (re)evaluates free disk,
// updates the emergency state, and acts on it (safe cleanup, and on emergency
// stops non-platform containers). Call it once at startup BEFORE starting apps
// so the emergency gate is active before any container start, instead of racing
// the background Run loop.
func (g *Guard) CheckNow() {
	g.runOnce()
}

// Run executes one pass immediately, then repeats until ctx is done. While in
// EMERGENCY it polls at EmergencyInterval (a few seconds) so it recovers — and
// reinstates the apps — promptly once space is freed; otherwise at Interval.
func (g *Guard) Run(ctx context.Context) {
	for {
		g.runOnce()

		interval := g.cfg.Interval
		if IsEmergency() {
			interval = g.cfg.EmergencyInterval
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func (g *Guard) runOnce() {
	free := g.freeBytes()
	// Latch a critical reading BEFORE attempting any cleanup: the emergency
	// flag (app-start gate + EMERGENCY heartbeat status) must never wait on
	// cleanup steps, which all talk to a daemon that may be wedged. The steps
	// are deadline-bounded too, but minutes of accumulated timeouts would
	// still delay the report; the flag itself needs none of them.
	if g.decide(free) == actEmergency {
		g.updateEmergency(free)
	}
	if g.decide(free) != actNone {
		log.Warn().Int64("free_mb", free>>20).Str("path", g.cfg.DataRoot).Msg("diskguard: low disk, running safe cleanup")
		g.safeCleanup()
		free = g.freeBytes()
	}
	g.updateEmergency(free)
}

// updateEmergency enters or clears the emergency state with hysteresis: enter
// below EmergencyFreeBytes, clear at/above WarnFreeBytes, hold in between.
func (g *Guard) updateEmergency(free int64) {
	switch g.decide(free) {
	case actEmergency:
		if setEmergency(true) {
			log.Error().Int64("free_mb", free>>20).
				Msg("diskguard: ENTERING disk-emergency — failing new app start/build/download and stopping non-platform containers")
		}
		g.stopForeignContainers()
	case actNone:
		if setEmergency(false) {
			log.Info().Int64("free_mb", free>>20).Msg("diskguard: cleared disk-emergency state; reinstating app states")
			if g.cfg.OnRecover != nil {
				safe.Go(g.cfg.OnRecover)
			}
		}
	}
}

func (g *Guard) safeCleanup() {
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
	g.run("journalctl", "--vacuum-size=100M")
	if _, err := exec.LookPath("apt-get"); err == nil {
		g.run("apt-get", "clean")
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
	if err := exec.CommandContext(ctx, name, args...).Run(); err != nil { //nolint:gosec // G204: fixed system commands (systemctl/journalctl) chosen by the agent
		log.Warn().Err(err).Str("cmd", name).Msg("diskguard: command failed")
	}
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
