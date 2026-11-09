package diskguard

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reagent/common"
	"reagent/container"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	tMiB uint64 = 1 << 20
	tGiB uint64 = 1 << 30
)

// fakeFS is one filesystem of a fake host. Tests change free between passes
// (fakeHost.set) to fill or free it.
type fakeFS struct {
	key        string
	mount      string
	total      uint64
	free       uint64
	inodes     uint64
	inodesFree uint64
	readOnly   bool
	pseudo     bool
}

// fakeHost maps paths to the filesystems holding them; a path it does not
// know does not exist. Nothing reads or changes the real disk through it.
type fakeHost struct {
	mu    sync.Mutex
	paths map[string]*fakeFS
	// mounts overrides the mount point a path reads (a bind mount).
	mounts map[string]string
	// block, when set, makes stat of that path wait for the channel.
	block     map[string]chan struct{}
	statCalls map[string]int
}

func (h *fakeHost) stat(path string) (reading, error) {
	h.mu.Lock()
	h.statCalls[path]++
	wait := h.block[path]
	h.mu.Unlock()
	if wait != nil {
		<-wait
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	f, ok := h.paths[path]
	if !ok {
		return reading{}, fs.ErrNotExist
	}
	mount := f.mount
	if m, ok := h.mounts[path]; ok {
		mount = m
	}
	return reading{
		key:         f.key,
		mount:       mount,
		readOnly:    f.readOnly,
		pseudo:      f.pseudo,
		total:       f.total,
		used:        f.total - f.free,
		free:        f.free,
		inodesTotal: f.inodes,
		inodesFree:  f.inodesFree,
	}, nil
}

func (h *fakeHost) set(f *fakeFS, free uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f.free = free
}

// testDevice is a guard on a fake host, with every command, Docker call and
// file it touches recorded or confined to a temporary tree.
type testDevice struct {
	g        *Guard
	host     *fakeHost
	docker   *fakeDocker
	cmds     *[]string
	root     *fakeFS
	data     *fakeFS // nil on a single-disk host
	esp      *fakeFS // nil on a single-disk host
	logDir   string
	coreDir  string
	recovers chan struct{}
}

// newDevice builds a guard on a fake host. flockOS: a 256 MiB ESP at
// /boot/efi, a 4 GiB root slot holding /var/log and apt's cache, and a
// 500 GiB /data holding the journal, Docker's data-root and the agent's folder
// (the latter two through the /opt/ironflock bind mount). Otherwise a
// single-disk Linux host: a 64 GiB root holding all of it, with a /boot/efi
// directory that is no mount. Everything starts healthy.
func newDevice(t *testing.T, flockOS bool) *testDevice {
	t.Helper()
	t.Cleanup(func() { setEmergency(false) })

	tmp := t.TempDir()
	logDir := filepath.Join(tmp, "log")
	coreDir := filepath.Join(tmp, "coredump")
	journal := filepath.Join(logDir, "journal")
	for _, dir := range []string{logDir, coreDir, journal} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dataRoot := filepath.Join(tmp, "docker-apps") // never created: nothing to truncate
	agentDir := filepath.Join(tmp, "agent")

	d := &testDevice{
		docker: &fakeDocker{containers: []container.ContainerResult{
			{ID: "app", State: "running"},
			{ID: "appliance-caddy", State: "running", Labels: map[string]string{composeProjectLabel: applianceComposeProject}},
		}},
		cmds:     &[]string{},
		logDir:   logDir,
		coreDir:  coreDir,
		recovers: make(chan struct{}, 10),
	}
	d.host = &fakeHost{paths: map[string]*fakeFS{}, mounts: map[string]string{}, block: map[string]chan struct{}{}, statCalls: map[string]int{}}
	if flockOS {
		d.root = &fakeFS{key: "8:2", mount: "/", total: 4 * tGiB, free: 2 * tGiB, inodes: 262144, inodesFree: 200000}
		d.data = &fakeFS{key: "8:4", mount: "/data", total: 500 * tGiB, free: 400 * tGiB, inodes: 32 << 20, inodesFree: 30 << 20}
		d.esp = &fakeFS{key: "8:1", mount: "/boot/efi", total: 256 * tMiB, free: 240 * tMiB}
		d.host.paths = map[string]*fakeFS{
			"/":               d.root,
			"/data":           d.data,
			"/boot/efi":       d.esp,
			logDir:            d.root,
			journal:           d.data,
			"/var/cache/apt":  d.root,
			coreDir:           d.root,
			dataRoot:          d.data,
			agentDir:          d.data,
			"/var/lib/docker": d.root, // a leftover directory, no mount: not Docker's data
		}
		// What the walk-up finds for the paths reached through bind mounts.
		d.host.mounts[journal] = journal
		d.host.mounts[dataRoot] = "/opt/ironflock"
		d.host.mounts[agentDir] = "/opt/ironflock"
	} else {
		d.root = &fakeFS{key: "8:1", mount: "/", total: 64 * tGiB, free: 40 * tGiB, inodes: 4 << 20, inodesFree: 3 << 20}
		d.host.paths = map[string]*fakeFS{
			"/":              d.root,
			"/boot/efi":      d.root, // a plain directory: no ESP mounted
			logDir:           d.root,
			journal:          d.root,
			"/var/cache/apt": d.root,
			coreDir:          d.root,
			dataRoot:         d.root,
			agentDir:         d.root,
		}
	}

	g := New(d.docker, Config{
		DataRoot:  dataRoot,
		AgentDir:  agentDir,
		OnRecover: func() { d.recovers <- struct{}{} },
	})
	g.stat = d.host.stat
	g.runCmd = func(ctx context.Context, name string, args ...string) error {
		*d.cmds = append(*d.cmds, strings.Join(append([]string{name}, args...), " "))
		return nil
	}
	g.lookPath = func(file string) (string, error) { return "/usr/bin/" + file, nil }
	g.paths = hostPaths{
		root:         "/",
		data:         "/data",
		boot:         "/boot/efi",
		logs:         logDir,
		journal:      journal,
		varLibDocker: "/var/lib/docker",
		aptCache:     "/var/cache/apt",
		coredumps:    coreDir,
	}
	d.g = g
	return d
}

// writeLogs fills the device's log and core dump directories with active
// logs, rotated archives and a core dump.
func (d *testDevice) writeLogs(t *testing.T) {
	t.Helper()
	for _, name := range []string{"syslog", "syslog.1", "syslog.2.gz"} {
		if err := os.WriteFile(filepath.Join(d.logDir, name), []byte("log"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(d.coreDir, "core.app.0.abc.123.zst"), []byte("core"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// onlyApp reports whether the app container, and nothing else, was stopped
// (the fake keeps reporting it running, so a pass may stop it more than once).
func onlyApp(stopped []string) bool {
	return len(stopped) > 0 && !slices.ContainsFunc(stopped, func(id string) bool { return id != "app" })
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (d *testDevice) byMount(t *testing.T, fss []Filesystem, mount string) Filesystem {
	t.Helper()
	for _, f := range fss {
		if f.Mount == mount {
			return f
		}
	}
	t.Fatalf("no filesystem %q in %+v", mount, fss)
	return Filesystem{}
}

func TestThresholds(t *testing.T) {
	g := New(nil, Config{})
	cases := []struct {
		name            string
		total           uint64
		appSpace        bool
		warn, emergency uint64
	}{
		// App space: the absolute floors, whatever its size.
		{"app space: 500 GiB", 500 * tGiB, true, 3 * tGiB, 1 * tGiB},
		{"app space: 20 GiB", 20 * tGiB, true, 3 * tGiB, 1 * tGiB},
		{"app space: 8 GiB", 8 * tGiB, true, 3 * tGiB, 1 * tGiB},
		{"app space: 7 GiB IronFlockOS /data on a 16 GB SD card", 7 * tGiB, true, 3 * tGiB, 1 * tGiB},
		{"app space: smaller than the warn floor", 2 * tGiB, true, 3 * tGiB, 1 * tGiB},
		// System filesystems: a share of their size, capped by the floors.
		{"system: 40 GiB root reaches the caps", 40 * tGiB, false, 3 * tGiB, 1 * tGiB},
		{"system: 8 GiB root slot (nvidia)", 8 * tGiB, false, 8 * tGiB * 15 / 100, 8 * tGiB * 5 / 100}, // 1.2 GiB, 410 MiB
		{"system: 4 GiB root slot", 4 * tGiB, false, 644245094, 214748364},                             // 614.4 MiB, 204.8 MiB
		{"system: 256 MiB ESP", 256 * tMiB, false, 40265318, 13421772},                                 // 38.4 MiB, 12.8 MiB
		{"system: 32 MiB, the small floors hold a band", 32 * tMiB, false, 8 * tMiB, 2 * tMiB},         // shares: 4.8 MiB, 1.6 MiB
		{"system: no size at all", 0, false, 8 * tMiB, 2 * tMiB},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			warn, emergency := g.thresholds(c.total, c.appSpace)
			if warn != c.warn || emergency != c.emergency {
				t.Errorf("thresholds(%d, %v) = (%d, %d), want (%d, %d)", c.total, c.appSpace, warn, emergency, c.warn, c.emergency)
			}
		})
	}

	t.Run("configured floors", func(t *testing.T) {
		g := New(nil, Config{WarnFreeBytes: 10 << 30, EmergencyFreeBytes: 2 << 30})
		if warn, emergency := g.thresholds(500*tGiB, true); warn != 10*tGiB || emergency != 2*tGiB {
			t.Errorf("got (%d, %d), want the configured 10 GiB / 2 GiB", warn, emergency)
		}
	})
	t.Run("a band never inverts", func(t *testing.T) {
		g := New(nil, Config{WarnFreeBytes: 1 << 30, EmergencyFreeBytes: 2 << 30})
		if warn, emergency := g.thresholds(500*tGiB, true); warn != 2*tGiB || emergency != 2*tGiB {
			t.Errorf("got (%d, %d), want warn raised to the emergency floor", warn, emergency)
		}
	})
}

func TestMeasuredThresholds(t *testing.T) {
	cases := []struct {
		name            string
		flockOS         bool
		mount           string
		appSpace        bool
		warn, emergency uint64
	}{
		{"IronFlockOS /data is app space", true, "/data", true, 3 * tGiB, 1 * tGiB},
		{"IronFlockOS root slot without app roles is a system filesystem", true, "/", false, 644245094, 214748364},
		{"IronFlockOS ESP is a system filesystem", true, "/boot/efi", false, 40265318, 13421772},
		{"single-disk host: / carries docker, so it is app space", false, "/", true, 3 * tGiB, 1 * tGiB},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := newDevice(t, c.flockOS)
			f := d.byMount(t, d.g.measure(), c.mount)
			if f.isAppSpace() != c.appSpace || f.WarnFree != c.warn || f.EmergencyFree != c.emergency {
				t.Errorf("%s %v: app space %v, floors (%d, %d); want %v, (%d, %d)",
					f.Mount, f.Roles, f.isAppSpace(), f.WarnFree, f.EmergencyFree, c.appSpace, c.warn, c.emergency)
			}
		})
	}

	t.Run("small app space keeps the absolute floors", func(t *testing.T) {
		d := newDevice(t, true)
		d.data.total = 7 * tGiB // IronFlockOS /data on a 16 GB SD card
		for _, c := range []struct {
			free uint64
			want Level
		}{
			{3 * tGiB, LevelOK},
			{2 * tGiB, LevelWarn},        // a share of 7 GiB would warn only below 1.05 GiB
			{900 * tMiB, LevelEmergency}, // and declare an emergency only below 358 MiB
		} {
			d.host.set(d.data, c.free)
			if f := d.byMount(t, d.g.measure(), "/data"); f.Level != c.want {
				t.Errorf("level %q at %d MiB free of 7 GiB app space, want %q", f.Level, c.free>>20, c.want)
			}
		}
	})
}

func TestLevel(t *testing.T) {
	g := New(nil, Config{})
	root := func(free, inodes, inodesFree uint64) Filesystem {
		warn, emergency := g.thresholds(4*tGiB, false) // a root slot without app roles
		return Filesystem{Total: 4 * tGiB, Free: free, InodesTotal: inodes, InodesFree: inodesFree, WarnFree: warn, EmergencyFree: emergency}
	}
	esp := func(free uint64) Filesystem { // FAT: no inodes
		warn, emergency := g.thresholds(256*tMiB, false)
		return Filesystem{Total: 256 * tMiB, Free: free, WarnFree: warn, EmergencyFree: emergency}
	}
	const inodes = 262144

	cases := []struct {
		name         string
		fs           Filesystem
		wasEmergency bool
		want         Level
	}{
		{"root: plenty", root(2*tGiB, inodes, inodes/2), false, LevelOK},
		{"root: at the warn floor", root(644245094, inodes, inodes/2), false, LevelOK},
		{"root: just below the warn floor", root(644245094-1, inodes, inodes/2), false, LevelWarn},
		{"root: at the emergency floor", root(214748364, inodes, inodes/2), false, LevelWarn},
		{"root: just below the emergency floor", root(214748364-1, inodes, inodes/2), false, LevelEmergency},
		{"root: full", root(0, inodes, inodes/2), false, LevelEmergency},

		{"inodes: 10% free is ok", root(2*tGiB, inodes, inodes/10+1), false, LevelOK},
		{"inodes: below 10% free warns", root(2*tGiB, inodes, inodes*5/100), false, LevelWarn},
		{"inodes: below 3% free is an emergency with bytes to spare", root(2*tGiB, inodes, inodes*2/100), false, LevelEmergency},
		{"inodes: exhausted", root(2*tGiB, inodes, 0), false, LevelEmergency},
		{"inodes: the worse of bytes and inodes", root(300*tMiB, inodes, inodes/2), false, LevelWarn},

		{"FAT without inodes: plenty", esp(200 * tMiB), false, LevelOK},
		{"FAT without inodes: warn", esp(20 * tMiB), false, LevelWarn},
		{"FAT without inodes: emergency", esp(10 * tMiB), false, LevelEmergency},

		{"hysteresis: an emergency holds in the warn band", root(400*tMiB, inodes, inodes/2), true, LevelEmergency},
		{"hysteresis: it clears at the warn floor", root(644245094, inodes, inodes/2), true, LevelOK},
		{"hysteresis: inodes back in the warn band hold it too", root(2*tGiB, inodes, inodes*5/100), true, LevelEmergency},
		{"hysteresis: no emergency before, the band is only warn", root(400*tMiB, inodes, inodes/2), false, LevelWarn},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := g.level(c.fs, c.wasEmergency); got != c.want {
				t.Errorf("level = %q, want %q", got, c.want)
			}
		})
	}
}

func TestMeasureFlockOS(t *testing.T) {
	d := newDevice(t, true)
	fss := d.g.measure()

	want := []struct {
		mount string
		roles []string
	}{
		{"/", []string{RoleRoot, RoleLogs}},
		{"/data", []string{RoleData, RoleJournal, RoleDocker, RoleAgent}},
		{"/boot/efi", []string{RoleBoot}},
	}
	if len(fss) != len(want) {
		t.Fatalf("watched %+v, want %d filesystems", fss, len(want))
	}
	for i, w := range want {
		if fss[i].Mount != w.mount || !slices.Equal(fss[i].Roles, w.roles) {
			t.Errorf("filesystem %d = %s %v, want %s %v", i, fss[i].Mount, fss[i].Roles, w.mount, w.roles)
		}
		if fss[i].Level != LevelOK {
			t.Errorf("%s: level %q, want ok", fss[i].Mount, fss[i].Level)
		}
	}
	// The leftover /var/lib/docker on the root slot is no mount: the root
	// slot must not count as Docker's.
	if slices.Contains(fss[0].Roles, RoleDocker) {
		t.Errorf("root slot has role docker: %v", fss[0].Roles)
	}
}

func TestMeasureSingleDiskHost(t *testing.T) {
	d := newDevice(t, false)
	fss := d.g.measure()
	if len(fss) != 1 {
		t.Fatalf("watched %+v, want the one root filesystem", fss)
	}
	want := []string{RoleRoot, RoleLogs, RoleJournal, RoleDocker, RoleAgent}
	if fss[0].Mount != "/" || !slices.Equal(fss[0].Roles, want) {
		t.Errorf("got %s %v, want / %v (no boot: /boot/efi is no mount there)", fss[0].Mount, fss[0].Roles, want)
	}
}

func TestMeasureDedupesByDevice(t *testing.T) {
	d := newDevice(t, true)
	// Docker's data-root and the agent's folder read /data's device under
	// the bind mount's name: folded into /data (see TestMeasureFlockOS). A
	// separate disk at a path below /data is a filesystem of its own.
	ssd := &fakeFS{key: "259:1", mount: "/data/ssd", total: 1000 * tGiB, free: 900 * tGiB}
	d.host.paths[d.g.cfg.AgentDir] = ssd
	delete(d.host.mounts, d.g.cfg.AgentDir)
	fss := d.g.measure()
	if len(fss) != 4 {
		t.Fatalf("watched %d filesystems, want 4: %+v", len(fss), fss)
	}
	if f := d.byMount(t, fss, "/data/ssd"); !slices.Equal(f.Roles, []string{RoleAgent}) {
		t.Errorf("/data/ssd roles %v, want [agent]", f.Roles)
	}
	if f := d.byMount(t, fss, "/data"); slices.Contains(f.Roles, RoleAgent) {
		t.Errorf("/data roles %v, want no agent", f.Roles)
	}
}

func TestMeasureSkipsReadOnlyAndPseudo(t *testing.T) {
	d := newDevice(t, true)
	d.root.readOnly = true // e.g. an overlayroot: nothing to watch
	tmpfsLog := &fakeFS{key: "0:30", mount: d.logDir, total: 64 * tMiB, free: 1 * tMiB, pseudo: true}
	d.host.paths[d.logDir] = tmpfsLog
	fss := d.g.measure()
	for _, f := range fss {
		if f.Mount == "/" || f.Mount == d.logDir {
			t.Errorf("watched %s, which is read-only or pseudo", f.Mount)
		}
	}
	if len(fss) != 2 {
		t.Errorf("watched %+v, want /data and /boot/efi", fss)
	}
}

func TestEmergencyFromRootFullOfLogs(t *testing.T) {
	d := newDevice(t, true)
	d.writeLogs(t)

	// The root slot fills up with logs; /data and the ESP are fine.
	d.host.set(d.root, 100*tMiB)
	d.g.runOnce()

	if !IsEmergency() {
		t.Fatal("expected a device-wide emergency from the root slot")
	}
	if !onlyApp(d.docker.stopped) {
		t.Errorf("stopped %v, want only the app container (the appliance stack stays)", d.docker.stopped)
	}
	if d.docker.prunedImages != 0 || d.docker.prunedCache != 0 {
		t.Errorf("Docker steps ran (%d image, %d cache prunes) though Docker's filesystem is fine", d.docker.prunedImages, d.docker.prunedCache)
	}
	if !slices.Equal(*d.cmds, []string{"apt-get clean"}) {
		t.Errorf("commands %v, want only apt-get clean (the journal is on /data)", *d.cmds)
	}
	if !exists(filepath.Join(d.logDir, "syslog")) {
		t.Error("the active log was removed")
	}
	for _, name := range []string{"syslog.1", "syslog.2.gz"} {
		if exists(filepath.Join(d.logDir, name)) {
			t.Errorf("rotated archive %s was kept", name)
		}
	}
	if exists(filepath.Join(d.coreDir, "core.app.0.abc.123.zst")) {
		t.Error("the core dump was kept")
	}

	// Freed into the warn band: the emergency holds (hysteresis).
	d.host.set(d.root, 400*tMiB)
	d.g.runOnce()
	if !IsEmergency() {
		t.Fatal("expected the emergency to hold in the root slot's warn band")
	}
	select {
	case <-d.recovers:
		t.Fatal("OnRecover ran while the emergency holds")
	default:
	}

	// Back at the root slot's warn floor (614.4 MiB): recovered.
	d.host.set(d.root, 700*tMiB)
	d.g.runOnce()
	if IsEmergency() {
		t.Fatal("expected the emergency cleared once the root slot recovered")
	}
	select {
	case <-d.recovers:
	case <-time.After(5 * time.Second):
		t.Fatal("OnRecover did not run")
	}
}

func TestEmergencyClearsOnlyWhenEveryFilesystemRecovered(t *testing.T) {
	d := newDevice(t, true)
	d.host.set(d.root, 100*tMiB)
	d.host.set(d.data, 500*tMiB)
	d.g.runOnce()
	if !IsEmergency() {
		t.Fatal("expected an emergency")
	}

	d.host.set(d.data, 300*tGiB)
	d.g.runOnce()
	if !IsEmergency() {
		t.Fatal("cleared while the root slot is still in emergency")
	}

	d.host.set(d.root, 2*tGiB)
	d.g.runOnce()
	if IsEmergency() {
		t.Fatal("expected the emergency cleared")
	}
	select {
	case <-d.recovers:
	case <-time.After(5 * time.Second):
		t.Fatal("OnRecover did not run")
	}
}

func TestESPAtEmergencyIsReportedButStopsNoApps(t *testing.T) {
	d := newDevice(t, true)
	d.host.set(d.esp, 5*tMiB)
	d.g.runOnce()

	if IsEmergency() {
		t.Fatal("a full ESP put the device into its emergency; apps cannot fill it")
	}
	if len(d.docker.stopped) != 0 {
		t.Errorf("stopped %v for a full ESP", d.docker.stopped)
	}
	// Nothing the guard cleans lives on the ESP.
	if len(*d.cmds) != 0 || d.docker.prunedImages != 0 {
		t.Errorf("cleanup ran for the ESP: commands %v, %d prunes", *d.cmds, d.docker.prunedImages)
	}
	// The platform still hears about it.
	fields := d.g.statsFields()
	if fields["disk_level"] != "emergency" {
		t.Errorf("disk_level %v, want emergency", fields["disk_level"])
	}
	for _, f := range fields["filesystems"].([]common.Dict) {
		if f["mount"] == "/boot/efi" && f["level"] != "emergency" {
			t.Errorf("ESP level %v, want emergency", f["level"])
		}
	}
}

func TestSeparateLogFilesystemAtEmergencyStopsNoApps(t *testing.T) {
	d := newDevice(t, true)
	d.writeLogs(t)
	// A 50 MiB zram /var/log (Armbian, log2ram), nearly full.
	zram := &fakeFS{key: "252:1", mount: d.logDir, total: 50 * tMiB, free: 1 * tMiB, inodes: 12800, inodesFree: 12000}
	d.host.paths[d.logDir] = zram
	d.g.runOnce()

	f := d.byMount(t, d.g.Report(), d.logDir)
	if !slices.Equal(f.Roles, []string{RoleLogs}) || f.Level != LevelEmergency {
		t.Fatalf("log filesystem %v at %q, want [logs] at emergency", f.Roles, f.Level)
	}
	if IsEmergency() {
		t.Fatal("a full log-only filesystem put the device into its emergency")
	}
	if len(d.docker.stopped) != 0 {
		t.Errorf("stopped %v for a full log filesystem", d.docker.stopped)
	}
	if exists(filepath.Join(d.logDir, "syslog.1")) || exists(filepath.Join(d.logDir, "syslog.2.gz")) {
		t.Error("the rotated archives were kept; the log filesystem's cleanup must still run")
	}
	if !exists(filepath.Join(d.logDir, "syslog")) {
		t.Error("the active log was removed")
	}
	// The core dumps are on the root slot, which is fine.
	if !exists(filepath.Join(d.coreDir, "core.app.0.abc.123.zst")) {
		t.Error("the core dumps were removed though their filesystem is fine")
	}
}

func TestStopsApps(t *testing.T) {
	cases := []struct {
		roles []string
		want  bool
	}{
		{[]string{RoleRoot}, true},
		{[]string{RoleRoot, RoleLogs}, true},
		{[]string{RoleData, RoleJournal}, true},
		{[]string{RoleDocker}, true},
		{[]string{RoleAgent}, true},
		{[]string{RoleBoot}, false},
		{[]string{RoleLogs}, false},
		{[]string{RoleLogs, RoleJournal}, false},
		{[]string{RoleBoot, RoleLogs}, false},
	}
	for _, c := range cases {
		if got := (Filesystem{Roles: c.roles}).stopsApps(); got != c.want {
			t.Errorf("stopsApps(%v) = %v, want %v", c.roles, got, c.want)
		}
	}
}

func TestCleanupSelection(t *testing.T) {
	type want struct {
		docker    bool
		cmds      []string
		archives  bool // rotated logs removed
		coreDumps bool
	}
	cases := []struct {
		name    string
		flockOS bool
		noApt   bool
		fill    func(d *testDevice)
		want    want
	}{
		{"IronFlockOS: /data low", true, false,
			func(d *testDevice) { d.host.set(d.data, 2*tGiB) },
			want{docker: true, cmds: []string{"journalctl --vacuum-size=100M"}}},
		{"IronFlockOS: root slot low", true, false,
			func(d *testDevice) { d.host.set(d.root, 400*tMiB) },
			want{cmds: []string{"apt-get clean"}}},
		{"IronFlockOS: root slot in emergency", true, false,
			func(d *testDevice) { d.host.set(d.root, 100*tMiB) },
			want{cmds: []string{"apt-get clean"}, archives: true, coreDumps: true}},
		{"IronFlockOS: ESP in emergency", true, false,
			func(d *testDevice) { d.host.set(d.esp, 5*tMiB) },
			want{}},
		{"IronFlockOS: root slot out of inodes", true, false,
			func(d *testDevice) { d.root.inodesFree = 1000 },
			want{cmds: []string{"apt-get clean"}, archives: true, coreDumps: true}},
		{"single disk: root low", false, false,
			func(d *testDevice) { d.host.set(d.root, 2*tGiB) },
			want{docker: true, cmds: []string{"journalctl --vacuum-size=100M", "apt-get clean"}}},
		{"single disk: root in emergency, no apt", false, true,
			func(d *testDevice) { d.host.set(d.root, 500*tMiB) },
			want{docker: true, cmds: []string{"journalctl --vacuum-size=100M"}, archives: true, coreDumps: true}},
		{"single disk: healthy", false, false,
			func(d *testDevice) {},
			want{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := newDevice(t, c.flockOS)
			d.writeLogs(t)
			if c.noApt {
				d.g.lookPath = func(string) (string, error) { return "", fs.ErrNotExist }
			}
			c.fill(d)
			d.g.cleanup(d.g.measure())

			if got := d.docker.prunedImages > 0 && d.docker.prunedCache > 0; got != c.want.docker {
				t.Errorf("Docker steps ran: %v, want %v", got, c.want.docker)
			}
			if !slices.Equal(*d.cmds, c.want.cmds) {
				t.Errorf("commands %v, want %v", *d.cmds, c.want.cmds)
			}
			if got := !exists(filepath.Join(d.logDir, "syslog.1")); got != c.want.archives {
				t.Errorf("rotated logs removed: %v, want %v", got, c.want.archives)
			}
			if got := !exists(filepath.Join(d.coreDir, "core.app.0.abc.123.zst")); got != c.want.coreDumps {
				t.Errorf("core dumps removed: %v, want %v", got, c.want.coreDumps)
			}
			if !exists(filepath.Join(d.logDir, "syslog")) {
				t.Error("the active log was removed")
			}
		})
	}
}

func TestCleanupCadence(t *testing.T) {
	d := newDevice(t, true)
	d.host.set(d.data, 2*tGiB) // warn
	d.g.runOnce()
	d.g.runOnce()
	if d.docker.prunedImages != 1 {
		t.Errorf("pruned %d times in two warn passes, want once per Interval", d.docker.prunedImages)
	}

	d.host.set(d.data, 500*tMiB) // emergency: every pass cleans
	d.g.runOnce()
	d.g.runOnce()
	if d.docker.prunedImages != 3 {
		t.Errorf("pruned %d times, want every emergency pass to clean", d.docker.prunedImages)
	}
}

func TestCheckNowLatchesWithoutCleanup(t *testing.T) {
	d := newDevice(t, true)
	d.host.set(d.root, 100*tMiB)
	d.g.CheckNow()
	if !IsEmergency() {
		t.Fatal("expected CheckNow to latch the emergency")
	}
	if !onlyApp(d.docker.stopped) {
		t.Errorf("stopped %v, want the app container", d.docker.stopped)
	}
	if len(*d.cmds) != 0 || d.docker.prunedImages != 0 {
		t.Errorf("CheckNow cleaned up (commands %v, %d prunes); that is Run's job", *d.cmds, d.docker.prunedImages)
	}
}

func TestRemoveRotatedLogs(t *testing.T) {
	d := newDevice(t, false)
	files := map[string]bool{ // path -> removed
		"syslog":                        false,
		"syslog.1":                      true,
		"syslog.2.gz":                   true,
		"kern.log":                      false,
		"Xorg.0.log":                    false,
		"Xorg.0.log.old":                true,
		"dmesg.0":                       true,
		"nginx/access.log":              false,
		"nginx/access.log.1":            true,
		"nginx/error.log.14.gz":         true,
		"apt/history.log":               false,
		"journal/abc/system.journal":    false,
		"journal/abc/system@x.journal~": false,
	}
	for name := range files {
		path := filepath.Join(d.logDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A symlink named like an archive points at an active log: never followed or removed.
	if err := os.Symlink(filepath.Join(d.logDir, "syslog"), filepath.Join(d.logDir, "link.1")); err != nil {
		t.Fatal(err)
	}

	d.g.removeRotatedLogs(d.logDir)

	for name, removed := range files {
		if got := !exists(filepath.Join(d.logDir, name)); got != removed {
			t.Errorf("%s removed: %v, want %v", name, got, removed)
		}
	}
	if _, err := os.Lstat(filepath.Join(d.logDir, "link.1")); err != nil {
		t.Error("the symlink was removed")
	}
}

func TestRemoveCoreDumps(t *testing.T) {
	d := newDevice(t, false)
	if err := os.WriteFile(filepath.Join(d.coreDir, "core.a.0.b.1.zst"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(d.coreDir, "keep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	d.g.removeCoreDumps(d.coreDir)
	if exists(filepath.Join(d.coreDir, "core.a.0.b.1.zst")) {
		t.Error("the core dump was kept")
	}
	if !exists(filepath.Join(sub, "file")) {
		t.Error("a file below the core dump directory was removed")
	}
}

func TestReportRefreshesAndWakesTheGuard(t *testing.T) {
	d := newDevice(t, true)
	d.g.reportMaxAge = 0 // every report reads afresh
	d.g.runOnce()        // all ok

	d.host.set(d.root, 100*tMiB)
	fss := d.g.Report()
	if f := d.byMount(t, fss, "/"); f.Level != LevelEmergency {
		t.Errorf("report: root level %q, want emergency", f.Level)
	}
	select {
	case <-d.g.wake:
	default:
		t.Error("the report did not wake the guard")
	}
	if IsEmergency() {
		t.Error("a report changed the emergency state; only a pass does")
	}

	// Once a pass has seen it, the same reading wakes nobody.
	d.g.runOnce()
	d.g.Report()
	select {
	case <-d.g.wake:
		t.Error("woke the guard for a level its last pass already saw")
	default:
	}
}

func TestReportDoesNotWaitForAHungStat(t *testing.T) {
	d := newDevice(t, true)
	d.g.runOnce()
	d.g.reportMaxAge = 0
	d.g.reportTimeout = 50 * time.Millisecond

	release := make(chan struct{})
	d.host.mu.Lock()
	d.host.block["/boot/efi"] = release
	d.host.mu.Unlock()
	defer close(release)

	start := time.Now()
	first := d.g.Report()
	second := d.g.Report()
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("reports took %v with a hung statfs", elapsed)
	}
	if len(first) != 3 || len(second) != 3 {
		t.Errorf("got %d and %d filesystems, want the last pass's 3", len(first), len(second))
	}
	d.host.mu.Lock()
	calls := d.host.statCalls["/boot/efi"]
	d.host.mu.Unlock()
	if calls != 2 { // the pass's, and one hung refresh: the second report did not start another
		t.Errorf("stat of the hung path ran %d times, want 2", calls)
	}
}

func TestStatsFields(t *testing.T) {
	d := newDevice(t, true)
	d.host.set(d.root, 400*tMiB) // warn
	got := d.g.statsFields()

	if len(got) != 2 || got["disk_level"] != "warn" {
		t.Fatalf("stats fields %v, want filesystems and disk_level warn", got)
	}
	list, ok := got["filesystems"].([]common.Dict)
	if !ok || len(list) != 3 {
		t.Fatalf("filesystems = %#v, want 3 entries", got["filesystems"])
	}
	keys := []string{"mount", "roles", "total", "used", "free", "inodes_total", "inodes_free", "warn_free", "emergency_free", "level"}
	for _, entry := range list {
		if len(entry) != len(keys) {
			t.Errorf("entry %v has %d keys, want exactly %v", entry, len(entry), keys)
		}
		for _, k := range keys {
			if _, ok := entry[k]; !ok {
				t.Errorf("entry %v lacks %q", entry, k)
			}
		}
	}
	root := list[0]
	if root["mount"] != "/" || root["level"] != "warn" || root["free"] != 400*tMiB || root["total"] != 4*tGiB ||
		root["used"] != 4*tGiB-400*tMiB || root["warn_free"] != uint64(644245094) || root["emergency_free"] != uint64(214748364) ||
		root["inodes_total"] != uint64(262144) || root["inodes_free"] != uint64(200000) ||
		!slices.Equal(root["roles"].([]string), []string{"root", "logs"}) {
		t.Errorf("root entry %v", root)
	}
	esp := list[2]
	if esp["mount"] != "/boot/efi" || esp["inodes_total"] != uint64(0) || esp["level"] != "ok" {
		t.Errorf("ESP entry %v", esp)
	}
	if _, err := json.Marshal(got); err != nil {
		t.Errorf("stats fields do not marshal: %v", err)
	}
}

func TestStatsFieldsEmpty(t *testing.T) {
	d := newDevice(t, true)
	d.host.mu.Lock()
	d.host.paths = map[string]*fakeFS{}
	d.host.mu.Unlock()
	if got := d.g.statsFields(); len(got) != 0 {
		t.Errorf("stats fields %v with nothing readable, want none", got)
	}

	prev := reporter.Swap(nil)
	defer reporter.Store(prev)
	if got := StatsFields(); len(got) != 0 {
		t.Errorf("StatsFields() = %v without a guard, want none", got)
	}
}
