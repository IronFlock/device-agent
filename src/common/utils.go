package common

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"reagent/config"
	"reagent/messenger/topics"
	"reagent/release"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog/log"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
)

func BuildContainerName(stage Stage, appKey uint64, appName string) string {
	return strings.ToLower(fmt.Sprintf("%s_%d_%s", stage, appKey, appName))
}

func BuildComposeContainerName(stage Stage, appKey uint64, appName string) string {
	return strings.ToLower(fmt.Sprintf("%s_%d_%s_compose", stage, appKey, appName))
}

// NormalizeComposeProjectName mirrors how docker compose derives a default
// project name from a directory name: lowercase, drop characters outside
// [a-z0-9_-], and trim leading separators. The agent runs compose without -p,
// so the project name (and the com.docker.compose.project label on every
// container/volume of the app) is the normalized compose directory name,
// which BuildComposeContainerName provides.
func NormalizeComposeProjectName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	return strings.TrimLeft(b.String(), "_-")
}

func BuildImageName(stage Stage, arch string, appKey uint64, appName string) string {
	return strings.ToLower(fmt.Sprintf("%s_%s_%d_%s", stage, arch, appKey, appName))
}

func BuildRegistryImageName(registryURL string, mainRepositoryName string, imageName string) string {
	return strings.ToLower(fmt.Sprintf("%s%s%s", registryURL, mainRepositoryName, imageName))
}

// NormalizeRegistryHost reduces any registry reference to the canonical
// `host[:port]` form used as the docker_credentials map key across the whole
// platform (UI, backend, SQL and agent apply the same rule): trimmed, scheme
// stripped, anything after the first "/" dropped, lowercased.
// `https://MyRegistry.azurecr.io/repo/` → `myregistry.azurecr.io`. Registry
// hosts are case-insensitive and image refs only ever carry the lowercase
// form, so lookups against user-typed keys must go through this.
func NormalizeRegistryHost(entry string) string {
	entry = strings.TrimSpace(entry)
	entry = strings.TrimPrefix(entry, "http://")
	entry = strings.TrimPrefix(entry, "https://")
	entry, _, _ = strings.Cut(entry, "/")
	return strings.ToLower(entry)
}

// NormalizeDockerCredentialKeys rekeys a credential map by the canonical
// registry-host form, dropping entries whose key normalizes to nothing.
// Backends and the UI normalize before storing, but payloads authored before
// that (or hand-edited rows) may still carry `https://…`, trailing-slash or
// mixed-case keys.
func NormalizeDockerCredentialKeys(credentials map[string]DockerCredential) map[string]DockerCredential {
	if credentials == nil {
		return nil
	}
	normalized := make(map[string]DockerCredential, len(credentials))
	for host, credential := range credentials {
		if key := NormalizeRegistryHost(host); key != "" {
			normalized[key] = credential
		}
	}
	return normalized
}

func BuildAgentUpdateProgress(serialNumber string) string {
	return fmt.Sprintf("%s.%s.%s", topicPrefix, serialNumber, topics.AgentProgress)
}

func BuildDownloadOSUpdateProgress(serialNumber string) string {
	return fmt.Sprintf("%s.%s.%s", topicPrefix, serialNumber, topics.DownloadOSUpdateProgress)
}

func BuildInstallOSUpdateProgress(serialNumber string) string {
	return fmt.Sprintf("%s.%s.%s", topicPrefix, serialNumber, topics.InstallOSUpdateProgress)
}

func BuildPerformOSUpdateProgress(serialNumber string) string {
	return fmt.Sprintf("%s.%s.%s", topicPrefix, serialNumber, topics.PerformOSUpdateProgress)
}

func BuildTunnelStateUpdate(serialNumber string) string {
	return fmt.Sprintf("%s.%s.%s/onreload", topicPrefix, serialNumber, topics.TunnelStateUpdate)
}

const topicPrefix = "re.mgmt"

func BuildLogTopic(serialNumber string, containerName string) string {
	return fmt.Sprintf("reswarm.logs.%s.%s", serialNumber, containerName)
}

func BuildExternalApiTopic(serialNumber string, topic string) string {
	return fmt.Sprintf("%s.%s.%s", topicPrefix, serialNumber, topic)
}

func BuildDockerBuildID(appKey uint64, appName string) string {
	return fmt.Sprintf("build_%d_%s", appKey, appName)
}

func BuildDockerPullID(appKey uint64, appName string) string {
	return fmt.Sprintf("pull_%d_%s", appKey, appName)
}

func BuildDockerPushID(appKey uint64, appName string) string {
	return fmt.Sprintf("push_%d_%s", appKey, appName)
}

func EscapeNewlineCharacters(input string) string {
	input = strings.ReplaceAll(input, "\n", "\\n")
	input = strings.ReplaceAll(input, "\t", "\\t")
	input = strings.ReplaceAll(input, "\r", "\\r")
	return input
}

func EnvironmentTemplateToStringArray(environmentsTemplateMap map[string]interface{}) []string {
	stringArray := make([]string, 0)

	for key, entry := range environmentsTemplateMap {
		value := entry.(map[string]interface{})["defaultValue"]
		if value != nil {
			stringArray = append(stringArray, fmt.Sprintf("%s=%s", key, EscapeNewlineCharacters(fmt.Sprint(value))))
		}
	}

	return stringArray
}

func ListDirectories(path string) ([]string, error) {
	folder, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = folder.Close() }()

	subfolders, err := folder.Readdir(-1)
	if err != nil {
		return nil, err
	}

	var directories []string
	for _, item := range subfolders {
		if item.IsDir() {
			directories = append(directories, item.Name())
		}
	}

	return directories, nil
}

func EnvironmentVarsToStringArray(environmentsMap map[string]interface{}) []string {
	stringArray := make([]string, 0)

	for key, entry := range environmentsMap {
		value := entry.(map[string]interface{})["value"]
		stringArray = append(stringArray, fmt.Sprintf("%s=%s", key, EscapeNewlineCharacters(fmt.Sprint(value))))
	}

	return stringArray
}

// ToUint64 coerces a number decoded from a WAMP message to a uint64.
//
// nexus deserializes JSON with ugorji codec at its default SignedInteger:false,
// so a non-negative integer arrives as uint64, a negative one as int64, and
// only a genuinely fractional value as float64. Handlers that assert a single
// concrete type therefore drop perfectly valid input on the floor — Docker.Logs
// asserted `tail.(string)` and silently served the *whole* log whenever a
// caller sent a number. Coerce here once instead of repeating the ladder.
func ToUint64(value interface{}) (uint64, bool) {
	switch typed := value.(type) {
	case uint64:
		return typed, true
	case uint32:
		return uint64(typed), true
	case uint:
		return uint64(typed), true
	case int64:
		if typed < 0 {
			return 0, false
		}
		return uint64(typed), true
	case int:
		if typed < 0 {
			return 0, false
		}
		return uint64(typed), true
	case float64:
		if typed < 0 || typed != math.Trunc(typed) {
			return 0, false
		}
		return uint64(typed), true
	case json.Number:
		parsed, err := strconv.ParseUint(typed.String(), 10, 64)
		return parsed, err == nil
	case string:
		parsed, err := strconv.ParseUint(typed, 10, 64)
		return parsed, err == nil
	}
	return 0, false
}

// ParseLogTime resolves a caller-supplied log-window bound to an absolute time.
//
// Three forms are accepted, because the three callers that matter each reach
// for a different one: RFC3339 ("2026-08-04T09:12:00Z") from a machine that
// already knows the moment, a bare Unix-seconds value from anything speaking
// Docker's own vocabulary, and a Go duration meaning "this long ago" ("30m",
// "2h", "1h30m") from an assistant reasoning in relative terms.
//
// A duration is resolved against *now* on the DEVICE, not on the caller. That
// is the correct reading of "the last 30 minutes" — the logs are the device's —
// but it does mean an absolute bound and a relative one can disagree when the
// device clock has drifted. Every log response echoes the device's own clock so
// the caller can see the skew rather than silently mis-read an empty window.
func ParseLogTime(value string, now time.Time) (time.Time, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return time.Time{}, errors.New("empty log time")
	}

	if parsed, err := time.Parse(time.RFC3339, trimmed); err == nil {
		return parsed, nil
	}

	// A duration is "ago", so it may arrive with or without a leading '-'.
	if duration, err := time.ParseDuration(strings.TrimPrefix(trimmed, "-")); err == nil {
		if duration < 0 {
			duration = -duration
		}
		return now.Add(-duration), nil
	}

	if seconds, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
		return time.Unix(seconds, 0).UTC(), nil
	}

	return time.Time{}, fmt.Errorf("%q is not an RFC3339 time, a Unix timestamp, or a duration", trimmed)
}

var StatusRegex = regexp.MustCompile(`\((.*?)\)`)

func ParseExitCodeFromContainerStatus(status string) (int64, error) {
	statusString := StatusRegex.FindString(status)
	exitCodeString := strings.TrimRight(strings.TrimLeft(statusString, "("), ")")
	exitCodeInt, err := strconv.ParseInt(exitCodeString, 10, 64)
	if err != nil {
		return -1, err
	}

	return exitCodeInt, nil
}

func GetRandomFreePort() (port int, err error) {
	var a *net.TCPAddr
	if a, err = net.ResolveTCPAddr("tcp", "localhost:0"); err == nil {
		var l *net.TCPListener
		if l, err = net.ListenTCP("tcp", a); err == nil {
			defer func() { _ = l.Close() }()
			return l.Addr().(*net.TCPAddr).Port, nil
		}
	}
	return
}

func GetFreePortFromStart(startPort int) (int, error) {
	for port := startPort; port <= 65535; port++ {
		addr := fmt.Sprintf("127.0.0.1:%d", port)
		listener, err := net.Listen("tcp", addr)
		if err == nil {
			defer func() { _ = listener.Close() }()
			return listener.Addr().(*net.TCPAddr).Port, nil
		}
	}
	return 0, fmt.Errorf("no free port available")
}

// Ordinal gives you the input number in a rank/ordinal format.
//
// Ordinal(3) -> 3rd
func Ordinal(x uint) string {
	suffix := "th"
	switch x % 10 {
	case 1:
		if x%100 != 11 {
			suffix = "st"
		}
	case 2:
		if x%100 != 12 {
			suffix = "nd"
		}
	case 3:
		if x%100 != 13 {
			suffix = "rd"
		}
	}
	return strconv.Itoa(int(x)) + suffix
}

func ParseContainerName(containerName string) (Stage, uint64, string, error) {
	if containerName == "" {
		return "", 0, "", errors.New("container name is empty")
	}

	// cleanup container name
	if string([]rune(containerName)[0]) == "/" {
		// get index of the rune that == /
		_, i := utf8.DecodeRuneInString(containerName)
		// remove that rune from the string
		containerName = containerName[i:]
	}

	var stage Stage
	var appKey uint64
	var name string

	containerSplit := strings.Split(containerName, "_")
	if len(containerSplit) < 3 {
		return "", 0, "", errors.New("invalid container name")
	}

	if containerSplit[0] == "dev" {
		stage = DEV
	} else if containerSplit[0] == "prod" {
		stage = PROD
	} else if containerSplit[0] == "pub" {
		stage = DEV
	} else {
		stage = ""
	}

	parsedAppKey, err := strconv.ParseUint(containerSplit[1], 10, 64)
	if err != nil {
		return "", 0, "", err
	}
	appKey = parsedAppKey

	// also handles names like dev_6_net_data, aka 2 _'s at the end
	name = strings.Join(containerSplit[2:], "_")

	return stage, appKey, name, nil
}

func ParseComposeContainerName(containerName string) (Stage, uint64, string, error) {
	if containerName == "" {
		return "", 0, "", errors.New("container name is empty")
	}

	// cleanup container name
	if string([]rune(containerName)[0]) == "/" {
		// get index of the rune that == /
		_, i := utf8.DecodeRuneInString(containerName)
		// remove that rune from the string
		containerName = containerName[i:]
	}

	var stage Stage
	var appKey uint64
	var name string

	containerSplit := strings.Split(containerName, "_")
	// Bounds check before indexing: this is called with arbitrary names
	// (foreign compose projects on the same host included), and a short name
	// would otherwise panic.
	if len(containerSplit) < 4 || containerSplit[3] != "compose" {
		return "", 0, "", errors.New("invalid compose container name")
	}

	if containerSplit[0] == "dev" {
		stage = DEV
	} else if containerSplit[0] == "prod" {
		stage = PROD
	} else if containerSplit[0] == "pub" {
		stage = DEV
	} else {
		stage = ""
	}

	parsedAppKey, err := strconv.ParseUint(containerSplit[1], 10, 64)
	if err != nil {
		return "", 0, "", err
	}
	appKey = parsedAppKey

	name = containerSplit[2]

	return stage, appKey, name, nil
}

func (tp *TransitionPayload) initContainerData(appKey uint64, appName string, config *config.Config) {
	publishContainer := BuildContainerName("pub", appKey, appName)
	devContainerName := BuildContainerName(DEV, appKey, appName)
	prodContainerName := BuildContainerName(PROD, appKey, appName)

	sysInfo := release.GetSystemInfo()
	imageArchName := sysInfo.Arch + sysInfo.Variant

	devImageName := BuildImageName(DEV, imageArchName, appKey, appName)
	devRegImageName := BuildRegistryImageName(config.ReswarmConfig.DockerRegistryURL, config.ReswarmConfig.DockerMainRepository, devImageName)

	prodImageName := BuildImageName(PROD, imageArchName, appKey, appName)
	prodRegImageName := BuildRegistryImageName(config.ReswarmConfig.DockerRegistryURL, config.ReswarmConfig.DockerMainRepository, prodImageName)

	tp.PublishContainerName = publishContainer
	tp.ContainerName = StageBasedResult{
		Dev:  devContainerName,
		Prod: prodContainerName,
	}
	tp.ImageName = StageBasedResult{
		Dev:  devImageName,
		Prod: prodImageName,
	}
	tp.RegistryImageName = StageBasedResult{
		Dev:  devRegImageName,
		Prod: prodRegImageName,
	}
}

func PrettyPrintDebug(data interface{}) {
	pretty, err := PrettyFormat(data)
	if err != nil {
		pretty = fmt.Sprintf("%+v", pretty)
	}

	log.Debug().Msg(pretty)
}

func Min(a int64, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func Log(format string, stdout bool, logLevel string, val ...interface{}) {
	levelMsg := strings.ToUpper(logLevel)

	if stdout {
		args := make([]interface{}, 0)
		args = append(args, fmt.Sprintf("%s:", levelMsg))
		args = append(args, val...)

		if format == "" {
			fmt.Println(args)
			return
		} else {
			fmt.Printf(format, args)
			return
		}
	}

	event := log.Debug()
	switch logLevel {
	case "error":
		event = log.Error()
	case "info":
		event = log.Info()
	case "warning":
		event = log.Warn()
	}

	if format == "" {
		event.Msg(fmt.Sprint(val...))
		return
	} else {
		event.Msgf(format, val...)
		return
	}
}

func PrettyFormat(data interface{}) (string, error) {
	var p []byte
	//    var err := error
	p, err := json.MarshalIndent(data, "", "\t")
	if err != nil {
		return "", err
	}

	return string(p), nil
}

// Stats contains system resource usage information
type Stats struct {
	CPUCount          int     `json:"cpu_count"`           // Number of logical CPUs
	CPUUsagePercent   float64 `json:"cpu_usage_percent"`   // CPU usage percentage (0-100)
	MemoryTotal       uint64  `json:"memory_total"`        // Total memory in bytes
	MemoryUsed        uint64  `json:"memory_used"`         // Used memory in bytes
	MemoryAvailable   uint64  `json:"memory_available"`    // Available memory in bytes
	StorageTotal      uint64  `json:"storage_total"`       // Total storage in bytes (root filesystem)
	StorageUsed       uint64  `json:"storage_used"`        // Used storage in bytes (root filesystem)
	StorageFree       uint64  `json:"storage_free"`        // Free storage in bytes (root filesystem)
	DockerAppsTotal   uint64  `json:"docker_apps_total"`   // Total storage in bytes (<agentDir>/docker-apps)
	DockerAppsUsed    uint64  `json:"docker_apps_used"`    // Used storage in bytes (<agentDir>/docker-apps)
	DockerAppsFree    uint64  `json:"docker_apps_free"`    // Free storage in bytes (<agentDir>/docker-apps)
	DockerAppsMounted bool    `json:"docker_apps_mounted"` // Whether <agentDir>/docker-apps is a separate mount
}

// GetStats returns current system resource statistics. dockerAppsPath is
// Docker's data-root as ironflock-init sets it up (<agentDir>/docker-apps).
// This function is cross-platform (Linux, macOS, Windows).
func GetStats(dockerAppsPath string) Stats {
	stats := Stats{}

	// Get CPU count
	if cpuCount, err := cpu.Counts(true); err == nil {
		stats.CPUCount = cpuCount
	}

	// Get CPU usage (percentage is the only meaningful metric for CPU)
	if cpuPercent, err := cpu.Percent(0, false); err == nil && len(cpuPercent) > 0 {
		stats.CPUUsagePercent = cpuPercent[0]
	}

	// Get Memory - absolute values
	if vmStat, err := mem.VirtualMemory(); err == nil {
		stats.MemoryTotal = vmStat.Total
		stats.MemoryUsed = vmStat.Used
		stats.MemoryAvailable = vmStat.Available
	}

	// Get Disk - absolute values for root filesystem
	if diskStat, err := disk.Usage("/"); err == nil {
		stats.StorageTotal = diskStat.Total
		stats.StorageUsed = diskStat.Used
		stats.StorageFree = diskStat.Free
	}

	// Get Docker apps storage - <agentDir>/docker-apps mount
	if diskStat, err := disk.Usage(dockerAppsPath); err == nil {
		stats.DockerAppsTotal = diskStat.Total
		stats.DockerAppsUsed = diskStat.Used
		stats.DockerAppsFree = diskStat.Free
		// Check if it's a separate mount (different total than root)
		stats.DockerAppsMounted = stats.DockerAppsTotal != stats.StorageTotal
	}

	return stats
}
