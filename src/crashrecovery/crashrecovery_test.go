package crashrecovery

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"reagent/osupdate"
)

const (
	bootA = "aaaaaaaa-0000-0000-0000-000000000000"
	bootB = "bbbbbbbb-0000-0000-0000-000000000000"
)

// fake is a host in a temporary directory. active names the units that count
// as active and enabled; every other systemctl query fails, as for a missing
// unit.
type fake struct {
	host
	t      *testing.T
	calls  []string
	active map[string]bool
	fail   map[string]bool
	dir    string
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{t: t, active: map[string]bool{}, fail: map[string]bool{}, dir: t.TempDir()}
	f.host = host{
		root: t.TempDir(),
		now:  func() time.Time { return time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC) },
		readOS: func() (osupdate.Running, error) {
			return osupdate.Running{ID: "debian"}, nil
		},
		run: func(_ context.Context, name string, args ...string) error {
			call := strings.TrimSpace(name + " " + strings.Join(args, " "))
			f.calls = append(f.calls, call)
			if name == "systemctl" && len(args) == 3 && args[1] == "--quiet" {
				if f.active[args[2]] {
					return nil
				}
				return errors.New("no such unit")
			}
			if f.fail[name] {
				return errors.New("failed")
			}
			return nil
		},
	}
	last.Store(nil)
	t.Cleanup(func() { last.Store(nil) })
	return f
}

func (f *fake) put(path, content string) {
	f.t.Helper()
	full := f.path(path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fake) record(path string, at time.Time) {
	f.t.Helper()
	f.put(path, "Panic#1 Part1\n")
	if err := os.Chtimes(f.path(path), at, at); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fake) count(call string) int {
	n := 0
	for _, c := range f.calls {
		if c == call {
			n++
		}
	}
	return n
}

func (f *fake) dropIn() string {
	data, err := os.ReadFile(f.path(watchdogDropIn))
	if err != nil {
		return ""
	}
	return string(data)
}

func (f *fake) boot(id string) {
	f.t.Helper()
	f.put("/proc/sys/kernel/random/boot_id", id+"\n")
}

func (f *fake) lastCrash() *Crash {
	f.t.Helper()
	fields := StatsFields()
	if len(fields) == 0 {
		return nil
	}
	data, err := json.Marshal(fields["last_crash"])
	if err != nil {
		f.t.Fatal(err)
	}
	var c Crash
	if err := json.Unmarshal(data, &c); err != nil {
		f.t.Fatal(err)
	}
	return &c
}

func TestConfigureSkipsIronFlockOS(t *testing.T) {
	f := newFake(t)
	f.put("/dev/watchdog0", "")
	f.readOS = func() (osupdate.Running, error) {
		return osupdate.Running{ID: "ironflockos", ImageID: "ironflockos-amd64-efi-base", ImageVersion: "0.13.0"}, nil
	}
	f.configure()
	if f.exists(sysctlFile) || f.exists(watchdogDropIn) || len(f.calls) != 0 {
		t.Fatalf("IronFlockOS must be left alone: calls %v", f.calls)
	}
}

func TestConfigureLeavesAnUnknownOSAlone(t *testing.T) {
	f := newFake(t)
	f.readOS = func() (osupdate.Running, error) { return osupdate.Running{}, errors.New("no os-release") }
	f.configure()
	if f.exists(sysctlFile) {
		t.Fatal("an OS that cannot be identified must be left alone")
	}
}

func TestSysctlWrittenAndAppliedOnce(t *testing.T) {
	f := newFake(t)
	f.put("/usr/lib/systemd/systemd-sysctl", "")
	f.configure()
	data, err := os.ReadFile(f.path(sysctlFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"kernel.panic = 10", "kernel.panic_on_oops = 1", "kernel.softlockup_panic = 1", "kernel.hardlockup_panic = 1"} {
		if !strings.Contains(string(data), want+"\n") {
			t.Errorf("missing %q in %s", want, data)
		}
	}
	apply := "/usr/lib/systemd/systemd-sysctl --prefix=kernel.panic --prefix=kernel.panic_on_oops --prefix=kernel.softlockup_panic --prefix=kernel.hardlockup_panic"
	if f.count(apply) != 1 {
		t.Fatalf("expected one apply, calls %v", f.calls)
	}
	f.configure()
	if f.count(apply) != 1 {
		t.Fatalf("unchanged settings must not be applied again, calls %v", f.calls)
	}
}

func TestSysctlOldLayoutBinary(t *testing.T) {
	f := newFake(t)
	f.put("/lib/systemd/systemd-sysctl", "")
	f.configure()
	if len(f.calls) != 1 || !strings.HasPrefix(f.calls[0], "/lib/systemd/systemd-sysctl ") {
		t.Fatalf("expected the /lib binary, calls %v", f.calls)
	}
}

func TestWatchdogNeedsADevice(t *testing.T) {
	f := newFake(t)
	f.configure()
	if f.exists(watchdogDropIn) || f.count("systemctl daemon-reexec") != 0 {
		t.Fatalf("no device, no watchdog: calls %v", f.calls)
	}
}

func TestWatchdogTimeouts(t *testing.T) {
	for _, tc := range []struct {
		name, limit, model, want string
	}{
		{name: "default", want: "RuntimeWatchdogSec=60s\n"},
		{name: "hardware allows more", limit: "300\n", want: "RuntimeWatchdogSec=60s\n"},
		{name: "hardware limit", limit: "30\n", want: "RuntimeWatchdogSec=30s\n"},
		{name: "unreadable limit", limit: "n/a\n", want: "RuntimeWatchdogSec=60s\n"},
		{name: "raspberry pi", model: "Raspberry Pi 4 Model B Rev 1.4\x00", want: "RuntimeWatchdogSec=15s\n"},
		{name: "raspberry pi with lower limit", limit: "10\n", model: "Raspberry Pi 3\x00", want: "RuntimeWatchdogSec=10s\n"},
		{name: "other board", model: "Some Industrial Board\x00", want: "RuntimeWatchdogSec=60s\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			f.put("/dev/watchdog0", "")
			if tc.limit != "" {
				f.put("/sys/class/watchdog/watchdog0/max_timeout", tc.limit)
			}
			if tc.model != "" {
				f.put("/proc/device-tree/model", tc.model)
			}
			f.configure()
			if !strings.Contains(f.dropIn(), tc.want) {
				t.Fatalf("want %q in %q", tc.want, f.dropIn())
			}
			if f.count("systemctl daemon-reexec") != 1 {
				t.Fatalf("expected one re-exec, calls %v", f.calls)
			}
		})
	}
}

func TestWatchdogUnchangedIsNotAppliedAgain(t *testing.T) {
	f := newFake(t)
	f.put("/dev/watchdog", "")
	f.configure()
	f.configure()
	if f.count("systemctl daemon-reexec") != 1 {
		t.Fatalf("expected one re-exec, calls %v", f.calls)
	}
}

func TestWatchdogConfiguredElsewhereIsLeftAlone(t *testing.T) {
	for _, tc := range []struct {
		name, file, content string
		active              string
	}{
		{name: "system.conf", file: "/etc/systemd/system.conf", content: "[Manager]\nRuntimeWatchdogSec=20s\n"},
		{name: "switched off", file: "/etc/systemd/system.conf", content: "[Manager]\nRuntimeWatchdogSec=off\n"},
		{name: "admin drop-in", file: "/etc/systemd/system.conf.d/50-site.conf", content: "[Manager]\n  RuntimeWatchdogSec=10s\n"},
		{name: "vendor drop-in", file: "/usr/lib/systemd/system.conf.d/20-vendor.conf", content: "[Manager]\nRuntimeWatchdogSec=30s\n"},
		{name: "watchdog daemon", active: "watchdog.service"},
		{name: "keepalive daemon", active: "wd_keepalive.service"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			f.put("/dev/watchdog0", "")
			if tc.file != "" {
				f.put(tc.file, tc.content)
			}
			if tc.active != "" {
				f.active[tc.active] = true
			}
			f.configure()
			if f.exists(watchdogDropIn) || f.count("systemctl daemon-reexec") != 0 {
				t.Fatalf("must be left alone: calls %v", f.calls)
			}
		})
	}
}

func TestCommentedOutWatchdogSettingIsNotAnOwner(t *testing.T) {
	f := newFake(t)
	f.put("/dev/watchdog0", "")
	f.put("/etc/systemd/system.conf", "[Manager]\n#RuntimeWatchdogSec=off\n")
	f.configure()
	if !f.exists(watchdogDropIn) {
		t.Fatal("the packaged system.conf has this line commented out and must not block the setting")
	}
}

func TestOwnDropInIsNotAnOwner(t *testing.T) {
	f := newFake(t)
	f.put("/dev/watchdog0", "")
	f.configure()
	f.put("/sys/class/watchdog/watchdog0/max_timeout", "30\n")
	f.configure()
	if !strings.Contains(f.dropIn(), "RuntimeWatchdogSec=30s\n") {
		t.Fatalf("our own earlier file must be replaced: %q", f.dropIn())
	}
}

func TestFailedApplyStaysHarmless(t *testing.T) {
	f := newFake(t)
	f.put("/dev/watchdog0", "")
	f.fail["systemctl"] = true
	f.configure()
	if !f.exists(watchdogDropIn) {
		t.Fatal("the setting must still be there for the next boot")
	}
}

func TestObserveKernelCrash(t *testing.T) {
	for _, path := range []string{
		"/sys/fs/pstore/dmesg-efi-176123456701001",
		"/var/lib/systemd/pstore/176123456701/001/dmesg-efi-176123456701001",
		"/var/lib/systemd/pstore/176123456701/dmesg.txt",
	} {
		t.Run(path, func(t *testing.T) {
			f := newFake(t)
			at := time.Date(2026, 11, 7, 3, 14, 15, 0, time.UTC)
			f.boot(bootA)
			f.record(path, at)
			f.observe(f.dir)
			got := f.lastCrash()
			if got == nil || got.Kind != KindKernel || got.At != "2026-11-07T03:14:15Z" {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestObserveOneCrashIsReportedAsOneCrash(t *testing.T) {
	f := newFake(t)
	at := time.Date(2026, 11, 7, 3, 14, 15, 0, time.UTC)
	f.boot(bootA)
	f.record("/sys/fs/pstore/dmesg-efi-1", at)
	f.observe(f.dir)

	// A later boot with the same record and a clean reset: still that crash.
	last.Store(nil)
	f.boot(bootB)
	f.observe(f.dir)
	got := f.lastCrash()
	if got == nil || got.At != "2026-11-07T03:14:15Z" {
		t.Fatalf("the old crash must stay known: %+v", got)
	}
	var st state
	data, err := os.ReadFile(filepath.Join(f.dir, stateFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	if st.BootID != bootB {
		t.Fatalf("boot id not updated: %+v", st)
	}
}

func TestObserveNewerCrashReplacesTheOld(t *testing.T) {
	f := newFake(t)
	f.boot(bootA)
	f.record("/sys/fs/pstore/dmesg-efi-1", time.Date(2026, 11, 7, 3, 0, 0, 0, time.UTC))
	f.observe(f.dir)
	f.boot(bootB)
	f.record("/sys/fs/pstore/dmesg-efi-2", time.Date(2026, 11, 8, 4, 0, 0, 0, time.UTC))
	f.observe(f.dir)
	if got := f.lastCrash(); got == nil || got.At != "2026-11-08T04:00:00Z" {
		t.Fatalf("got %+v", got)
	}
}

func TestObserveWatchdogReset(t *testing.T) {
	f := newFake(t)
	f.boot(bootA)
	f.put("/sys/class/watchdog/watchdog0/bootstatus", "32\n")
	f.put("/proc/uptime", "3600.50 7000.00\n")
	f.observe(f.dir)
	got := f.lastCrash()
	if got == nil || got.Kind != KindWatchdog || got.At != "2026-11-09T10:59:59Z" {
		t.Fatalf("got %+v", got)
	}
}

func TestObserveWatchdogStatusWithoutResetIsNoCrash(t *testing.T) {
	f := newFake(t)
	f.boot(bootA)
	f.put("/sys/class/watchdog/watchdog0/bootstatus", "0\n")
	f.observe(f.dir)
	if got := f.lastCrash(); got != nil {
		t.Fatalf("got %+v", got)
	}
}

func TestObserveNewKernelRecordBeatsWatchdogStatus(t *testing.T) {
	f := newFake(t)
	f.boot(bootA)
	f.put("/sys/class/watchdog/watchdog0/bootstatus", "32\n")
	f.record("/sys/fs/pstore/dmesg-efi-1", time.Date(2026, 11, 7, 3, 0, 0, 0, time.UTC))
	f.observe(f.dir)
	if got := f.lastCrash(); got == nil || got.Kind != KindKernel {
		t.Fatalf("got %+v", got)
	}
}

func TestObserveOldKernelRecordDoesNotHideAWatchdogReset(t *testing.T) {
	f := newFake(t)
	f.boot(bootA)
	f.record("/sys/fs/pstore/dmesg-efi-1", time.Date(2026, 11, 7, 3, 0, 0, 0, time.UTC))
	f.observe(f.dir)
	f.boot(bootB)
	f.put("/sys/class/watchdog/watchdog0/bootstatus", "32\n")
	f.observe(f.dir)
	if got := f.lastCrash(); got == nil || got.Kind != KindWatchdog {
		t.Fatalf("got %+v", got)
	}
}

func TestObserveAgentRestartWithinABootChangesNothing(t *testing.T) {
	f := newFake(t)
	f.boot(bootA)
	f.observe(f.dir)
	// Appears while the boot runs: counted at the next boot, not at this restart.
	f.record("/sys/fs/pstore/dmesg-efi-1", time.Date(2026, 11, 7, 3, 0, 0, 0, time.UTC))
	f.observe(f.dir)
	if got := f.lastCrash(); got != nil {
		t.Fatalf("got %+v", got)
	}
}

func TestObserveAgentRestartKeepsReporting(t *testing.T) {
	f := newFake(t)
	f.boot(bootA)
	f.record("/sys/fs/pstore/dmesg-efi-1", time.Date(2026, 11, 7, 3, 0, 0, 0, time.UTC))
	f.observe(f.dir)
	last.Store(nil)
	f.observe(f.dir)
	if got := f.lastCrash(); got == nil {
		t.Fatal("a restarted agent must keep reporting the crash")
	}
}

func TestObserveCleanHost(t *testing.T) {
	f := newFake(t)
	f.boot(bootA)
	f.observe(f.dir)
	if got := StatsFields(); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestObserveSurvivesADamagedStateFile(t *testing.T) {
	f := newFake(t)
	f.boot(bootA)
	if err := os.WriteFile(filepath.Join(f.dir, stateFile), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.record("/sys/fs/pstore/dmesg-efi-1", time.Date(2026, 11, 7, 3, 0, 0, 0, time.UTC))
	f.observe(f.dir)
	if got := f.lastCrash(); got == nil {
		t.Fatal("a damaged state file must not stop the detection")
	}
}

func TestObserveWithoutBootIDOrAgentDir(t *testing.T) {
	f := newFake(t)
	f.observe(f.dir)
	f.boot(bootA)
	f.record("/sys/fs/pstore/dmesg-efi-1", time.Date(2026, 11, 7, 3, 0, 0, 0, time.UTC))
	f.observe("")
	if got := StatsFields(); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}
