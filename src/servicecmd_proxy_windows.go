//go:build windows

package main

// The Windows side of the agent's proxy (docs/WINDOWS-PROXY.md): the host the
// start sequence and the commands use, and `service configure`, `service
// proxy` and `service capabilities`. The decisions are in serviceproxy.go.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reagent/config"
	"reagent/migratestate"
	"reagent/winservice"
	"time"
	"unsafe"

	"github.com/rs/zerolog/log"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// winProxyHost is the store in agentDir and the registry of service name.
type winProxyHost struct {
	agentDir string
	service  string
}

func (h winProxyHost) storePath() string { return filepath.Join(h.agentDir, winservice.ProxyEnvName) }

func (h winProxyHost) ReadStore() ([]byte, error) {
	return os.ReadFile(h.storePath())
}

func (h winProxyHost) WriteStore(data []byte) error {
	return winservice.WriteProtectedFile(h.storePath(), data)
}

func (h winProxyHost) WriteOld(data []byte) error {
	return winservice.WriteProtectedFile(filepath.Join(h.agentDir, winservice.ProxyEnvOldName), data)
}

func (h winProxyHost) ProtectStore() (bool, error) {
	return winservice.ProtectFile(h.storePath())
}

func (h winProxyHost) ServiceEnvironment() ([]string, error) {
	return winservice.Environment(h.service)
}

func (h winProxyHost) SetServiceEnvironment(env []string) error {
	err := winservice.SetEnvironment(h.service, env)
	if err == nil {
		winservice.FlushRegistry(winservice.ServicesKey)
	}
	return err
}

func (h winProxyHost) Setenv(name, value string) error { return os.Setenv(name, value) }
func (h winProxyHost) Unsetenv(name string) error      { return os.Unsetenv(name) }

func (h winProxyHost) SetProxyConfigured(configured bool) error {
	return winservice.SetProxyConfigured(h.service, configured)
}

// installedAgent finds the installed agent service, its folder (the
// -agentDir of its ImagePath, else the default one) and its .flock.
func installedAgent(m *mgr.Mgr) (name string, service *mgr.Service, agentDir string, flockPath string, err error) {
	name, service, err = openInstalledService(m)
	if err != nil {
		return "", nil, "", "", preconditionError{err}
	}
	cfg, err := service.Config()
	if err == nil {
		if fields, err := windows.DecomposeCommandLine(cfg.BinaryPathName); err == nil {
			agentDir = agentDirFromArgs(fields)
			for i, f := range fields {
				if f == "-config" && i+1 < len(fields) {
					flockPath = fields[i+1]
				}
			}
		}
	}
	if agentDir == "" {
		agentDir = filepath.Join(winservice.Root(os.Getenv("ProgramData")), winservice.Folder)
	}
	if flockPath == "" {
		flockPath = filepath.Join(agentDir, winservice.ConfigName)
	}
	return name, service, agentDir, flockPath, nil
}

// --- service capabilities ---------------------------------------------------------

func serviceCapabilitiesCommand(args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, "usage: device-agent.exe service capabilities")
		return exitUsage
	}
	for _, c := range serviceCapabilities {
		fmt.Println(c)
	}
	return exitOK
}

// --- service proxy ----------------------------------------------------------------

func serviceProxyCommand(args []string) int {
	err := serviceProxyShow(args, os.Stdout)
	if err != nil && !errors.Is(err, errAlreadyReported) {
		fmt.Fprintln(os.Stderr, "error:", err)
	}
	return exitCodeOf(err)
}

// errAlreadyReported: the command printed what went wrong itself.
var errAlreadyReported = errors.New("reported")

func serviceProxyShow(args []string, out io.Writer) error {
	if len(args) > 0 {
		return usagef("usage: device-agent.exe service proxy")
	}
	if !windows.GetCurrentProcessToken().IsElevated() {
		// Without administrator rights neither the service manager (as
		// mgr.Connect opens it) nor the store opens: the non-secret value in
		// the service's registry key is all there is.
		name := ""
		for _, candidate := range []string{winservice.ServiceName, winservice.LegacyServiceName} {
			if winservice.ServiceKeyExists(candidate) {
				name = candidate
				break
			}
		}
		if name == "" {
			return preconditionf("service %q is not installed", winservice.ServiceName)
		}
		configured, known, err := winservice.ProxyConfigured(name)
		value := "unknown"
		if err == nil && known {
			value = fmt.Sprintf("%t", configured)
		}
		_, _ = fmt.Fprintln(out, "proxy.readable=false")
		_, _ = fmt.Fprintln(out, "proxy.configured="+value)
		return nil
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("failed to connect to the service manager: %w", err)
	}
	defer func() { _ = m.Disconnect() }()
	name, service, agentDir, flockPath, err := installedAgent(m)
	if err != nil {
		return err
	}
	_ = service.Close()

	h := winProxyHost{agentDir: agentDir, service: name}
	data, err := h.ReadStore()
	storeExists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("could not read %s: %w", h.storePath(), err)
	}
	stored := winservice.ParseProxyEnv(data)
	env, _ := h.ServiceEnvironment()
	fromEnv, inEnv := winservice.ProxyFromEnvironment(env)
	settings, source := stored, proxySourceStore
	switch {
	case inEnv && (!storeExists || fromEnv != stored):
		settings, source = fromEnv, proxySourceServiceEnv
	case !stored.Configured():
		source = proxySourceNone
	}
	_, _ = fmt.Fprintln(out, "proxy.readable=true")
	effective := ""
	if settings.Configured() {
		effective = effectiveNoProxy(gatherNoProxyInputs(settings.NoProxy, flockPath))
	}
	for _, line := range proxyReportLines(settings, h.storePath(), effective) {
		_, _ = fmt.Fprintln(out, line)
	}
	_, _ = fmt.Fprintln(out, "proxy.source="+source)
	return nil
}

// --- service configure ------------------------------------------------------------

func serviceConfigureCommand(args []string) int {
	err := serviceConfigure(args, os.Stdin, os.Stdout, os.Stderr)
	if err != nil && !errors.Is(err, flag.ErrHelp) && !errors.Is(err, errAlreadyReported) {
		fmt.Fprintln(os.Stderr, "error:", err)
	}
	var written writtenNotRunError
	if errors.As(err, &written) {
		return exitWrittenNotRun
	}
	return exitCodeOf(err)
}

// writtenNotRunError: the settings are written, the restart did not work.
type writtenNotRunError struct{ error }

func serviceConfigure(args []string, stdin io.Reader, out, errOut io.Writer) error {
	opts, err := parseServiceConfigureFlags(args, errOut)
	if err != nil {
		return err
	}
	if !windows.GetCurrentProcessToken().IsElevated() {
		return preconditionf("this command must be run from an elevated (Administrator) prompt")
	}
	if migratestate.InProgress() {
		return preconditionf("the move to the %s service is in progress; `service configure` would interrupt it (see `device-agent.exe migrate-layout status`)", winservice.ServiceName)
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("failed to connect to the service manager: %w", err)
	}
	defer func() { _ = m.Disconnect() }()
	name, service, agentDir, flockPath, err := installedAgent(m)
	if err != nil {
		return err
	}
	defer func() { _ = service.Close() }()
	h := winProxyHost{agentDir: agentDir, service: name}

	data, err := h.ReadStore()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("could not read %s: %w", h.storePath(), err)
	}
	storeExists := err == nil
	from := winservice.ParseProxyEnv(data)
	env, err := h.ServiceEnvironment()
	if err != nil {
		return fmt.Errorf("could not read the environment of service %s: %w", name, err)
	}
	fromEnv, inEnv := winservice.ProxyFromEnvironment(env)
	if inEnv {
		// The agent's own move is pending: these are the values in effect.
		from = fromEnv
	}

	var password string
	if opts.Given("proxy-password-file") {
		if password, err = openProxyPassword(opts.PasswordFile, stdin); err != nil {
			return err
		}
	}
	to, err := configuredProxy(opts, from, password)
	if err != nil {
		return err
	}
	effective := ""
	if to.Configured() {
		effective = effectiveNoProxy(gatherNoProxyInputs(to.NoProxy, flockPath))
	}
	changed := to != from || (storeExists && !to.Configured()) || (!storeExists && to.Configured())

	status, err := service.Query()
	if err != nil {
		return fmt.Errorf("could not query service %s: %w", name, err)
	}
	restart := changed && !opts.NoRestart && status.State != svc.Stopped
	if restart {
		inside, err := insideService(status.ProcessId)
		if err != nil {
			_, _ = fmt.Fprintf(errOut, "warning: could not tell whether this runs inside the agent: %v\n", err)
		}
		if inside {
			return preconditionf("this runs inside the agent (the device terminal): restarting the service would end this command before it starts the service again; nothing changed. Run it with -no-restart and restart the agent from IronFlock, or run it in a terminal of this machine")
		}
	}

	if changed {
		if to.Configured() {
			err = h.WriteStore([]byte(winservice.RenderProxyEnv(to)))
		} else {
			err = os.Remove(h.storePath())
			if errors.Is(err, os.ErrNotExist) {
				err = nil
			}
		}
		if err != nil {
			return fmt.Errorf("could not write %s: %w", h.storePath(), err)
		}
		if inEnv {
			// Until the agent finishes its move, a rollback to the agent
			// before it reads only the registry: it keeps the new values
			// (docs/WINDOWS-PROXY.md, O2).
			// The agent before reads NO_PROXY as it is: the whole list.
			mirrored := to
			mirrored.NoProxy = effective
			if err := h.SetServiceEnvironment(winservice.WithProxyVariables(env, mirrored)); err != nil {
				_, _ = fmt.Fprintf(errOut, "warning: could not update the proxy in the environment of service %s, which the agent moves to %s: %v\n", name, winservice.ProxyEnvName, err)
			}
		}
	}
	if err := h.SetProxyConfigured(to.Configured()); err != nil {
		_, _ = fmt.Fprintf(errOut, "warning: could not record whether a proxy is configured: %v\n", err)
	}

	var restartErr error
	restarted := false
	if restart {
		_, _ = fmt.Fprintf(errOut, "Restarting service %s for the new proxy settings.\n", name)
		restartErr = restartService(service)
		restarted = restartErr == nil
	} else if changed && status.State != svc.Stopped {
		_, _ = fmt.Fprintf(errOut, "Not restarted (-no-restart): the settings apply at the next start of service %s.\n", name)
	} else if changed {
		_, _ = fmt.Fprintf(errOut, "Service %s is stopped: the settings apply at its next start.\n", name)
	}
	state := "unknown"
	if status, err := service.Query(); err == nil {
		state = serviceStateName(status.State)
	}

	_, _ = fmt.Fprintf(out, "proxy.changed=%t\n", changed)
	for _, line := range proxyReportLines(to, h.storePath(), effective) {
		_, _ = fmt.Fprintln(out, line)
	}
	_, _ = fmt.Fprintln(out, "service.name="+name)
	_, _ = fmt.Fprintf(out, "service.restarted=%t\n", restarted)
	_, _ = fmt.Fprintln(out, "service.state="+state)
	if restartErr != nil {
		return writtenNotRunError{fmt.Errorf("the proxy settings are written, but restarting service %s failed: %w; they apply at its next start", name, restartErr)}
	}
	return nil
}

// restartService stops the service (up to 30 s), starts it and waits up to
// 60 s for it to report running.
func restartService(service *mgr.Service) error {
	status, err := service.Control(svc.Stop)
	if err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
		return fmt.Errorf("stopping: %w", err)
	}
	for i := 0; i < 30 && err == nil && status.State != svc.Stopped; i++ {
		time.Sleep(time.Second)
		status, err = service.Query()
	}
	if err == nil && status.State != svc.Stopped {
		return fmt.Errorf("the service did not stop within 30 s")
	}
	if err := service.Start(); err != nil {
		return fmt.Errorf("starting: %w", err)
	}
	return waitForRunning(service, 60*time.Second)
}

func serviceStateName(state svc.State) string {
	switch state {
	case svc.Running:
		return "running"
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "start pending"
	case svc.StopPending:
		return "stop pending"
	}
	return "unknown"
}

// insideService reports whether this process descends from the service's
// process (pid): a terminal session the agent opened, whose job ends with the
// agent.
func insideService(pid uint32) (bool, error) {
	if pid == 0 {
		return false, nil
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false, err
	}
	defer func() { _ = windows.CloseHandle(snapshot) }()
	parent := map[uint32]uint32{}
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		parent[entry.ProcessID] = entry.ParentProcessID
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return false, err
	}
	return underProcess(parent, windows.GetCurrentProcessId(), pid), nil
}

// installProxyPassword reads the password of `service install`'s
// -proxy-password-file ("" without one).
func installProxyPassword(opts *serviceInstallOptions) (string, error) {
	if opts.PasswordFile == "" {
		return "", nil
	}
	return openProxyPassword(opts.PasswordFile, os.Stdin)
}

// protectExistingStore tightens the ACL of a store an earlier install left.
func protectExistingStore(path string) {
	if _, err := os.Stat(path); err != nil {
		return
	}
	repaired, err := winservice.ProtectFile(path)
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "warning: could not restrict %s to SYSTEM and Administrators: %v\n", path, err)
	case repaired:
		fmt.Printf("Restricted %s to SYSTEM and Administrators.\n", path)
	}
}

// gatherNoProxyInputs reads what the list (noproxy.go) is computed from on
// this machine. Nothing here waits on the network; whatever fails is logged
// and left out.
func gatherNoProxyInputs(user string, flockPath string) noProxyInputs {
	in := noProxyInputs{User: user}
	if name, err := os.Hostname(); err == nil {
		in.Hostnames = append(in.Hostnames, name)
	} else {
		log.Warn().Err(err).Msg("proxy: could not read the host name for NO_PROXY")
	}
	in.Hostnames = append(in.Hostnames, windowsDNSHostname()...)
	interfaces, err := net.Interfaces()
	if err != nil {
		log.Warn().Err(err).Msg("proxy: could not list the network interfaces for NO_PROXY")
	}
	var found []netInterface
	for _, iface := range interfaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		found = append(found, netInterface{Up: iface.Flags&net.FlagUp != 0, Loopback: iface.Flags&net.FlagLoopback != 0, Addrs: addrs})
	}
	in.Networks = interfaceNetworks(found)
	if flockPath != "" {
		flock, err := readFlockConfig(flockPath)
		if err != nil {
			log.Warn().Err(err).Msg("proxy: could not read the .flock for NO_PROXY")
		} else {
			in.Flock = flock
		}
	}
	return in
}

// readFlockConfig loads and parses a device .flock from disk.
func readFlockConfig(flockPath string) (*config.ReswarmConfig, error) {
	raw, err := os.ReadFile(flockPath)
	if err != nil {
		return nil, err
	}
	var flockCfg config.ReswarmConfig
	err = json.Unmarshal(raw, &flockCfg)
	if err != nil {
		return nil, fmt.Errorf("could not parse %s: %w", flockPath, err)
	}
	return &flockCfg, nil
}

// windowsDNSHostname is the machine's DNS name with its domain, when it has
// one beyond the host name.
func windowsDNSHostname() []string {
	n := uint32(256)
	buf := make([]uint16, n)
	if err := windows.GetComputerNameEx(windows.ComputerNameDnsFullyQualified, &buf[0], &n); err != nil {
		return nil
	}
	if name := windows.UTF16ToString(buf[:n]); name != "" {
		return []string{name}
	}
	return nil
}
