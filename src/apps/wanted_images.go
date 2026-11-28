package apps

import (
	"reagent/common"
	"reagent/config"
	"strings"
)

// WantedAppImages is every image tag (repo:tag) the apps on the device still
// need, which the disk guard never reclaims: for each app its single image in
// both stages at its present and newest version, under the agent's own name
// and under the name the platform gave for that version, and every service image of
// its Compose definitions, the one it runs and the one an update brings. A
// Compose image is named the way the agent pulls it (resolveComposeStoreImages
// moves the app's own store images onto this device's registry) and as it
// is tagged after a pinned pull (tagPinnedImages), and also as the release
// names it. More names only ever keep more.
func WantedAppImages(reswarmConfig *config.ReswarmConfig, payloads []common.TransitionPayload) map[string]bool {
	wanted := make(map[string]bool, len(payloads)*4)
	for _, p := range payloads {
		for _, repo := range []string{p.RegistryImageName.Prod, p.RegistryImageName.Dev} {
			if repo == "" {
				continue
			}
			for _, version := range []string{p.PresentVersion, p.NewestVersion, p.Version} {
				if version != "" {
					wanted[repo+":"+version] = true
				}
			}
		}
		// and under the names the platform gave for them (prodImageFor), and
		// the one the device recorded pulling or running it under
		for _, version := range []string{p.PresentVersion, p.NewestVersion, p.Version} {
			for _, ref := range prodImageCandidates(reswarmConfig, p, version) {
				wanted[ref] = true
			}
		}
		if ownsImageRef(p, p.InstalledImage) {
			wanted[p.InstalledImage] = true
		}

		for _, compose := range []map[string]interface{}{p.DockerCompose, p.NewDockerCompose} {
			for _, ref := range composeImageRefs(compose) {
				if tag := imageTag(ref); tag != "" {
					wanted[tag] = true
				}
				if reswarmConfig == nil {
					continue
				}
				registryBase := strings.TrimSuffix(strings.TrimSpace(reswarmConfig.DockerRegistryURL), "/")
				resolved := resolveStoreImageRef(reswarmConfig.DockerMainRepository, registryBase, p.AppKey, p.AppName, ref)
				if tag := imageTag(resolved); tag != "" {
					wanted[tag] = true
				}
			}
		}
	}
	return wanted
}

// imageTag is the tag Docker lists for an image pulled by ref: ref without a
// digest, with :latest where it names no tag. "" for a reference by digest
// only, which leaves the image without a tag.
func imageTag(ref string) string {
	ref = strings.TrimSpace(ref)
	digest := false
	if at := strings.Index(ref, "@"); at != -1 {
		ref, digest = ref[:at], true
	}
	if ref == "" {
		return ""
	}
	if strings.LastIndex(ref, ":") > strings.LastIndex(ref, "/") {
		return ref
	}
	if digest {
		return ""
	}
	return ref + ":latest"
}
