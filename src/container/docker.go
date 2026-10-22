package container

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	cerrdefs "github.com/containerd/errdefs"

	dockerbuild "github.com/docker/docker/api/types/build"

	"github.com/docker/docker/api/types/image"

	"io"
	"os"
	"os/exec"
	"reagent/common"
	"reagent/config"
	"reagent/errdefs"
	"reagent/safe"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/rs/zerolog/log"
)

// Docker container implentation using the Docker API

type DockerPull struct {
	BuildID string
	Stream  io.ReadCloser
}
type DockerStream struct {
	StreamID string
	Stream   io.ReadCloser
}

type Docker struct {
	client         *client.Client
	config         *config.Config
	compose        *Compose
	paths          *enginePaths
	activeStreams  map[string]*DockerStream
	streamMapMutex sync.Mutex
}

func NewDocker(config *config.Config) (*Docker, error) {
	client, err := newDockerClient()
	activeBuilds := make(map[string]*DockerStream)

	if err != nil {
		return nil, err
	}

	paths := newEnginePaths(runtime.GOOS, client.Info)
	compose := NewCompose(config)
	compose.paths = paths
	return &Docker{client: client, config: config, activeStreams: activeBuilds, compose: compose, paths: paths}, nil
}

// For now stick only with Docker as implementation
func newDockerClient() (*client.Client, error) {
	return client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
}

func (docker *Docker) ListenForContainerEvents(ctx context.Context) (<-chan events.Message, <-chan error) {
	eventFilters := filters.NewArgs()
	eventFilters.Add("type", "container")

	return docker.client.Events(ctx, events.ListOptions{Filters: eventFilters})
}

// TODO: simplifiy option parsing
//
// ListContainers lists containers on the host
func (docker *Docker) ListContainers(ctx context.Context, options common.Dict) ([]ContainerResult, error) {
	listOptions := container.ListOptions{}

	if options != nil {
		allKw := options["all"]
		sizeKw := options["size"]
		latestKw := options["latest"]
		sinceKw := options["since"]
		beforeKw := options["before"]
		limitKw := options["limit"]
		filtersKw := options["filters"]

		all, ok := allKw.(bool)
		if ok {
			listOptions.All = all
		}
		size, ok := sizeKw.(bool)
		if ok {
			listOptions.Size = size
		}
		latest, ok := latestKw.(bool)
		if ok {
			listOptions.Latest = latest
		}
		since, ok := sinceKw.(string)
		if ok {
			listOptions.Since = since
		}
		before, ok := beforeKw.(string)
		if ok {
			listOptions.Before = before
		}
		limit, ok := limitKw.(int)
		if ok {
			listOptions.Limit = limit
		}
		filters, ok := filtersKw.(filters.Args)
		if ok {
			listOptions.Filters = filters
		}
	}

	cList, err := docker.client.ContainerList(ctx, listOptions)
	if err != nil {
		return nil, err
	}

	listOfDict := make([]ContainerResult, 0)
	for _, cont := range cList {

		exitCode := int64(-1)
		if cont.State == "exited" {
			exitCode, err = common.ParseExitCodeFromContainerStatus(cont.Status)
			if err != nil {
				return nil, err
			}
		}

		dict := ContainerResult{
			ID:      cont.ID,
			Names:   cont.Names,
			ImageID: cont.ImageID,
			Command: cont.Command,
			Labels:  cont.Labels,
			State:   cont.State,
			Status:  cont.Status,
		}

		if exitCode != -1 {
			dict.ExitCode = exitCode
		}

		listOfDict = append(listOfDict, dict)
	}

	return listOfDict, nil
}

// GetComposePublishedPorts returns the host ports currently published by the
// containers of a compose project, keyed like PublishedPortKey
// (<service>|<targetPort>/<protocol>). It reads them straight from the Docker
// API — never shell out `docker compose ps` for this: it runs on the
// transition's critical section, and on boot (when a restarted agent recovers
// its ports) dockerd is at its most contended, where a compose-CLI invocation
// can stall the whole transition for tens of seconds. An empty map when the
// project has no published ports (e.g. it is not up).
func (docker *Docker) GetComposePublishedPorts(ctx context.Context, projectName string) (map[string]uint64, error) {
	project := common.NormalizeComposeProjectName(projectName)
	cList, err := docker.client.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.Arg("label", "com.docker.compose.project="+project)),
	})
	if err != nil {
		return nil, err
	}

	published := make(map[string]uint64)
	for _, cont := range cList {
		service := cont.Labels["com.docker.compose.service"]
		for _, port := range cont.Ports {
			// A stopped container has no host binding (PublicPort == 0); only a
			// live mapping is worth recovering.
			if port.PublicPort == 0 {
				continue
			}
			key := PublishedPortKey(service, uint64(port.PrivatePort), port.Type)
			published[key] = uint64(port.PublicPort)
		}
	}

	return published, nil
}

func (docker *Docker) GetContainer(ctx context.Context, containerName string) (container.Summary, error) {
	filters := filters.NewArgs()
	filters.Add("name", containerName)
	containers, err := docker.client.ContainerList(ctx, container.ListOptions{All: true, Filters: filters})
	if err != nil {
		return container.Summary{}, err
	}

	if len(containers) > 0 {
		return containers[0], nil
	}

	return container.Summary{}, errdefs.ContainerNotFound(errors.New("container not found"))
}

func (docker *Docker) RemoveContainerByName(ctx context.Context, containerName string, options map[string]interface{}) error {
	container, err := docker.GetContainer(ctx, containerName)

	if err != nil {
		return err
	}

	return docker.RemoveContainerByID(ctx, container.ID, options)
}

func (docker *Docker) RemoveContainerByID(ctx context.Context, containerID string, options map[string]interface{}) error {
	optionStruct := container.RemoveOptions{}

	removeVolumesKw := options["removeVolumes"]
	removeLinksKw := options["removeLinks"]
	forceKw := options["force"]

	if removeVolumesKw != nil {
		removeVolumes, ok := removeVolumesKw.(bool)
		if ok {
			optionStruct.RemoveVolumes = removeVolumes
		}
	}

	if removeLinksKw != nil {
		removeLinks, ok := removeLinksKw.(bool)
		if ok {
			optionStruct.RemoveLinks = removeLinks
		}
	}

	if forceKw != nil {
		force, ok := forceKw.(bool)
		if ok {
			optionStruct.Force = force
		}
	}

	err := docker.client.ContainerRemove(ctx, containerID, optionStruct)
	if err != nil {
		if strings.Contains(err.Error(), "No such container") {
			return errdefs.ContainerNotFound(err)
		}
		if strings.Contains(err.Error(), "is already in progress") {
			return errdefs.ContainerRemovalAlreadyInProgress(err)
		}
		return err
	}

	return nil
}

func (docker *Docker) StopContainerByID(ctx context.Context, containerID string, timeout time.Duration) error {
	timeoutInSeconds := int(timeout.Seconds())
	err := docker.client.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &timeoutInSeconds})
	if err != nil && strings.Contains(err.Error(), "No such container") {
		// Same classification as RemoveContainerByID: a container that is
		// already gone is "stopped" for every caller that tolerates absence.
		return errdefs.ContainerNotFound(err)
	}
	return err
}

func (docker *Docker) StopContainerByName(ctx context.Context, containerName string, timeout time.Duration) error {
	c, err := docker.GetContainer(ctx, containerName)
	if err != nil {
		return err
	}

	timeoutInSeconds := int(timeout.Seconds())
	return docker.client.ContainerStop(ctx, c.ID, container.StopOptions{Timeout: &timeoutInSeconds})
}

func (docker *Docker) GetImages(ctx context.Context, fullImageName string) ([]ImageResult, error) {
	options := image.ListOptions{}

	if fullImageName != "" {
		filters := filters.NewArgs()
		filters.Add("reference", fullImageName)
		options.Filters = filters
	} else {
		options.All = true
	}

	imagesResult, err := docker.client.ImageList(ctx, options)
	if err != nil {
		if strings.Contains(err.Error(), "No such image") {
			return []ImageResult{}, errdefs.ImageNotFound(err)
		}
		return []ImageResult{}, err
	}

	if len(imagesResult) == 0 {
		return []ImageResult{}, nil
	}

	var images []ImageResult
	for i := range imagesResult {
		image := imagesResult[i]

		images = append(images, ImageResult{
			Created:     image.Created,
			Containers:  image.Containers,
			SharedSize:  image.SharedSize,
			VirtualSize: image.Size, // Docker API >= 1.44 no longer sends VirtualSize; Size is the same total
			ID:          image.ID,
			Labels:      image.Labels,
			Size:        image.Size,
			RepoTags:    image.RepoTags,
		})
	}

	return images, nil
}

func (docker *Docker) GetImage(ctx context.Context, fullImageName string, tag string) (ImageResult, error) {

	filters := filters.NewArgs()
	fullImageNameWithTag := fmt.Sprintf("%s:%s", fullImageName, tag)
	filters.Add("reference", fullImageNameWithTag)

	options := image.ListOptions{Filters: filters}

	images, err := docker.client.ImageList(ctx, options)
	if err != nil {
		if strings.Contains(err.Error(), "No such image") {
			return ImageResult{}, errdefs.ImageNotFound(err)
		}
		return ImageResult{}, err
	}

	if len(images) == 0 {
		return ImageResult{}, errdefs.ImageNotFound(fmt.Errorf("no image found with name: %s:%s", fullImageName, tag))
	}

	image := images[0]

	return ImageResult{
		Created:     image.Created,
		Containers:  image.Containers,
		SharedSize:  image.SharedSize,
		VirtualSize: image.Size, // Docker API >= 1.44 no longer sends VirtualSize; Size is the same total
		ID:          image.ID,
		Labels:      image.Labels,
		Size:        image.Size,
		RepoTags:    image.RepoTags,
	}, nil
}

func (docker *Docker) PruneImages(ctx context.Context, options common.Dict) error {
	filters := filters.NewArgs()

	if options["all"] != nil {
		all, ok := options["all"].(bool)
		if !ok {
			return errors.New("all value for container prune is not a boolean")
		}
		if all {
			filters.Add("dangling", "false")
		}
	}

	_, err := docker.client.ImagesPrune(ctx, filters)
	return err
}

func (docker *Docker) PruneSystem() (string, error) {
	cmd := exec.Command("docker", "system", "prune", "-af", "--volumes")
	cmd.Stderr = cmd.Stdout
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}

	return string(output), nil
}

func (docker *Docker) PruneAllImages() (string, error) {
	cmd := exec.Command("docker", "image", "prune", "-af")
	cmd.Stderr = cmd.Stdout
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}

	return string(output), nil
}

// PruneDanglingImages removes dangling images. ctx bounds the run — on a
// disk-full box the daemon can wedge and an unbounded prune then hangs its
// caller forever (observed live: it kept diskguard from ever reporting the
// emergency).
func (docker *Docker) PruneDanglingImages(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", "image", "prune", "-f")
	cmd.Stderr = cmd.Stdout
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}

	return string(output), nil
}

// VolumeResult is the subset of a Docker volume the callers need to decide
// whether it is safe to remove.
type VolumeResult struct {
	Name   string
	Labels map[string]string
}

// ListDanglingVolumes lists volumes not referenced by ANY container, running
// or stopped.
func (docker *Docker) ListDanglingVolumes(ctx context.Context) ([]VolumeResult, error) {
	args := filters.NewArgs()
	args.Add("dangling", "true")

	resp, err := docker.client.VolumeList(ctx, volume.ListOptions{Filters: args})
	if err != nil {
		return nil, err
	}

	results := make([]VolumeResult, 0, len(resp.Volumes))
	for _, v := range resp.Volumes {
		if v == nil {
			continue
		}
		results = append(results, VolumeResult{Name: v.Name, Labels: v.Labels})
	}
	return results, nil
}

// RemoveVolume removes a volume by name. Not forced: a volume a container
// attached to since it was listed errors instead of being deleted.
func (docker *Docker) RemoveVolume(ctx context.Context, name string) error {
	return docker.client.VolumeRemove(ctx, name, false)
}

// PruneBuildCache removes the dangling build cache (docker builder prune).
// ctx bounds the run (see PruneDanglingImages).
func (docker *Docker) PruneBuildCache(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", "builder", "prune", "-f")
	cmd.Stderr = cmd.Stdout
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}

	return string(output), nil
}

// ListImages lists all images available on the host.
func (docker *Docker) ListImages(ctx context.Context, options map[string]interface{}) ([]ImageResult, error) {
	allKw := options["all"]

	rOptions := image.ListOptions{}

	if allKw != nil {
		all, ok := allKw.(bool)
		if !ok {
			return nil, fmt.Errorf("invalid type for 'all' option")
		}
		rOptions.All = all
	}

	imageList, err := docker.client.ImageList(ctx, rOptions)
	if err != nil {
		return nil, err
	}

	imageResults := make([]ImageResult, 0)
	for _, image := range imageList {
		imageResult := ImageResult{
			Created:     image.Created,
			Containers:  image.Containers,
			SharedSize:  image.SharedSize,
			VirtualSize: image.Size, // Docker API >= 1.44 no longer sends VirtualSize; Size is the same total
			ID:          image.ID,
			Labels:      image.Labels,
			Size:        image.Size,
			RepoTags:    image.RepoTags,
		}
		imageResults = append(imageResults, imageResult)
	}

	return imageResults, nil
}

// Login runs `docker login` with the password on stdin: as an argument (-p)
// it would be readable by every local user (ps, /proc/<pid>/cmdline) while
// the command runs.
func (c *Docker) Login(ctx context.Context, dockerRegistryURL string, username string, password string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", "login", dockerRegistryURL, "-u", username, "--password-stdin") //nolint:gosec // G204: fixed docker CLI, registry and user from the platform's credentials
	cmd.Stdin = strings.NewReader(password)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

// A login that exceeds this deadline is SIGKILLed by CommandContext. A healthy
// login (local or via a working proxy) finishes in well under a second; hitting
// this deadline means the connection is being black-holed (e.g. a corporate
// proxy swallowing traffic to the registry because NO_PROXY is missing/stale).
const registryLoginTimeout = time.Second * 10

func (c *Docker) HandleRegistryLogins(credentials map[string]common.DockerCredential) error {
	for registryURL, credential := range credentials {
		ctx, cancelLogin := context.WithTimeout(context.Background(), registryLoginTimeout)
		output, err := c.Login(ctx, registryURL, credential.Username, credential.Password)
		if err != nil {
			cancelLogin()

			// A killed-by-deadline login surfaces as the bare "signal: killed",
			// which is useless in the device logs; name the actual failure.
			if ctx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("registry login to %s timed out after %s: the registry is not responding (check that the registry is reachable and, on proxied hosts, that NO_PROXY covers it)", registryURL, registryLoginTimeout)
			}

			// Prefer the CLI's own error line. Match case-insensitively: Docker
			// 29 lowercased messages that used to start with "Error" (notably
			// "error saving credentials: ..."), and matching only the capital
			// form silently degraded such failures to a bare "exit status 1".
			scanner := bufio.NewScanner(strings.NewReader(string(output)))
			for scanner.Scan() {
				line := scanner.Text()
				if strings.Contains(strings.ToLower(line), "error") {
					return errors.New(line)
				}
			}

			// No recognizable error line: never drop the output — an exec
			// error on its own ("exit status 1") is undiagnosable.
			trimmedOutput := strings.TrimSpace(string(output))
			if trimmedOutput != "" {
				return fmt.Errorf("registry login to %s failed: %w: %s", registryURL, err, trimmedOutput)
			}

			return fmt.Errorf("registry login to %s failed: %w", registryURL, err)
		}

		cancelLogin()

		log.Debug().Msgf("Logged into Docker Registry: %s", registryURL)
	}

	return nil
}

func (docker *Docker) GetConfig() *config.Config {
	return docker.config
}

// DataRootDir returns the daemon's data-root directory (default /var/lib/docker),
// as reported by the daemon itself. On FlockOS devices the data-root is moved to
// the apps partition (e.g. /apps/docker), so callers that watch disk usage must
// not assume the default path.
func (docker *Docker) DataRootDir(ctx context.Context) (string, error) {
	info, err := docker.client.Info(ctx)
	if err != nil {
		return "", err
	}
	return info.DockerRootDir, nil
}

type PullOptions struct {
	AuthConfig AuthConfig
	PullID     string
}

type PushOptions struct {
	AuthConfig AuthConfig
	PushID     string
}

// Pull pulls a container image from a registry
func (docker *Docker) Pull(ctx context.Context, imageName string, options PullOptions) (io.ReadCloser, error) {
	dockerAuthConfig := AuthConfig{
		Username: options.AuthConfig.Username,
		Password: options.AuthConfig.Password,
	}

	encodedJSON, err := json.Marshal(dockerAuthConfig) //nolint:gosec // G117: registry auth header for the Docker API, never written to disk or logs
	if err != nil {
		return nil, err
	}
	authStr := base64.URLEncoding.EncodeToString(encodedJSON)

	reader, err := docker.client.ImagePull(ctx, imageName, image.PullOptions{RegistryAuth: authStr})
	if err != nil {
		if reader != nil {
			_ = reader.Close()
		}

		return nil, err
	}

	pullID := options.PullID
	pullStream := DockerStream{
		Stream:   reader,
		StreamID: pullID,
	}

	if pullID != "" {
		docker.streamMapMutex.Lock()
		docker.activeStreams[pullID] = &pullStream
		docker.streamMapMutex.Unlock()
	}

	return reader, nil
}

// Push pushes a container image to a registry
func (docker *Docker) Push(ctx context.Context, imageName string, options PushOptions) (io.ReadCloser, error) {
	dockerAuthConfig := AuthConfig{
		Username: options.AuthConfig.Username,
		Password: options.AuthConfig.Password,
	}

	encodedJSON, err := json.Marshal(dockerAuthConfig) //nolint:gosec // G117: registry auth header for the Docker API, never written to disk or logs
	if err != nil {
		return nil, err
	}

	authStr := base64.URLEncoding.EncodeToString(encodedJSON)
	pushOptions := image.PushOptions{
		RegistryAuth: authStr,
	}

	reader, err := docker.client.ImagePush(ctx, imageName, pushOptions)
	if err != nil {
		if reader != nil {
			_ = reader.Close()
		}

		return nil, err
	}

	pushID := options.PushID
	pushStream := DockerStream{
		Stream:   reader,
		StreamID: pushID,
	}

	if pushID != "" {
		docker.streamMapMutex.Lock()
		docker.activeStreams[pushID] = &pushStream
		docker.streamMapMutex.Unlock()
	}

	return reader, nil
}

// logsOptionsFromDict maps the agent's option vocabulary onto the Docker SDK's.
//
// An unrecognised type for a known key is ignored rather than rejected, which
// is the behaviour every call site has always relied on. `tail` is the one
// exception worth calling out: it accepts a string ("50", "all") or a number,
// because nexus hands a WAMP integer over as uint64 and the previous
// string-only assertion silently fell through to Docker's "all" default.
func logsOptionsFromDict(options common.Dict) container.LogsOptions {
	containerOptions := container.LogsOptions{}

	if stdout, ok := options["stdout"].(bool); ok {
		containerOptions.ShowStdout = stdout
	}

	if stderr, ok := options["stderr"].(bool); ok {
		containerOptions.ShowStderr = stderr
	}

	if follow, ok := options["follow"].(bool); ok {
		containerOptions.Follow = follow
	}

	if tailKw := options["tail"]; tailKw != nil {
		if tail, ok := tailKw.(string); ok {
			containerOptions.Tail = tail
		} else if tail, ok := common.ToUint64(tailKw); ok {
			containerOptions.Tail = strconv.FormatUint(tail, 10)
		}
	}

	// Since/Until pass through verbatim. The daemon parses RFC3339 and Unix
	// seconds; relative durations are a CLI-side convenience and are resolved by
	// the caller (common.ParseLogTime) before they reach here.
	if since, ok := options["since"].(string); ok {
		containerOptions.Since = since
	}

	if until, ok := options["until"].(string); ok {
		containerOptions.Until = until
	}

	if timestamps, ok := options["timestamps"].(bool); ok {
		containerOptions.Timestamps = timestamps
	}

	return containerOptions
}

// Logs reads a container's logs.
//
// Recognised option keys: stdout, stderr, follow, tail, since, until,
// timestamps. The last three exist because Docker's json-file driver is the
// only store on the device that stamps every record with a time — the agent's
// own in-memory ring and its SQLite LogHistory blob keep bare strings — so a
// time-windowed log query can only be answered here.
func (docker *Docker) Logs(ctx context.Context, containerName string, options common.Dict) (io.ReadCloser, error) {
	containerOptions := logsOptionsFromDict(options)

	reader, err := docker.client.ContainerLogs(ctx, containerName, containerOptions)
	if err != nil {
		if reader != nil {
			_ = reader.Close()
		}

		if strings.Contains(err.Error(), "No such container") {
			return nil, errdefs.ContainerNotFound(err)
		}
		if strings.Contains(err.Error(), "container which is dead or marked for removal") {
			return nil, errdefs.ContainerNotFound(err)
		}
		return nil, err
	}

	return reader, nil
}

func (docker *Docker) WaitForContainerByName(ctx context.Context, containerName string, condition container.WaitCondition) (int64, error) {
	container, err := docker.GetContainer(ctx, containerName)
	if err != nil {
		return -1, err
	}

	statusChan, errChan := docker.client.ContainerWait(ctx, container.ID, condition)
	select {
	case err := <-errChan:
		if strings.Contains(err.Error(), "No such container") {
			return -1, errdefs.ContainerNotFound(err)
		}
		return -1, err
	case status := <-statusChan:
		return status.StatusCode, nil
	}
}

func (docker *Docker) WaitForContainerByID(ctx context.Context, containerID string, condition container.WaitCondition) (int64, error) {
	statusChan, errChan := docker.client.ContainerWait(ctx, containerID, condition)
	select {
	case err := <-errChan:
		if strings.Contains(err.Error(), "No such container") {
			return -1, errdefs.ContainerNotFound(err)
		}
		return -1, err
	case status := <-statusChan:
		return status.StatusCode, nil
	}
}

func (docker *Docker) Attach(ctx context.Context, containerName string, shell string) (HijackedResponse, error) {
	attachOpts := container.AttachOptions{
		Stream: true,
		Stdout: true,
		Stderr: true,
		Logs:   false,
		Stdin:  true,
	}

	response, err := docker.client.ContainerAttach(ctx, containerName, attachOpts)
	if err != nil {
		return HijackedResponse{}, err
	}

	return HijackedResponse{
		Conn:   response.Conn,
		Reader: response.Reader,
	}, nil
}

func (docker *Docker) ResizeExecContainer(ctx context.Context, execID string, dimension TtyDimension) error {
	return docker.client.ContainerExecResize(ctx, execID, container.ResizeOptions{
		Height: dimension.Height,
		Width:  dimension.Width,
	})
}

func (docker *Docker) ExecAttach(ctx context.Context, containerName string, shell string) (HijackedResponse, error) {
	execConfig := container.ExecOptions{
		AttachStderr: true,
		AttachStdout: true,
		AttachStdin:  true,
		Tty:          true,
		Cmd:          []string{shell},
	}
	execOptions := container.ExecAttachOptions{
		Tty: true,
	}

	container, err := docker.GetContainer(ctx, containerName)
	if err != nil {
		return HijackedResponse{}, err
	}

	respID, err := docker.client.ContainerExecCreate(ctx, container.ID, execConfig)
	if err != nil {
		return HijackedResponse{}, err
	}

	response, err := docker.client.ContainerExecAttach(ctx, respID.ID, execOptions)
	if err != nil {
		return HijackedResponse{}, err
	}

	return HijackedResponse{
		Conn:   response.Conn,
		Reader: response.Reader,
		ExecID: respID.ID,
	}, nil
}

func (docker *Docker) ExecCommand(ctx context.Context, containerName string, cmd []string) (HijackedResponse, error) {
	execConfig := container.ExecOptions{
		AttachStderr: true,
		AttachStdout: true,
		Tty:          false,
		Cmd:          cmd,
	}
	execOptions := container.ExecAttachOptions{}

	container, err := docker.GetContainer(ctx, containerName)
	if err != nil {
		return HijackedResponse{}, err
	}

	respID, err := docker.client.ContainerExecCreate(ctx, container.ID, execConfig)
	if err != nil {
		return HijackedResponse{}, err
	}

	response, err := docker.client.ContainerExecAttach(ctx, respID.ID, execOptions)
	if err != nil {
		return HijackedResponse{}, err
	}

	return HijackedResponse{
		Conn:   response.Conn,
		Reader: response.Reader,
		ExecID: respID.ID,
	}, nil
}

func (docker *Docker) CreateContainer(ctx context.Context,
	cConfig container.Config,
	hConfig container.HostConfig,
	nConfig network.NetworkingConfig,
	containerName string) (string, error) {

	// A Linux engine outside Docker Desktop cannot open Windows paths (engine_paths.go).
	if docker.paths.wslDriveMounts(ctx) {
		hConfig.Mounts = wslDriveMountsFor(hConfig.Mounts)
	}

	resp, err := docker.client.ContainerCreate(ctx, &cConfig, &hConfig, &nConfig, nil, containerName)
	if err != nil {
		if strings.Contains(err.Error(), "is already in use by container") {
			return "", errdefs.ContainerNameAlreadyInUse(err)
		}

		if strings.Contains(err.Error(), "No such image") {
			return "", errdefs.ImageNotFound(err)
		}

		return "", err
	}

	return resp.ID, nil
}

func (docker *Docker) GetContainers(ctx context.Context) ([]container.Summary, error) {
	options := container.ListOptions{All: true}
	return docker.client.ContainerList(ctx, options)
}

func (docker *Docker) GetContainerState(ctx context.Context, containerName string) (ContainerState, error) {
	res, err := docker.client.ContainerInspect(ctx, containerName)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return ContainerState{}, errdefs.ContainerNotFound(err)
		}
		return ContainerState{}, err
	}

	state := res.State
	return ContainerState{
		Status:     state.Status,
		Running:    state.Running,
		Paused:     state.Paused,
		Restarting: state.Restarting,
		OOMKilled:  state.OOMKilled,
		Dead:       state.Dead,
		Pid:        state.Pid,
		ExitCode:   state.ExitCode,
		Error:      state.Error,
		StartedAt:  state.StartedAt,
		FinishedAt: state.FinishedAt,
	}, nil
}

// GetContainerPortBindings returns the container's configured host port
// bindings keyed by "<containerPort>/<proto>" (e.g. "80/tcp"). It reads
// HostConfig.PortBindings — the creation-time config — which, unlike
// NetworkSettings.Ports, is also populated for stopped containers. A binding
// on all interfaces is preferred when a port is bound to several host IPs.
// Host-networked containers have no bindings and yield an empty map.
func (docker *Docker) GetContainerPortBindings(ctx context.Context, containerName string) (map[string]uint64, error) {
	res, err := docker.client.ContainerInspect(ctx, containerName)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil, errdefs.ContainerNotFound(err)
		}
		return nil, err
	}

	bindings := make(map[string]uint64)
	if res.HostConfig == nil {
		return bindings, nil
	}

	for containerPort, hostBindings := range res.HostConfig.PortBindings {
		for _, hostBinding := range hostBindings {
			hostPort, err := strconv.ParseUint(hostBinding.HostPort, 10, 64)
			if err != nil || hostPort == 0 {
				continue
			}

			_, seen := bindings[string(containerPort)]
			allInterfaces := hostBinding.HostIP == "" || hostBinding.HostIP == "0.0.0.0"
			if !seen || allInterfaces {
				bindings[string(containerPort)] = hostPort
			}
		}
	}

	return bindings, nil
}

// GetContainerNetworkMode returns the container's HostConfig.NetworkMode
// (e.g. "host", "bridge", "default").
func (docker *Docker) GetContainerNetworkMode(ctx context.Context, containerName string) (string, error) {
	res, err := docker.client.ContainerInspect(ctx, containerName)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return "", errdefs.ContainerNotFound(err)
		}
		return "", err
	}

	if res.HostConfig == nil {
		return "", nil
	}

	return string(res.HostConfig.NetworkMode), nil
}

func (docker *Docker) PollContainerState(ctx context.Context, containerID string, pollingRate time.Duration) (<-chan ContainerState, <-chan error) {
	errC := make(chan error, 1)
	stateC := make(chan ContainerState, 1)

	safe.Go(func() {
		for {
			state, err := docker.GetContainerState(ctx, containerID)
			if err != nil {
				if strings.Contains(err.Error(), "no such container") {
					err = errdefs.ContainerNotFound(err)
				}

				errC <- err
				close(errC)
				close(stateC)
				return
			}

			stateC <- state
			time.Sleep(pollingRate)
		}
	})

	return stateC, errC
}

// WaitForRunning will poll a container's status at a given interval until the running state is achieved.
// If the container fails to start with an 'exited' or 'dead' status, it will throw an error
func (docker *Docker) WaitForRunning(ctx context.Context, containerID string, pollingRate time.Duration) (<-chan struct{}, <-chan error) {
	errC := make(chan error, 1)
	runningC := make(chan struct{}, 1)

	safe.Go(func() {
		for {
			state, err := docker.GetContainerState(ctx, containerID)
			if err != nil {
				errC <- err
				close(errC)
				close(runningC)
				return
			}

			if state.Running {
				runningC <- struct{}{}
				close(errC)
				close(runningC)
				return
			}

			if state.Status == "exited" || state.Status == "dead" {
				errC <- errors.New("the container has exited")
				close(errC)
				close(runningC)
				return
			}

			if state.Error != "" {
				errC <- errors.New(state.Error)
				close(errC)
				close(runningC)
				return
			}

			time.Sleep(pollingRate)
		}
	})

	return runningC, errC
}

// TODO: make more generic
//
// StartContainer creates and starts a specific container
func (docker *Docker) StartContainer(ctx context.Context, containerID string) error {
	if err := docker.client.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return err
	}

	return nil
}

func (docker *Docker) RemoveImagesByName(ctx context.Context, imageName string, options map[string]interface{}) error {
	filters := filters.NewArgs()
	filters.Add("reference", imageName)

	images, err := docker.client.ImageList(ctx, image.ListOptions{Filters: filters})
	if err != nil {
		return err
	}

	for _, image := range images {
		for _, repoTag := range image.RepoTags {
			if repoTag != "" {
				err := docker.RemoveImage(ctx, repoTag, options)
				if err != nil {
					// Listed a moment ago but gone now — that IS the goal.
					if errdefs.IsImageNotFound(err) {
						continue
					}
					return err
				}
			}
		}
	}

	return nil
}

func (docker *Docker) RemoveImageByName(ctx context.Context, imageName string, tag string, options map[string]interface{}) error {
	filters := filters.NewArgs()

	filters.Add("reference", fmt.Sprintf("%s:%s", imageName, tag))

	images, err := docker.client.ImageList(ctx, image.ListOptions{Filters: filters})
	if err != nil {
		return err
	}

	if len(images) == 0 {
		return fmt.Errorf("no image was found with name: %s:%s", imageName, tag)
	}

	if len(images) > 1 {
		return fmt.Errorf("multiple images were found with name: %s:%s", imageName, tag)
	}

	image := images[0]

	return docker.RemoveImage(ctx, image.ID, options)
}

// RemoveImage removes an image from the host
func (docker *Docker) RemoveImage(ctx context.Context, imageID string, options map[string]interface{}) error {
	forceKw := options["force"]
	pruneChildrenKw := options["pruneChildren"]

	rOptions := image.RemoveOptions{}
	if forceKw != nil {
		force, ok := forceKw.(bool)
		if !ok {
			return fmt.Errorf("invalid type for 'force' option")
		}
		rOptions.Force = force
	}

	if pruneChildrenKw != nil {
		pruneChildren, ok := pruneChildrenKw.(bool)
		if !ok {
			return fmt.Errorf("invalid type for 'pruneChilderen' option")
		}
		rOptions.PruneChildren = pruneChildren
	}

	_, err := docker.client.ImageRemove(ctx, imageID, rOptions)
	if err != nil {
		// Typed so removal flows can treat an already-absent image as done:
		// an app whose images never arrived (interrupted transfer, incomplete
		// store sync) must still be removable — its uninstall used to abort
		// on this 404 and strand the app in a FAILED retry loop.
		if strings.Contains(err.Error(), "No such image") {
			return errdefs.ImageNotFound(err)
		}
		return err
	}

	return nil
}

type Ping struct {
	APIVersion     string
	OSType         string
	Experimental   bool
	BuilderVersion string
}

func (docker *Docker) WaitForDaemon(retryTimeoutParam ...time.Duration) error {
	var retryTimeout time.Duration

	if len(retryTimeoutParam) == 0 {
		retryTimeout = time.Second * 5
	} else {
		retryTimeout = retryTimeoutParam[0]
	}

	timeoutTimer := time.Now()
	for {
		_, err := docker.Ping(context.Background())
		if err != nil {
			log.Debug().Msg("Ping to Docker Daemon failed, retrying...")
			time.Sleep(time.Millisecond * 100)
		} else {
			break
		}

		if time.Since(timeoutTimer) > retryTimeout {
			return errors.New("maxium retry timeout for docker daemon ping was exceeded")
		}
	}

	return nil
}

func (docker *Docker) Ping(ctx context.Context) (Ping, error) {
	pingRes, err := docker.client.Ping(ctx)
	if err != nil {
		return Ping{}, err
	}

	return Ping{
		APIVersion:     pingRes.APIVersion,
		OSType:         pingRes.OSType,
		Experimental:   pingRes.Experimental,
		BuilderVersion: string(pingRes.BuilderVersion),
	}, nil
}

func (docker *Docker) CancelStream(streamID string) error {
	docker.streamMapMutex.Lock()
	activeStreamEntry := docker.activeStreams[streamID]
	docker.streamMapMutex.Unlock()

	if activeStreamEntry == nil {
		return errors.New("no active stream was found")
	}

	stream := activeStreamEntry.Stream
	if stream != nil {
		err := stream.Close()
		if err != nil {
			return err
		}
	}

	return nil
}

func (docker *Docker) CancelAllStreams() error {
	docker.streamMapMutex.Lock()
	defer docker.streamMapMutex.Unlock()

	for _, stream := range docker.activeStreams {
		if stream != nil {
			err := stream.Stream.Close()
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (docker *Docker) Tag(ctx context.Context, source string, target string) error {
	return docker.client.ImageTag(ctx, source, target)
}

func (docker *Docker) Compose() *Compose {
	return docker.compose
}

// Build builds a Docker image using a tarfile as context
func (docker *Docker) Build(ctx context.Context, compressedBuildFilesPath string, options dockerbuild.ImageBuildOptions) (io.ReadCloser, error) {
	dockerBuildContext, err := os.Open(compressedBuildFilesPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errdefs.DockerBuildFilesNotFound(err)
		}
		return nil, err
	}

	buildResponse, err := docker.client.ImageBuild(ctx, dockerBuildContext, options)
	if err != nil {
		if buildResponse.Body != nil {
			_ = buildResponse.Body.Close()
		}

		if strings.Contains(err.Error(), "the Dockerfile (Dockerfile) cannot be empty") {
			return nil, errdefs.DockerfileCannotBeEmpty(err)
		}
		if strings.Contains(err.Error(), "Cannot locate specified Dockerfile: Dockerfile") {
			return nil, errdefs.DockerfileIsMissing(err)
		}
		return nil, err
	}

	reader := buildResponse.Body
	if options.BuildID != "" {
		docker.streamMapMutex.Lock()
		docker.activeStreams[options.BuildID] = &DockerStream{
			StreamID: options.BuildID,
			Stream:   reader,
		}
		docker.streamMapMutex.Unlock()
	}

	return reader, nil
}
