package system

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"reagent/codesign"
	"reagent/common"
	"reagent/config"
	"reagent/embedded"
	"reagent/errdefs"
	"reagent/filesystem"
	"reagent/lifecycle"
	"reagent/messenger"
	"reagent/messenger/topics"
	"reagent/release"
	"reagent/safe"
	"reagent/selfupdate"
	"reagent/updatesig"
	"runtime"
	"strings"
	"sync"
	"time"

	_ "embed"

	"github.com/Masterminds/semver"
	"github.com/rs/zerolog/log"
)

type System struct {
	config    *config.Config
	messenger messenger.Messenger
}

type UpdateResult struct {
	CurrentVersion  string
	LatestVersion   string
	Message         string
	DidAgentUpdate  bool
	DidTunnelUpdate bool
	DidUpdate       bool
	InProgress      bool
	TotalFileSize   uint64
}

func New(config *config.Config, messenger messenger.Messenger) System {
	return System{
		config:    config,
		messenger: messenger,
	}
}

// ------------------------------------------------------------------------- //
// Reboot / Poweroff / RestartAgent live in system_unix.go and
// system_windows.go.
// ------------------------------------------------------------------------- //

// updateBaseURL returns the base URL for OTA downloads. A per-device
// update_url in the .flock config (the appliance's own /dl mirror, or the
// cloud's instance-registry.ironflock.com/dl) wins over the --remoteUpdateURL
// flag default, which is the same cloud /dl base.
func (sys *System) updateBaseURL() string {
	if sys.config.ReswarmConfig != nil && sys.config.ReswarmConfig.UpdateURL != "" {
		return sys.config.ReswarmConfig.UpdateURL
	}
	return sys.config.CommandLineArguments.RemoteUpdateURL
}

// Agent download trees below the update base.
//
//	<base>/device-agent/manifest-v2.json                     signed channel map (updatesig.Envelope),
//	                                                         signed in CI with the Cloud KMS key
//	<base>/device-agent/manifest.json                        the same, frozen at the bridge release
//	                                                         (last one signed with the OpenBao key)
//	<base>/device-agent/<os>/<arch>/<v>/device-agent[.exe]   (+ .sha256, .sig)
//	<base>/device-agent/frpc/<os>/<arch>/<v>/frpc[.exe]      Windows only
//
// Agents read manifest-v2.json first. manifest.json stays frozen at the bridge
// release: agents that trust only the OpenBao key (and device-init installs)
// read it, land on the bridge release, which trusts both keys and reads
// manifest-v2.json from then on.
//
// The legacy tree (re-agent/: availableVersions.json, binaries named reagent)
// is frozen at the version that moves devices to the current layout. An agent
// reads it only when the current tree does not exist at its base: an
// appliance whose mirror has not been updated yet. Every /dl base serves it
// next to the current tree (the registry-proxy's /dl/re-agent route).
const (
	agentTree           = "device-agent"
	legacyAgentTree     = "re-agent"
	agentManifestName   = "manifest.json"
	agentManifestV2Name = "manifest-v2.json"
	legacyManifestName  = "availableVersions.json"
)

// agentSource is where agent binaries (and frpc) are downloaded from.
type agentSource struct {
	base          string
	tree          string // agentTree or legacyAgentTree
	publishedName string // "device-agent" or "reagent"
}

func (s agentSource) legacy() bool { return s.tree == legacyAgentTree }

// agentManifestSubject and agentManifestV2Subject are what the two channel
// maps are signed as. The subject line differs, so neither file can stand in
// for the other.
var (
	agentManifestSubject   = updatesig.Subject{Context: updatesig.ContextAgentManifest, Line: agentTree + "/" + agentManifestName}
	agentManifestV2Subject = updatesig.Subject{Context: updatesig.ContextAgentManifest, Line: agentTree + "/" + agentManifestV2Name}
)

// agentManifests are the current tree's channel maps, in the order an agent
// reads them: the next one only when the previous does not exist (404).
var agentManifests = []struct {
	name    string
	subject updatesig.Subject
}{
	{agentManifestV2Name, agentManifestV2Subject},
	{agentManifestName, agentManifestSubject},
}

// agentManifest is the payload of device-agent/manifest.json.
type agentManifest struct {
	Channels map[string]string `json:"channels"`
	IssuedAt string            `json:"issued_at"`
}

// resolveAgentVersions returns the channel → version map and the tree it came
// from. The current tree's manifests (manifest-v2.json, then manifest.json)
// must carry a valid signature; the next one is read only when the previous
// does not exist at all (404), and the legacy tree's unsigned
// availableVersions.json only when neither exists. A manifest that exists but
// does not verify is an error, never a reason to fall back.
func (sys *System) resolveAgentVersions() (map[string]string, agentSource, error) {
	base := sys.updateBaseURL()
	current := agentSource{base: base, tree: agentTree, publishedName: "device-agent"}
	legacy := agentSource{base: base, tree: legacyAgentTree, publishedName: "reagent"}

	for _, manifest := range agentManifests {
		manifestURL := current.base + "/" + current.tree + "/" + manifest.name
		data, status, err := fetchSmall(manifestURL)
		if err != nil {
			return nil, agentSource{}, err
		}
		switch status {
		case http.StatusOK:
			channels, err := openAgentManifest(data, manifest.subject)
			if err != nil {
				return nil, agentSource{}, fmt.Errorf("refusing %s: %w", manifestURL, err)
			}
			return channels, current, nil
		case http.StatusNotFound:
			log.Debug().Msgf("no %s", manifestURL)
		default:
			return nil, agentSource{}, fmt.Errorf("%s returned status %d", manifestURL, status)
		}
	}
	log.Debug().Msg("no agent manifest in the current tree; reading the legacy update tree")

	legacyURL := legacy.base + "/" + legacy.tree + "/" + legacyManifestName
	data, status, err := fetchSmall(legacyURL)
	if err != nil {
		return nil, agentSource{}, err
	}
	if status != http.StatusOK {
		return nil, agentSource{}, fmt.Errorf("%s returned status %d", legacyURL, status)
	}
	var channels map[string]string
	if err := json.Unmarshal(data, &channels); err != nil {
		return nil, agentSource{}, fmt.Errorf("%s is not a version map: %w", legacyURL, err)
	}
	return channels, legacy, nil
}

// openAgentManifest verifies a channel map signed as subject and returns its
// channels. AGENT_UPDATE_ALLOW_UNSIGNED (for a broken signing key only)
// accepts one that does not verify, with a warning.
func openAgentManifest(data []byte, subject updatesig.Subject) (map[string]string, error) {
	keys, err := trustedUpdateKeys()
	if err != nil {
		return nil, err
	}
	payload, keyID, err := updatesig.OpenEnvelope(keys, subject, data)
	if err != nil {
		if !updatesig.AllowUnsigned() {
			return nil, err
		}
		var envelope updatesig.Envelope
		if jsonErr := json.Unmarshal(data, &envelope); jsonErr != nil {
			return nil, err
		}
		log.Warn().Err(err).Msgf("WARNING: %s is set; using an agent manifest whose signature did not verify", updatesig.AGENT_UPDATE_ALLOW_UNSIGNED)
		payload = envelope.Payload
	} else {
		log.Debug().Str("key_id", keyID).Msg("agent manifest signature verified")
	}
	var manifest agentManifest
	if err := json.Unmarshal(payload, &manifest); err != nil {
		return nil, fmt.Errorf("agent manifest payload: %w", err)
	}
	if len(manifest.Channels) == 0 {
		return nil, errors.New("agent manifest names no channel")
	}
	return manifest.Channels, nil
}

// fetchSmall GETs a small document (a manifest) and returns its body and
// status.
func fetchSmall(url string) ([]byte, int, error) {
	body, status, err := filesystem.GetRemoteFileWithStatus(url)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(io.LimitReader(body, 64<<10))
	if err != nil {
		return nil, status, err
	}
	return data, status, nil
}

// localBinaryName is what downloaded agent updates are called in AgentDir
// (see layout.Paths.BinaryName); legacy when unset.
func (sys *System) localBinaryName() string {
	if name := sys.config.CommandLineArguments.BinaryName; name != "" {
		return name
	}
	return "reagent"
}

// buildBinaryDownloadURL builds the OTA URL for a published binary:
//
//	<base>/<tree>/<goos>/<arch>/<version>/<fileName>[.exe]
//
// It is the single source of truth for the on-device URL layout, so the frpc
// sub-tree path ("<tree>/frpc") and the agent path stay in step and are
// unit-testable without a network.
func buildBinaryDownloadURL(base, bucketName, goos, arch, versionString, fileName string) string {
	url := fmt.Sprintf("%s/%s/%s/%s/%s/%s", base, bucketName, goos, arch, versionString, fileName)
	if goos == "windows" {
		url += ".exe"
	}
	return url
}

// downloadBinary fetches the binary published as fileName below
// <base>/<tree>, verifies it and moves it into AgentDir as localName. The file
// only gets there with a valid signature for signatureContext
// (updatesig.ContextAgentUpdate / ContextFrpcDownload) and exactly the
// os/arch/version/file it is downloaded as; see verifyRemoteSignature.
func (sys *System) downloadBinary(base, tree, fileName, localName string, versionString string, includeVersionString bool, signatureContext string, progressCallback func(filesystem.DownloadProgress)) error {
	isWindows := runtime.GOOS == "windows"
	agentHomedir := sys.config.CommandLineArguments.AgentDir
	goos, arch := runtime.GOOS, release.GetBuildArch()
	agentURL := buildBinaryDownloadURL(base, tree, goos, arch, versionString, fileName)

	// What the signature must name, built from what this agent asked for (the
	// URL comes from the same values), never from what the server answered.
	publishedName := fileName
	if isWindows {
		publishedName += ".exe"
	}
	signatureTarget := updatesig.Target{Context: signatureContext, OS: goos, Arch: arch, Version: versionString, FileName: publishedName}

	var actualFileDestination string
	if includeVersionString {
		actualFileDestination = fmt.Sprintf("%s/%s-v%s", agentHomedir, localName, versionString)
	} else {
		actualFileDestination = fmt.Sprintf("%s/%s", agentHomedir, localName)
	}

	if isWindows {
		actualFileDestination += ".exe"
	}

	tmpFilePath := sys.config.CommandLineArguments.DownloadDir + "/" + localName + "-v" + versionString
	log.Debug().Msgf("Attempting to download latest %s binary at %s", fileName, agentURL)
	err := filesystem.DownloadURL(tmpFilePath, agentURL, progressCallback)
	if err != nil {
		log.Error().Err(err).Msgf("Failed to download from URL: %s", agentURL)
		return err
	}

	// Never install a corrupted or tampered binary: verify against the
	// .sha256 manifest published next to it. Absent manifests (releases that
	// predate them) only log a warning.
	err = verifyRemoteChecksum(tmpFilePath, agentURL)
	if err != nil {
		removeErr := os.Remove(tmpFilePath)
		if removeErr != nil {
			log.Error().Err(removeErr).Msg("failed to remove download that failed checksum verification")
		}
		return err
	}

	// The .sha256 manifest is same-origin, so it is not an authenticity
	// anchor. The Ed25519 signature is: on Linux, reagent-manager.sh
	// activates any reagent-v<version> it finds in AgentDir, and the tunnel
	// manager runs whatever frpc is there, so nothing unverified may be
	// renamed into it.
	err = verifyRemoteSignature(tmpFilePath, agentURL, signatureTarget)
	if err != nil {
		removeErr := os.Remove(tmpFilePath)
		if removeErr != nil {
			log.Error().Err(removeErr).Msg("failed to remove download that failed signature verification")
		}
		return err
	}

	// Authenticode (Windows): warn-only during the pre-signing transition;
	// rejected once enforcement is on (codesign.Enforcing).
	err = codesign.Verify(tmpFilePath)
	if err != nil {
		if codesign.Enforcing() {
			removeErr := os.Remove(tmpFilePath)
			if removeErr != nil {
				log.Error().Err(removeErr).Msg("failed to remove download that failed signature verification")
			}
			return fmt.Errorf("refusing to install an improperly signed binary from %s: %w", agentURL, err)
		}
		log.Warn().Err(err).Msgf("signature verification failed for %s (proceeding: pre-cutover)", agentURL)
	}

	err = os.Chmod(tmpFilePath, 0755) //nolint:gosec // G302: downloaded binary, must be executable; contents are verified by signature
	if err != nil {
		log.Error().Err(err).Msgf("Failed to set permissions for %s binary", fileName)
		return err
	}

	err = os.Rename(tmpFilePath, actualFileDestination)
	if err != nil {
		log.Error().Err(err).Msgf("Failed to move %s binary to AgentDir", fileName)
		return err
	}

	return nil
}

// verifyRemoteChecksum compares filePath's SHA-256 against the manifest
// published at <binaryURL>.sha256 (first whitespace-separated token, hex).
// A missing manifest or a failure to FETCH it is tolerated with a warning —
// TLS to the update server remains the trust anchor and releases published
// before the manifests existed must stay installable. A manifest that exists
// and does not match is a hard error.
func verifyRemoteChecksum(filePath string, binaryURL string) error {
	manifestURL := binaryURL + ".sha256"

	body, statusCode, err := filesystem.GetRemoteFileWithStatus(manifestURL)
	if err != nil {
		log.Warn().Err(err).Msgf("could not fetch checksum manifest %s, skipping verification", manifestURL)
		return nil
	}
	defer func() { _ = body.Close() }()

	if statusCode == http.StatusNotFound {
		log.Warn().Msgf("no checksum manifest published at %s, skipping verification", manifestURL)
		return nil
	}
	if statusCode != http.StatusOK {
		log.Warn().Msgf("checksum manifest %s returned status %d, skipping verification", manifestURL, statusCode)
		return nil
	}

	manifest, err := io.ReadAll(io.LimitReader(body, 1024))
	if err != nil {
		log.Warn().Err(err).Msgf("could not read checksum manifest %s, skipping verification", manifestURL)
		return nil
	}

	expected := strings.ToLower(strings.Fields(string(manifest))[0])
	if len(expected) != sha256.Size*2 {
		log.Warn().Msgf("checksum manifest %s is malformed, skipping verification", manifestURL)
		return nil
	}

	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	hasher := sha256.New()
	_, err = io.Copy(hasher, file)
	if err != nil {
		return err
	}

	actual := hex.EncodeToString(hasher.Sum(nil))
	if actual != expected {
		return fmt.Errorf("checksum mismatch for %s: got %s, manifest says %s", binaryURL, actual, expected)
	}

	log.Debug().Msgf("checksum verified for %s", binaryURL)
	return nil
}

// trustedUpdateKeys supplies the keys downloaded binaries must be signed
// with. A variable so tests can sign with a key generated at test time.
var trustedUpdateKeys = updatesig.TrustedKeys

// verifyRemoteSignature checks filePath against the Ed25519 signature
// published at <binaryURL>.sig (format: package updatesig) and refuses the
// binary unless one of the compiled-in keys signed it as target: this kind of
// binary, for this os/arch/version/file. A signature for another version or
// platform therefore fails, which is what stops a mirror from serving an old
// signed release as the new one. Unlike the .sha256 manifest, a signature that
// is absent or cannot be fetched is not tolerated: an origin able to serve a
// forged binary can just as well withhold the signature.
//
// Releases published before signing existed carry no .sig, so an agent that
// verifies can never install one of them again, not even as a downgrade (nor
// the frpc published alongside them). That is accepted: updates only ever
// move to a newer version (compareVersion), and every release from the first
// signed one on publishes a .sig for the agent and for frpc.
//
// AGENT_UPDATE_ALLOW_UNSIGNED=true turns the refusal into a warning; it exists
// for a broken signing key only.
func verifyRemoteSignature(filePath string, binaryURL string, target updatesig.Target) error {
	keyID, err := fetchAndVerifySignature(filePath, binaryURL, target)
	if err == nil {
		log.Info().Str("key_id", keyID).Msgf("signature verified for %s (%s)", binaryURL, target)
		return nil
	}

	if updatesig.AllowUnsigned() {
		log.Warn().Err(err).
			Msgf("WARNING: %s is set; installing %s although its signature did not verify. Anyone who controls the download server can push a binary this way. Unset it once the agent trusts the current signing key", updatesig.AGENT_UPDATE_ALLOW_UNSIGNED, binaryURL)
		return nil
	}

	log.Error().Err(err).
		Msgf("Refusing %s: it is not signed by a trusted IronFlock release key as %s (%s=true bypasses this check, for a broken signing key only)", binaryURL, target, updatesig.AGENT_UPDATE_ALLOW_UNSIGNED)
	return fmt.Errorf("refusing %s: %w", binaryURL, err)
}

// fetchAndVerifySignature returns the ID of the key that signed filePath as
// target.
func fetchAndVerifySignature(filePath string, binaryURL string, target updatesig.Target) (string, error) {
	keys, err := trustedUpdateKeys()
	if err != nil {
		return "", err
	}

	signatureURL := binaryURL + ".sig"
	body, statusCode, err := filesystem.GetRemoteFileWithStatus(signatureURL)
	if err != nil {
		return "", fmt.Errorf("could not fetch update signature %s: %w", signatureURL, err)
	}
	defer func() { _ = body.Close() }()

	if statusCode == http.StatusNotFound {
		return "", fmt.Errorf("%w: nothing published at %s", updatesig.ErrMissingSignature, signatureURL)
	}
	if statusCode != http.StatusOK {
		return "", fmt.Errorf("update signature %s returned status %d", signatureURL, statusCode)
	}

	signature, err := io.ReadAll(io.LimitReader(body, 1024))
	if err != nil {
		return "", fmt.Errorf("could not read update signature %s: %w", signatureURL, err)
	}

	return updatesig.VerifyFile(keys, target, filePath, signature)
}

func GetOSReleaseCurrent() (map[string]string, error) {
	osInfoBytes, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return nil, err
	}
	osInfoString := string(osInfoBytes)
	osInfoSplit := strings.Split(osInfoString, "\n")

	dict := make(map[string]string)
	for _, line := range osInfoSplit {
		keyValuePair := strings.Split(line, "=")
		if len(keyValuePair) == 2 {
			key := keyValuePair[0]
			var value string

			valueSplit := strings.Split(keyValuePair[1], "\"")
			if len(valueSplit) == 3 {
				value = valueSplit[1]
			} else {
				value = valueSplit[0]
			}

			dict[key] = value
		}
	}

	return dict, nil
}

func GetOSReleaseLatest() (map[string]interface{}, error) {
	osInfoBytes, err := os.ReadFile("/etc/os-release-latest.json")
	if err != nil {
		return nil, err
	}
	dict := make(map[string]interface{})
	err = json.Unmarshal(osInfoBytes, &dict)
	if err != nil {
		return nil, err
	}

	return dict, nil
}

// getOSUpdateTags read and extracts info tags about latest available update
func getOSUpdateTags() (string, string, error) {

	// obtain release URL from OS that observes and logs latest OS version
	updateInfoBytes, err := os.ReadFile("/etc/reswarmos-update")
	if err != nil {
		return "", "", err
	}
	updateInfo := strings.Split(string(updateInfoBytes), "\n")[0]
	updateURL := strings.Split(updateInfo, ",")[2]
	updateURLSplit := strings.Split(updateURL, "/")
	updateFile := updateURLSplit[len(updateURLSplit)-1]

	log.Debug().Msgf("getOSUpdateTags(): %s : %s : %s", updateInfo, updateURL, updateFile)

	return updateURL, updateFile, nil
}

// ------------------------------------------------------------------------- //

// GetOSUpdate downloads the actual update-bundle to the device
func GetOSUpdate(progressCallback func(filesystem.DownloadProgress)) error {

	// find release tags from update file regularly updated by the system
	updateURL, updateFile, err := getOSUpdateTags()
	if err != nil {
		log.Error().Err(err).Msgf("Failed to obtain update tags")
	}

	log.Debug().Msg("Starting to download ReswarmOS bundle from " + updateURL + " to /tmp/" + updateFile)

	// download update bundle at given URL
	err = filesystem.DownloadURL("/tmp/"+updateFile, updateURL, progressCallback)
	if err != nil {
		log.Error().Err(err).Msgf("Failed to download from URL: %s", updateURL)
		return err
	}

	log.Debug().Msg("ReswarmOS update bundle download finished from " + updateURL + "...")

	return nil
}

// InstallOSUpdate installs the latest update-bundle available on the device
func InstallOSUpdate(progressCallback func(operationName string, progressPercent uint64)) error {

	// find update-bundle installer file
	_, updateFile, err := getOSUpdateTags()
	if err != nil {
		log.Error().Err(err).Msgf("Failed to obtain update tags")
	}
	bundleFile := "/tmp/" + updateFile

	log.Debug().Msg("installing ReswarmOS update bundle " + bundleFile)

	err = raucInstallBundle(bundleFile, progressCallback)
	if err != nil {
		log.Error().Err(err).Msgf("Failed to install ReswarmOS update bundle")
	}

	return nil
}

func GetEnvironment(config *config.Config) string {
	env := config.ReswarmConfig.Environment
	if env != "" {
		return env
	}

	endpoint := config.ReswarmConfig.DeviceEndpointURL
	if strings.Contains(endpoint, "datapods") {
		return "test"
	} else if strings.Contains(endpoint, "record-evolution") {
		return "production"
	}

	return "local"
}

// GetLatestVersion returns the agent version this device's channel names.
func (system *System) GetLatestVersion() (string, error) {
	channels, _, err := system.resolveAgentVersions()
	if err != nil {
		return "", err
	}
	return system.channelVersion(channels), nil
}

func (system *System) channelVersion(channels map[string]string) string {
	versionString := channels[GetEnvironment(system.config)]
	if versionString == "" {
		versionString = channels["all"]
	}
	return versionString
}

func (system *System) compareVersion(currentVersion string, latestVersion string) (bool, []error, error) {
	constraint, err := semver.NewConstraint(fmt.Sprintf("> %s", currentVersion))
	if err != nil {
		return false, nil, err
	}

	newVersion, err := semver.NewVersion(latestVersion)
	if err != nil {
		return false, nil, err
	}

	shouldUpdate, errors := constraint.Validate(newVersion)
	return shouldUpdate, errors, nil
}

func (system *System) DownloadFrpIfNotExists() error {
	frpcPath := filesystem.GetTunnelBinaryPath(system.config, "frpc")

	// Skip when the on-disk frpc already matches the pinned version.
	exists, err := filesystem.PathExists(frpcPath)
	if err != nil {
		return err
	}
	if exists {
		currentVersion, verErr := system.GetFrpCurrentVersion()
		if verErr != nil {
			log.Warn().Err(verErr).Msg("Could not determine frpc version, will re-acquire")
		} else if currentVersion == embedded.FRP_VERSION {
			log.Debug().Msgf("frpc v%s already present, skipping", currentVersion)
			return nil
		} else {
			log.Info().Msgf("frpc version mismatch (current: %s, want: %s), updating...", currentVersion, embedded.FRP_VERSION)
		}
	}

	// Platforms that embed frpc (Linux/macOS) extract the compiled-in blob.
	if embedded.IsEmbedded() {
		log.Info().Msgf("Extracting embedded frpc v%s to %s", embedded.FRP_VERSION, frpcPath)
		err = embedded.ExtractFrpc(frpcPath)
		if err != nil {
			return fmt.Errorf("failed to extract embedded frpc: %w", err)
		}
		return nil
	}

	// Windows: frpc is not embedded (its bytes would trip antivirus on the
	// agent binary), so it is downloaded as a separate, signed, out-of-process
	// binary. It rides the existing re-agent bucket + appliance /dl proxy via
	// the "re-agent/frpc" sub-path. The download lands at frpcPath exactly.
	log.Info().Msgf("Downloading frpc v%s to %s", embedded.FRP_VERSION, frpcPath)
	err = system.downloadFrpc()
	if err != nil {
		return fmt.Errorf("failed to download frpc: %w", err)
	}

	return nil
}

// downloadFrpc downloads the pinned frpc, which must carry a valid
// frpc-download signature for exactly that version and this platform. It
// comes from the same tree as agent updates.
func (system *System) downloadFrpc() error {
	_, source, err := system.resolveAgentVersions()
	if err != nil {
		return err
	}
	return system.downloadBinary(source.base, source.tree+"/frpc", "frpc", "frpc", embedded.FRP_VERSION, false, updatesig.ContextFrpcDownload, nil)
}

func (system *System) updateFrpIfRequired(progressCallback func(filesystem.DownloadProgress)) (UpdateResult, error) {
	// frpc is pinned to embedded.FRP_VERSION; ensure the on-disk binary is
	// present and current (extracted from the embed on Linux/macOS, downloaded
	// on Windows).
	err := system.DownloadFrpIfNotExists()
	if err != nil {
		return UpdateResult{}, err
	}

	message := "frpc is embedded in binary"
	if !embedded.IsEmbedded() {
		message = "frpc is downloaded on this platform"
	}

	return UpdateResult{
		CurrentVersion: embedded.FRP_VERSION,
		LatestVersion:  embedded.FRP_VERSION,
		Message:        message,
		DidUpdate:      false,
	}, nil
}

func (system *System) GetFrpCurrentVersion() (string, error) {
	frpPath := filesystem.GetTunnelBinaryPath(system.config, "frpc")

	exists, err := filesystem.PathExists(frpPath)
	if err != nil {
		return "", err
	}

	if !exists {
		return "", errdefs.ErrNotFound
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	cmd := exec.CommandContext(ctx, frpPath, "--version") //nolint:gosec // G204: frpc binary installed by the agent itself
	stdout, err := cmd.Output()
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(stdout)), nil
}

// UpdateDeviceMetadata refreshes the locally cached device/project identity
// from the backend and persists it to the .flock config. It reports whether the
// project (swarm) the device belongs to changed, because that also invalidates
// our WAMP identity: authid is "<swarm_key>-<device_key>" and is fixed for the
// life of a connection, so the caller has to reconnect before anything else is
// sent — every device-scoped backend write is keyed on swarm_key and the
// backend rejects a payload whose swarm_key disagrees with the session authid.
//
// The lookup is by device_key alone, so it works from a session whose authid
// still carries the previous project.
func (system *System) UpdateDeviceMetadata() (swarmChanged bool, err error) {
	ctx := context.Background()
	args := []interface{}{common.Dict{"device_key": system.config.ReswarmConfig.DeviceKey}}
	res, err := system.messenger.Call(ctx, topics.GetDeviceMetadata, args, nil, nil, nil)
	if err != nil {
		return false, err
	}

	resultPayload, ok := res.Arguments[0].(map[string]interface{})
	if !ok {
		return false, errors.New("invalid payload")
	}

	deviceName := fmt.Sprint(resultPayload["device_name"])
	swarmName := fmt.Sprint(resultPayload["swarm_name"])
	swarmOwnerName := fmt.Sprint(resultPayload["ownername"])
	// Coerce instead of asserting a single concrete type: which of uint64 /
	// int64 / float64 a number arrives as depends on the negotiated serializer,
	// and getting this wrong strands the device — this is the only code path
	// that can pick up a project transfer.
	swarmKey, ok := common.ToUint64(resultPayload["swarm_key"])
	if !ok {
		return false, fmt.Errorf("swarm_key has invalid type %T", resultPayload["swarm_key"])
	}

	swarmChanged = system.config.ReswarmConfig.SwarmKey != int(swarmKey)

	system.config.ReswarmConfig.Name = deviceName
	system.config.ReswarmConfig.SwarmName = swarmName
	system.config.ReswarmConfig.SwarmOwnerName = swarmOwnerName
	system.config.ReswarmConfig.SwarmKey = int(swarmKey)

	err = config.SaveReswarmConfig(system.config.CommandLineArguments.ConfigFileLocation, system.config.ReswarmConfig)

	return swarmChanged, err
}

func (system *System) UpdateSystem(progressCallback func(filesystem.DownloadProgress), updateAgent bool) (UpdateResult, error) {
	var wg sync.WaitGroup

	progressChan := make(chan filesystem.DownloadProgress)
	progressFunction := func(dp filesystem.DownloadProgress) {
		progressChan <- dp
	}

	startUpdate := time.Now()

	didTunnelUpdate := false
	didAgentUpdate := false
	var agentUpdateResult UpdateResult

	// frpc is delivered on all supported platforms now: embedded on Linux/
	// macOS, downloaded on Windows (updateFrpIfRequired dispatches on that).
	wg.Add(1)
	safe.Go(func() {

		defer wg.Done()

		updateResult, err := system.updateFrpIfRequired(progressFunction)
		if err != nil {
			log.Error().Stack().Err(err).Msgf("Failed to update frpc.. continuing...")
		}

		if !updateResult.DidUpdate {
			log.Debug().Msgf("Not downloading frpc because: %s", updateResult.Message)
		}

		didTunnelUpdate = updateResult.DidUpdate
	})

	if updateAgent {
		wg.Add(1)
		safe.Go(func() {

			defer wg.Done()

			updateResult, err := system.updateAgentIfRequired(progressFunction)
			if err != nil {
				log.Error().Stack().Err(err).Msgf("Failed to update.. continuing...")
			}

			if !updateResult.DidUpdate {
				log.Debug().Msgf("Not downloading Agent because: %s", updateResult.Message)
			}

			didAgentUpdate = updateResult.DidUpdate
			agentUpdateResult = updateResult
		})
	}

	safe.Go(func() {
		wg.Wait()

		// if all download processes end, make sure to close the channel so we don't wait
		close(progressChan)
	})

	bufferProgress := &filesystem.DownloadProgress{
		Increment:     0,
		CurrentBytes:  0,
		TotalFileSize: 0,
	}

	progressMap := make(map[string]*filesystem.DownloadProgress)
	for progress := range progressChan {
		currentProgress := &filesystem.DownloadProgress{}
		if progressMap[progress.FilePath] == nil {
			progressMap[progress.FilePath] = &filesystem.DownloadProgress{
				Increment:     progress.Increment,
				CurrentBytes:  progress.CurrentBytes,
				FilePath:      progress.FilePath,
				TotalFileSize: progress.TotalFileSize,
			}
		} else {
			currentProgress = progressMap[progress.FilePath]
		}

		currentProgress.Increment = progress.Increment
		currentProgress.CurrentBytes = progress.CurrentBytes

		for _, storedProgress := range progressMap {
			bufferProgress.Increment = storedProgress.Increment
			bufferProgress.CurrentBytes += storedProgress.CurrentBytes
			bufferProgress.TotalFileSize += storedProgress.TotalFileSize
		}

		if progressCallback != nil {
			progressCallback(*bufferProgress)
		}

		if bufferProgress.CurrentBytes == bufferProgress.TotalFileSize {
			break
		}

		bufferProgress.Increment = 0
		bufferProgress.CurrentBytes = 0
		bufferProgress.TotalFileSize = 0
	}

	updateTime := time.Since(startUpdate)

	log.Debug().Msgf("Time it took to update system: %s", updateTime)

	// The progress loop above can break on the last downloaded byte, before
	// the goroutines finish verifying/renaming; wait so agentUpdateResult is
	// final before acting on it.
	wg.Wait()

	if didAgentUpdate {
		// On Windows service installs this swaps the new binary into place
		// and schedules a restart; everywhere else it is a no-op (Linux
		// activation is reagent-manager.sh's job, console mode stays
		// download-only).
		system.maybeActivateAgentUpdate(agentUpdateResult)
	}

	return UpdateResult{
		DidAgentUpdate:  didAgentUpdate,
		DidTunnelUpdate: didTunnelUpdate,
		DidUpdate:       didTunnelUpdate || didAgentUpdate,
		InProgress:      false,
	}, nil
}

func (system *System) updateAgentIfRequired(progressCallback func(filesystem.DownloadProgress)) (UpdateResult, error) {
	channels, source, err := system.resolveAgentVersions()
	if err != nil {
		return UpdateResult{}, err
	}
	latestVersion := system.channelVersion(channels)

	currentVersion := release.GetVersion()

	log.Info().Msgf("Should update? Latest: %s, Current: %s", latestVersion, currentVersion)

	shouldUpdate, errorsArr, err := system.compareVersion(currentVersion, latestVersion)
	if err != nil {
		return UpdateResult{}, err
	}

	if !shouldUpdate {
		return UpdateResult{
			CurrentVersion: currentVersion,
			LatestVersion:  latestVersion,
			Message:        fmt.Sprintf("%+v", errorsArr),
			DidUpdate:      false,
		}, nil
	}

	// A version that already failed its probation here stays blocked until a
	// newer one appears; downloading it again would only cost bandwidth.
	if lifecycle.Supervised() && selfupdate.New(system.config.CommandLineArguments.AgentDir).IsVersionBlocked(latestVersion) {
		return UpdateResult{
			CurrentVersion: currentVersion,
			LatestVersion:  latestVersion,
			Message:        fmt.Sprintf("v%s failed on this device before and is blocked", latestVersion),
			DidUpdate:      false,
		}, nil
	}

	log.Info().Msgf("Agent not up to date, downloading: %s", latestVersion)
	err = system.downloadBinary(source.base, source.tree, source.publishedName, system.localBinaryName(), latestVersion, true, updatesig.ContextAgentUpdate, progressCallback)
	if err != nil {
		if errdefs.IsInProgress(err) {
			return UpdateResult{
				CurrentVersion: currentVersion,
				LatestVersion:  latestVersion,
				DidUpdate:      false,
				InProgress:     true,
			}, err
		}
		return UpdateResult{}, err
	}

	return UpdateResult{
		CurrentVersion: currentVersion,
		LatestVersion:  latestVersion,
		DidUpdate:      true,
	}, nil
}

// ------------------------------------------------------------------------- //
