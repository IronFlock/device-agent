//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reagent/codesign"
	"reagent/config"
	"reagent/filesystem"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

// installTrustAnchors writes every embedded code-signing root cert to the
// agent dir and imports each into the machine trust stores: Trusted Root (so
// the self-signed signature chain validates) and TrustedPublisher (so UAC
// shows a verified publisher and WDAC/AppLocker publisher rules can allow it).
// More than one root during a rotation's overlap window. No-ops when no real
// root is embedded yet (pre-signing transition). All failures are warnings —
// the service runs regardless.
func installTrustAnchors(agentDir string) {
	for _, root := range codesign.EmbeddedRoots() {
		certPath := filepath.Join(agentDir, root.FileName)
		if err := os.WriteFile(certPath, root.PEM, 0644); err != nil { //nolint:gosec // G306: public CA certificate / repair script / Docker engine config, no secrets
			fmt.Fprintf(os.Stderr, "warning: could not write code-signing root %q: %v\n", root.FileName, err)
			continue
		}
		for _, store := range []string{"Root", "TrustedPublisher"} {
			if err := runCommand("certutil", certImportArgs(store, certPath)...); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not import %q into %s: %v\n", root.FileName, store, err)
			}
		}
	}
}

// removeTrustAnchors reverses installTrustAnchors at uninstall — leaving a
// self-signed root in Trusted Root is a lasting liability. Removes every
// embedded root by its common name.
func removeTrustAnchors(agentDir string) {
	for _, root := range codesign.EmbeddedRoots() {
		for _, store := range []string{"Root", "TrustedPublisher"} {
			if err := runCommand("certutil", certDeleteArgs(store, root.CommonName)...); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not remove %q from %s: %v\n", root.CommonName, store, err)
			}
		}
		_ = os.Remove(filepath.Join(agentDir, root.FileName))
	}
}

// Language-neutral SIDs (localized group names like "Administratoren" would
// break icacls on non-English Windows).
const (
	sidSystem         = "*S-1-5-18"
	sidAdministrators = "*S-1-5-32-544"
	sidUsers          = "*S-1-5-32-545"
)

func runServiceCommand(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: reagent service <install|uninstall|start|stop|status>")
		return 2
	}

	var err error
	switch args[0] {
	case "install":
		err = serviceInstall(args[1:])
	case "uninstall":
		err = serviceUninstall()
	case "start":
		err = serviceControl(func(s *mgr.Service) error { return s.Start() })
	case "stop":
		err = serviceStop()
	case "status":
		err = serviceStatus()
	default:
		fmt.Fprintf(os.Stderr, "unknown service command %q\nusage: reagent service <install|uninstall|start|stop|status>\n", args[0])
		return 2
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

func requireElevation() error {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return fmt.Errorf("this command must be run from an elevated (Administrator) prompt")
	}
	return nil
}

func serviceInstall(args []string) error {
	err := requireElevation()
	if err != nil {
		return err
	}

	opts, err := parseServiceInstallFlags(args, os.Getenv("ProgramData"), os.Stderr)
	if err != nil {
		return err
	}

	// Data from a previous manual installation must not be silently orphaned:
	// force an explicit -agentDir decision when any is found.
	if !opts.AgentDirSet {
		existingDirs := findExistingAgentDirs()
		if len(existingDirs) > 0 {
			return fmt.Errorf("%s", migrationAbortMessage(existingDirs, opts.AgentDir))
		}
	}

	_, err = os.Stat(opts.ConfigPath)
	if err != nil {
		return fmt.Errorf("cannot read the .flock config file: %w", err)
	}

	err = filesystem.InitDirectories(installCliArgs(opts))
	if err != nil {
		return fmt.Errorf("failed to create agent directories: %w", err)
	}

	// Harden the agent dir: the service self-update renames-and-executes
	// binaries from here as SYSTEM, and the .flock/.env-compose files contain
	// the device secret — regular users get no access at all. The apps dir
	// gets an explicit modify grant back: Docker Desktop bind mounts access
	// app /data in the interactive user's context.
	err = runCommand("icacls", opts.AgentDir, "/inheritance:r",
		"/grant", sidSystem+":(OI)(CI)F",
		"/grant", sidAdministrators+":(OI)(CI)F")
	if err != nil {
		return fmt.Errorf("failed to restrict permissions on %s: %w", opts.AgentDir, err)
	}
	err = runCommand("icacls", opts.AppsDir, "/grant", sidUsers+":(OI)(CI)M")
	if err != nil {
		return fmt.Errorf("failed to grant app-data permissions on %s: %w", opts.AppsDir, err)
	}

	installedExe := filepath.Join(opts.AgentDir, "reagent.exe")
	currentExe, err := os.Executable()
	if err != nil {
		return err
	}
	err = copyFileIfDifferent(currentExe, installedExe)
	if err != nil {
		return fmt.Errorf("failed to install the agent binary: %w", err)
	}

	installedConfig := filepath.Join(opts.AgentDir, serviceConfigName)
	err = copyFileIfDifferent(opts.ConfigPath, installedConfig)
	if err != nil {
		return fmt.Errorf("failed to install the .flock config: %w", err)
	}

	repairCmd := filepath.Join(opts.AgentDir, "repair.cmd")
	err = os.WriteFile(repairCmd, []byte(repairCmdContent(opts.AgentDir)), 0644) //nolint:gosec // G306: public CA certificate / repair script / Docker engine config, no secrets
	if err != nil {
		return fmt.Errorf("failed to write repair.cmd: %w", err)
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("failed to connect to the service manager: %w", err)
	}
	defer func() { _ = m.Disconnect() }()

	existing, err := m.OpenService(serviceName)
	if err == nil {
		_ = existing.Close()
		return fmt.Errorf("service %q is already installed — run 'reagent service uninstall' first", serviceName)
	}

	service, err := m.CreateService(serviceName, installedExe, mgr.Config{
		DisplayName:      serviceDisplayName,
		Description:      serviceDescription,
		StartType:        mgr.StartAutomatic,
		DelayedAutoStart: true,
		ErrorControl:     mgr.ErrorNormal,
	}, serviceBinaryArgs(opts)...)
	if err != nil {
		return fmt.Errorf("failed to create the service: %w", err)
	}
	defer func() { _ = service.Close() }()

	// From here on the service exists and can run. The remaining steps only harden it, so a
	// failure is a warning: leaving a freshly registered device offline because a resilience
	// nicety could not be configured is the worse outcome.

	// Restart on every failure, never give up: the last action repeats when
	// the list is exhausted, so the 120s delay throttles a crash loop while
	// deliberate exit-for-restart (updates, remote restart) recovers in 5s.
	err = service.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 120 * time.Second},
	}, 86400)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not set service recovery actions (the service will not auto-restart after a crash): %v\n", err)
	}
	// Also fire recovery when the process reports STOPPED with a non-zero
	// exit code (belt and braces next to the crash-style exits).
	err = service.SetRecoveryActionsOnNonCrashFailures(true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not enable recovery on non-crash failures: %v\n", err)
	}

	err = eventlog.InstallAsEventCreate(serviceName, eventlog.Error|eventlog.Warning|eventlog.Info)
	if err != nil && !strings.Contains(err.Error(), "registry key already exists") {
		fmt.Fprintf(os.Stderr, "warning: could not register the event log source: %v\n", err)
	}

	// Boot-time repair task: SCM recovery cannot fix a service whose binary
	// is missing (start failures are not service failures), the scheduled
	// task can.
	err = runCommand("schtasks", "/Create", "/F",
		"/TN", repairTaskName,
		"/SC", "ONSTART",
		"/RU", "SYSTEM",
		"/TR", "\""+repairCmd+"\"")
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not register the repair scheduled task (an interrupted self-update will need a manual repair): %v\n", err)
	}

	// Proxy settings come from proxy.env when the device already has one, so a
	// reinstall restores this site's configuration without the operator
	// remembering any flags. Flags seed that file on a fresh device.
	proxyEnvPath := filepath.Join(opts.AgentDir, proxyEnvFileName)
	proxyCfg, seeded, err := resolveProxyConfig(proxyEnvPath, opts, installedConfig)
	if err != nil {
		return err
	}
	if proxyCfg.configured() {
		err = setServiceProxy(proxyCfg)
		if err != nil {
			return fmt.Errorf("failed to set the service proxy environment: %w", err)
		}
		if seeded {
			fmt.Printf("Proxy configured, and written to %s — edit that file to change it.\n", proxyEnvPath)
		} else {
			fmt.Printf("Proxy configured from %s\n", proxyEnvPath)
		}
		fmt.Printf("Reached directly (NO_PROXY): %s\n", proxyCfg.NoProxy)
	}

	// Docker must trust the appliance's plain-HTTP registry or every app
	// install fails at the image pull. Best-effort: on failure the entry can
	// be added by hand via Docker Desktop → Settings → Docker Engine.
	err = ensureDockerInsecureRegistries(installedConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not configure Docker's insecure-registries (app installs will fail until the appliance registry is added via Docker Desktop → Settings → Docker Engine): %v\n", err)
	}

	// The registry token used as the docker username is longer than Windows
	// Credential Manager accepts, so the service account must not use the
	// credential helper — otherwise every `docker login` fails at the store
	// step and no app can ever be pulled.
	err = ensureDockerCredentialStore(installedConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not opt the registry out of the Windows credential helper (app installs may fail with \"error saving credentials\"): %v\n", err)
	}

	// Import our code-signing root so the agent's signed self-updates are
	// trusted (UAC verified publisher; on-device pinning). No-ops until a real
	// root is embedded. Non-fatal — an un-imported cert only weakens the
	// verified-publisher UX, it doesn't stop the service.
	installTrustAnchors(opts.AgentDir)

	// Exclude frpc.exe from Defender (frp is flagged PUA). Best-effort;
	// Tamper Protection / managed policy may ignore it.
	frpcPath := filepath.Join(opts.AgentDir, "frpc.exe")
	err = runCommand("powershell", "-NonInteractive", "-Command", defenderAddExclusionCmd(frpcPath))
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not add the Defender exclusion for frpc (tunnels may be quarantined on managed devices): %v\n", err)
	}

	fmt.Printf("Installed service %q (agent dir: %s)\n", serviceName, opts.AgentDir)

	if !opts.StartNow {
		fmt.Println("Not started (-noStart). Start it with: reagent service start")
		return nil
	}

	err = service.Start()
	if err != nil {
		return fmt.Errorf("installed, but failed to start the service: %w", err)
	}
	err = waitForRunning(service, 30*time.Second)
	if err != nil {
		// The service is installed and set to start automatically, so this is not
		// fatal — but say so plainly and point at the two places that explain why.
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		fmt.Fprintf(os.Stderr, "the service is installed and will retry at boot; check %s and the Windows event log (source %q)\n",
			filepath.Join(opts.AgentDir, "reagent.log"), serviceName)
		return nil
	}
	fmt.Println("Service started.")

	return nil
}

// waitForRunning polls until the service reports RUNNING, so `service install` reports what
// actually happened rather than merely that a start was requested. Execute() signals RUNNING
// before its WAMP/Docker init finishes, so this resolves quickly and a timeout is a real
// symptom rather than slow startup.
func waitForRunning(service *mgr.Service, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		status, err := service.Query()
		if err != nil {
			return fmt.Errorf("could not query the service after starting it: %w", err)
		}
		switch status.State {
		case svc.Running:
			return nil
		case svc.Stopped:
			return fmt.Errorf("the service stopped immediately after starting (exit code %d)", status.Win32ExitCode)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the service did not report running within %s", timeout)
		}
		time.Sleep(time.Second)
	}
}

func serviceUninstall() error {
	err := requireElevation()
	if err != nil {
		return err
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("failed to connect to the service manager: %w", err)
	}
	defer func() { _ = m.Disconnect() }()

	service, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("service %q is not installed", serviceName)
	}
	defer func() { _ = service.Close() }()

	// Recover the agent dir from the baked ImagePath before deleting, so the
	// dir-scoped side effects (Defender exclusion, cert file) can be reversed.
	agentDir := ""
	if cfg, cfgErr := service.Config(); cfgErr == nil {
		agentDir = agentDirFromImagePath(cfg.BinaryPathName)
	}

	status, err := service.Control(svc.Stop)
	if err == nil {
		// wait for the stop to complete before deleting
		for i := 0; i < 30 && status.State != svc.Stopped; i++ {
			time.Sleep(time.Second)
			status, err = service.Query()
			if err != nil {
				break
			}
		}
	}

	err = service.Delete()
	if err != nil {
		return fmt.Errorf("failed to delete the service: %w", err)
	}

	err = eventlog.Remove(serviceName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not remove the event log source: %v\n", err)
	}

	err = runCommand("schtasks", "/Delete", "/F", "/TN", repairTaskName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not remove the repair scheduled task: %v\n", err)
	}

	// Reverse the install-time trust + AV side effects (symmetric uninstall).
	if agentDir != "" {
		frpcPath := filepath.Join(agentDir, "frpc.exe")
		if rmErr := runCommand("powershell", "-NonInteractive", "-Command", defenderRemoveExclusionCmd(frpcPath)); rmErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not remove the Defender exclusion: %v\n", rmErr)
		}
		removeTrustAnchors(agentDir)
	}

	fmt.Printf("Uninstalled service %q. The agent directory and app data were kept.\n", serviceName)
	return nil
}

func serviceStop() error {
	return serviceControl(func(s *mgr.Service) error {
		status, err := s.Control(svc.Stop)
		if err != nil {
			return err
		}
		for i := 0; i < 30 && status.State != svc.Stopped; i++ {
			time.Sleep(time.Second)
			status, err = s.Query()
			if err != nil {
				return err
			}
		}
		if status.State != svc.Stopped {
			return fmt.Errorf("service did not stop in time")
		}
		return nil
	})
}

func serviceStatus() error {
	return serviceControl(func(s *mgr.Service) error {
		status, err := s.Query()
		if err != nil {
			return err
		}

		stateNames := map[svc.State]string{
			svc.Stopped:         "stopped",
			svc.StartPending:    "start pending",
			svc.StopPending:     "stop pending",
			svc.Running:         "running",
			svc.ContinuePending: "continue pending",
			svc.PausePending:    "pause pending",
			svc.Paused:          "paused",
		}
		name, ok := stateNames[status.State]
		if !ok {
			name = fmt.Sprintf("unknown (%d)", status.State)
		}
		fmt.Println(name)
		return nil
	})
}

func serviceControl(action func(*mgr.Service) error) error {
	err := requireElevation()
	if err != nil {
		return err
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("failed to connect to the service manager: %w", err)
	}
	defer func() { _ = m.Disconnect() }()

	service, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("service %q is not installed", serviceName)
	}
	defer func() { _ = service.Close() }()

	return action(service)
}

// findExistingAgentDirs locates data from previous manual (console)
// installations. The installer runs elevated, so %USERPROFILE% would be the
// admin's profile — scan every user profile instead. FlockFlasher's
// throwaway test-device dir (~\.Reflasher\agent) is deliberately not matched.
func findExistingAgentDirs() []string {
	systemDrive := os.Getenv("SystemDrive")
	if systemDrive == "" {
		systemDrive = "C:"
	}

	matches, err := filepath.Glob(filepath.Join(systemDrive+`\`, "Users", "*", "reagent", "reagent.db"))
	if err != nil {
		return nil
	}

	dirs := make([]string, 0, len(matches))
	for _, match := range matches {
		dirs = append(dirs, filepath.Dir(match))
	}
	return dirs
}

// dockerDaemonJSONPath resolves the daemon config file the Docker on this
// host actually reads. Docker Desktop (the supported runtime for
// Linux-container apps on edge PCs) reads the installing user's
// %USERPROFILE%\.docker\daemon.json — the file behind Settings → Docker
// Engine; a plain Docker Engine Windows service reads
// %ProgramData%\docker\config\daemon.json. The engine path is chosen only
// when Docker Desktop is absent and the engine's data dir exists; otherwise
// the Desktop path, which is also the right seed when Docker is not yet
// installed at all.
func dockerDaemonJSONPath() (string, error) {
	desktopExe := filepath.Join(os.Getenv("ProgramFiles"), "Docker", "Docker", "Docker Desktop.exe")
	_, desktopErr := os.Stat(desktopExe) //nolint:gosec // G703: paths from the installer layout and Program Files, not remote input

	if desktopErr != nil {
		engineDir := filepath.Join(os.Getenv("ProgramData"), "docker")
		if _, err := os.Stat(engineDir); err == nil { //nolint:gosec // G703: paths from the installer layout and Program Files, not remote input
			return filepath.Join(engineDir, "config", "daemon.json"), nil
		}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not resolve the user profile for .docker\\daemon.json: %w", err)
	}
	return filepath.Join(home, ".docker", "daemon.json"), nil
}

// ensureDockerInsecureRegistries merges the registries from the .flock into
// the Docker daemon config, so image pulls from the appliance's plain-HTTP
// registry work. Without this every app install on an edge PC fails: the
// daemon insists on HTTPS against http://<appliance>:15001 (or, behind a
// corporate proxy, times out before that). Docker only re-reads the file on
// restart, so the caller's output must say so when something was added.
func ensureDockerInsecureRegistries(flockPath string) error {
	flockCfg, err := readFlockConfig(flockPath)
	if err != nil {
		return err
	}

	entries := insecureRegistryEntries(flockCfg)
	if len(entries) == 0 {
		return nil
	}

	daemonPath, err := dockerDaemonJSONPath()
	if err != nil {
		return err
	}
	current, err := os.ReadFile(daemonPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	merged, changed, err := mergeInsecureRegistries(current, entries)
	if err != nil {
		return fmt.Errorf("%s: %w", daemonPath, err)
	}
	if !changed {
		return nil
	}

	err = os.MkdirAll(filepath.Dir(daemonPath), 0755)
	if err != nil {
		return err
	}
	err = os.WriteFile(daemonPath, merged, 0644) //nolint:gosec // G306: public CA certificate / repair script / Docker engine config, no secrets
	if err != nil {
		return err
	}

	fmt.Printf("Added insecure-registries %s to %s — restart Docker (Docker Desktop, or the docker service) to apply.\n",
		strings.Join(entries, ", "), daemonPath)
	return nil
}

// localSystemDockerConfigPath is the Docker CLI config the agent's service
// account reads. The service runs as LocalSystem, whose profile is under
// System32\config\systemprofile — never the installing administrator's, so
// this cannot be derived from the environment at install time.
func localSystemDockerConfigPath() string {
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		systemRoot = `C:\Windows`
	}
	return filepath.Join(systemRoot, "System32", "config", "systemprofile", ".docker", "config.json")
}

// ensureDockerCredentialStore opts the app registry out of the Windows
// credential helper for the LocalSystem account, so `docker login` can store
// the credential in config.json instead of failing against Credential
// Manager's username length limit. See mergeCredentialHelperOptOut for why
// this is mandatory rather than cosmetic.
func ensureDockerCredentialStore(flockPath string) error {
	raw, err := os.ReadFile(flockPath)
	if err != nil {
		return err
	}
	var flockCfg config.ReswarmConfig
	err = json.Unmarshal(raw, &flockCfg)
	if err != nil {
		return fmt.Errorf("could not parse %s: %w", flockPath, err)
	}

	registry := normalizeRegistryHost(flockCfg.DockerRegistryURL)
	if registry == "" {
		return nil
	}

	configPath := localSystemDockerConfigPath()
	current, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	merged, changed, err := mergeCredentialHelperOptOut(current, []string{registry})
	if err != nil {
		return fmt.Errorf("%s: %w", configPath, err)
	}
	if !changed {
		return nil
	}

	err = os.MkdirAll(filepath.Dir(configPath), 0700)
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, merged, 0600) //nolint:gosec // G703: paths from the installer layout and Program Files, not remote input
}

// setServiceProxy writes the proxy into the service's Environment registry
// value (REG_MULTI_SZ). A LocalSystem service only inherits machine-wide
// environment, and Go does not read the WinHTTP/IE proxy settings.
//
// All three entries are written together because the value is a single
// REG_MULTI_SZ. This key is NOT the place to keep the operator's settings:
// `service uninstall` deletes the service and the value with it, so the
// durable copy lives in proxy.env and this only mirrors it.
func setServiceProxy(cfg proxyConfig) error {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\`+serviceName, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer func() { _ = key.Close() }()

	return key.SetStringsValue("Environment", cfg.environmentEntries())
}

func runCommand(name string, args ...string) error {
	output, err := exec.Command(name, args...).CombinedOutput() //nolint:gosec // G204: fixed Windows service/registry tools chosen by the installer
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func copyFileIfDifferent(src string, dst string) error {
	srcAbs, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	dstAbs, err := filepath.Abs(dst)
	if err != nil {
		return err
	}
	if strings.EqualFold(srcAbs, dstAbs) {
		return nil
	}

	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()

	// Write to a temp name first so a torn copy never leaves a half-written
	// file at the final path.
	tmp := dst + ".tmp"
	destination, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755) //nolint:gosec // G302: installed agent executable, must be executable
	if err != nil {
		return err
	}

	_, err = io.Copy(destination, source)
	closeErr := destination.Close()
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}

	return os.Rename(tmp, dst)
}
