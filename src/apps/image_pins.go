package apps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reagent/container"
	"regexp"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// composeImageDigestField is the compose extension field in which a release
// pins a service's image to a manifest digest. The platform sets it when it
// creates a release, from the digest the store registry held for the image
// right after the publish was checked (backend containers.ts,
// pinReleaseImages). Compose itself ignores x- fields, so agents that predate
// it install by tag as before.
const composeImageDigestField = "x-image-digest"

// imageDigestPattern is the only form a pinned digest may take.
var imageDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// pinComposeImages rewrites, in place, the image of every service its release
// pins (x-image-digest) to "<image>@<digest>" — "registry/apps/x:1.0@sha256:…".
// Docker then pulls and runs exactly that manifest and verifies it: whatever
// the tag is made to point to after the release was created, a device never
// installs it. The tag stays in the reference for the reader; docker resolves
// a reference that carries both by its digest.
//
// Run it after resolveComposeStoreImages: the digest belongs to the content,
// not to the registry host the device reads it from. A pin that is not a
// sha256 digest fails the app rather than letting it run unpinned. An image
// already pinned in its own reference is left as it is.
func pinComposeImages(compose map[string]interface{}) (int, error) {
	services, ok := compose["services"].(map[string]interface{})
	if !ok {
		return 0, nil
	}

	pinned := 0
	for serviceName, serviceInterface := range services {
		service, ok := serviceInterface.(map[string]interface{})
		if !ok {
			continue
		}

		digestValue, found := service[composeImageDigestField]
		if !found {
			continue
		}
		digest, _ := digestValue.(string)
		if !imageDigestPattern.MatchString(digest) {
			return pinned, fmt.Errorf("the release pins service %s to an invalid image digest %q", serviceName, fmt.Sprint(digestValue))
		}

		imageRef, _ := service["image"].(string)
		if imageRef == "" {
			return pinned, fmt.Errorf("the release pins service %s to an image digest but names no image", serviceName)
		}
		if strings.Contains(imageRef, "@") {
			continue
		}

		service["image"] = imageRef + "@" + digest
		pinned++
	}

	return pinned, nil
}

// pinnedImageTag is the tag a pinned image gets after its pull: Source names
// the image by digest, Target by the tag of its reference.
type pinnedImageTag struct {
	Source string
	Target string
}

// pinnedImageTags returns, for every service image of a materialised compose
// that is pinned in its reference ("repo:tag@digest"), the tag to put on the
// pulled image. A reference without a tag has nothing to restore.
func pinnedImageTags(compose map[string]interface{}) []pinnedImageTag {
	tags := []pinnedImageTag{}
	for _, imageRef := range composeImageRefs(compose) {
		at := strings.Index(imageRef, "@")
		if at == -1 {
			continue
		}
		tagged, digest := imageRef[:at], imageRef[at+1:]
		colon := strings.LastIndex(tagged, ":")
		if colon <= strings.LastIndex(tagged, "/") {
			continue
		}
		tags = append(tags, pinnedImageTag{Source: tagged[:colon] + "@" + digest, Target: tagged})
	}
	return tags
}

// tagPinnedImages gives every pinned image of the compose file at
// dockerComposePath its tag back. Docker stores an image pulled by digest
// without a tag, but the agent finds, prunes and removes app images by tag
// (composeServicesWithMissingImages for unpinned services, the uninstall and
// update clean-up, diskguard), so an untagged one would be left on the disk
// forever. Best effort: the app runs by digest either way, a failure is only
// logged.
func (sm *StateMachine) tagPinnedImages(dockerComposePath string) {
	content, err := os.ReadFile(dockerComposePath) //nolint:gosec // G304: the agent's own compose file, written by SetupComposeFiles
	if err != nil {
		log.Warn().Err(err).Str("compose", dockerComposePath).Msg("could not read the compose file to tag its pinned images")
		return
	}
	var compose map[string]interface{}
	if err := json.Unmarshal(content, &compose); err != nil {
		log.Warn().Err(err).Str("compose", dockerComposePath).Msg("could not parse the compose file to tag its pinned images")
		return
	}

	for _, tag := range pinnedImageTags(compose) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := sm.Container.Tag(ctx, tag.Source, tag.Target)
		cancel()
		if err != nil {
			log.Warn().Err(err).Str("image", tag.Source).Str("tag", tag.Target).Msg("could not tag a pinned image")
		}
	}
}

// pinnedImagePresent reports whether a local image holds the manifest a
// service is pinned to: one of its RepoDigests names the same repository
// (registry host included, as the reference names it) and the digest.
func pinnedImagePresent(imageRef, digest string, images []container.ImageResult) bool {
	repository := imageRef
	if at := strings.Index(repository, "@"); at != -1 {
		repository = repository[:at]
	}
	if colon := strings.LastIndex(repository, ":"); colon > strings.LastIndex(repository, "/") {
		repository = repository[:colon]
	}

	want := repository + "@" + digest
	for _, image := range images {
		for _, repoDigest := range image.RepoDigests {
			if repoDigest == want {
				return true
			}
		}
	}
	return false
}
