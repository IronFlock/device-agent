// Package crashrecovery makes a Linux host come back by itself after a kernel
// crash or a hang, and remembers that it happened. A device that recovers by
// itself would otherwise hide the crash, so the agent reports the last one with
// its heartbeat (StatsFields).
//
// IronFlockOS gets the same settings from its image, so the configuration is
// left alone there; the crash report works on every Linux host.
//
// Everything here is best effort and runs in the background: a failure is
// logged and never holds up the agent's start or its connection.
package crashrecovery

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"reagent/common"
	"reagent/osupdate"

	"github.com/rs/zerolog/log"
)

const (
	// Sorted after the distribution's files, and before the 99-sysctl.conf
	// link and whatever an administrator adds later, which win over this one.
	sysctlFile = "/etc/sysctl.d/60-ironflock-crash-recovery.conf"
	sysctlConf = `# IronFlock: restart by itself after a kernel crash or lockup.
# A file that sorts later in /etc/sysctl.d overrides these.
kernel.panic = 10
kernel.panic_on_oops = 1
kernel.softlockup_panic = 1
kernel.hardlockup_panic = 1
`

	watchdogDropIn = "/etc/systemd/system.conf.d/10-ironflock-watchdog.conf"

	// The same timeout IronFlockOS uses. Long enough that a heavily loaded host
	// is not reset for being slow.
	watchdogSeconds = 60
	// The Pi's watchdog counts to about 15 seconds at most, and systemd fails
	// to set anything longer.
	raspberryPiWatchdogSeconds = 15

	stateFile      = "crash-recovery.json"
	commandTimeout = 30 * time.Second

	// WDIOF_CARDRESET in linux/watchdog.h: the last reset was the watchdog's.
	wdiofCardReset = 0x20

	// KindKernel is a kernel panic or oops the kernel recorded in pstore.
	KindKernel = "kernel"
	// KindWatchdog is a reset by the hardware watchdog after a hang.
	KindWatchdog = "watchdog"
)

// The settings to apply. As names, not /proc/sys paths: systemd 257 mangles a
// path given to --prefix, and then nothing matches.
var sysctlKeys = []string{
	"kernel.panic",
	"kernel.panic_on_oops",
	"kernel.softlockup_panic",
	"kernel.hardlockup_panic",
}

// Crash is the last crash of this host.
type Crash struct {
	Kind string `json:"kind"`
	At   string `json:"at"` // RFC 3339, UTC
}

// state is what the agent keeps between its starts.
type state struct {
	BootID     string    `json:"boot_id"`
	KernelSeen time.Time `json:"kernel_seen"` // the newest pstore record counted so far
	LastCrash  *Crash    `json:"last_crash,omitempty"`
}

// last is what the heartbeat reports, nil until a crash is known.
var last atomic.Pointer[Crash]

// host is the machine, with a root directory and commands that tests replace.
type host struct {
	root   string
	run    func(ctx context.Context, name string, args ...string) error
	now    func() time.Time
	readOS func() (osupdate.Running, error)
}

func system() host {
	return host{
		run: func(ctx context.Context, name string, args ...string) error {
			return exec.CommandContext(ctx, name, args...).Run() //nolint:gosec // G204: fixed system commands chosen by the agent
		},
		now:    time.Now,
		readOS: osupdate.ReadRunning,
	}
}

// Run looks for a crash of the previous boot and configures the host. The agent
// calls it once at its start, in the background.
func Run(agentDir string) {
	h := system()
	h.observe(agentDir)
	h.configure()
}

// StatsFields is the last crash for the heartbeat's stats, which the backend
// keeps on the device row. Empty while no crash is known.
func StatsFields() common.Dict {
	c := last.Load()
	if c == nil {
		return common.Dict{}
	}
	return common.Dict{"last_crash": common.Dict{"kind": c.Kind, "at": c.At}}
}

func (h host) path(p string) string {
	return filepath.Join(h.root, p)
}

func (h host) configure() {
	running, err := h.readOS()
	if err != nil {
		log.Warn().Err(err).Msg("crashrecovery: cannot tell which OS this is; leaving its settings alone")
		return
	}
	if running.IsFlockOS() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	h.ensureSysctl(ctx)
	h.ensureWatchdog(ctx)
}

// ensureSysctl writes the crash settings and applies them to the running
// kernel. systemd-sysctl reads every sysctl file for the named keys, so a value
// an administrator set in a later file stays in force.
func (h host) ensureSysctl(ctx context.Context) {
	if current, err := os.ReadFile(h.path(sysctlFile)); err == nil && string(current) == sysctlConf {
		return
	}
	if err := h.write(sysctlFile, sysctlConf); err != nil {
		log.Error().Err(err).Msg("crashrecovery: failed to write the kernel crash settings")
		return
	}
	log.Info().Msg("crashrecovery: the host now restarts by itself after a kernel crash")

	args := make([]string, 0, len(sysctlKeys))
	for _, key := range sysctlKeys {
		args = append(args, "--prefix="+key)
	}
	for _, binary := range []string{"/usr/lib/systemd/systemd-sysctl", "/lib/systemd/systemd-sysctl"} {
		if _, err := os.Stat(h.path(binary)); err != nil {
			continue
		}
		if err := h.run(ctx, binary, args...); err != nil {
			log.Warn().Err(err).Msg("crashrecovery: kernel crash settings not applied; they apply at the next boot")
		}
		return
	}
}

// ensureWatchdog makes systemd drive the hardware watchdog, where the host has
// one nobody else uses. The device is only looked at, never opened: opening a
// watchdog can arm it.
func (h host) ensureWatchdog(ctx context.Context) {
	if !h.exists("/dev/watchdog0") && !h.exists("/dev/watchdog") {
		return
	}
	want := "# IronFlock: restart a hung host through its hardware watchdog.\n" +
		"[Manager]\n" +
		"RuntimeWatchdogSec=" + strconv.Itoa(h.watchdogTimeout()) + "s\n" +
		"RebootWatchdogSec=5min\n"
	if current, err := os.ReadFile(h.path(watchdogDropIn)); err == nil && string(current) == want {
		return
	}
	if h.watchdogTaken(ctx) {
		log.Info().Msg("crashrecovery: the hardware watchdog is configured elsewhere; leaving it alone")
		return
	}
	if err := h.write(watchdogDropIn, want); err != nil {
		log.Error().Err(err).Msg("crashrecovery: failed to write the watchdog setting")
		return
	}
	// systemd reads system.conf only when it starts. Re-executing it leaves
	// every service running.
	if err := h.run(ctx, "systemctl", "daemon-reexec"); err != nil {
		log.Warn().Err(err).Msg("crashrecovery: watchdog not applied; it applies at the next boot")
		return
	}
	log.Info().Msg("crashrecovery: systemd now drives the hardware watchdog")
}

func (h host) watchdogTimeout() int {
	timeout := watchdogSeconds
	if data, err := os.ReadFile(h.path("/sys/class/watchdog/watchdog0/max_timeout")); err == nil {
		if limit, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && limit > 0 && limit < timeout {
			timeout = limit
		}
	}
	if model, err := os.ReadFile(h.path("/proc/device-tree/model")); err == nil &&
		strings.HasPrefix(string(model), "Raspberry Pi") && timeout > raspberryPiWatchdogSeconds {
		timeout = raspberryPiWatchdogSeconds
	}
	return timeout
}

// watchdogTaken reports whether an administrator set the watchdog in systemd
// (also to off) or a watchdog daemon uses the device, which only one program
// can hold open.
func (h host) watchdogTaken(ctx context.Context) bool {
	files := []string{h.path("/etc/systemd/system.conf")}
	for _, dir := range []string{"/etc/systemd/system.conf.d", "/usr/lib/systemd/system.conf.d"} {
		found, _ := filepath.Glob(h.path(dir) + "/*.conf")
		files = append(files, found...)
	}
	for _, file := range files {
		if file != h.path(watchdogDropIn) && setsRuntimeWatchdog(file) {
			return true
		}
	}
	for _, unit := range []string{"watchdog.service", "wd_keepalive.service"} {
		if h.run(ctx, "systemctl", "is-active", "--quiet", unit) == nil ||
			h.run(ctx, "systemctl", "is-enabled", "--quiet", unit) == nil {
			return true
		}
	}
	return false
}

func setsRuntimeWatchdog(file string) bool {
	data, err := os.ReadFile(file)
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "RuntimeWatchdogSec=") {
			return true
		}
	}
	return false
}

// observe records a crash of the previous boot, once per boot. An agent restart
// within a boot only loads what is known.
func (h host) observe(agentDir string) {
	if agentDir == "" {
		return
	}
	data, err := os.ReadFile(h.path("/proc/sys/kernel/random/boot_id"))
	if err != nil {
		log.Warn().Err(err).Msg("crashrecovery: cannot read the boot id; not looking for a crash")
		return
	}
	bootID := strings.TrimSpace(string(data))
	statePath := filepath.Join(agentDir, stateFile)

	var st state
	if data, err := os.ReadFile(statePath); err == nil {
		if err := json.Unmarshal(data, &st); err != nil {
			log.Warn().Err(err).Msg("crashrecovery: starting over from an unreadable state file")
			st = state{}
		}
	}

	if st.BootID != bootID {
		newest := h.newestKernelRecord()
		switch {
		case newest.After(st.KernelSeen):
			st.KernelSeen = newest
			st.LastCrash = &Crash{Kind: KindKernel, At: newest.UTC().Format(time.RFC3339)}
		case h.watchdogReset():
			st.LastCrash = &Crash{Kind: KindWatchdog, At: h.bootTime().UTC().Format(time.RFC3339)}
		}
		st.BootID = bootID
		if out, err := json.Marshal(st); err != nil {
			log.Error().Err(err).Msg("crashrecovery: failed to encode the state file")
		} else if err := os.WriteFile(statePath, out, 0o600); err != nil {
			log.Error().Err(err).Msg("crashrecovery: failed to write the state file")
		}
		if st.LastCrash != nil {
			log.Warn().Str("kind", st.LastCrash.Kind).Str("at", st.LastCrash.At).Msg("crashrecovery: the last crash of this host")
		}
	}
	if st.LastCrash != nil {
		last.Store(st.LastCrash)
	}
}

// newestKernelRecord is the time of the newest kernel crash record. The kernel
// leaves them in /sys/fs/pstore, where the boot's systemd-pstore usually moves
// them away to /var/lib/systemd/pstore.
func (h host) newestKernelRecord() time.Time {
	var newest time.Time
	for _, dir := range []string{"/sys/fs/pstore", "/var/lib/systemd/pstore"} {
		err := filepath.WalkDir(h.path(dir), func(_ string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !strings.HasPrefix(entry.Name(), "dmesg") {
				return nil //nolint:nilerr // an unreadable entry is one record less
			}
			if info, err := entry.Info(); err == nil && info.ModTime().After(newest) {
				newest = info.ModTime()
			}
			return nil
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Warn().Err(err).Str("dir", dir).Msg("crashrecovery: cannot read kernel crash records")
		}
	}
	return newest
}

func (h host) watchdogReset() bool {
	data, err := os.ReadFile(h.path("/sys/class/watchdog/watchdog0/bootstatus"))
	if err != nil {
		return false
	}
	status, err := strconv.ParseUint(strings.TrimSpace(string(data)), 0, 32)
	return err == nil && status&wdiofCardReset != 0
}

func (h host) bootTime() time.Time {
	data, err := os.ReadFile(h.path("/proc/uptime"))
	if err != nil {
		return h.now()
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return h.now()
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return h.now()
	}
	return h.now().Add(-time.Duration(seconds * float64(time.Second)))
}

func (h host) exists(path string) bool {
	_, err := os.Stat(h.path(path))
	return err == nil
}

func (h host) write(path, content string) error {
	full := h.path(path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, []byte(content), 0o644) //nolint:gosec // G306: system configuration, world-readable like its neighbours
}
