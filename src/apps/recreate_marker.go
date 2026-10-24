package apps

// The recreate marker: after the Windows layout migration moved the agent
// folder (%ProgramData%\IronFlock\Reagent -> DeviceAgent), every app
// container still bind-mounts the old folder as its /data and /shared. The
// migration leaves <agentDir>\recreate-containers behind, and the agent
// consumes it once, at its start, once Docker is reachable and before the
// first app starts: the containers of single-container apps are removed, and
// each one is recreated by its app's next start with the new folder — and
// with the volumes the old container had (an image's VOLUME, such as a
// database's data directory, is an anonymous volume: a plain recreate would
// hand the app a fresh, empty one). Compose apps are recreated by compose:
// the rewritten absolute paths change the config hash.
//
// A comparison of the containers' bind sources with the folder is not
// used: the engine reports its own view of them (/run/desktop/mnt/host/c/…
// under Docker Desktop, /mnt/c/… under a WSL engine), which would never
// match and recreate every container at every start.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reagent/common"
	"reagent/errdefs"
	"reagent/winservice"
	"strings"
	"time"

	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/rs/zerolog/log"
)

// recreateMarker is the marker's content once the agent has looked at it:
// the containers it removed, each with the volumes its new container must
// carry. The migration writes the file empty.
type recreateMarker struct {
	// Listed: the old containers were listed and removed; the entries wait
	// for their apps' next start. Never listed again (a container made
	// after the move must not be removed).
	Listed     bool                      `json:"listed"`
	Containers map[string]*recreateEntry `json:"containers"`
}

type recreateEntry struct {
	Volumes   []recordedVolume `json:"volumes"`
	Recreated bool             `json:"recreated,omitempty"`
}

type recordedVolume struct {
	Name   string `json:"name"`
	Target string `json:"target"`
}

func recreateMarkerPath(agentDir string) string {
	return filepath.Join(agentDir, winservice.RecreateMarkerName)
}

// readRecreateMarker reads the marker; exists is false without one. An
// empty file is the migration's untouched marker.
func readRecreateMarker(path string) (marker *recreateMarker, exists bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	marker = &recreateMarker{}
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, marker); err != nil {
			return nil, true, fmt.Errorf("unreadable %s: %w", path, err)
		}
	}
	if marker.Containers == nil {
		marker.Containers = map[string]*recreateEntry{}
	}
	return marker, true, nil
}

// syncMarkerFile flushes the written marker to the disk before it is renamed
// into place: NTFS journals the rename, not the data, and an entry recorded
// here is the only record of a removed container's volumes (a test counts
// its calls).
var syncMarkerFile = (*os.File).Sync

// recreateRetries and recreateRetryDelay: a listing or removal that fails
// right after the daemon answered (a transient error) is tried again before
// the apps start on their stale containers.
var (
	recreateRetries    = 3
	recreateRetryDelay = 10 * time.Second
)

func writeRecreateMarker(path string, marker *recreateMarker) error {
	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644) //nolint:gosec // G302/G304: container names and volume names, no secrets
	if err != nil {
		return err
	}
	_, err = out.Write(data)
	if err == nil {
		err = syncMarkerFile(out)
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// done: every removed container was recreated; the marker has served.
func (m *recreateMarker) done() bool {
	if !m.Listed {
		return false
	}
	for _, entry := range m.Containers {
		if !entry.Recreated {
			return false
		}
	}
	return true
}

// RecreateContainersAfterMove consumes the recreate marker of agentDir, if
// there is one: the containers of single-container apps are removed (their
// volumes recorded first, so a crash in between loses no data), and the
// marker keeps their entries until createContainer has recreated each one.
// It runs before the first app start of a process, with Docker reachable; a
// transient Docker error is tried again a few times (the apps would
// otherwise start on their stale containers until the next start); an error
// that stays (Docker, the file) keeps the marker for the next start.
func (am *AppManager) RecreateContainersAfterMove(agentDir string) error {
	path := recreateMarkerPath(agentDir)
	marker, exists, err := readRecreateMarker(path)
	if err != nil || !exists {
		return err
	}
	if marker.Listed {
		return finishRecreateMarker(path, marker)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for attempt := 1; ; attempt++ {
		err = am.removeContainersToRecreate(ctx, path, marker)
		if err == nil || attempt >= recreateRetries {
			return err
		}
		log.Warn().Err(err).Int("attempt", attempt).Msg("recreating the app containers after the folder move; trying again")
		select {
		case <-ctx.Done():
			return err
		case <-time.After(recreateRetryDelay):
		}
	}
}

// removeContainersToRecreate lists the containers once and removes the
// single-container apps' ones, recording each in the marker first; then the
// marker says listed.
func (am *AppManager) removeContainersToRecreate(ctx context.Context, path string, marker *recreateMarker) error {
	containers, err := am.StateMachine.Container.GetContainers(ctx)
	if err != nil {
		return fmt.Errorf("listing the containers to recreate after the folder move: %w", err)
	}
	am.StateMachine.recreateMu.Lock()
	defer am.StateMachine.recreateMu.Unlock()
	for _, c := range containers {
		if c.Labels[composeProjectLabel] != "" {
			continue // compose recreates its own
		}
		name := summaryName(c)
		if _, _, _, err := common.ParseContainerName(name); err != nil {
			continue // not an app container
		}
		entry := &recreateEntry{}
		for _, m := range c.Mounts {
			if m.Type == mount.TypeVolume && m.Name != "" {
				entry.Volumes = append(entry.Volumes, recordedVolume{Name: m.Name, Target: m.Destination})
			}
		}
		// Recorded before the removal: the container is gone for good, its
		// volumes must not be.
		marker.Containers[name] = entry
		if err := writeRecreateMarker(path, marker); err != nil {
			return err
		}
		log.Info().Str("container", name).Int("volumes", len(entry.Volumes)).Msg("removing the app container to recreate it from the moved folder")
		if err := am.StateMachine.Container.RemoveContainerByID(ctx, c.ID, map[string]interface{}{"force": true}); err != nil && !errdefs.IsContainerNotFound(err) {
			return fmt.Errorf("removing %s to recreate it after the folder move: %w", name, err)
		}
		if _, err := am.StateMachine.Container.WaitForContainerByID(ctx, c.ID, dockercontainer.WaitConditionRemoved); err != nil && !errdefs.IsContainerNotFound(err) {
			return fmt.Errorf("waiting for the removal of %s: %w", name, err)
		}
	}
	marker.Listed = true
	if err := writeRecreateMarker(path, marker); err != nil {
		return err
	}
	return finishRecreateMarker(path, marker)
}

// finishRecreateMarker removes the marker once every entry was recreated.
func finishRecreateMarker(path string, marker *recreateMarker) error {
	if !marker.done() {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	log.Info().Msg("every app container was recreated from the moved folder")
	return nil
}

func summaryName(c dockercontainer.Summary) string {
	if len(c.Names) == 0 {
		return c.ID
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

// carryRecordedVolumes gives a container that is recreated after the
// folder move the volumes its old container had: the recorded volume is
// mounted at the same target, unless the host config already mounts
// something there.
func (sm *StateMachine) carryRecordedVolumes(containerName string, hConfig *dockercontainer.HostConfig) {
	marker, entry := sm.recreateEntry(containerName)
	if marker == nil || entry == nil {
		return
	}
	for _, v := range entry.Volumes {
		if hasMountTarget(hConfig.Mounts, v.Target) {
			continue
		}
		hConfig.Mounts = append(hConfig.Mounts, mount.Mount{Type: mount.TypeVolume, Source: v.Name, Target: v.Target})
	}
	if len(entry.Volumes) > 0 {
		log.Info().Str("container", containerName).Int("volumes", len(entry.Volumes)).Msg("recreating the container with the volumes of the one before the folder move")
	}
}

// markRecreatedAfterRead is a test seam between the read and the write of
// the marker (nil in production).
var markRecreatedAfterRead func(containerName string)

// markRecreated records that containerName was created (with its recorded
// volumes, or found again); the marker goes once every entry is. The apps
// start in parallel: the read-modify-write of the marker is serialised.
func (sm *StateMachine) markRecreated(containerName string) {
	sm.recreateMu.Lock()
	defer sm.recreateMu.Unlock()
	marker, entry := sm.recreateEntry(containerName)
	if marker == nil || entry == nil {
		return
	}
	if markRecreatedAfterRead != nil {
		markRecreatedAfterRead(containerName)
	}
	entry.Recreated = true
	path := recreateMarkerPath(sm.Container.GetConfig().CommandLineArguments.AgentDir)
	if err := writeRecreateMarker(path, marker); err != nil {
		log.Warn().Err(err).Msg("could not update the recreate marker")
		return
	}
	if err := finishRecreateMarker(path, marker); err != nil {
		log.Warn().Err(err).Msg("could not remove the recreate marker")
	}
}

// recreateEntry is containerName's pending entry of the marker, nil without.
func (sm *StateMachine) recreateEntry(containerName string) (*recreateMarker, *recreateEntry) {
	cfg := sm.Container.GetConfig()
	if cfg == nil || cfg.CommandLineArguments == nil {
		return nil, nil
	}
	marker, exists, err := readRecreateMarker(recreateMarkerPath(cfg.CommandLineArguments.AgentDir))
	if err != nil || !exists {
		return nil, nil
	}
	entry := marker.Containers[containerName]
	if entry == nil || entry.Recreated {
		return nil, nil
	}
	return marker, entry
}

func hasMountTarget(mounts []mount.Mount, target string) bool {
	for _, m := range mounts {
		if m.Target == target {
			return true
		}
	}
	return false
}
