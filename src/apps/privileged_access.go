package apps

import (
	"context"
	"fmt"
	"reagent/common"
	"reagent/container"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// privilegedLabel records on every container of a compose app whether it was
// started with privileged access ("true"/"false"), so a running app can be
// compared with the grant later. A container without it was started before
// the grant existed, with the settings as authored: it counts as "true".
const privilegedLabel = "io.ironflock.privileged"

// grantsPrivileged reports whether a release runs with privileged access. nil
// comes from a backend that predates the grant and keeps the behaviour from
// before it: privileged.
func grantsPrivileged(privileged *bool) bool {
	return privileged == nil || *privileged
}

// setComposePrivilegedLabel stamps the privileged access a compose service is
// started with onto its containers. An authored label of the same name is
// replaced: the label has to say what the agent did, or a change of the grant
// would go unnoticed (or be noticed on every sync).
func setComposePrivilegedLabel(service map[string]interface{}, privileged bool) {
	value := strconv.FormatBool(privileged)

	switch labels := service["labels"].(type) {
	case map[string]interface{}:
		labels[privilegedLabel] = value
	case []interface{}:
		kept := make([]interface{}, 0, len(labels)+1)
		for _, entry := range labels {
			if label, ok := entry.(string); ok && (label == privilegedLabel || strings.HasPrefix(label, privilegedLabel+"=")) {
				continue
			}
			kept = append(kept, entry)
		}
		service["labels"] = append(kept, privilegedLabel+"="+value)
	default:
		service["labels"] = map[string]interface{}{privilegedLabel: value}
	}
}

// unprivilegedStartNotice returns the app-log lines for an app that starts
// without privileged access: for a compose app one line per service (and one
// for the top-level settings) whose privileged settings are stripped, for a
// single-container app one line. Nothing when the access is granted.
func unprivilegedStartNotice(payload common.TransitionPayload) []string {
	if grantsPrivileged(payload.Privileged) {
		return nil
	}

	// A DEV run follows the developer's own declaration; a release needs the
	// declaration and the project's grant.
	prefix := "started without privileged access (the release does not request it, or the project has not granted it)"
	if payload.Stage == common.DEV {
		prefix = "started without privileged access (the development build does not declare `privileged` with a `reason` in .ironflock/data-template.yml)"
	}

	if payload.DockerCompose == nil {
		return []string{prefix + ": no host devices, no extra capabilities and no host D-Bus"}
	}

	// The features come grouped by service, the top-level ones last.
	features := classifyComposePrivileges(payload.DockerCompose)
	var lines []string
	var removed []string
	for i, feature := range features {
		item := feature.Feature
		if feature.Feature != "privileged" || feature.Value != "true" {
			item += " " + feature.Value
		}
		removed = append(removed, item)

		if i+1 < len(features) && features[i+1].Service == feature.Service {
			continue
		}

		from := "from service " + feature.Service
		if feature.Service == "" {
			from = "from the top-level settings"
		}
		lines = append(lines, fmt.Sprintf("%s: removed %s %s", prefix, strings.Join(removed, ", "), from))
		removed = nil
	}

	return lines
}

// writeUnprivilegedStartNotice writes unprivilegedStartNotice to the app's
// log, so the user learns why a device or host path the app expects is
// missing.
func (sm *StateMachine) writeUnprivilegedStartNotice(payload common.TransitionPayload, logTopic string) {
	for _, line := range unprivilegedStartNotice(payload) {
		err := sm.LogManager.Write(logTopic, line)
		if err != nil {
			log.Debug().Err(err).Msg("failed to write the privileged access notice to the app log")
		}
	}
}

// privilegedAccessChanged reports whether a running app was started with
// other privileged access than the payload grants now: a single-container app
// by its container's HostConfig.Privileged, a compose app by privilegedLabel
// on the running containers of its services. Whatever it cannot decide (no
// container, a Docker error) counts as unchanged: a wrong "changed" would
// restart the app on every sync.
func (sm *StateMachine) privilegedAccessChanged(payload common.TransitionPayload, app *common.App) bool {
	granted := grantsPrivileged(payload.Privileged)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	if payload.DockerCompose != nil {
		project := common.BuildComposeContainerName(payload.Stage, app.AppKey, app.AppName)
		containers, err := container.ListComposeProjectContainers(ctx, sm.Container, project)
		if err != nil {
			log.Debug().Err(err).Str("app", payload.AppName).Msg("Could not list the compose containers to compare the privileged access")
			return false
		}

		// Containers of services the definition no longer has are orphans a
		// restart does not replace; comparing them would restart forever.
		services, _ := payload.DockerCompose["services"].(map[string]interface{})
		for _, cont := range containers {
			if cont.State != "running" {
				continue
			}
			if _, ok := services[cont.Labels[container.ComposeServiceLabel]]; !ok {
				continue
			}
			if (cont.Labels[privilegedLabel] != "false") != granted {
				return true
			}
		}
		return false
	}

	containerName := payload.ContainerName.Prod
	if payload.Stage == common.DEV {
		containerName = payload.ContainerName.Dev
	}

	privileged, err := sm.Container.GetContainerPrivileged(ctx, containerName)
	if err != nil {
		log.Debug().Err(err).Str("container", containerName).Msg("Could not inspect the container to compare the privileged access")
		return false
	}
	return privileged != granted
}

// restartOnPrivilegedAccessChange is the RUNNING -> RUNNING transition. It has
// nothing to do, unless the project granted or revoked privileged access since
// the app started: then the app is stopped and started again, so the change
// takes effect within one sync.
func (sm *StateMachine) restartOnPrivilegedAccessChange(payload common.TransitionPayload, app *common.App) error {
	if !sm.privilegedAccessChanged(payload, app) {
		return sm.noActionTransitionFunc(payload, app)
	}

	logTopic := payload.ContainerName.Prod
	if payload.Stage == common.DEV {
		logTopic = payload.ContainerName.Dev
	}

	change := "revoked"
	if grantsPrivileged(payload.Privileged) {
		change = "granted"
	}
	log.Info().Str("app", payload.AppName).Str("stage", string(payload.Stage)).Msgf("Privileged access was %s; restarting the app", change)
	err := sm.LogManager.Write(logTopic, fmt.Sprintf("Privileged access was %s; restarting %s", change, payload.AppName))
	if err != nil {
		log.Debug().Err(err).Msg("failed to write the privileged access restart notice to the app log")
	}

	err = sm.stopApp(payload, app)
	if err != nil {
		return err
	}

	return sm.runApp(payload, app)
}
