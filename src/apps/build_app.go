package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reagent/common"
	"reagent/config"
	"reagent/container"
	"reagent/errdefs"
	"reagent/filesystem"
	"reagent/release"
	"reagent/tunnel"
	"strings"

	dockerbuild "github.com/docker/docker/api/types/build"

	"github.com/rs/zerolog/log"
)

func (sm *StateMachine) buildApp(payload common.TransitionPayload, app *common.App) error {
	if payload.Stage != common.DEV {
		return errors.New("can only build dev apps")
	}

	return sm.buildDevApp(payload, app, false)
}

// isValidDotEnvKey reports whether key can safely serve as a variable name in
// the .env-compose file. docker compose's dotenv parser rejects the WHOLE file
// when a key contains whitespace ("key cannot contain a space"), an '=' inside
// the name would truncate it, and a leading '#' silently turns the line into a
// comment.
func isValidDotEnvKey(key string) bool {
	if key == "" || strings.HasPrefix(key, "#") {
		return false
	}
	return !strings.ContainsAny(key, " \t\r\n=")
}

// filterValidDotEnvLines splits KEY=VALUE lines into the ones safe to write to
// the .env-compose file and the names of the ones that would break docker
// compose's dotenv parsing (which aborts `compose up` for the whole app).
func filterValidDotEnvLines(envLines []string) (valid []string, skippedKeys []string) {
	for _, line := range envLines {
		key, _, found := strings.Cut(line, "=")
		// compose trims whitespace around the key itself, so a padded but
		// otherwise valid name still parses.
		key = strings.TrimSpace(key)
		if !found || !isValidDotEnvKey(key) {
			if key == "" {
				key = strings.TrimSpace(line)
			}
			skippedKeys = append(skippedKeys, key)
			continue
		}
		valid = append(valid, line)
	}
	return valid, skippedKeys
}

// dockerCompose is the authored compose definition the app is (re)started
// from; it identifies the port entries whose managed host port assignments are
// announced as DEVICE_PORT_FOR_<port>. Callers must have recorded those
// assignments first (rewriteComposeHostPorts runs before this).
func (sm *StateMachine) generateDotEnvContents(config *config.Config, payload common.TransitionPayload, app *common.App, dockerCompose map[string]interface{}) (string, []string, error) {
	var envLines []string

	systemDefaultVariables := buildDefaultEnvironmentVariables(config, payload, app.Stage, app, sm.AppCredKey())
	environmentVariables := buildProdEnvironmentVariables(systemDefaultVariables, payload.EnvironmentVariables)
	environmentTemplateDefaults := common.EnvironmentTemplateToStringArray(payload.EnvironmentTemplate)

	if payload.Stage == common.DEV {
		envLines = append(systemDefaultVariables, environmentTemplateDefaults...)
	} else {
		var missingDefaultEnvs []string
		for _, templateEnvString := range environmentTemplateDefaults {
			envStringSplit := strings.Split(templateEnvString, "=")
			environmentName := envStringSplit[0]

			found := false
			for _, envVariableString := range environmentVariables {
				if strings.Contains(envVariableString, environmentName) {
					found = true
				}
			}

			if !found {
				missingDefaultEnvs = append(missingDefaultEnvs, templateEnvString)
			}
		}

		portRules, err := tunnel.InterfaceToPortForwardRule(payload.Ports)
		if err != nil {
			return "", nil, err
		}

		remotePortEnvs := sm.remotePortEnvVars(config, app.AppName, app.Stage, portRules)

		envLines = append(environmentVariables, append(remotePortEnvs, missingDefaultEnvs...)...)
	}

	envLines = append(envLines, sm.StateObserver.AppManager.devicePortEnvsForCompose(payload, dockerCompose)...)

	validLines, skippedKeys := filterValidDotEnvLines(envLines)
	return strings.Join(validLines, "\n"), skippedKeys, nil
}

const DockerFileName = "docker-compose.json"

// DotEnvFileName lives in the container package: compose invocations locate
// the file next to the compose file to pass it as --env-file.
const DotEnvFileName = container.DotEnvFileName

// deepCopyCompose clones a compose definition through a JSON round trip (the
// maps only ever hold JSON-decoded values).
func deepCopyCompose(dockerCompose map[string]interface{}) (map[string]interface{}, error) {
	encoded, err := json.Marshal(dockerCompose)
	if err != nil {
		return nil, err
	}

	var copied map[string]interface{}
	err = json.Unmarshal(encoded, &copied)
	if err != nil {
		return nil, err
	}

	return copied, nil
}

func (sm *StateMachine) SetupComposeFiles(payload common.TransitionPayload, app *common.App, updatingApp bool) (string, error) {
	config := sm.Container.GetConfig()

	isProd := payload.Stage == common.PROD
	targetDir := config.CommandLineArguments.AppsBuildDir
	if isProd {
		targetDir = config.CommandLineArguments.AppsComposeDir
	}

	targetAppDir := targetDir + "/" + app.AppName
	dockerComposeFilePath := targetAppDir + "/" + DockerFileName
	dotEnvFilePath := targetAppDir + "/" + DotEnvFileName

	// The privileged access belongs to the release whose definition is
	// rendered: the newest one for an update, the present one otherwise.
	sourceCompose := payload.DockerCompose
	sourcePrivileged := payload.Privileged
	if payload.NewDockerCompose != nil && updatingApp {
		sourceCompose = payload.NewDockerCompose
		sourcePrivileged = payload.NewPrivileged
	}
	privileged := grantsPrivileged(sourcePrivileged)

	// Work on a copy: the payload map holds the authored definition, and the
	// host-port rewrite below must not leak into it — rewriting an already
	// rewritten definition would remap the managed ports again.
	dockerCompose, err := deepCopyCompose(sourceCompose)
	if err != nil {
		return "", err
	}

	// Without privileged access the app's privileged settings go, before the
	// agent adds its own binds below (those are not the app's to lose). The
	// start writes what was removed to the app log
	// (writeUnprivilegedStartNotice).
	if !privileged {
		stripComposePrivileges(dockerCompose)
	}

	dockerCompose["name"] = common.BuildComposeContainerName(payload.Stage, app.AppKey, app.AppName)

	// The release's own store images name a repository we host; which HOST
	// serves it is this device's business, not the publisher's. Resolve it
	// before anything reads the file — see resolveComposeStoreImages.
	if resolved := resolveComposeStoreImages(config.ReswarmConfig, app.AppKey, app.AppName, dockerCompose); resolved > 0 {
		log.Debug().Msgf("resolved %d store image reference(s) of %s onto %s", resolved, app.AppName, config.ReswarmConfig.DockerRegistryURL)
	}

	// An installed release runs exactly the images it was released with — see
	// pinComposeImages. A DEV build makes its own images and pins nothing.
	if isProd {
		if _, err := pinComposeImages(dockerCompose); err != nil {
			return "", err
		}
	}

	services, ok := (dockerCompose["services"]).(map[string]interface{})
	if !ok {
		return "", errors.New("failed to infer services")
	}

	envFilesHostDir := appEnvFilesHostDir(config, payload.Stage, app.AppName)
	caBundleHostDir := appCABundleHostDir(config)

	for _, serviceInterface := range services {
		service, ok := (serviceInterface).(map[string]interface{})
		if !ok {
			return "", errors.New("failed to infer service")
		}

		service["env_file"] = DotEnvFileName
		setComposePrivilegedLabel(service, privileged)
		addComposeExtraHost(service)
		addComposeEnvFilesMount(service, envFilesHostDir)
		addComposeCABundleMount(service, caBundleHostDir)
	}

	err = sm.rewriteComposeHostPorts(payload, dockerCompose)
	if err != nil {
		return "", err
	}

	dockerComposeJSONString, err := json.Marshal(dockerCompose)
	if err != nil {
		return "", err
	}

	_, err = os.Stat(targetAppDir)
	if err != nil {
		err = os.MkdirAll(targetAppDir, os.ModePerm)
		if err != nil {
			return "", err
		}
	}

	dotEnvFileContents, skippedEnvKeys, err := sm.generateDotEnvContents(config, payload, app, sourceCompose)
	if err != nil {
		return "", err
	}

	if len(skippedEnvKeys) > 0 {
		topic := payload.ContainerName.Dev
		if isProd {
			topic = payload.ContainerName.Prod
		}
		message := fmt.Sprintf(
			"Warning: ignored environment variable(s) with invalid name(s): %q — names cannot be empty, contain spaces or '=', or start with '#'",
			skippedEnvKeys,
		)
		writeErr := sm.LogManager.Write(topic, message)
		if writeErr != nil {
			log.Debug().Msgf("failed to write invalid env name warning: %s", writeErr)
		}
	}

	// 0600: the .env carries the app's parameters, including values marked
	// secret; only docker compose (root) reads it. WriteFile keeps the mode of
	// an existing file, hence the explicit chmod for files from older agents.
	err = os.WriteFile(dotEnvFilePath, []byte(dotEnvFileContents), 0600)
	if err == nil {
		err = os.Chmod(dotEnvFilePath, 0600)
	}
	if err != nil {
		return "", err
	}

	// Mirror the env vars as files in the /data/env mount injected above, like
	// single-container apps get: the start-time snapshot the live refresh
	// (refreshRemotePortEnvFiles) later updates in place.
	//
	// NOT during an update: the update renders the NEW release's files while
	// the OLD containers are still running (pull-before-teardown), and this
	// mirror is a live bind mount those containers read file-first — writing it
	// here would inject the new release's env values into the old app, and a
	// refused update would leave them there for the whole outage. The mirror is
	// (re)written from the promoted definition when the app next starts
	// (runApp -> SetupComposeFiles with updatingApp=false); only the
	// .env-compose file above, which no container mounts, is needed for the
	// update's `compose pull` interpolation.
	if !updatingApp {
		err = writeEnvironmentVariablesToFiles(filepath.Dir(envFilesHostDir), strings.Split(dotEnvFileContents, "\n"))
		if err != nil {
			return "", fmt.Errorf("failed to write environment variables to files: %w", err)
		}
	}

	err = os.WriteFile(dockerComposeFilePath, dockerComposeJSONString, os.ModePerm) //nolint:gosec // G306: the app developer's compose file plus an env_file reference; parameter values live in the 0600 .env
	if err != nil {
		return "", err
	}

	return dockerComposeFilePath, nil
}

func (sm *StateMachine) buildDevComposeApp(payload common.TransitionPayload, app *common.App, releaseBuild bool) error {
	err := sm.LogManager.ClearLogHistory(payload.ContainerName.Dev)
	if err != nil {
		return err
	}

	err = sm.setState(app, common.REMOVED)
	if err != nil {
		return err
	}

	config := sm.Container.GetConfig()
	buildsDir := config.CommandLineArguments.AppsBuildDir
	fileName := payload.AppName + "." + config.CommandLineArguments.CompressedBuildExtension
	appFilesTar := buildsDir + "/" + fileName
	targetAppDir := buildsDir + "/" + app.AppName

	_, err = os.Stat(targetAppDir)
	if err == nil {
		err := os.RemoveAll(targetAppDir)
		if err != nil {
			return err
		}
	}

	err = filesystem.ExtractTarGz(appFilesTar, targetAppDir)
	if err != nil {
		return fmt.Errorf("could not unpack the app: %w", err)
	}

	app.ReleaseBuild = releaseBuild
	topicForLogStream := payload.ContainerName.Dev
	if releaseBuild {
		topicForLogStream = payload.PublishContainerName
	}

	err = sm.LogManager.Write(topicForLogStream, "Starting image build...")
	if err != nil {
		return err
	}

	err = sm.setState(app, common.BUILDING)
	if err != nil {
		return err
	}

	compose := sm.Container.Compose()
	if !compose.Supported() {
		return errdefs.DockerComposeNotSupported(errors.New("docker compose is not supported"))
	}

	err = sm.HandleRegistryLoginsWithDefault(payload)
	if err != nil {
		return err
	}

	dockerComposePath, err := sm.SetupComposeFiles(payload, app, false)
	if err != nil {
		return err
	}

	buildID := common.BuildDockerBuildID(app.AppKey, app.AppName)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	compose.RegisterBuildCancel(buildID, cancel)
	defer compose.UnregisterBuildCancel(buildID)

	buildOutput, buildCmd, err := compose.Build(ctx, dockerComposePath)
	if err != nil {
		return err
	}

	_, err = sm.LogManager.StreamLogsChannel(buildOutput, topicForLogStream)
	if err != nil {
		return err
	}

	err = buildCmd.Wait()
	if err != nil {
		return err
	}

	if !releaseBuild {
		pullOutput, pullCmd, err := compose.PullIgnoreBuildable(dockerComposePath)
		if err != nil {
			return err
		}

		_, err = sm.LogManager.StreamLogsChannel(pullOutput, topicForLogStream)
		if err != nil {
			return err
		}

		err = pullCmd.Wait()
		if err != nil {
			return err
		}
	}

	buildMessage := "Compose Image built successfully"
	err = sm.LogManager.Write(topicForLogStream, buildMessage)
	if err != nil {
		return err
	}

	return sm.setState(app, common.BUILT)
}

// buildDevApp builds a DEV app, or the image a publish pushes (releaseBuild),
// and writes why a build failed to the build console the developer watches.
// Without this, a failure before the image build began (unpacking the app,
// registry login, the compose file) showed only "File transfer has finished,
// starting build..." followed by FAILED, with no reason anywhere but the
// agent's journal. The error is returned unchanged: callers classify it with
// errdefs type assertions.
func (sm *StateMachine) buildDevApp(payload common.TransitionPayload, app *common.App, releaseBuild bool) error {
	err := sm.runDevBuild(payload, app, releaseBuild)
	if err == nil || errdefs.IsDockerStreamCanceled(err) {
		// a canceled build writes its own notice and is not a failure
		return err
	}

	topic := payload.ContainerName.Dev
	if releaseBuild {
		topic = payload.PublishContainerName
	}
	writeErr := sm.LogManager.Write(topic, buildFailureMessage(err))
	if writeErr != nil {
		log.Debug().Err(writeErr).Msg("failed to write the build failure to the app log")
	}

	return err
}

// buildFailureMessage is the build console's line for a failed build.
func buildFailureMessage(err error) string {
	reason := err.Error()
	switch {
	case errdefs.IsDockerfileCannotBeEmpty(err):
		reason = "the Dockerfile cannot be empty, please fill out your Dockerfile"
	case errdefs.IsDockerfileIsMissing(err):
		reason = "could not find a Dockerfile, please create a Dockerfile in the root of your project"
	case errdefs.IsDockerBuildFilesNotFound(err):
		reason = "build files for app not found: " + err.Error()
	case errdefs.IsDockerComposeNotSupported(err):
		reason = "Docker Compose is not supported on this device"
	}
	return "Build failed: " + reason
}

func (sm *StateMachine) runDevBuild(payload common.TransitionPayload, app *common.App, releaseBuild bool) error {
	if payload.DockerCompose != nil {
		return sm.buildDevComposeApp(payload, app, releaseBuild)
	}

	err := sm.LogManager.ClearLogHistory(payload.ContainerName.Dev)
	if err != nil {
		return err
	}

	err = sm.setState(app, common.REMOVED)
	if err != nil {
		return err
	}

	config := sm.Container.GetConfig()
	buildsDir := config.CommandLineArguments.AppsBuildDir
	fileName := payload.AppName + "." + config.CommandLineArguments.CompressedBuildExtension
	appFilesTar := buildsDir + "/" + fileName

	dockerFileName := "Dockerfile"
	archSpecificDockerfile := fmt.Sprintf("Dockerfile.%s", dockerfileArch(payload))
	_, err = filesystem.ReadFileInTgz(appFilesTar, archSpecificDockerfile)
	if err == nil {
		dockerFileName = archSpecificDockerfile
	}

	// need to specify that this is a release build on remote update
	// this ensures that the dev release will be set to exists = true
	// prod ready builds will not be set to exists until after they are pushed
	app.ReleaseBuild = releaseBuild
	buildOptions := dockerbuild.ImageBuildOptions{
		Tags:       []string{payload.RegistryImageName.Dev},
		Dockerfile: dockerFileName,
		BuildID:    common.BuildDockerBuildID(app.AppKey, app.AppName),
	}

	topicForLogStream := payload.ContainerName.Dev
	if releaseBuild {
		topicForLogStream = payload.PublishContainerName
	}

	err = sm.LogManager.Write(topicForLogStream, "Starting image build...")
	if err != nil {
		return err
	}

	err = sm.setState(app, common.BUILDING)
	if err != nil {
		return err
	}

	// TODO: figure out context for build?
	reader, err := sm.Container.Build(context.Background(), appFilesTar, buildOptions)
	if err != nil {
		return err
	}

	var buildMessage string
	streamErr := sm.LogManager.StreamBlocking(topicForLogStream, common.BUILD, reader)
	if streamErr != nil {
		if errdefs.IsDockerStreamCanceled(streamErr) {
			buildMessage = "The build stream was canceled"
			writeErr := sm.LogManager.Write(topicForLogStream, buildMessage)
			if writeErr != nil {
				return writeErr
			}
			// this error will not cause a failed state and is handled upstream
			return streamErr
		}

		return streamErr
	}

	buildMessage = "Image built successfully"
	err = sm.LogManager.Write(topicForLogStream, buildMessage)
	if err != nil {
		return err
	}

	return sm.setState(app, common.BUILT)
}

// dockerfileArch is the architecture whose Dockerfile.<arch> a build uses:
// the one the build is for, the device's architecture as the platform stores
// it (TransitionPayload.Architecture, the sync answer's architecture), where it
// is one apps are built for; else, from an older backend or for a value such
// as ARM, the one the agent was built for, as before. A release build is
// pushed and registered for the platform's value, and the registry refuses an
// image of another platform, so the engine that builds it does not decide:
// an armv7 agent on a 64-bit engine still builds Dockerfile.armv7 while the
// platform says armv7. An app without that file builds its Dockerfile.
func dockerfileArch(payload common.TransitionPayload) string {
	switch payload.Architecture {
	case "amd64", "arm64", "armv7", "armv6", "armv5":
		return payload.Architecture
	default:
		return release.GetBuildArch()
	}
}
