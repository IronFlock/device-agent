package container

import (
	"bufio"
	"context"
	"io"
	"net"
	"reagent/common"
	"reagent/config"
	"time"

	dockerbuild "github.com/docker/docker/api/types/build"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/network"
)

// ContainerResult generic result for the container method ListContainers
type ContainerResult struct { // More fields can be added if needed, needs to be as generic as possible in case we want to use other container implementations
	ID       string
	Names    []string
	ImageID  string
	Labels   map[string]string
	Status   string
	State    string
	ExitCode int64
	Command  string
}

type AuthConfig struct {
	Username string
	Password string
}

// ImageResult generic result for the container method ListImages
type ImageResult struct {
	Created     int64             `json:"created,omitempty"`
	Containers  int64             `json:"containers,omitempty"`
	SharedSize  int64             `json:"sharedSize,omitempty"`
	VirtualSize int64             `json:"virtualSize,omitempty"`
	ID          string            `json:"id,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Size        int64             `json:"size,omitempty"`
	RepoTags    []string          `json:"repoTags,omitempty"`
}

type TtyDimension struct {
	Height uint
	Width  uint
}

type HijackedResponse struct {
	Conn   net.Conn
	Reader *bufio.Reader
	ExecID string
}

type ContainerState struct {
	Status     string // String representation of the container state. Can be one of "created", "running", "paused", "restarting", "removing", "exited", or "dead"
	Running    bool
	Paused     bool
	Restarting bool
	OOMKilled  bool
	Dead       bool
	Pid        int
	ExitCode   int
	Error      string
	StartedAt  string
	FinishedAt string
}

// Container generic interface for a Container API
type Container interface {
	Login(ctx context.Context, serverAddress string, username string, password string) (string, error)
	HandleRegistryLogins(credentials map[string]common.DockerCredential) error
	ResizeExecContainer(ctx context.Context, execID string, dimension TtyDimension) error
	Build(ctx context.Context, pathToTar string, options dockerbuild.ImageBuildOptions) (io.ReadCloser, error)
	GetContainerState(ctx context.Context, containerName string) (ContainerState, error)
	GetContainerPortBindings(ctx context.Context, containerName string) (map[string]uint64, error)
	GetComposePublishedPorts(ctx context.Context, projectName string) (map[string]uint64, error)
	GetContainerNetworkMode(ctx context.Context, containerName string) (string, error)
	ListenForContainerEvents(ctx context.Context) (<-chan events.Message, <-chan error)
	GetContainer(ctx context.Context, containerName string) (container.Summary, error)
	GetContainers(ctx context.Context) ([]container.Summary, error)
	Logs(ctx context.Context, containerName string, options common.Dict) (io.ReadCloser, error)
	ExecCommand(ctx context.Context, containerName string, cmd []string) (HijackedResponse, error)
	ExecAttach(ctx context.Context, containerName string, shell string) (HijackedResponse, error)
	Attach(ctx context.Context, containerName string, shell string) (HijackedResponse, error)
	StopContainerByID(ctx context.Context, containerID string, timeout time.Duration) error
	StopContainerByName(ctx context.Context, containerName string, timeout time.Duration) error
	RemoveContainerByName(ctx context.Context, containerName string, options map[string]interface{}) error
	RemoveContainerByID(ctx context.Context, containerID string, options map[string]interface{}) error
	Tag(ctx context.Context, source string, target string) error
	Pull(ctx context.Context, imageName string, options PullOptions) (io.ReadCloser, error)
	Push(ctx context.Context, imageName string, pushOptions PushOptions) (io.ReadCloser, error)
	CreateContainer(ctx context.Context, cConfig container.Config, hConfig container.HostConfig, nConfig network.NetworkingConfig, containerName string) (string, error)
	WaitForContainerByID(ctx context.Context, containerID string, condition container.WaitCondition) (int64, error)
	WaitForContainerByName(ctx context.Context, containerID string, condition container.WaitCondition) (int64, error)
	WaitForRunning(ctx context.Context, containerID string, pollingRate time.Duration) (<-chan struct{}, <-chan error)
	PollContainerState(ctx context.Context, containerID string, pollingRate time.Duration) (<-chan ContainerState, <-chan error)
	StartContainer(ctx context.Context, containerID string) error
	GetImage(ctx context.Context, fullImageName string, tag string) (ImageResult, error)
	GetImages(ctx context.Context, fullImageName string) ([]ImageResult, error)
	RemoveImage(ctx context.Context, imageID string, options map[string]interface{}) error
	RemoveImageByName(ctx context.Context, imageName string, tag string, options map[string]interface{}) error
	RemoveImagesByName(ctx context.Context, imageName string, options map[string]interface{}) error
	PruneImages(ctx context.Context, options common.Dict) error
	Compose() *Compose
	PruneSystem() (string, error)
	PruneAllImages() (string, error)
	PruneDanglingImages(ctx context.Context) (string, error)
	PruneBuildCache(ctx context.Context) (string, error)
	ListDanglingVolumes(ctx context.Context) ([]VolumeResult, error)
	RemoveVolume(ctx context.Context, name string) error
	ListImages(ctx context.Context, options map[string]interface{}) ([]ImageResult, error)
	ListContainers(ctx context.Context, options common.Dict) ([]ContainerResult, error)
	WaitForDaemon(retryTimeout ...time.Duration) error
	Ping(ctx context.Context) (Ping, error)
	CancelStream(cancelID string) error
	CancelAllStreams() error
	GetConfig() *config.Config
}
