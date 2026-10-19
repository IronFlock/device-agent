package migrate

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reagent/layout"
	"reagent/linuxservice"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testVersion = "1.3.0"

// errCrash is the panic a fake raises to cut the power mid-migration.
var errCrash = errors.New("power cut")

// device is a legacy ironflock-init install below a temporary root, with a
// fake systemd that keeps the unit links in the tree as the real one does.
type device struct {
	t     *testing.T
	root  string
	calls []string
	now   time.Time
	// failOn makes the systemctl call with these args fail.
	failOn map[string]error
	// crashAt panics on the n-th systemctl call (1-based; 0 = never).
	crashAt int
	// agentVersion is what the new unit's agent reports ready as ("" =
	// it never gets ready).
	agentVersion string
	agentPID     int
	// restartAfter makes the agent restart once the clock passed it.
	restartAfter time.Duration
	started      time.Time
	mounts       []string
	sameFS       bool
	// mountsAfterDockerStop stay after Docker stopped (live-restore).
	mountsAfterDockerStop []string
	// notLoaded units are unknown to systemd: stopping one fails.
	notLoaded map[string]bool
}

func newDevice(t *testing.T) *device {
	t.Helper()
	// Canonical: the migration resolves the config link (/var is a link on
	// macOS).
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	d := &device{
		t:            t,
		root:         root,
		now:          time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC),
		failOn:       map[string]error{},
		notLoaded:    map[string]bool{},
		agentVersion: testVersion,
		sameFS:       true,
		mounts:       []string{"/", "/boot", "/opt/reagent/docker-apps/overlay2/abc/merged"},
	}
	unitFile := strings.Join([]string{
		"[Service]",
		"ExecStart=/opt/reagent/Reagent-active -config /opt/reagent/device-config.flock -appsDir /apps -debug",
		"",
	}, "\n")
	d.file(layout.LegacyUnitFile, unitFile, 0o644)
	d.file(filepath.Join(legacyServicesDir, legacyManagerUnit), "[Service]\nExecStart=/opt/reagent/reswarmify/scripts/reagent-manager.sh\n", 0o644)
	d.file("/opt/reagent/reswarmify/scripts/reagent-manager.sh", "#!/bin/sh\n", 0o755)
	d.link("/etc/systemd/system/reagent.service", d.p(layout.LegacyUnitFile))
	d.link("/etc/systemd/system/reagent-manager.service", d.p(filepath.Join(legacyServicesDir, legacyManagerUnit)))
	d.file("/etc/systemd/system/reagent.service.d/proxy.conf", "[Service]\nEnvironment=HTTPS_PROXY=http://proxy:3128\n", 0o644)
	d.file(layout.LegacyEnrolled, "", 0o644)
	d.file("/opt/reagent/reagent-v1.2.13", "old agent", 0o755)
	d.file("/opt/reagent/reagent-v1.3.0", "agent 1.3.0", 0o755)
	d.link("/opt/reagent/Reagent-active", d.p("/opt/reagent/reagent-v1.3.0"))
	d.file("/opt/reagent/reagent.db", "database", 0o644)
	d.file("/opt/reagent/reagent.db-wal", "wal", 0o644)
	d.file("/opt/reagent/frpc", "frpc", 0o755)
	d.file("/opt/reagent/docker-apps/overlay2/abc/diff/file", "layer", 0o644)
	d.file("/boot/demo_swarm.flock", `{"secret":"s3cr3t"}`, 0o644)
	d.link("/opt/reagent/device-config.flock", d.p("/boot/demo_swarm.flock"))
	d.file(bootDeviceINI, "secret=s3cr3t\n", 0o644)
	d.file(daemonJSON, `{"data-root": "/opt/reagent/docker-apps", "log-driver": "json-file"}`, 0o644)
	d.file(legacyLog, "agent log", 0o644)
	d.file("/var/log/reagent-2026-10-17.log", "rotated", 0o644)
	d.file(legacyFrpcLog, "frpc log", 0o644)
	d.file("/apps/app/data", "app data", 0o644)
	return d
}

func (d *device) p(path string) string { return filepath.Join(d.root, path) }

func (d *device) file(path, content string, mode os.FileMode) {
	d.t.Helper()
	require.NoError(d.t, os.MkdirAll(filepath.Dir(d.p(path)), 0o755))
	require.NoError(d.t, os.WriteFile(d.p(path), []byte(content), mode))
	require.NoError(d.t, os.Chmod(d.p(path), mode))
}

func (d *device) link(path, target string) {
	d.t.Helper()
	require.NoError(d.t, os.MkdirAll(filepath.Dir(d.p(path)), 0o755))
	require.NoError(d.t, os.Symlink(target, d.p(path)))
}

func (d *device) env() Env {
	return Env{
		Root:      d.root,
		Systemctl: d.systemctl,
		MainPID: func(unit string) (int, error) {
			if unit != linuxservice.UnitName {
				return 0, nil
			}
			if d.agentPID != 0 && d.restartAfter != 0 && d.now.Sub(d.started) >= d.restartAfter {
				d.agentPID++
			}
			return d.agentPID, nil
		},
		UnitLoaded: func(unit string) bool { return !d.notLoaded[unit] },
		Unmount: func(path string) error {
			d.calls = append(d.calls, "umount "+path)
			var kept []string
			for _, m := range d.mounts {
				if m != path {
					kept = append(kept, m)
				}
			}
			d.mounts = kept
			return nil
		},
		MountPoints: func() ([]string, error) { return d.mounts, nil },
		SameFilesystem: func(a, b string) (bool, error) {
			return d.sameFS, nil
		},
		Now:           func() time.Time { return d.now },
		Sleep:         func(dur time.Duration) { d.now = d.now.Add(dur) },
		Version:       testVersion,
		HealthTimeout: time.Minute,
		StableFor:     time.Minute,
	}
}

func (d *device) systemctl(args ...string) error {
	call := strings.Join(args, " ")
	d.calls = append(d.calls, call)
	if d.crashAt != 0 && len(d.calls) == d.crashAt {
		panic(errCrash)
	}
	if err := d.failOn[call]; err != nil {
		return err
	}
	if args[0] == "stop" && d.notLoaded[args[len(args)-1]] {
		return fmt.Errorf("Unit %s not loaded", args[len(args)-1])
	}
	unitLink := func(unit string) string { return d.p(filepath.Join("/etc/systemd/system", unit)) }
	switch args[0] {
	case "disable":
		for _, unit := range args[1:] {
			// systemd removes the link of a linked unit file on disable.
			if target, err := os.Readlink(unitLink(unit)); err == nil && target != "/dev/null" {
				_ = os.Remove(unitLink(unit))
			}
		}
	case "mask":
		for _, unit := range args[1:] {
			if info, err := os.Lstat(unitLink(unit)); err == nil && info.Mode().IsRegular() {
				return fmt.Errorf("Failed to mask unit: File %s already exists", unitLink(unit))
			}
			_ = os.Remove(unitLink(unit))
			require.NoError(d.t, os.Symlink("/dev/null", unitLink(unit)))
		}
	case "unmask":
		for _, unit := range args[1:] {
			if target, err := os.Readlink(unitLink(unit)); err == nil && target == "/dev/null" {
				_ = os.Remove(unitLink(unit))
			}
		}
	case "enable":
		for _, unit := range args[1:] {
			if strings.HasPrefix(unit, "/") {
				// a unit file outside the search path: linked in
				if _, err := os.Lstat(unitLink(filepath.Base(unit))); err != nil {
					require.NoError(d.t, os.Symlink(d.p(unit), unitLink(filepath.Base(unit))))
				}
			}
		}
	case "start":
		if args[len(args)-1] == linuxservice.UnitName && d.agentVersion != "" && d.agentPID == 0 {
			d.agentPID = 4242
			d.started = d.now
			require.NoError(d.t, os.MkdirAll(filepath.Dir(d.p(connectedFile)), 0o755))
			require.NoError(d.t, os.WriteFile(d.p(connectedFile), []byte(fmt.Sprintf("%d %s\n", d.agentPID, d.agentVersion)), 0o644))
		}
	case "stop":
		if args[len(args)-1] == linuxservice.UnitName {
			d.agentPID = 0
		}
		if args[len(args)-1] == "docker.service" && d.mountsAfterDockerStop != nil {
			d.mounts = d.mountsAfterDockerStop
		} else if args[len(args)-1] == "docker.service" {
			d.mounts = []string{"/", "/boot"}
		}
	}
	return nil
}

// stage starts a migration as the legacy agent would.
func (d *device) stage(e Env) {
	d.t.Helper()
	d.file("/proc/self/exe", "agent 1.3.0", 0o755)
	require.NoError(d.t, Check(e))
	require.NoError(d.t, Stage(e, d.p("/proc/self/exe")))
}

// snapshot is the device's files and links (not directories), minus what
// the migration keeps for itself and the stand-in for the running binary.
func (d *device) snapshot() map[string]string {
	d.t.Helper()
	skip := []string{JournalPath, migrateDir, migrateUnitPath, "/proc", "/run"}
	out := map[string]string{}
	err := filepath.WalkDir(d.root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := "/" + strings.TrimPrefix(strings.TrimPrefix(path, d.root), "/")
		for _, s := range skip {
			if rel == s || strings.HasPrefix(rel, s+"/") {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			out[rel] = "-> " + strings.TrimPrefix(target, d.root)
		case info.Mode().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out[rel] = fmt.Sprintf("%v %s", info.Mode().Perm(), data)
		}
		return nil
	})
	require.NoError(d.t, err)
	return out
}

func (d *device) journal() *Journal {
	d.t.Helper()
	j, err := LoadJournal(d.root)
	require.NoError(d.t, err)
	require.NotNil(d.t, j)
	return j
}

func TestMigrateMovesALegacyInstall(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	require.NoError(t, Run(e))

	assert.True(t, d.journal().Done)
	assert.Equal(t, layout.Current, layout.Detect(d.root).Kind)

	agentDir := layout.AgentDir
	snap := d.snapshot()
	want := map[string]string{
		agentDir + "/device-agent":                       "-rwxr-xr-x agent 1.3.0",
		agentDir + "/device-agent-prev":                  "-rwxr-xr-x agent 1.3.0",
		agentDir + "/device-agent.db":                    "-rw-r--r-- database",
		agentDir + "/device-agent.db-wal":                "-rw-r--r-- wal",
		agentDir + "/device-config.flock":                `-rw------- {"secret":"s3cr3t"}`,
		agentDir + "/enrolled":                           "-rw-r--r-- ",
		agentDir + "/frpc":                               "-rwxr-xr-x frpc",
		agentDir + "/docker-apps/overlay2/abc/diff/file": "-rw-r--r-- layer",
		agentDir + "/repair.sh":                          "-rwxr-xr-x " + linuxservice.DefaultPaths().RepairScript(),
		"/opt/reagent":                                   "-> " + agentDir,
		linuxservice.UnitPath:                            "-rw-r--r-- " + withArgs("-debug").Unit(),
		linuxservice.DropInDir + "/proxy.conf":           "-rw-r--r-- [Service]\nEnvironment=HTTPS_PROXY=http://proxy:3128\n",
		daemonJSON:                                       "-rw-r--r-- {\n  \"data-root\": \"/opt/ironflock/device-agent/docker-apps\",\n  \"log-driver\": \"json-file\"\n}\n",
		"/var/log/ironflock/device-agent.log":            "-rw-r--r-- agent log",
		"/var/log/ironflock/device-agent-2026-10-17.log": "-rw-r--r-- rotated",
		"/var/log/ironflock/frpc.log":                    "-rw-r--r-- frpc log",
		"/apps/app/data":                                 "-rw-r--r-- app data",
	}
	assert.Equal(t, want, snap)

	// The legacy units are gone, not left masked; the new one is enabled.
	assert.Contains(t, d.calls, "mask "+legacyManagerUnit+" "+legacyAgentUnit)
	assert.Contains(t, d.calls, "unmask "+legacyManagerUnit+" "+legacyAgentUnit)
	assert.Contains(t, d.calls, "enable "+linuxservice.UnitName)
	assert.Contains(t, d.calls, "disable "+MigrateUnitName)
	// Docker held the old folder: stopped before the move, started after.
	assert.Less(t, index(d.calls, "stop docker.service"), index(d.calls, "start docker.service"))
}

func index(calls []string, call string) int {
	for i, c := range calls {
		if c == call {
			return i
		}
	}
	return -1
}

// A failure at any step brings back the device exactly as it was, and the
// legacy agent runs again.
func TestMigrateRollsBackAtEveryStep(t *testing.T) {
	for _, name := range StepNames() {
		t.Run(name, func(t *testing.T) {
			d := newDevice(t)
			before := d.snapshot()
			e := d.env()
			d.stage(e)
			e.Fault = name

			err := Run(e)
			require.ErrorContains(t, err, "fault injected at "+name)

			assert.Equal(t, before, d.snapshot())
			assert.Equal(t, layout.Legacy, layout.Detect(d.root).Kind)
			j := d.journal()
			assert.True(t, j.Failed())
			assert.Contains(t, j.Error, name)
			assert.Contains(t, LastError(d.root), name)
			assert.Equal(t, "start "+legacyManagerUnit, lastCall(d.calls, "start "+legacyManagerUnit))
			assert.Contains(t, d.calls, "disable "+MigrateUnitName)
			// The new unit must not stay enabled next to the legacy ones.
			if i := index(d.calls, "enable "+linuxservice.UnitName); i >= 0 {
				assert.Greater(t, lastIndex(d.calls, "disable "+linuxservice.UnitName), i)
			}
			// Docker runs again whenever the migration stopped it, and it
			// was stopped again before the folder moved back under it.
			if i := index(d.calls, "stop docker.service"); i >= 0 {
				assert.Greater(t, lastIndex(d.calls, "start docker.service"), i)
				if started := index(d.calls, "start docker.service"); started < lastIndex(d.calls, "start docker.service") {
					assert.Greater(t, lastIndex(d.calls, "stop docker.service"), started)
				}
			}
		})
	}
}

func lastCall(calls []string, call string) string {
	if lastIndex(calls, call) >= 0 {
		return call
	}
	return ""
}

func lastIndex(calls []string, call string) int {
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i] == call {
			return i
		}
	}
	return -1
}

// finalState is the device after a migration that was never interrupted.
func finalState(t *testing.T) map[string]string {
	d := newDevice(t)
	e := d.env()
	d.stage(e)
	require.NoError(t, Run(e))
	return d.snapshot()
}

// A power cut at any systemctl call (in the middle of a step) or after any
// step: the migration unit runs again at boot and ends where an
// uninterrupted migration ends.
func TestMigrateResumesAfterAPowerCut(t *testing.T) {
	want := finalState(t)

	reference := newDevice(t)
	reference.stage(reference.env())
	staged := len(reference.calls)
	require.NoError(t, Run(reference.env()))
	total := len(reference.calls)

	for n := staged + 1; n <= total; n++ {
		t.Run(fmt.Sprintf("call %d %s", n, reference.calls[n-1]), func(t *testing.T) {
			d := newDevice(t)
			d.stage(d.env())
			d.crashAt = n
			require.PanicsWithValue(t, errCrash, func() { _ = Run(d.env()) })
			d.crashAt = 0
			d.agentPID = 0 // nothing survives a power cut
			require.NoError(t, Run(d.env()))
			assert.True(t, d.journal().Done)
			assert.Equal(t, want, d.snapshot())
		})
	}

	for _, name := range StepNames() {
		t.Run("after "+name, func(t *testing.T) {
			d := newDevice(t)
			e := d.env()
			d.stage(e)
			e.Pause = name
			e.Sleep = func(dur time.Duration) {
				if dur == time.Hour {
					panic(errCrash)
				}
				d.now = d.now.Add(dur)
			}
			require.PanicsWithValue(t, errCrash, func() { _ = Run(e) })
			d.agentPID = 0
			require.NoError(t, Run(d.env()))
			assert.Equal(t, want, d.snapshot())
		})
	}
}

func TestMigrateRollsBackAnAgentThatNeverGetsReady(t *testing.T) {
	cases := map[string]func(d *device){
		"no ready file":    func(d *device) { d.agentVersion = "" },
		"another version":  func(d *device) { d.agentVersion = "1.2.13" },
		"restarts at once": func(d *device) { d.restartAfter = 30 * time.Second },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			d := newDevice(t)
			before := d.snapshot()
			setup(d)
			e := d.env()
			d.stage(e)
			err := Run(e)
			require.ErrorContains(t, err, "health")
			assert.Equal(t, before, d.snapshot())
		})
	}
}

// Docker with live-restore leaves the containers (and their mounts below
// /opt/reagent) running; moving the folder under them must not happen.
func TestMigrateStopsWhenDockerLeavesMountsBehind(t *testing.T) {
	d := newDevice(t)
	before := d.snapshot()
	d.mountsAfterDockerStop = []string{"/", "/opt/reagent/docker-apps/overlay2/abc/merged"}
	e := d.env()
	e.Unmount = func(string) error { return errors.New("target is busy") } // containers still run
	d.stage(e)
	err := Run(e)
	require.ErrorContains(t, err, "still mounted")
	assert.Equal(t, before, d.snapshot())
	assert.Greater(t, lastIndex(d.calls, "start docker.service"), index(d.calls, "stop docker.service"))
}

func TestMigrateLeavesDockerAloneWhenItsDataIsElsewhere(t *testing.T) {
	d := newDevice(t)
	d.file(daemonJSON, `{"data-root": "/var/lib/docker"}`, 0o644)
	d.mounts = []string{"/", "/boot", "/var/lib/docker/overlay2/abc/merged"}
	e := d.env()
	d.stage(e)
	require.NoError(t, Run(e))
	assert.Equal(t, -1, index(d.calls, "stop docker.service"))
	data, err := os.ReadFile(d.p(daemonJSON))
	require.NoError(t, err)
	assert.Equal(t, `{"data-root": "/var/lib/docker"}`, string(data))
}

// Installs from before 2024-09 named the configuration device-config.reswarm.
func TestMigrateTakesTheOldConfigurationName(t *testing.T) {
	d := newDevice(t)
	require.NoError(t, os.Remove(d.p("/opt/reagent/device-config.flock")))
	d.link("/opt/reagent/device-config.reswarm", d.p("/boot/demo_swarm.flock"))
	d.file(layout.LegacyUnitFile, "[Service]\nExecStart=/opt/reagent/Reagent-active -config /opt/reagent/device-config.reswarm\n", 0o644)
	before := d.snapshot()

	e := d.env()
	d.stage(e)
	e.Fault = "health"
	require.Error(t, Run(e))
	assert.Equal(t, before, d.snapshot(), "a rollback removes the link prepare added")

	d2 := newDevice(t)
	require.NoError(t, os.Remove(d2.p("/opt/reagent/device-config.flock")))
	d2.link("/opt/reagent/device-config.reswarm", d2.p("/boot/demo_swarm.flock"))
	d2.file(layout.LegacyUnitFile, "[Service]\nExecStart=/opt/reagent/Reagent-active -config /opt/reagent/device-config.reswarm\n", 0o644)
	e2 := d2.env()
	d2.stage(e2)
	require.NoError(t, Run(e2))
	snap := d2.snapshot()
	assert.Equal(t, `-rw------- {"secret":"s3cr3t"}`, snap[layout.AgentDir+"/device-config.flock"])
	assert.NotContains(t, snap, layout.AgentDir+"/device-config.reswarm")
	assert.NotContains(t, snap, "/boot/demo_swarm.flock")
}

func withArgs(args ...string) linuxservice.Paths {
	p := linuxservice.DefaultPaths()
	p.ExtraArgs = args
	return p
}

// Every legacy install moves (migrate_variants_test.go has the kinds);
// what is not one stays.
func TestCheck(t *testing.T) {
	cases := map[string]struct {
		setup func(d *device)
		want  string
	}{
		"ironflock-init":           {func(d *device) {}, ""},
		"a mount below the folder": {func(d *device) { d.mounts = append(d.mounts, "/opt/reagent/data") }, ""},
		"another filesystem":       {func(d *device) { d.sameFS = false }, ""},
		"no /opt/reagent":          {func(d *device) { require.NoError(t, os.RemoveAll(d.p("/opt/reagent"))) }, "no /opt/reagent"},
		"moved already":            {func(d *device) { d.file(layout.AgentDir+"/x", "", 0o644) }, "exists already"},
		"no unit runs it": {func(d *device) {
			require.NoError(t, os.Remove(d.p("/etc/systemd/system/reagent.service")))
		}, "no reagent.service"},
		"the unit is masked": {func(d *device) {
			require.NoError(t, os.Remove(d.p("/etc/systemd/system/reagent.service")))
			d.link("/etc/systemd/system/reagent.service", "/dev/null")
		}, "no reagent.service"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			d := newDevice(t)
			c.setup(d)
			err := Check(d.env())
			if c.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, ErrNotEligible)
			assert.ErrorContains(t, err, c.want)
		})
	}
}

func TestDue(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	due, err := Due(e)
	require.NoError(t, err)
	assert.True(t, due, "never tried")

	d.stage(e)
	due, err = Due(e)
	require.NoError(t, err)
	assert.False(t, due, "in progress: the unit resumes it")

	e.Fault = "move-folder"
	require.Error(t, Run(e))
	due, err = Due(e)
	require.NoError(t, err)
	assert.False(t, due, "failed just now")

	d.now = d.now.Add(retryAfterFailure)
	due, err = Due(e)
	require.NoError(t, err)
	assert.True(t, due, "a day after the failure")

	e.Fault = ""
	d.stage(e)
	require.NoError(t, Run(e))
	d.now = d.now.Add(100 * retryAfterFailure)
	due, err = Due(e)
	require.NoError(t, err)
	assert.False(t, due, "done")
	assert.Empty(t, LastError(d.root))
}

func TestBeginRecordsAFailedStart(t *testing.T) {
	d := newDevice(t)
	d.failOn["start --no-block "+MigrateUnitName] = errors.New("unit not found")
	d.file("/proc/self/exe", "agent 1.3.0", 0o755)
	e := d.env()
	require.Error(t, Begin(e, d.p("/proc/self/exe")))
	assert.True(t, d.journal().Failed(), "retried tomorrow, not resumed at boot")
	assert.Contains(t, d.calls, "disable "+MigrateUnitName)
}

func TestMigrateUnitCarriesTheTestHooks(t *testing.T) {
	d := newDevice(t)
	e := d.env()
	e.Fault = "health"
	unit := migrateUnit(e)
	assert.Contains(t, unit, "Environment="+FaultEnv+"=health")
	assert.NotContains(t, unit, PauseEnv)
	assert.Contains(t, unit, "ExecStart="+migrateBinary+" migrate-layout")
}

func TestStepNamesAreUnique(t *testing.T) {
	names := StepNames()
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	for i := 1; i < len(sorted); i++ {
		assert.NotEqual(t, sorted[i-1], sorted[i])
	}
}
