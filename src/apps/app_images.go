package apps

import (
	"context"
	"fmt"
	"reagent/common"
	"reagent/config"
	"reagent/container"
	"reagent/errdefs"
	"regexp"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// The image of a single-image app (an app without a Compose file), PROD.
//
// The platform names it in the sync answer, for the version the device runs
// (present_image_name, for present_version) and for the one it is to go to
// (new_image_name, for newest_version): <repository>:<tag> below the
// registry's main repository (platform-db app.f_image_name), e.g.
//
//	prod_1953_myapp:1.2.0        a release with one name per version, an
//	                             image index with an image per platform
//	prod_amd64_1953_myapp:1.1.0  a release from before: one name per version
//	                             and architecture
//
// The device puts its own registry URL and main repository in front, as it
// always did. Before, the agent formed the name itself, from the architecture
// it was built for (TransitionPayload.RegistryImageName.Prod); it still does
// where the platform names none the agent can use: an older backend or
// appliance, an empty field, a name for another version, a name of a shape
// it does not know (which is logged and otherwise ignored: it never makes the
// agent pull, run or remove anything under it), and every DEV app, whose
// image the device builds itself. A release build always pushes under the
// agent's own name (publishApp), which is what the backend expects.

// imageTagPattern is a Docker tag.
var imageTagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

// imageArchPattern is the architecture part of a name from before: a
// lower-case word that does not start with a digit (amd64, arm64, armv7,
// ...). The second part of a new name is the app key, a number.
var imageArchPattern = regexp.MustCompile(`^[a-z][a-z0-9]*$`)

// platformImageRepository is the repository and tag of name, a name the
// platform gives for this app's single PROD image at version, when it is one
// the agent understands: prod_<app_key>_<name> or
// prod_<architecture>_<app_key>_<name> of exactly this app, with a tag that
// is version (letter case aside: the platform's names are lower case). ok is
// false for anything else.
func platformImageRepository(payload common.TransitionPayload, name string, version string) (repository string, tag string, ok bool) {
	if payload.Stage != common.PROD || name == "" || version == "" || payload.AppName == "" {
		return "", "", false
	}

	colon := strings.LastIndex(name, ":")
	if colon == -1 {
		return "", "", false
	}
	repository, tag = name[:colon], name[colon+1:]
	if !imageTagPattern.MatchString(tag) || !strings.EqualFold(tag, version) {
		return "", "", false
	}

	appPart := fmt.Sprintf("%d_%s", payload.AppKey, strings.ToLower(payload.AppName))
	if repository == "prod_"+appPart {
		return repository, tag, true
	}
	arch, found := strings.CutPrefix(repository, "prod_")
	if !found {
		return "", "", false
	}
	arch, found = strings.CutSuffix(arch, "_"+appPart)
	if !found || !imageArchPattern.MatchString(arch) {
		return "", "", false
	}
	return repository, tag, true
}

// platformProdImage is the reference of the platform's name for the app's
// single PROD image at version (its registry and main repository in front),
// or "" when name is not one for it (platformImageRepository), and when there
// is no configuration to put in front. Where the name is the one the agent
// forms itself, the reference is exactly the agent's own, the version as the
// sync answer spells it: releases under the old names are pulled and run as
// they always were.
func platformProdImage(rc *config.ReswarmConfig, payload common.TransitionPayload, name string, version string) string {
	repository, tag, ok := platformImageRepository(payload, name, version)
	if !ok {
		return ""
	}
	if repository == payload.ImageName.Prod && payload.RegistryImageName.Prod != "" {
		return payload.RegistryImageName.Prod + ":" + version
	}
	if rc == nil {
		return ""
	}
	return common.BuildRegistryImageName(rc.DockerRegistryURL, rc.DockerMainRepository, repository) + ":" + tag
}

// platformImageNames are the names the platform gave for the app's single
// image: the present one where the release the device runs is a single-image
// one, the new one where the release it goes to is. A Compose release's
// platform entry carries a name too, which names no image.
func platformImageNames(payload common.TransitionPayload) []string {
	names := make([]string, 0, 2)
	if payload.DockerCompose == nil && payload.PresentImageName != "" {
		names = append(names, payload.PresentImageName)
	}
	if payload.NewDockerCompose == nil && payload.NewImageName != "" {
		names = append(names, payload.NewImageName)
	}
	return names
}

// ownProdImage is the name the agent forms itself for the app's image at
// version (RegistryImageName.Prod:version), "" without either.
func ownProdImage(payload common.TransitionPayload, version string) string {
	if payload.RegistryImageName.Prod == "" || version == "" {
		return ""
	}
	return payload.RegistryImageName.Prod + ":" + version
}

// prodImageFor is the reference of the app's single PROD image at version:
// the platform's name for that version, where the sync answer gives one the
// agent understands, else the name the agent forms itself
// (RegistryImageName.Prod:version), as before. Pulls and updates try it first
// (pullProdImage).
func prodImageFor(rc *config.ReswarmConfig, payload common.TransitionPayload, version string) string {
	for _, name := range platformImageNames(payload) {
		if ref := platformProdImage(rc, payload, name, version); ref != "" {
			return ref
		}
	}
	if payload.RegistryImageName.Prod != "" && version != "" {
		logUnusedPlatformImageNames(payload)
	}
	return payload.RegistryImageName.Prod + ":" + version
}

// logUnusedPlatformImageNames says, at debug level, that the platform named an
// image the agent does not use. A name for another version is normal (the
// present one while the newest is asked for); one of another shape is not.
func logUnusedPlatformImageNames(payload common.TransitionPayload) {
	for _, name := range platformImageNames(payload) {
		understood := false
		for _, version := range []string{payload.PresentVersion, payload.NewestVersion} {
			if _, _, ok := platformImageRepository(payload, name, version); ok {
				understood = true
			}
		}
		if !understood {
			log.Debug().Str("app", payload.AppName).Str("image", name).
				Msg("the platform names an image the agent does not understand; using the agent's own name")
		}
	}
}

// ownsImageRef reports whether ref (registry, repository, tag) is a reference
// of this app's single PROD image: under the agent's own name, or in a
// repository of the platform's form for this app.
func ownsImageRef(payload common.TransitionPayload, ref string) bool {
	if payload.Stage != common.PROD || ref == "" || strings.Contains(ref, "@") {
		return false
	}
	repository, tag := splitImageTag(ref)
	if tag == "" {
		return false
	}
	if repository == payload.RegistryImageName.Prod {
		return true
	}
	last := repository[strings.LastIndex(repository, "/")+1:]
	_, _, ok := platformImageRepository(payload, last+":"+tag, tag)
	return ok
}

// installedProdImage is the reference the device really pulled or ran the
// app's version under (TransitionPayload.InstalledImage), "" when none is
// recorded for exactly that version.
func installedProdImage(payload common.TransitionPayload, version string) string {
	if version == "" || payload.InstalledImageVersion != version || !ownsImageRef(payload, payload.InstalledImage) {
		return ""
	}
	return payload.InstalledImage
}

// prodImageCandidates are the references under which the device may hold the
// app's single PROD image at version, in the order they are used: the one it
// really pulled or ran it under (installedProdImage), the platform's name for
// it (prodImageFor), then the agent's own name. An app installed before the
// agent took its names from the platform, or before the platform moved the
// version to a new name, has its image under the agent's own name only, and
// runs with it.
func prodImageCandidates(rc *config.ReswarmConfig, payload common.TransitionPayload, version string) []string {
	if version == "" {
		return nil
	}
	refs := []string{}
	add := func(ref string) {
		if ref == "" || strings.HasSuffix(ref, ":") {
			return
		}
		for _, r := range refs {
			if r == ref {
				return
			}
		}
		refs = append(refs, ref)
	}
	add(installedProdImage(payload, version))
	add(prodImageFor(rc, payload, version))
	add(ownProdImage(payload, version))
	return refs
}

// prodImageRepositories are the repositories of the app's single PROD image:
// the agent's own, those the platform names for it, and the one the device
// really pulled or ran it under. Every tag in them is one of this app's PROD
// images (the names carry its key and name).
func prodImageRepositories(rc *config.ReswarmConfig, payload common.TransitionPayload) []string {
	repositories := []string{}
	add := func(repository string) {
		if repository == "" {
			return
		}
		for _, r := range repositories {
			if r == repository {
				return
			}
		}
		repositories = append(repositories, repository)
	}

	add(payload.RegistryImageName.Prod)
	for _, name := range platformImageNames(payload) {
		for _, version := range []string{payload.PresentVersion, payload.NewestVersion} {
			if ref := platformProdImage(rc, payload, name, version); ref != "" {
				add(imageRepository(ref))
				break
			}
		}
	}
	if ownsImageRef(payload, payload.InstalledImage) {
		add(imageRepository(payload.InstalledImage))
	}
	return repositories
}

// appImageRepositories are the repositories of the app's image in its stage:
// for DEV the one its build tags, for PROD prodImageRepositories.
func appImageRepositories(rc *config.ReswarmConfig, payload common.TransitionPayload) []string {
	if payload.Stage == common.DEV {
		if payload.RegistryImageName.Dev == "" {
			return nil
		}
		return []string{payload.RegistryImageName.Dev}
	}
	return prodImageRepositories(rc, payload)
}

// hasImageInRepositories reports whether one of the images carries a tag in
// one of the repositories.
func hasImageInRepositories(images []container.ImageResult, repositories []string) bool {
	for _, image := range images {
		for _, tag := range image.RepoTags {
			repository := imageRepository(tag)
			for _, r := range repositories {
				if repository == r {
					return true
				}
			}
		}
	}
	return false
}

// reswarmConfig is the device's .flock configuration, nil when there is none.
func (sm *StateMachine) reswarmConfig() *config.ReswarmConfig {
	cfg := sm.Container.GetConfig()
	if cfg == nil {
		return nil
	}
	return cfg.ReswarmConfig
}

// reswarmConfigFor is the configuration the platform's names of payload are
// resolved with, nil where it names none: then the configuration is not read,
// call for call as before.
func (sm *StateMachine) reswarmConfigFor(payload common.TransitionPayload) *config.ReswarmConfig {
	if len(platformImageNames(payload)) == 0 {
		return nil
	}
	return sm.reswarmConfig()
}

// splitImageTag splits "repo:tag" into its repository and tag ("" without).
func splitImageTag(ref string) (string, string) {
	if colon := strings.LastIndex(ref, ":"); colon > strings.LastIndex(ref, "/") {
		return ref[:colon], ref[colon+1:]
	}
	return ref, ""
}

// withInstalledImage is payload with the image the device recorded as really
// pulled or run (InstalledImage), from the stored requested state: a sync
// answer never carries it. Best effort: payload as it is when it cannot be read.
func (sm *StateMachine) withInstalledImage(payload common.TransitionPayload) common.TransitionPayload {
	if payload.Stage != common.PROD || sm.StateObserver == nil || sm.StateObserver.AppStore == nil {
		return payload
	}
	stored, err := sm.StateObserver.AppStore.GetRequestedState(payload.AppKey, payload.Stage)
	if err != nil || stored.InstalledImage == "" {
		return payload
	}
	payload.InstalledImage, payload.InstalledImageVersion = stored.InstalledImage, stored.InstalledImageVersion
	return payload
}

// rememberInstalledImage records ref as the reference the device holds the
// app's version under. Best effort: without the record the agent finds the
// image by the other names.
func (sm *StateMachine) rememberInstalledImage(payload common.TransitionPayload, version string, ref string) {
	if payload.Stage != common.PROD || version == "" || !ownsImageRef(payload, ref) ||
		sm.StateObserver == nil || sm.StateObserver.AppStore == nil {
		return
	}
	if err := sm.StateObserver.AppStore.SetInstalledImage(payload.AppKey, payload.Stage, version, ref); err != nil {
		log.Debug().Err(err).Str("image", ref).Msg("could not record the image the app was installed under")
	}
}

// localProdImage is the reference the app's single PROD image at version is
// on the device under: the first of prodImageCandidates that is there
// (recorded as the one it runs with, when it is not yet). found is false when
// none is. When Docker cannot say (err), ref is the one the device recorded
// for the version, else the agent's own name, as before: never a platform's
// name that nothing confirmed.
func (sm *StateMachine) localProdImage(payload common.TransitionPayload, version string) (ref string, found bool, err error) {
	payload = sm.withInstalledImage(payload)
	candidates := prodImageCandidates(sm.reswarmConfigFor(payload), payload, version)
	fallback := installedProdImage(payload, version)
	if fallback == "" {
		fallback = payload.RegistryImageName.Prod + ":" + version
	}
	if len(candidates) == 0 {
		// no version: looked for as before
		candidates = []string{fallback}
	}
	for _, candidate := range candidates {
		repository, tag := splitImageTag(candidate)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
		_, getErr := sm.Container.GetImage(ctx, repository, tag)
		cancel()
		if getErr == nil {
			if candidate != installedProdImage(payload, version) {
				sm.rememberInstalledImage(payload, version, candidate)
			}
			return candidate, true, nil
		}
		if !errdefs.IsImageNotFound(getErr) {
			return fallback, false, getErr
		}
	}
	return candidates[0], false, nil
}

// pullProdImage pulls the app's single PROD image at version, by tag: under
// the platform's name first (prodImageFor), and, should that pull fail for
// any reason, once more under the agent's own name where that is another
// reference. Until the platform matches a device by its Docker engine, a
// version's image index is sure to hold the agent's platform only, and an
// engine of another platform finds no image of its own in it; the agent's
// per-architecture name, which the registry keeps during the transition, is
// what such a device pulled before. The first error is logged; when both
// pulls fail, the second one's error is returned, the one the agent got
// before. A canceled pull is not tried again. started runs once a pull has
// begun (before its stream is read), on every attempt. It answers the
// reference that was pulled.
func (sm *StateMachine) pullProdImage(payload common.TransitionPayload, version string, started func() error) (string, error) {
	ref := prodImageFor(sm.reswarmConfigFor(payload), payload, version)
	pullErr, otherErr := sm.pullImage(payload, ref, started)
	if otherErr != nil {
		return ref, otherErr
	}
	own := ownProdImage(payload, version)
	if pullErr == nil || errdefs.IsDockerStreamCanceled(pullErr) || own == "" || own == ref {
		return ref, pullErr
	}

	log.Warn().Err(pullErr).Str("app", payload.AppName).Str("image", ref).Str("fallback", own).
		Msg("the pull under the platform's name failed; pulling the agent's own name")
	_ = sm.LogManager.Write(payload.ContainerName.Prod, fmt.Sprintf("Pulling %s instead...", own))
	ownErr, otherErr := sm.pullImage(payload, own, started)
	if otherErr != nil {
		return own, otherErr
	}
	return own, ownErr
}

// pullImage pulls ref and streams its progress into the app's log. pullErr is
// a failure of the pull itself; otherErr one of started.
func (sm *StateMachine) pullImage(payload common.TransitionPayload, ref string, started func() error) (pullErr error, otherErr error) {
	topic := payload.ContainerName.Prod
	pullOptions := container.PullOptions{
		AuthConfig: sm.authConfigForImage(payload, ref, topic),
		PullID:     common.BuildDockerPullID(payload.AppKey, payload.AppName),
	}

	log.Debug().Msgf("PULLING IMAGE: %s", ref)
	reader, err := sm.Container.Pull(context.Background(), ref, pullOptions)
	if err != nil {
		_ = sm.LogManager.Write(topic, fmt.Sprintf("Error occured while trying to pull the image: %s", err.Error()))
		return err, nil
	}
	if started != nil {
		if err := started(); err != nil {
			_ = reader.Close()
			return nil, err
		}
	}
	return sm.LogManager.StreamBlocking(topic, common.PULL, reader), nil
}

// wantedByOthers is every image tag another app's or stage's requested state
// still wants (WantedAppImages over all but payload's own); ok is false when
// the requested states cannot be read, and then nothing may be removed.
func (sm *StateMachine) wantedByOthers(payload common.TransitionPayload) (map[string]bool, bool) {
	states, err := sm.StateObserver.AppStore.GetRequestedStates()
	if err != nil {
		log.Warn().Err(err).Str("app", payload.AppName).Msg("could not read the requested states; leaving the app's images")
		return nil, false
	}
	others := make([]common.TransitionPayload, 0, len(states))
	var rc *config.ReswarmConfig
	if len(platformImageNames(payload)) > 0 {
		rc = sm.reswarmConfig()
	}
	for _, state := range states {
		if state.AppKey == payload.AppKey && state.Stage == payload.Stage {
			continue
		}
		others = append(others, state)
		if rc == nil && len(platformImageNames(state)) > 0 {
			rc = sm.reswarmConfig()
		}
	}
	return WantedAppImages(rc, others), true
}

// removeSupersededProdImage removes, after an update, the tags of the version
// the app ran before (PresentVersion, under the name it was recorded under,
// the platform's name and the agent's own), but not keep (the image the
// update installed: a name of the old and the new version can be one) and
// none that another app's or stage's requested state still wants. Only the
// tag is removed, never forced: an image with another tag keeps it, and one a
// container still uses stays. When the other requested states cannot be read,
// nothing is removed. Best effort; the diskguard reclaims what is left.
func (sm *StateMachine) removeSupersededProdImage(payload common.TransitionPayload, keep string) {
	payload = sm.withInstalledImage(payload)
	superseded := prodImageCandidates(sm.reswarmConfigFor(payload), payload, payload.PresentVersion)
	if len(superseded) == 0 {
		return
	}

	wanted, ok := sm.wantedByOthers(payload)
	if !ok {
		return
	}

	for _, ref := range superseded {
		if ref == keep || wanted[ref] {
			log.Debug().Str("app", payload.AppName).Str("image", ref).Msg("keeping a superseded tag that is still wanted")
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
		err := sm.Container.RemoveImage(ctx, ref, map[string]interface{}{"force": false, "pruneChildren": true})
		cancel()
		if err != nil {
			if !errdefs.IsImageNotFound(err) {
				log.Debug().Err(err).Str("image", ref).Msg("could not remove the superseded image")
			}
			continue
		}
		log.Debug().Str("image", ref).Msg("removed the superseded image")
	}
}

// removeProdImageRepositories removes, with options, every tag of the app's
// single PROD image (prodImageRepositories) that no other app's or stage's
// requested state still wants: a repository nobody else wants a tag of goes
// whole, as before; in one that holds a wanted tag, the others go tag by tag.
// When the requested states cannot be read, nothing is removed. The first
// error is returned after all repositories were tried.
func (sm *StateMachine) removeProdImageRepositories(payload common.TransitionPayload, options map[string]interface{}) error {
	payload = sm.withInstalledImage(payload)
	wanted, ok := sm.wantedByOthers(payload)
	if !ok {
		return nil
	}

	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, repository := range prodImageRepositories(sm.reswarmConfigFor(payload), payload) {
		wantedHere := false
		for tag := range wanted {
			if imageRepository(tag) == repository {
				wantedHere = true
				break
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
		if !wantedHere {
			keep(sm.Container.RemoveImagesByName(ctx, repository, options))
			cancel()
			continue
		}

		images, err := sm.Container.GetImages(ctx, repository)
		cancel()
		if err != nil {
			keep(err)
			continue
		}
		for _, image := range images {
			for _, tag := range image.RepoTags {
				if imageRepository(tag) != repository || wanted[tag] {
					continue
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
				err := sm.Container.RemoveImage(ctx, tag, options)
				cancel()
				if err != nil && !errdefs.IsImageNotFound(err) {
					keep(err)
				}
			}
		}
	}
	return firstErr
}
