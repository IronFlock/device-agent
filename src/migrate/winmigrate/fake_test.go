package winmigrate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reagent/winservice"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const testVersion = "1.4.0"

// errCrash is the panic the fake raises to cut the power mid-migration.
var errCrash = errors.New("power cut")

// fakeService is a service as the fake SCM has it.
type fakeService struct {
	spec            winservice.Spec
	startType       winservice.StartType
	delayed         bool
	state           string
	pid             int
	markedForDelete bool
	// restartAt: the SCM's recovery actions start it again at this time
	// (a killed process, unless the service is disabled).
	restartAt time.Time
}

// fakeProcess is a running process; exitsAt is set once the SCM reported
// its service stopped (the process lingers until then).
type fakeProcess struct {
	pid     int
	exe     string
	service string
	exitsAt time.Time
}

type fakeContainer struct {
	Container
	// cutRunning: it ran when the power went; Docker starts it again at
	// boot when its restart policy says so.
	cutRunning bool
	// configured is the restart policy the app's configuration gives a
	// container the agent creates; wanted whether the app should run (the
	// agent recreates a wanted app's container from the moved folder).
	configured string
	wanted     bool
}

// fakeSystem models everything the migration touches: the files below a
// temporary root, the service control manager, processes, the ONSTART
// repair task (with an interpreter for exactly the scripts the agent
// generates), Docker, open handles and ownership. It counts every call that
// changes something, and crashAt cuts the power at the n-th of them.
type fakeSystem struct {
	t      *testing.T
	layout Layout
	now    time.Time

	calls      []string
	crashAt    int
	counting   bool
	failOn     map[string]error
	services   map[string]*fakeService
	processes  map[int]*fakeProcess
	nextPID    int
	task       *fakeTask
	events     map[string]bool
	defender   map[string]bool
	containers []*fakeContainer
	dockerDown bool
	// dockerAtBoot: Docker runs at boot (Docker Desktop starts at user
	// login, a WSL engine at boot).
	dockerAtBoot bool
	// holders block the rename of a path (an Explorer window, a scan);
	// releaseAfter frees them after that many refused renames (0: never).
	holders      map[string][]string
	releaseAfter map[string]int
	refused      map[string]int
	owners       map[string]string
	// linkTypes: what Stat reports as the type of these paths, the way
	// Windows does for a link the test's filesystem holds as a symbolic link
	// (a junction: os.ModeIrregular, no os.ModeDir).
	linkTypes   map[string]os.FileMode
	otherVolume map[string]bool
	hardened    []string
	hyperV      bool
	shared      []string
	elevated    bool
	brokenExe   map[string]bool
	// connectFails: the agent of this service never connects.
	connectFails map[string]bool
	// agentKeepsStaleConnected: an agent that does not remove the connected
	// file at its start (what the real one does); for the test of why it
	// must.
	agentKeepsStaleConnected bool
	// stopLinger: how long a stopped service's process keeps running after
	// the SCM reported it stopped.
	stopLinger time.Duration
	// adopts: the simulated new agent runs Adopt when it connects, like the
	// real one.
	adopts bool
	// consumedMarkers counts the recreate markers the simulated agents
	// consumed (the marker goes once every container is recreated).
	consumedMarkers map[string]int
	// locked: the machine-wide lock of the engine is held.
	locked bool
}

type fakeTask struct {
	script string
}

func newFakeSystem(t *testing.T, root string) *fakeSystem {
	return &fakeSystem{
		t:               t,
		layout:          Layout{Root: root},
		now:             time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC),
		failOn:          map[string]error{},
		services:        map[string]*fakeService{},
		processes:       map[int]*fakeProcess{},
		nextPID:         1000,
		events:          map[string]bool{},
		defender:        map[string]bool{},
		dockerAtBoot:    true,
		holders:         map[string][]string{},
		releaseAfter:    map[string]int{},
		refused:         map[string]int{},
		owners:          map[string]string{},
		otherVolume:     map[string]bool{},
		elevated:        true,
		brokenExe:       map[string]bool{},
		connectFails:    map[string]bool{},
		stopLinger:      5 * time.Second,
		adopts:          true,
		consumedMarkers: map[string]int{},
	}
}

// call records a call that changes something; the n-th one cuts the power.
func (f *fakeSystem) call(format string, args ...any) {
	name := fmt.Sprintf(format, args...)
	if !f.counting {
		return
	}
	f.calls = append(f.calls, name)
	if f.crashAt != 0 && len(f.calls) == f.crashAt {
		panic(errCrash)
	}
}

func (f *fakeSystem) fail(call string) error {
	if err := f.failOn[call]; err != nil {
		return err
	}
	return nil
}

// reap applies what time did: lingering processes exit, killed services
// restart (the SCM's recovery actions).
func (f *fakeSystem) reap() {
	for pid, p := range f.processes {
		if !p.exitsAt.IsZero() && !f.now.Before(p.exitsAt) {
			delete(f.processes, pid)
		}
	}
	for name, s := range f.services {
		if !s.restartAt.IsZero() && !f.now.Before(s.restartAt) {
			s.restartAt = time.Time{}
			if s.startType != winservice.StartDisabled && !s.markedForDelete {
				f.startService(name)
			}
		}
		if s.markedForDelete && s.state == StateStopped {
			delete(f.services, name)
		}
	}
}

// --- files ---

func (f *fakeSystem) Stat(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if mode, ok := f.linkTypes[path]; ok && err == nil && info.Mode()&os.ModeSymlink != 0 {
		return typedInfo{FileInfo: info, typ: mode}, nil
	}
	return info, err
}

// typedInfo is a file's information with the type Windows would report.
type typedInfo struct {
	os.FileInfo
	typ os.FileMode
}

func (i typedInfo) Mode() os.FileMode { return i.typ | i.FileInfo.Mode().Perm() }
func (i typedInfo) IsDir() bool       { return i.Mode().IsDir() }

func (f *fakeSystem) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }
func (f *fakeSystem) ReadDir(path string) ([]os.DirEntry, error) {
	return os.ReadDir(path)
}

// WriteFile is the real adapter's shape: a uniquely named temporary file,
// then a replacing rename.
func (f *fakeSystem) WriteFile(path string, data []byte, mode os.FileMode) error {
	f.call("write %s", f.rel(path))
	if err := f.fail("write " + f.rel(path)); err != nil {
		return err
	}
	return writeViaTemp(path, data, mode)
}

func writeViaTemp(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), mode)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	// Like the real adapter's replace: a destination another writer holds for
	// a moment (Windows refuses the replace then) is retried, and a temporary
	// file is never left behind.
	for attempt := 0; ; attempt++ {
		err = os.Rename(tmp.Name(), path)
		if err == nil || attempt >= 20 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

// Rename never overwrites, and fails while a process runs from the source
// or below it, or a holder keeps the source or something below it open (a
// sharing violation). A holder on a folder (an Explorer window) blocks the
// folder's rename, not the renames of files inside it.
func (f *fakeSystem) Rename(from, to string) error {
	f.call("rename %s %s", f.rel(from), f.rel(to))
	if err := f.fail("rename " + f.rel(from)); err != nil {
		return err
	}
	if _, err := os.Lstat(from); err != nil {
		return err
	}
	if _, err := os.Lstat(to); err == nil {
		return fmt.Errorf("renaming %s to %s: %w", from, to, os.ErrExist)
	}
	f.reap()
	for _, p := range f.processes {
		if underDir(p.exe, from) {
			return fmt.Errorf("renaming %s: sharing violation (%s runs)", from, filepath.Base(p.exe))
		}
	}
	for held, names := range f.holders {
		if underDir(held, from) {
			f.refused[held]++
			if n := f.releaseAfter[held]; n > 0 && f.refused[held] >= n {
				delete(f.holders, held)
				continue
			}
			return fmt.Errorf("renaming %s: sharing violation (held by %s)", from, strings.Join(names, ", "))
		}
	}
	return os.Rename(from, to)
}

// Remove fails for the executable a process runs (Windows keeps a mapped
// image), like Rename does for a folder a process runs from.
func (f *fakeSystem) Remove(path string) error {
	f.call("remove %s", f.rel(path))
	if err := f.fail("remove " + f.rel(path)); err != nil {
		return err
	}
	f.reap()
	for _, p := range f.processes {
		if samePath(p.exe, path) {
			return fmt.Errorf("remove %s: Access is denied (%s runs it)", path, p.service)
		}
	}
	return os.Remove(path)
}

func (f *fakeSystem) RemoveAll(path string) error {
	f.call("remove-all %s", f.rel(path))
	return os.RemoveAll(path)
}

func (f *fakeSystem) MkdirAll(path string, mode os.FileMode) error {
	if _, err := os.Lstat(path); err == nil {
		return nil
	}
	f.call("mkdir %s", f.rel(path))
	return os.MkdirAll(path, mode)
}

func (f *fakeSystem) CopyFile(from, to string, mode os.FileMode) error {
	f.call("copy %s %s", f.rel(from), f.rel(to))
	if err := f.fail("copy " + f.rel(to)); err != nil {
		return err
	}
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return writeViaTemp(to, data, mode)
}

// LockMigration models the engine's named mutex.
func (f *fakeSystem) LockMigration() (func(), error) {
	if f.locked {
		return nil, errors.New("Global\\ironflock-device-agent-migrate is held")
	}
	f.locked = true
	return func() { f.locked = false }, nil
}

func (f *fakeSystem) SameVolume(a, b string) (bool, error) {
	return !f.otherVolume[a] && !f.otherVolume[b], nil
}

func (f *fakeSystem) HoldersOf(path string) []string {
	var names []string
	for held, holders := range f.holders {
		if underDir(held, path) {
			names = append(names, holders...)
		}
	}
	sort.Strings(names)
	return names
}

// --- processes ---

// ExecVersion: the fake executables hold their version as their content.
func (f *fakeSystem) ExecVersion(exe string) (string, error) {
	if f.brokenExe[exe] {
		return "", errors.New("exit status 3221225781")
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func (f *fakeSystem) ProcessesUnder(dir string) ([]Process, error) {
	f.reap()
	var out []Process
	for _, p := range f.processes {
		if underDir(p.exe, dir) {
			out = append(out, Process{PID: p.pid, Exe: p.exe})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out, nil
}

func (f *fakeSystem) ProcessAlive(pid int) bool {
	f.reap()
	_, ok := f.processes[pid]
	return ok
}

// KillProcess: a killed service process is a crash; recovery starts the
// service again unless it is disabled (reap).
func (f *fakeSystem) KillProcess(pid int) error {
	f.call("kill %d", pid)
	p, ok := f.processes[pid]
	if !ok {
		return nil
	}
	delete(f.processes, pid)
	if s := f.services[p.service]; s != nil && s.state == StateRunning && s.pid == pid {
		s.state, s.pid = StateStopped, 0
		if len(s.spec.RestartDelays) > 0 {
			s.restartAt = f.now.Add(time.Duration(s.spec.RestartDelays[0]) * time.Second)
		}
	}
	return nil
}

// --- the service control manager ---

func (f *fakeSystem) Service(name string) (ServiceConfig, error) {
	s, ok := f.services[name]
	if !ok {
		return ServiceConfig{}, fmt.Errorf("service %s: %w", name, os.ErrNotExist)
	}
	return ServiceConfig{
		Exe:         s.spec.Exe,
		Args:        append([]string(nil), s.spec.Args...),
		StartType:   s.startType,
		Delayed:     s.delayed,
		Env:         append([]string(nil), s.spec.Env...),
		Description: s.spec.Description,
	}, nil
}

func (f *fakeSystem) ServiceStatus(name string) (ServiceStatus, error) {
	f.reap()
	s, ok := f.services[name]
	if !ok {
		return ServiceStatus{}, fmt.Errorf("service %s: %w", name, os.ErrNotExist)
	}
	return ServiceStatus{State: s.state, PID: s.pid}, nil
}

func (f *fakeSystem) CreateOrUpdateService(spec winservice.Spec) error {
	f.call("create-or-update %s", spec.Name)
	if err := f.fail("create-or-update " + spec.Name); err != nil {
		return err
	}
	f.reap()
	s, ok := f.services[spec.Name]
	if ok && s.markedForDelete {
		return fmt.Errorf("creating %s: %w", spec.Name, ErrServiceMarkedForDelete)
	}
	// Like the SCM (TestContractDuplicateDisplayName): a display name another
	// service holds is accepted.
	if !ok {
		s = &fakeService{state: StateStopped}
		f.services[spec.Name] = s
	}
	env := s.spec.Env
	if spec.Env != nil {
		env = append([]string(nil), spec.Env...)
	}
	s.spec = spec
	s.spec.Env = env
	s.startType, s.delayed = spec.StartType, spec.Delayed
	return nil
}

func (f *fakeSystem) SetStartType(name string, start winservice.StartType, delayed bool) error {
	f.call("start-type %s %s", name, start)
	if err := f.fail(fmt.Sprintf("start-type %s %s", name, start)); err != nil {
		return err
	}
	s, ok := f.services[name]
	if !ok {
		return fmt.Errorf("service %s: %w", name, os.ErrNotExist)
	}
	s.startType, s.delayed = start, delayed
	return nil
}

func (f *fakeSystem) SetDisplayName(name, display string) error {
	f.call("display-name %s %s", name, display)
	s, ok := f.services[name]
	if !ok {
		return fmt.Errorf("service %s: %w", name, os.ErrNotExist)
	}
	if err := f.fail("display-name " + name); err != nil {
		return err
	}
	s.spec.DisplayName = display
	return nil
}

func (f *fakeSystem) StartService(name string) error {
	f.call("start %s", name)
	if err := f.fail("start " + name); err != nil {
		return err
	}
	return f.startService(name)
}

// startService is what the SCM does: a disabled service cannot start, one
// without its executable cannot, a running one already runs. An agent
// service's process behaves like the agent: it removes the connected file
// and, with its files complete, connects and writes it; the new agent also
// removes a done migration's leftovers and adopts a rolled-back one.
func (f *fakeSystem) startService(name string) error {
	f.reap()
	s, ok := f.services[name]
	if !ok {
		return fmt.Errorf("service %s: %w", name, os.ErrNotExist)
	}
	if s.startType == winservice.StartDisabled {
		return fmt.Errorf("starting %s: the service cannot be started, either because it is disabled or because it has no enabled devices associated with it", name)
	}
	if s.state == StateRunning {
		return nil
	}
	if info, err := os.Lstat(s.spec.Exe); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("starting %s: the system cannot find the file specified (%s)", name, s.spec.Exe)
	}
	if f.brokenExe[s.spec.Exe] {
		return fmt.Errorf("starting %s: %s is not a valid Win32 application", name, s.spec.Exe)
	}
	s.restartAt = time.Time{}
	s.state = StateRunning
	s.pid = f.spawn(s.spec.Exe, name)
	if name == winservice.ServiceName || name == winservice.LegacyServiceName {
		f.runAgent(name, s)
	}
	return nil
}

func (f *fakeSystem) spawn(exe string, service string) int {
	pid := f.nextPID
	f.nextPID++
	f.processes[pid] = &fakeProcess{pid: pid, exe: exe, service: service}
	return pid
}

// runAgent simulates the agent process of a service start.
func (f *fakeSystem) runAgent(name string, s *fakeService) {
	agentDir := flagValue(s.spec.Args, "-agentDir", filepath.Dir(s.spec.Exe))
	connected := filepath.Join(agentDir, winservice.ConnectedName)
	if !f.agentKeepsStaleConnected {
		_ = os.Remove(connected)
	}
	RemoveLeftovers(f.env(), name)
	version, err := f.ExecVersion(s.spec.Exe)
	if err != nil {
		return
	}
	config := flagValue(s.spec.Args, "-config", filepath.Join(agentDir, winservice.ConfigName))
	database := flagValue(s.spec.Args, "-dbFileName", filepath.Join(agentDir, "reagent.db"))
	for _, path := range []string{config, database} {
		if _, err := os.Stat(path); err != nil {
			return
		}
	}
	if f.connectFails[name] {
		return
	}
	// frpc runs from the agent folder while the agent is up.
	if _, err := os.Stat(filepath.Join(agentDir, "frpc.exe")); err == nil {
		f.spawn(filepath.Join(agentDir, "frpc.exe"), "")
	}
	// The real agent writes the file once its session is up; the fake's
	// session is up at once.
	require.NoError(f.t, os.WriteFile(connected, []byte(fmt.Sprintf("%d %s %s\n", s.pid, version, name)), 0o644))
	// With Docker reachable, the agent recreates its containers from the
	// moved folder once (the recreate marker).
	if !f.dockerDown {
		f.consumeMarker(agentDir)
	}
	if name == winservice.ServiceName && f.adopts {
		Adopt(f.env())
	}
}

// consumeMarker is the agent's one-shot recreation of its containers after
// the folder moved (apps/recreate_marker.go, compose through its config
// hash): a container of an app bound into another folder is removed and,
// when the app should run, created anew (a new id, bound into agentDir,
// with the policy the app's configuration gives it). A stopped compose app
// keeps its old container: compose only recreates what it brings up.
func (f *fakeSystem) consumeMarker(agentDir string) {
	marker := filepath.Join(agentDir, winservice.RecreateMarkerName)
	if _, err := os.Stat(marker); err != nil {
		return
	}
	kept := make([]*fakeContainer, 0, len(f.containers))
	for _, c := range f.containers {
		app := strings.HasPrefix(c.Name, "prod_") || strings.HasPrefix(c.Name, "dev_")
		compose := strings.Contains(c.Name, "-")
		if !app || bindsUnder(c.Container, agentDir) || (compose && !c.wanted) {
			kept = append(kept, c)
			continue
		}
		if c.wanted {
			binds := make([]string, 0, len(c.Binds))
			for _, bind := range c.Binds {
				binds = append(binds, filepath.Join(agentDir, "apps", filepath.FromSlash(relativeToApps(bind))))
			}
			kept = append(kept, &fakeContainer{
				Container:  Container{ID: c.ID + "'", Name: c.Name, RestartPolicy: c.configured, Binds: binds, Running: true},
				configured: c.configured, wanted: true,
			})
		}
	}
	f.containers = kept
	require.NoError(f.t, os.Remove(marker))
	f.consumedMarkers[marker]++
}

// relativeToApps is a bind source's path below its apps folder (prod/app1).
func relativeToApps(bind string) string {
	s := strings.ReplaceAll(bind, `\`, "/")
	if i := strings.LastIndex(s, "/apps/"); i >= 0 {
		return s[i+len("/apps/"):]
	}
	return filepath.Base(s)
}

// env is the Env the simulated agent uses for its own calls into the
// package (outside the counted migrator calls).
func (f *fakeSystem) env() Env {
	return Env{Layout: f.layout, Sys: &uncounted{f}, Version: testVersion}
}

// StopService: the SCM reports stopped at once; the process lingers.
func (f *fakeSystem) StopService(name string) error {
	f.call("stop %s", name)
	if err := f.fail("stop " + name); err != nil {
		return err
	}
	s, ok := f.services[name]
	if !ok {
		return fmt.Errorf("service %s: %w", name, os.ErrNotExist)
	}
	if s.state != StateRunning {
		return nil
	}
	s.state = StateStopped
	for _, p := range f.processes {
		if p.pid == s.pid || (p.service == "" && underDir(p.exe, filepath.Dir(s.spec.Exe)) && f.stopLinger > 0) {
			p.exitsAt = f.now.Add(f.stopLinger)
		}
	}
	s.pid = 0
	return nil
}

func (f *fakeSystem) DeleteService(name string) error {
	f.call("delete %s", name)
	if err := f.fail("delete " + name); err != nil {
		return err
	}
	f.reap()
	s, ok := f.services[name]
	if !ok {
		return fmt.Errorf("service %s: %w", name, os.ErrNotExist)
	}
	if s.state == StateRunning {
		s.markedForDelete = true
		return nil
	}
	delete(f.services, name)
	return nil
}

func (f *fakeSystem) InstallEventSource(name string) error {
	f.call("event-source %s", name)
	f.events[name] = true
	return nil
}

func (f *fakeSystem) RemoveEventSource(name string) error {
	f.call("remove-event-source %s", name)
	delete(f.events, name)
	return nil
}

// --- the repair scheduled task ---

func (f *fakeSystem) ExportTask(name string) ([]byte, error) {
	if f.task == nil {
		return nil, fmt.Errorf("task %s: %w", name, os.ErrNotExist)
	}
	return []byte("<Task><Exec><Command>" + f.task.script + "</Command></Exec></Task>"), nil
}

func (f *fakeSystem) CreateTask(name, script string) error {
	f.call("task %s", f.rel(script))
	if err := f.fail("task " + f.rel(script)); err != nil {
		return err
	}
	f.task = &fakeTask{script: script}
	return nil
}

func (f *fakeSystem) ImportTask(name string, xml []byte) error {
	f.call("task-import")
	if err := f.fail("task-import"); err != nil {
		return err
	}
	text := string(xml)
	_, rest, ok := strings.Cut(text, "<Command>")
	if !ok {
		return errors.New("no <Command> in the task definition")
	}
	script, _, _ := strings.Cut(rest, "</Command>")
	f.task = &fakeTask{script: script}
	return nil
}

// --- Windows Defender ---

func (f *fakeSystem) AddDefenderExclusion(path string) error {
	f.call("defender-add %s", f.rel(path))
	f.defender[path] = true
	return nil
}

func (f *fakeSystem) RemoveDefenderExclusion(path string) error {
	f.call("defender-remove %s", f.rel(path))
	delete(f.defender, path)
	return nil
}

// --- Docker ---

func (f *fakeSystem) Containers() ([]Container, error) {
	if f.dockerDown {
		return nil, errors.New("error during connect: Get \"http://%2F%2F.%2Fpipe%2Fdocker_engine/v1.47/containers/json\": open //./pipe/docker_engine: The system cannot find the file specified")
	}
	out := make([]Container, 0, len(f.containers))
	for _, c := range f.containers {
		out = append(out, c.Container)
	}
	return out, nil
}

func (f *fakeSystem) container(id string) (*fakeContainer, error) {
	if f.dockerDown {
		return nil, errors.New("Docker is not reachable")
	}
	for _, c := range f.containers {
		if c.ID == id {
			return c, nil
		}
	}
	return nil, fmt.Errorf("container %s: %w", id, os.ErrNotExist)
}

func (f *fakeSystem) SetRestartPolicy(id, policy string) error {
	f.call("restart-policy %s %s", id, policy)
	if err := f.fail("restart-policy " + id); err != nil {
		return err
	}
	c, err := f.container(id)
	if err != nil {
		return err
	}
	c.RestartPolicy = policy
	return nil
}

func (f *fakeSystem) StopContainer(id string) error {
	f.call("stop-container %s", id)
	if err := f.fail("stop-container " + id); err != nil {
		return err
	}
	c, err := f.container(id)
	if err != nil {
		return err
	}
	c.Running = false
	return nil
}

// --- the machine ---

func (f *fakeSystem) Harden(root string) error {
	f.call("harden %s", f.rel(root))
	f.hardened = append(f.hardened, root)
	return nil
}

func (f *fakeSystem) OwnedByAdministrators(path string) (bool, error) {
	return f.owners[path] != "user", nil
}

func (f *fakeSystem) DockerDesktopSharing() (bool, []string) { return f.hyperV, f.shared }
func (f *fakeSystem) Elevated() bool                         { return f.elevated }
func (f *fakeSystem) Now() time.Time                         { return f.now }

func (f *fakeSystem) Sleep(d time.Duration) {
	f.call("sleep %s", d)
	f.now = f.now.Add(d)
	f.reap()
}

// rel names a path below the root with forward slashes on every OS, so the
// tests' fault keys ("rename Reagent/reagent.db-wal") match on Windows too.
func (f *fakeSystem) rel(path string) string {
	rel, err := filepath.Rel(f.layout.Root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return filepath.ToSlash(rel)
}

// uncounted is the fake as the simulated agent and the boot use it: their
// calls are not the migrator's and never cut the power.
type uncounted struct{ *fakeSystem }

func (u *uncounted) WriteFile(path string, data []byte, mode os.FileMode) error {
	return u.pause(func() error { return u.fakeSystem.WriteFile(path, data, mode) })
}
func (u *uncounted) Rename(from, to string) error {
	return u.pause(func() error { return u.fakeSystem.Rename(from, to) })
}
func (u *uncounted) Remove(path string) error {
	return u.pause(func() error { return u.fakeSystem.Remove(path) })
}
func (u *uncounted) RemoveAll(path string) error {
	return u.pause(func() error { return u.fakeSystem.RemoveAll(path) })
}
func (u *uncounted) MkdirAll(path string, mode os.FileMode) error {
	return u.pause(func() error { return u.fakeSystem.MkdirAll(path, mode) })
}
func (u *uncounted) CopyFile(from, to string, mode os.FileMode) error {
	return u.pause(func() error { return u.fakeSystem.CopyFile(from, to, mode) })
}
func (u *uncounted) KillProcess(pid int) error {
	return u.pause(func() error { return u.fakeSystem.KillProcess(pid) })
}
func (u *uncounted) CreateOrUpdateService(spec winservice.Spec) error {
	return u.pause(func() error { return u.fakeSystem.CreateOrUpdateService(spec) })
}
func (u *uncounted) SetStartType(name string, start winservice.StartType, delayed bool) error {
	return u.pause(func() error { return u.fakeSystem.SetStartType(name, start, delayed) })
}
func (u *uncounted) SetDisplayName(name, display string) error {
	return u.pause(func() error { return u.fakeSystem.SetDisplayName(name, display) })
}
func (u *uncounted) StartService(name string) error {
	return u.pause(func() error { return u.fakeSystem.StartService(name) })
}
func (u *uncounted) StopService(name string) error {
	return u.pause(func() error { return u.fakeSystem.StopService(name) })
}
func (u *uncounted) DeleteService(name string) error {
	return u.pause(func() error { return u.fakeSystem.DeleteService(name) })
}
func (u *uncounted) InstallEventSource(name string) error {
	return u.pause(func() error { return u.fakeSystem.InstallEventSource(name) })
}
func (u *uncounted) RemoveEventSource(name string) error {
	return u.pause(func() error { return u.fakeSystem.RemoveEventSource(name) })
}
func (u *uncounted) CreateTask(name, script string) error {
	return u.pause(func() error { return u.fakeSystem.CreateTask(name, script) })
}
func (u *uncounted) ImportTask(name string, xml []byte) error {
	return u.pause(func() error { return u.fakeSystem.ImportTask(name, xml) })
}
func (u *uncounted) AddDefenderExclusion(path string) error {
	return u.pause(func() error { return u.fakeSystem.AddDefenderExclusion(path) })
}
func (u *uncounted) RemoveDefenderExclusion(path string) error {
	return u.pause(func() error { return u.fakeSystem.RemoveDefenderExclusion(path) })
}
func (u *uncounted) SetRestartPolicy(id, policy string) error {
	return u.pause(func() error { return u.fakeSystem.SetRestartPolicy(id, policy) })
}
func (u *uncounted) StopContainer(id string) error {
	return u.pause(func() error { return u.fakeSystem.StopContainer(id) })
}
func (u *uncounted) Harden(root string) error {
	return u.pause(func() error { return u.fakeSystem.Harden(root) })
}
func (u *uncounted) Sleep(d time.Duration) {
	_ = u.pause(func() error { u.fakeSystem.Sleep(d); return nil })
}

func (u *uncounted) pause(fn func() error) error {
	counting := u.counting
	u.counting = false
	defer func() { u.counting = counting }()
	return fn()
}

// --- the device ---

// device is a Windows PC with a service install from before 1.4.0 whose
// reagent service already updated itself to 1.4.0 (so it runs reagent.exe
// of this version), an app with a bind mount into the folder, and a compose
// app with a restart policy.
type device struct {
	t    *testing.T
	root string
	sys  *fakeSystem
	// legacyDir is where the install sits (the default folder unless a test
	// chooses another).
	legacyDir string
}

const (
	legacyPID    = 1000
	legacyExe    = "reagent.exe"
	legacyRepair = "@echo off\r\nrem IronFlock agent repair (registered as scheduled task 'IronFlock Agent Repair')\r\nrem Restores the service binary if an interrupted self-update left it missing.\r\nset \"AGENTDIR=%s\"\r\nif exist \"%%AGENTDIR%%\\reagent.exe\" goto start\r\nif exist \"%%AGENTDIR%%\\reagent-prev.exe\" (\r\n  ren \"%%AGENTDIR%%\\reagent-prev.exe\" \"reagent.exe\"\r\n  goto start\r\n)\r\nfor /f \"delims=\" %%%%F in ('dir /b /o-d \"%%AGENTDIR%%\\reagent-v*.exe\" 2^>nul') do (\r\n  ren \"%%AGENTDIR%%\\%%%%F\" \"reagent.exe\"\r\n  goto start\r\n)\r\n:start\r\nsc start reagent\r\n"
)

func newDevice(t *testing.T) *device {
	return newDeviceIn(t, "")
}

// newDeviceIn makes the device with the agent folder below root at dir
// ("" for the default Reagent folder).
func newDeviceIn(t *testing.T, dir string) *device {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	root = filepath.Join(root, "IronFlock")
	require.NoError(t, os.MkdirAll(root, 0o755))
	d := &device{t: t, root: root, sys: newFakeSystem(t, root)}
	d.legacyDir = filepath.Join(root, winservice.LegacyFolder)
	if dir != "" {
		d.legacyDir = dir
	}
	ld := d.legacyDir
	d.file(filepath.Join(ld, legacyExe), testVersion, 0o755)
	d.file(filepath.Join(ld, "reagent-prev.exe"), "1.3.8", 0o755)
	d.file(filepath.Join(ld, "reagent-v1.3.8.exe"), "1.3.8", 0o755)
	d.file(filepath.Join(ld, "reagent-failed-v1.3.9.exe"), "1.3.9", 0o755)
	d.file(filepath.Join(ld, "reagent.db"), "database", 0o644)
	d.file(filepath.Join(ld, "reagent.db-wal"), "wal", 0o644)
	d.file(filepath.Join(ld, "reagent.db-shm"), "shm", 0o644) // a terminated SQLite leaves both
	d.file(filepath.Join(ld, "reagent.log"), "log", 0o644)
	d.file(filepath.Join(ld, "reagent-2026-10-17T10-00-00.000.log"), "rotated", 0o644)
	d.file(filepath.Join(ld, winservice.ConfigName), `{"secret":"s3cr3t"}`, 0o600)
	d.file(filepath.Join(ld, "frpc.exe"), "frpc", 0o755)
	d.file(filepath.Join(ld, "frpc.yaml"), "frpc config", 0o644)
	d.file(filepath.Join(ld, "proxy.env"), "HTTPS_PROXY=http://proxy:3128\n", 0o600)
	d.file(filepath.Join(ld, "last-connected"), "2026-10-23T11:50:00Z\n", 0o644)
	d.file(filepath.Join(ld, winservice.RepairName), fmt.Sprintf(legacyRepair, ld), 0o644)
	d.file(filepath.Join(ld, "downloads", "reagent-v1.3.8"), "partial", 0o644)
	d.file(filepath.Join(ld, "apps", "prod", "app1", "data.txt"), "app data", 0o644)
	d.file(filepath.Join(ld, "apps", "prod", "app2", "pg", "base"), "postgres", 0o644)
	d.file(filepath.Join(ld, "apps", "shared", "x"), "shared", 0o644)

	args := []string{
		"-config", filepath.Join(ld, winservice.ConfigName),
		"-agentDir", ld,
		"-appsDir", filepath.Join(ld, "apps"),
		"-dbFileName", filepath.Join(ld, "reagent.db"),
		"-logFile", filepath.Join(ld, "reagent.log"),
		"-debug",
	}
	d.sys.services[winservice.LegacyServiceName] = &fakeService{
		spec: winservice.Spec{
			Name: winservice.LegacyServiceName, DisplayName: winservice.DisplayName, Description: winservice.Description,
			Exe: filepath.Join(ld, legacyExe), Args: args,
			RestartDelays: []int{5, 30, 120}, ResetAfter: 86400, NonCrashRecovery: true,
			Env: []string{"HTTPS_PROXY=http://proxy:3128"},
		},
		startType: winservice.StartAutomatic,
		delayed:   true,
	}
	d.sys.task = &fakeTask{script: filepath.Join(ld, winservice.RepairName)}
	d.sys.events[winservice.LegacyServiceName] = true
	d.sys.defender[filepath.Join(ld, "frpc.exe")] = true
	d.sys.containers = []*fakeContainer{
		{Container: Container{ID: "c1", Name: "prod_1_app1", RestartPolicy: "no",
			Binds: []string{filepath.Join(ld, "apps", "prod", "app1"), filepath.Join(ld, "apps", "shared")}, Running: true}, configured: "no", wanted: true},
		{Container: Container{ID: "c2", Name: "prod_2_app2-db-1", RestartPolicy: "always",
			// as a WSL engine or Docker Desktop reports it: lower case, forward slashes
			Binds: []string{strings.ToLower(strings.ReplaceAll(filepath.Join(ld, "apps", "prod", "app2"), `\`, "/"))}, Running: true}, configured: "always", wanted: true},
		{Container: Container{ID: "c3", Name: "other", RestartPolicy: "always", Binds: []string{filepath.Join(root, "..", "elsewhere")}, Running: true}, configured: "always", wanted: true},
	}
	// The legacy agent runs and is connected.
	d.sys.nextPID = legacyPID
	require.NoError(t, d.sys.startService(winservice.LegacyServiceName))
	require.Equal(t, legacyPID, d.sys.services[winservice.LegacyServiceName].pid)
	return d
}

func (d *device) file(path, content string, mode os.FileMode) {
	d.t.Helper()
	require.NoError(d.t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(d.t, os.WriteFile(path, []byte(content), mode))
}

func (d *device) newDir() string { return filepath.Join(d.root, winservice.Folder) }

func (d *device) env() Env {
	return Env{
		Layout:        Layout{Root: d.root},
		Sys:           d.sys,
		Version:       testVersion,
		HealthTimeout: time.Minute,
		StableFor:     time.Minute,
		Logf:          func(format string, args ...any) { d.t.Logf(format, args...) },
	}
}

// selfExe is the running legacy agent's executable (what Begin copies).
func (d *device) selfExe() string { return filepath.Join(d.legacyDir, legacyExe) }

// stage starts a migration as the legacy agent would (Check + Stage), and
// starts the migrate service as Begin would, without running it. From here
// on the migrator's calls count.
func (d *device) stage(e Env) {
	d.t.Helper()
	require.NoError(d.t, Check(e))
	require.NoError(d.t, Begin(e, d.selfExe()))
	d.sys.counting = true
}

// run is the migrate service's process: Run, then the process is gone.
func (d *device) run(e Env) error {
	err := Run(e)
	d.migratorExits()
	return err
}

func (d *device) migratorExits() {
	if s := d.sys.services[winservice.MigrateServiceName]; s != nil && s.state == StateRunning {
		delete(d.sys.processes, s.pid)
		s.state, s.pid = StateStopped, 0
		d.sys.reap()
	}
}

// powerCut: every process is gone; the SCM's state and the files stay.
func (d *device) powerCut() {
	d.sys.counting = false
	d.sys.processes = map[int]*fakeProcess{}
	for name, s := range d.sys.services {
		s.state, s.pid, s.restartAt = StateStopped, 0, time.Time{}
		if s.markedForDelete {
			delete(d.sys.services, name)
		}
	}
	for _, c := range d.sys.containers {
		c.cutRunning = c.Running
		c.Running = false
	}
}

// boot is what Windows does after a power cut: the ONSTART repair task runs,
// the automatic services whose executable exists start (delayed ones last),
// Docker starts the containers whose restart policy says so ("always" even
// one that was stopped by hand, "unless-stopped" only one that ran) and
// recreates the folders their bind mounts name.
func (d *device) boot() {
	d.t.Helper()
	d.bootServices(nil)
}

// bootRealOrder is the boot with the migrate service's run where Windows
// puts it: the service is automatic and not delayed, so it runs before the
// delayed agent service starts.
func (d *device) bootRealOrder() {
	d.t.Helper()
	d.bootServices(func() {
		if d.migrateServiceRunning() {
			_ = d.run(d.env())
		}
	})
}

func (d *device) bootServices(betweenStarts func()) {
	d.t.Helper()
	d.powerCut()
	d.sys.now = d.sys.now.Add(90 * time.Second)
	d.runRepairTask()
	names := make([]string, 0, len(d.sys.services))
	for name := range d.sys.services {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := d.sys.services[names[i]], d.sys.services[names[j]]
		if a.delayed != b.delayed {
			return !a.delayed
		}
		return names[i] < names[j]
	})
	delayedStarted := false
	for _, name := range names {
		s, ok := d.sys.services[name] // the agent's start may have deleted one
		if !ok {
			continue
		}
		if s.delayed && !delayedStarted {
			delayedStarted = true
			if betweenStarts != nil {
				betweenStarts()
			}
		}
		if s.startType == winservice.StartAutomatic {
			_ = d.sys.startService(name)
		}
	}
	if d.sys.dockerAtBoot && !d.sys.dockerDown {
		for _, c := range d.sys.containers {
			if c.RestartPolicy == "always" || (c.cutRunning && c.RestartPolicy == "unless-stopped") {
				c.Running = true
				for _, bind := range c.Binds {
					require.NoError(d.t, os.MkdirAll(d.hostPath(bind), 0o755))
				}
			}
		}
	}
	d.sys.counting = true
}

// hostPath is the host folder of a bind source as the fake's containers
// spell it (lower case and forward slashes stand for an engine's view).
func (d *device) hostPath(bind string) string {
	// Windows paths are case-insensitive: an engine's lower-case spelling
	// names the folder the device has (the same on a case-sensitive Linux
	// test host). Known folders keep the device's own spelling.
	normalized := strings.ToLower(strings.ReplaceAll(bind, `\`, "/"))
	for _, dir := range []string{d.legacyDir, d.newDir(), d.root} {
		prefix := strings.ToLower(strings.ReplaceAll(dir, `\`, "/"))
		if rest, ok := strings.CutPrefix(normalized, prefix); ok && (rest == "" || strings.HasPrefix(rest, "/")) {
			return filepath.Join(dir, filepath.FromSlash(rest))
		}
	}
	return bind
}

// migrateServiceRunning reports whether the boot started the migrator.
func (d *device) migrateServiceRunning() bool {
	s := d.sys.services[winservice.MigrateServiceName]
	return s != nil && s.state == StateRunning
}

// runMigratorAfterBoot runs the migrate service as the SCM would after a
// boot: when it started, Run until the journal is final; the SCM's recovery
// starts it again after a stop that left the journal in progress.
func (d *device) runMigratorAfterBoot() {
	d.t.Helper()
	defer d.agentRetriesAdoption()
	for attempt := 0; attempt < 2*maxRuns+2 && d.migrateServiceRunning(); attempt++ {
		_ = d.run(d.env())
		j, err := LoadJournal(d.env().Layout)
		if err != nil || j == nil || j.Done || j.Failed() {
			return
		}
		// the migrate service's recovery action, 30 s later
		d.sys.now = d.sys.now.Add(30 * time.Second)
		_ = d.sys.startService(winservice.MigrateServiceName)
	}
}

// agentRetriesAdoption is the new agent's retry of Adopt (every 30 s while
// the migrate service runs): the agent, connected as ironflock-device-agent,
// adopts a rolled-back journal once the migrate service has stopped.
func (d *device) agentRetriesAdoption() {
	d.t.Helper()
	if !d.sys.adopts || d.connectedService() != winservice.ServiceName {
		return
	}
	Adopt(d.sys.env())
}

// runRepairTask interprets the ONSTART task's script: the legacy agent's
// repair.cmd (1.3.x), the current RepairCmd, or UniversalRepairCmd. It
// follows the generated scripts line by line in spirit; the Windows
// contract test runs the real ones over the same fixtures.
func (d *device) runRepairTask() {
	d.t.Helper()
	if d.sys.task == nil {
		return
	}
	data, err := os.ReadFile(d.sys.task.script)
	if err != nil {
		return // a task whose script is gone does nothing
	}
	script := string(data)
	vars := map[string]string{}
	for _, line := range strings.Split(script, "\r\n") {
		if strings.HasPrefix(line, "set \"") && strings.HasSuffix(line, "\"") {
			name, value, _ := strings.Cut(strings.TrimSuffix(strings.TrimPrefix(line, "set \""), "\""), "=")
			if value != "" {
				// The scripts join their own paths with a backslash
				// (winservice.UniversalRepairCmd); the fake runs them on
				// the host's filesystem.
				vars[name] = filepath.FromSlash(strings.ReplaceAll(value, `\`, "/"))
			}
		}
	}
	if strings.Contains(script, ":agents") {
		d.runUniversalRepair(vars)
		return
	}
	// RepairCmd or the legacy repair.cmd: one service, one folder.
	service, name := winservice.ServiceName, "device-agent"
	if strings.Contains(script, "sc start reagent") {
		service, name = winservice.LegacyServiceName, "reagent"
	}
	d.restoreExe(vars["AGENTDIR"], name)
	_ = d.sys.startService(service)
}

func (d *device) runUniversalRepair(vars map[string]string) {
	root, migrate, newDir, oldDir := vars["ROOT"], vars["MIGRATE"], vars["NEWDIR"], vars["OLDDIR"]
	migrateSvc := d.sys.services[winservice.MigrateServiceName]
	journal := filepath.Join(root, winservice.JournalName)
	if _, err := os.Stat(journal); err == nil {
		migrator := filepath.Join(migrate, winservice.ExeName)
		// :verified — the SHA-256 recorded when the migrator was staged, or
		// none recorded; nothing is run
		want, wantErr := os.ReadFile(migrator + ".sha256")
		verified := func(path string) bool {
			data, err := os.ReadFile(path)
			if err != nil {
				return false
			}
			if wantErr != nil {
				return true
			}
			sum := sha256.Sum256(data)
			return strings.EqualFold(hex.EncodeToString(sum[:]), strings.TrimSpace(string(want)))
		}
		if _, err := os.Stat(migrator); err == nil && !verified(migrator) {
			require.NoError(d.t, os.Remove(migrator))
			delete(d.sys.brokenExe, migrator)
		}
		if _, err := os.Stat(migrator); err != nil {
			_ = os.MkdirAll(migrate, 0o755)
			candidates := []string{
				filepath.Join(newDir, winservice.ExeName), filepath.Join(oldDir, winservice.LegacyExeName), filepath.Join(oldDir, winservice.ExeName),
				filepath.Join(newDir, "device-agent-prev.exe"), filepath.Join(oldDir, "device-agent-prev.exe"),
			}
			candidates = append(candidates, newestDownload(newDir, "device-agent-v*.exe")...)
			candidates = append(candidates, newestDownload(oldDir, "reagent-v*.exe")...)
			fallback := ""
			for _, candidate := range candidates {
				if _, err := os.Stat(candidate); err != nil {
					continue
				}
				if fallback == "" {
					fallback = candidate
				}
				if verified(candidate) {
					fallback = candidate
					break
				}
			}
			if fallback != "" {
				data, err := os.ReadFile(fallback)
				require.NoError(d.t, err)
				require.NoError(d.t, writeViaTemp(migrator, data, 0o755))
			}
		}
		// :migrate — an enabled migrate service is started; a disabled one
		// is a rolled-back or done move (the agents' branch), unless the
		// journal is neither: then the power cut took its configuration, and
		// it is enabled again (sc config start= auto) and started
		if migrateSvc != nil {
			text, _ := os.ReadFile(journal)
			final := strings.Contains(string(text), `"done"`) || strings.Contains(string(text), `"failed_at"`)
			if migrateSvc.startType == winservice.StartDisabled && !final {
				migrateSvc.startType = winservice.StartAutomatic
			}
			if migrateSvc.startType != winservice.StartDisabled {
				_ = d.sys.startService(winservice.MigrateServiceName)
				return
			}
		}
	}
	for _, svc := range []struct{ service, dir, name string }{{winservice.ServiceName, newDir, "device-agent"}, {winservice.LegacyServiceName, oldDir, "reagent"}} {
		s := d.sys.services[svc.service]
		if s == nil || s.startType == winservice.StartDisabled {
			continue
		}
		if !d.restoreExe(svc.dir, svc.name) {
			// the universal script's last resort: the move's own rollback target
			if data, err := os.ReadFile(filepath.Join(svc.dir, "device-agent-prev.exe")); err == nil {
				require.NoError(d.t, os.WriteFile(filepath.Join(svc.dir, svc.name+".exe"), data, 0o755))
			}
		}
		_ = d.sys.startService(svc.service)
	}
}

// restoreExe is the scripts' "if the exe is missing, rename prev, else the
// newest -v download, into place"; it reports whether the exe is there.
func (d *device) restoreExe(dir, name string) bool {
	exe := filepath.Join(dir, name+".exe")
	if _, err := os.Stat(exe); err == nil {
		return true
	}
	prev := filepath.Join(dir, name+"-prev.exe")
	if _, err := os.Stat(prev); err == nil {
		require.NoError(d.t, os.Rename(prev, exe))
		return true
	}
	if downloads := newestDownload(dir, name+"-v*.exe"); len(downloads) > 0 {
		require.NoError(d.t, os.Rename(downloads[0], exe))
		return true
	}
	return false
}

// pausedAfter runs a migration as the legacy agent and the migrate service
// would, up to the test hook's pause after step name, where the power goes.
func (d *device) pausedAfter(name string) {
	d.t.Helper()
	e := d.env()
	require.NoError(d.t, Check(e))
	require.NoError(d.t, Begin(e, d.selfExe()))
	e.Pause = name
	e.Sys = &crashOnLongSleep{d.sys}
	require.PanicsWithValue(d.t, errCrash, func() { _ = Run(e) })
	d.powerCut()
}

// newestDownload lists dir's files matching pattern, newest first (dir /o-d).
func newestDownload(dir, pattern string) []string {
	matches, _ := filepath.Glob(filepath.Join(dir, pattern))
	sort.Slice(matches, func(i, j int) bool {
		a, _ := os.Stat(matches[i])
		b, _ := os.Stat(matches[j])
		if a == nil || b == nil {
			return matches[i] < matches[j]
		}
		return a.ModTime().After(b.ModTime())
	})
	return matches
}

// snapshot is the device's files (not directories) below root, minus what
// the migration keeps for itself, the connected file and the recreate
// marker (both are the running agent's business). The root's path inside a
// file (the repair scripts name their folders) is replaced by "<root>", so
// the snapshots of two devices compare.
func (d *device) snapshot() map[string]string {
	d.t.Helper()
	skip := []string{winservice.JournalName, winservice.JournalName + ".bak", winservice.MigrateFolder}
	skipNames := map[string]bool{winservice.ConnectedName: true, winservice.RecreateMarkerName: true}
	out := map[string]string{}
	err := filepath.WalkDir(d.root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(d.root, path)
		rel = filepath.ToSlash(rel)
		for _, s := range skip {
			if rel == s || strings.HasPrefix(rel, s+"/") {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		if entry.IsDir() || skipNames[entry.Name()] {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[rel] = strings.ReplaceAll(string(data), d.root, "<root>")
		return nil
	})
	require.NoError(d.t, err)
	return out
}

// withoutDropped: prepare removes the legacy update files for good, and the
// rollback keeps the move's own rollback target (this version) as the spare
// reagent-prev.exe in their place; after a rollback the device equals its
// original state but for that.
func (d *device) withoutDropped(snap map[string]string) map[string]string {
	out := map[string]string{}
	rel, _ := filepath.Rel(d.root, d.legacyDir)
	rel = filepath.ToSlash(rel)
	for path, content := range snap {
		name := strings.TrimPrefix(path, rel+"/")
		if droppedLegacyFile(name) || strings.HasPrefix(name, "downloads/reagent-") {
			continue
		}
		out[path] = content
	}
	out[rel+"/reagent-prev.exe"] = testVersion
	return out
}

// policies are the live containers' restart policies by name (the agent
// recreates containers under new ids).
func (d *device) policies() map[string]string {
	out := map[string]string{}
	for _, c := range d.sys.containers {
		out[c.Name] = c.RestartPolicy
	}
	return out
}

// markerConsumed reports whether a simulated agent consumed the recreate
// marker of dir.
func (d *device) markerConsumed(dir string) bool {
	return d.sys.consumedMarkers[filepath.Join(dir, winservice.RecreateMarkerName)] > 0
}

func (d *device) journal() *Journal {
	d.t.Helper()
	j, err := LoadJournal(Layout{Root: d.root})
	require.NoError(d.t, err)
	require.NotNil(d.t, j)
	return j
}

// connectedService returns the name of a running agent service whose
// connected file names its own process at this version, "" when none.
func (d *device) connectedService() string {
	d.sys.reap()
	for _, name := range []string{winservice.ServiceName, winservice.LegacyServiceName} {
		s := d.sys.services[name]
		if s == nil || s.state != StateRunning {
			continue
		}
		agentDir := flagValue(s.spec.Args, "-agentDir", filepath.Dir(s.spec.Exe))
		record := d.env().readConnected(agentDir)
		if record.pid == s.pid && record.version == testVersion && record.service == name {
			return name
		}
	}
	return ""
}

func (d *device) serviceExists(name string) bool {
	_, ok := d.sys.services[name]
	return ok
}

func (d *device) serviceRunning(name string) bool {
	d.sys.reap()
	s := d.sys.services[name]
	return s != nil && s.state == StateRunning
}

func (d *device) exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func (d *device) content(path string) string {
	d.t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(d.t, err)
	return string(data)
}
