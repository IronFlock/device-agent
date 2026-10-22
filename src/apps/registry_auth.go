package apps

import (
	"fmt"
	"reagent/common"
	"reagent/config"
	"reagent/container"
	"strings"
)

// splitRegistryHost splits an image reference into its registry host and the
// repository part that follows it, using docker's own convention: the first
// path component is a registry host only when it contains '.' or ':' or is
// exactly "localhost". The host is "" for a reference that carries none, in
// which case the whole reference is the repository.
func splitRegistryHost(imageRef string) (host string, repository string) {
	firstComponent, remainder, found := strings.Cut(imageRef, "/")
	if found && (strings.ContainsAny(firstComponent, ".:") || firstComponent == "localhost") {
		return firstComponent, remainder
	}
	return "", imageRef
}

// registryHostOfImage returns the registry host of an image reference; a
// reference that carries no host lives on Docker Hub.
func registryHostOfImage(imageRef string) string {
	if host, _ := splitRegistryHost(imageRef); host != "" {
		return host
	}
	return "docker.io"
}

// credentialForRegistryHost resolves the credential entry for a registry
// host. Both sides go through common.NormalizeRegistryHost, so a stored key
// like `https://MyRegistry.azurecr.io/` still matches the bare lowercase host
// parsed out of an image reference. The second return value reports whether
// any entry matched — a miss means the pull will run anonymously, and callers
// should say so in the app log instead of failing silently.
func credentialForRegistryHost(credentials map[string]common.DockerCredential, host string) (container.AuthConfig, bool) {
	normalizedHost := common.NormalizeRegistryHost(host)
	if normalizedHost == "" {
		return container.AuthConfig{}, false
	}

	cred, found := credentials[normalizedHost]
	if !found {
		for key, candidate := range credentials {
			if common.NormalizeRegistryHost(key) == normalizedHost {
				cred, found = candidate, true
				break
			}
		}
	}

	return container.AuthConfig{Username: cred.Username, Password: cred.Password}, found
}

// applianceRegistryHost returns the name an appliance's registry answers to on
// its own domain — registry.<appliance_domain>, the Caddy vhost in front of
// appstore-registry — or "" for a device that is not attached to an appliance
// domain.
//
// An appliance in domain mode has TWO names for ONE registry, and which name
// an image reference carries depends on who minted it: the appliance's own
// colocated agent is configured with the loopback form (localhost:15001, see
// install_ironflock.sh AGENT_DOCKER_REGISTRY_URL — the host's docker daemon
// resolves it and it survives the registry port leaving the LAN), while every
// ref the app-store sync rewrites carries the domain form (RESWARM
// appStore.ts, from the backend's DOCKER_REGISTRY_URL). Both names reach the
// same registry and the same regauth token service, so the device's own store
// credential authenticates against either.
func applianceRegistryHost(reswarmConfig *config.ReswarmConfig) string {
	if reswarmConfig == nil {
		return ""
	}
	domain := strings.TrimSpace(reswarmConfig.ApplianceDomain)
	if domain == "" {
		return ""
	}
	return common.NormalizeRegistryHost("registry." + domain)
}

// platformRegistryHosts lists the canonical registry hosts the device's own
// store credential (registry token + device secret) authenticates against:
// the configured registry URL plus, on an appliance, its domain-mode alias.
func platformRegistryHosts(reswarmConfig *config.ReswarmConfig) []string {
	if reswarmConfig == nil {
		return nil
	}

	hosts := make([]string, 0, 2)
	if primary := common.NormalizeRegistryHost(reswarmConfig.DockerRegistryURL); primary != "" {
		hosts = append(hosts, primary)
	}
	if alias := applianceRegistryHost(reswarmConfig); alias != "" && (len(hosts) == 0 || hosts[0] != alias) {
		hosts = append(hosts, alias)
	}

	return hosts
}

// isPlatformRegistryHost reports whether an image's registry host is one of
// the platform's own names, i.e. whether the store credential covers it.
func isPlatformRegistryHost(reswarmConfig *config.ReswarmConfig, host string) bool {
	normalizedHost := common.NormalizeRegistryHost(host)
	if normalizedHost == "" {
		return false
	}

	for _, platformHost := range platformRegistryHosts(reswarmConfig) {
		if platformHost == normalizedHost {
			return true
		}
	}

	return false
}

// composeImageRefs returns every service's `image:` value in a compose
// definition, in no particular order.
func composeImageRefs(compose map[string]interface{}) []string {
	services, ok := compose["services"].(map[string]interface{})
	if !ok {
		return nil
	}

	imageRefs := make([]string, 0, len(services))
	for _, serviceInterface := range services {
		service, ok := serviceInterface.(map[string]interface{})
		if !ok {
			continue
		}
		if imageRef, _ := service["image"].(string); imageRef != "" {
			imageRefs = append(imageRefs, imageRef)
		}
	}

	return imageRefs
}

// composeReferencesRegistryHost reports whether any service image in a compose
// definition lives on the given (already canonical) registry host.
func composeReferencesRegistryHost(compose map[string]interface{}, host string) bool {
	if compose == nil || host == "" {
		return false
	}

	for _, imageRef := range composeImageRefs(compose) {
		if common.NormalizeRegistryHost(registryHostOfImage(imageRef)) == host {
			return true
		}
	}

	return false
}

// isOwnStoreImageRepository reports whether a repository path names one of THIS
// app's store images. app.f_create_release mints one per compose service as
//
//	<docker_main_repository>prod_<arch>_<appKey>_<appName>_<n>:<version>
//
// lowercased, for every service of a release — image-only services included,
// whose authored reference is preserved beside it in x-source-image. The
// "prod" is a literal there, not the stage. Matching on the app's own key and
// name is what keeps this off an authored reference that merely happens to
// live under the same repository prefix.
func isOwnStoreImageRepository(mainRepository string, appKey uint64, appName string, repository string) bool {
	prefix := strings.ToLower(strings.TrimSpace(mainRepository))
	if prefix == "" || appName == "" {
		return false
	}

	repository = strings.ToLower(repository)
	if !strings.HasPrefix(repository, prefix) {
		return false
	}

	imageName := strings.TrimPrefix(repository, prefix)
	if !strings.HasPrefix(imageName, strings.ToLower(string(common.PROD))+"_") {
		return false
	}

	return strings.Contains(imageName, fmt.Sprintf("_%d_%s_", appKey, strings.ToLower(appName)))
}

// resolveComposeStoreImages rewrites, in place, every service image that names
// one of this app's own store images onto the registry host THIS device is
// configured for, and reports how many references it changed.
//
// A release's image references are minted once, at publish time, carrying
// whichever registry URL the platform that minted them was configured with
// (app.f_create_release, and the appliance app-store sync in RESWARM
// appStore.ts). That host is a LOCATION, and it is frozen into release data
// that outlives the address it names: an appliance changes IP, switches
// between plain and domain mode, and serves one registry under two names at
// once, so no single literal is right for every reader — the same release row
// is read by the appliance's own colocated agent, by off-host devices, and in
// the cloud. The repository part, by contrast, is stable everywhere. So the
// device resolves the location itself, here, when the compose file is
// materialised — the same treatment host ports, env_file and extra_hosts
// already get in SetupComposeFiles. Stored release data is never touched.
//
// A reference whose host actually changes makes the next `compose up` recreate
// that service: the image is a different tag to docker, so it is pulled once
// under the new name. That is the intended self-heal for references stranded
// by a mode switch, and it costs one pull.
func resolveComposeStoreImages(reswarmConfig *config.ReswarmConfig, appKey uint64, appName string, compose map[string]interface{}) int {
	if reswarmConfig == nil || compose == nil {
		return 0
	}

	registryBase := strings.TrimSuffix(strings.TrimSpace(reswarmConfig.DockerRegistryURL), "/")
	if registryBase == "" {
		return 0
	}

	services, ok := compose["services"].(map[string]interface{})
	if !ok {
		return 0
	}

	resolved := 0
	for _, serviceInterface := range services {
		service, ok := serviceInterface.(map[string]interface{})
		if !ok {
			continue
		}

		imageRef, _ := service["image"].(string)
		if imageRef == "" {
			continue
		}

		_, repository := splitRegistryHost(imageRef)
		if !isOwnStoreImageRepository(reswarmConfig.DockerMainRepository, appKey, appName, repository) {
			continue
		}

		resolvedRef := registryBase + "/" + repository
		if resolvedRef == imageRef {
			continue
		}

		service["image"] = resolvedRef
		resolved++
	}

	return resolved
}

// authConfigForImage picks pull credentials by the image's registry host: the
// platform's own registry gets the device's store token, any other host the
// app's docker_credentials entry. A miss on an external host degrades to an
// anonymous pull — logged to the app's log topic so a missing or mistyped
// credential host is visible instead of surfacing later as a bare
// "unauthorized" from the daemon.
func (sm *StateMachine) authConfigForImage(payload common.TransitionPayload, imageRef string, logTopic string) container.AuthConfig {
	config := sm.Container.GetConfig()
	host := registryHostOfImage(imageRef)

	if isPlatformRegistryHost(config.ReswarmConfig, host) {
		return container.AuthConfig{
			Username: payload.RegisteryToken,
			Password: config.ReswarmConfig.Secret,
		}
	}

	authConfig, found := credentialForRegistryHost(payload.DockerCredentials, host)
	if !found && logTopic != "" {
		_ = sm.LogManager.Write(logTopic, registryCredentialMissMessage(host))
	}

	return authConfig
}

// warnUncredentialedComposeRegistries logs, best-effort, every external
// registry referenced by a compose definition's `image:` fields for which no
// credential entry matches. It only speaks up when the app has credentials
// configured at all: anonymous pulls of public images are the norm, and
// warning on every public image would drown the log. Called on the compose
// pull/start paths (never on build paths, whose service image names are local
// build targets that would misread as Docker Hub references).
func (sm *StateMachine) warnUncredentialedComposeRegistries(payload common.TransitionPayload, compose map[string]interface{}, logTopic string) {
	if len(payload.DockerCredentials) == 0 || compose == nil {
		return
	}

	config := sm.Container.GetConfig()

	warned := make(map[string]bool)
	for _, imageRef := range composeImageRefs(compose) {
		host, repository := splitRegistryHost(imageRef)
		normalizedHost := common.NormalizeRegistryHost(host)
		if normalizedHost == "" || warned[normalizedHost] || isPlatformRegistryHost(config.ReswarmConfig, host) {
			continue
		}

		// A store image of this app is pulled from the device's own registry
		// whatever host the reference was minted with (resolveComposeStoreImages),
		// so the minted host is not a registry anyone needs credentials for.
		// Warning about it would send the reader looking for a credential that
		// changes nothing.
		if isOwnStoreImageRepository(config.ReswarmConfig.DockerMainRepository, payload.AppKey, payload.AppName, repository) {
			continue
		}

		if _, found := credentialForRegistryHost(payload.DockerCredentials, host); !found {
			warned[normalizedHost] = true
			_ = sm.LogManager.Write(logTopic, registryCredentialMissMessage(host))
		}
	}
}

// registryCredentialMissMessage names an anonymous pull against an external
// registry and where to fix it, for the app's log stream.
func registryCredentialMissMessage(host string) string {
	return fmt.Sprintf("No stored credentials match registry %s — pulling anonymously. If this registry is private, add credentials for host %q under App Settings → Docker Registry Credentials.", host, common.NormalizeRegistryHost(host))
}

// isRegistryAuthError reports whether a docker daemon error is a registry
// auth rejection (anonymous pull of a private image, or wrong credentials).
func isRegistryAuthError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unauthorized") ||
		strings.Contains(message, "authentication required") ||
		strings.Contains(message, "access denied") ||
		strings.Contains(message, "denied: requested access")
}
