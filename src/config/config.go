package config

import (
	"encoding/json"
	"flag"
	"fmt"
	"regexp"

	"github.com/rs/zerolog/log"

	"os"
	"path/filepath"
	"reagent/layout"
	"runtime"
)

// DefaultUpdateBaseURL is where agents fetch their updates when the .flock
// names no update_url: the registry-proxy's /dl routes, which stream the
// public download buckets, so a device behind a firewall needs only IronFlock
// hosts and never storage.googleapis.com. Appliance-managed agents carry the
// appliance's own /dl base instead.
const DefaultUpdateBaseURL = "https://instance-registry.ironflock.com/dl"

// ReswarmConfig types for the .flock file
type ReswarmConfig struct {
	Name   string `json:"name"`
	Secret string `json:"secret"`
	Board  struct {
		CPU          string      `json:"cpu"`
		Docs         interface{} `json:"docs"`
		Board        string      `json:"board"`
		Model        string      `json:"model"`
		Boardname    string      `json:"boardname"`
		Modelname    string      `json:"modelname"`
		Reflasher    bool        `json:"reflasher"`
		Architecture string      `json:"architecture"`
	} `json:"board"`
	Status         string      `json:"status"`
	Password       string      `json:"password"`
	Wlanssid       string      `json:"wlanssid"`
	SwarmKey       int         `json:"swarm_key"`
	DeviceKey      int         `json:"device_key"`
	SwarmName      string      `json:"swarm_name"`
	Description    interface{} `json:"description"` // can be null --> interface{}
	Architecture   string      `json:"architecture"`
	SerialNumber   string      `json:"serial_number"`
	Authentication struct {
		Key         string `json:"key"`
		Certificate string `json:"certificate"`
	} `json:"authentication"`
	SwarmOwnerName       string `json:"swarm_owner_name"`
	ConfigPassphrase     string `json:"config_passphrase"`
	DeviceEndpointURL    string `json:"device_endpoint_url"`
	Environment          string `json:"environment,omitempty"`
	DockerRegistryURL    string `json:"docker_registry_url"`
	InsecureRegistries   string `json:"insecure-registries,omitempty"`
	DockerMainRepository string `json:"docker_main_repository"`
	ApplianceDomain      string `json:"appliance_domain,omitempty"`
	// UpdateURL, when set, overrides the --remoteUpdateURL base for OTA
	// self-updates. Appliance-managed agents carry the appliance's own /dl
	// base here; the cloud backend hands out instance-registry.ironflock.com/dl
	// (new .flock files and every heartbeat). Empty: DefaultUpdateBaseURL.
	UpdateURL      string `json:"update_url,omitempty"`
	ReswarmBaseURL string `json:"-"`
}

type CommandLineArguments struct {
	AppsDirectory            string
	AppsComposeDir           string
	AppsBuildDir             string
	AppsSharedDir            string
	AgentDir                 string
	DownloadDir              string
	CompressedBuildExtension string
	RemoteUpdateURL          string
	Environment              string
	Debug                    bool
	DebugMessaging           bool
	Version                  bool
	Arch                     bool
	Offline                  bool
	Profiling                bool
	ProfilingPort            uint
	ShouldUpdateAgent        bool
	PrettyLogging            bool
	UseNetworkManager        bool
	LogFileLocation          string
	FrpcLogFileLocation      string
	ConfigFileLocation       string
	DatabaseFileName         string
	// Layout is the file layout this agent found itself in (see package
	// layout); BinaryName is what downloaded updates are called in AgentDir.
	Layout                     layout.Kind
	BinaryName                 string
	PingPongTimeout            uint
	ResponseTimeout            uint
	ConnectionEstablishTimeout uint
}

type Config struct {
	ReswarmConfig        *ReswarmConfig
	CommandLineArguments *CommandLineArguments
	StartupLogChannel    chan string
}

func New(cliArgs *CommandLineArguments, reswarmConfig *ReswarmConfig) Config {
	return Config{
		ReswarmConfig:        reswarmConfig,
		CommandLineArguments: cliArgs,
	}
}

func GetCliArguments() (*CommandLineArguments, error) {
	// The defaults follow the layout on disk: legacy units pass only -config
	// and -appsDir, and a database path that does not match the layout would
	// open an empty database (see package layout).
	paths := layout.Detect("")
	if runtime.GOOS != "linux" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		paths = layout.DetectHome(homeDir, runtime.GOOS)
	}
	defaultAgentDir := paths.AgentDir

	// By default apps are stored inside the default agent directory, as well, to
	// support a variety of distributions/systems with a rootfs only. However,
	// for the agent running on actual embedded linux we tend to use a separate
	// dedicated (update-)persistent partition mounted at "/apps"
	defaultAppsDir := filepath.Join(defaultAgentDir, "apps")

	logFile := flag.String("logFile", paths.LogFile, "log file of the agent")
	debug := flag.Bool("debug", true, "sets the log level to debug")
	shouldUpdate := flag.Bool("update", true, "determines if the agent should update on start")
	offline := flag.Bool("offline", false, "starts the agent without establishing a socket connection. meant for debugging")
	env := flag.String("env", "production", "determines in which environment the agent will operate. Possible values: (production, test, local)")
	arch := flag.Bool("arch", false, "displays the architecture for which the binary was built")
	version := flag.Bool("version", false, "displays the current version of the agent")
	profiling := flag.Bool("profiling", false, "spins up a pprof webserver on the defined port")
	profilingPort := flag.Uint("profilingPort", 80, "port of the profiling service")
	prettyLogging := flag.Bool("prettyLogging", false, "enables the pretty console writing, intended for debugging")
	remoteUpdateURL := flag.String("remoteUpdateURL", DefaultUpdateBaseURL, "download base for agent updates (the .flock update_url wins)")
	agentDir := flag.String("agentDir", defaultAgentDir, "default location of the agent binary")
	appsDir := flag.String("appsDir", defaultAppsDir, "default path for apps and app-data")
	databaseFileName := flag.String("dbFileName", paths.Database, "defines the name used to persist the database file")
	debugMessaging := flag.Bool("debugMessaging", false, "enables debug logs for messenging layer")
	nmw := flag.Bool("nmw", true, "enables the agent to use the NetworkManager API on Linux machines")
	compressedBuildExtension := flag.String("compressedBuildExtension", "tgz", "sets the extension in which the compressed build files will be provided")
	pingPongTimeout := flag.Uint("ppTimeout", 5000, "Sets the ping pong timeout of the client in milliseconds (0 means no timeout)")
	responseTimeout := flag.Uint("respTimeout", 7000, "Sets the response timeout of the client in milliseconds")
	socketConnectionEstablishTimeout := flag.Uint("connTimeout", 1250, "Sets the connection timeout for the socket connection in milliseconds. (0 means no timeout)")
	cfgFile := flag.String("config", "", "ironflock configuration file")
	flag.Parse()

	cliArgs := CommandLineArguments{
		AppsDirectory:              *appsDir,
		AppsBuildDir:               (*appsDir) + "/build",
		AppsComposeDir:             (*appsDir) + "/compose",
		AppsSharedDir:              (*appsDir) + "/shared",
		DownloadDir:                (*agentDir) + "/downloads",
		AgentDir:                   *agentDir,
		RemoteUpdateURL:            *remoteUpdateURL,
		CompressedBuildExtension:   *compressedBuildExtension,
		Debug:                      *debug,
		Version:                    *version,
		Offline:                    *offline,
		Environment:                *env,
		PrettyLogging:              *prettyLogging,
		DebugMessaging:             *debugMessaging,
		LogFileLocation:            *logFile,
		FrpcLogFileLocation:        paths.FrpcLog,
		ConfigFileLocation:         *cfgFile,
		Layout:                     paths.Kind,
		BinaryName:                 paths.BinaryName,
		Profiling:                  *profiling,
		ProfilingPort:              *profilingPort,
		ShouldUpdateAgent:          *shouldUpdate,
		DatabaseFileName:           *databaseFileName,
		PingPongTimeout:            *pingPongTimeout,
		ResponseTimeout:            *responseTimeout,
		ConnectionEstablishTimeout: *socketConnectionEstablishTimeout,
		Arch:                       *arch,
		UseNetworkManager:          *nmw,
	}

	return &cliArgs, nil
}

// retiredDeviceEndpoint matches the pre-router device endpoints
// (devices.reswarm.io:8080, devices.record-evolution.com:8080). They are shut
// down — and reswarm.io is no longer ours — so a .flock that still names one is
// moved to the current endpoint, where the device authenticates with its
// secret like every other device, instead of dialling a dead (or foreign) host.
var retiredDeviceEndpoint = regexp.MustCompile(`devices.*\.(com|io):8080`)

// SaveReswarmConfig writes the .flock atomically: the new content goes to a
// temporary file beside the real one (symlinks resolved, so a link such as
// /opt/reagent/device-config.flock -> /boot/<name>.flock stays a link), is
// synced, and is renamed over it. A power cut mid-save leaves the old or the
// new file, never a truncated one — the .flock usually lives on the FAT /boot
// partition of a device that can lose power at any moment (a car's AutoPi).
func SaveReswarmConfig(path string, reswarmConfig *ReswarmConfig) error {
	data, err := json.MarshalIndent(reswarmConfig, "", " ") //nolint:gosec // G117: writes the device .flock, whose purpose is to hold the device secret (0600 by default)
	if err != nil {
		return err
	}

	target := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		target = resolved
	}
	mode := os.FileMode(0o600)
	if info, err := os.Stat(target); err == nil {
		mode = info.Mode().Perm()
	}

	tmp, err := os.CreateTemp(filepath.Dir(target), ".flock-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // gone already after a successful rename
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Best effort: FAT ignores per-file modes and may refuse the call.
	_ = os.Chmod(tmpName, mode)
	if err := os.Rename(tmpName, target); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(target)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// LoadReswarmConfig reads the device's .flock. A file that cannot be parsed
// or carries no device_endpoint_url is an error and is left on disk exactly
// as found: writing back what was read from a truncated file is how a power
// cut used to turn into a device that dials an empty URL forever. Only a
// migration of a legacy URL is written back.
func LoadReswarmConfig(path string) (*ReswarmConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var reswarmConfig ReswarmConfig
	if err := json.Unmarshal(raw, &reswarmConfig); err != nil {
		return nil, fmt.Errorf("%s is not a valid .flock (%d bytes): %w; download the device configuration from IronFlock again", path, len(raw), err)
	}
	if reswarmConfig.DeviceEndpointURL == "" {
		return nil, fmt.Errorf("%s has no device_endpoint_url; download the device configuration from IronFlock again", path)
	}

	migrated := false
	if reswarmConfig.DockerRegistryURL == "registry.reswarm.io/" {
		reswarmConfig.DockerRegistryURL = "registry.ironflock.com/"
		migrated = true
	}
	if reswarmConfig.DeviceEndpointURL == "wss://cbw.record-evolution.com/ws-re-dev" ||
		retiredDeviceEndpoint.MatchString(reswarmConfig.DeviceEndpointURL) {
		reswarmConfig.DeviceEndpointURL = "wss://cbw.ironflock.com/ws-re-dev"
		migrated = true
	}
	if migrated {
		if err := SaveReswarmConfig(path, &reswarmConfig); err != nil {
			log.Warn().Err(err).Msg("could not persist the migrated .flock; continuing with the migrated values")
		}
	}

	return &reswarmConfig, nil
}
