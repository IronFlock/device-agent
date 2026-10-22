package container

import (
	"context"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/system"
	"github.com/rs/zerolog/log"
)

// Windows host paths on a Linux engine.
//
// On Windows the agent names bind-mount sources with host paths
// (C:\ProgramData\IronFlock\Reagent\apps\...): the /data, /shared and /data/env
// mounts of every app, the CA bundle, and whatever a compose file binds
// relative to its own directory. Docker Desktop translates such paths for its
// Linux VM. A plain Linux engine in a WSL distro - the Docker Desktop
// replacement set up by setup-docker-wsl.ps1 - does not: it rejects them, and
// sees the Windows drives under the WSL automount root instead (/mnt/c/...).
// The Docker layer therefore rewrites drive-letter mount sources for such an
// engine, for the containers it creates itself and for the compose files it
// hands to the CLI. Everywhere else nothing changes.

// EngineComposeFileName is the compose file the CLI gets instead of
// docker-compose.json when mount sources had to be rewritten for the engine.
// It sits next to the original, so the env file and anything else the CLI
// resolves relative to the project directory still resolves the same.
const EngineComposeFileName = "docker-compose.engine.json"

// wslAutomountRoot is where a WSL distro mounts the Windows drives unless its
// wsl.conf moves them; setup-docker-wsl.ps1 keeps the default.
const wslAutomountRoot = "/mnt"

// needsWSLDrivePaths reports whether an engine reached from hostOS sees the
// Windows drives only as WSL mounts: a Windows agent talking to a Linux engine
// that is not Docker Desktop (which translates Windows paths itself).
func needsWSLDrivePaths(hostOS string, info system.Info) bool {
	return hostOS == "windows" && info.OSType == "linux" && info.OperatingSystem != "Docker Desktop"
}

// enginePaths remembers whether the engine needs Windows drive paths
// rewritten. It asks the engine once: the engine cannot change under a running
// agent, since DOCKER_HOST is fixed when the process starts.
type enginePaths struct {
	hostOS string
	info   func(ctx context.Context) (system.Info, error)

	mu        sync.Mutex
	known     bool
	wslDrives bool
}

func newEnginePaths(hostOS string, info func(ctx context.Context) (system.Info, error)) *enginePaths {
	return &enginePaths{hostOS: hostOS, info: info}
}

// wslDriveMounts reports whether mount sources must be rewritten for this
// engine. Agents not on Windows never ask the engine. Until the engine answers
// (daemon still starting), paths stay as they are and the next call asks again.
func (e *enginePaths) wslDriveMounts(ctx context.Context) bool {
	if e == nil || e.hostOS != "windows" {
		return false
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.known {
		return e.wslDrives
	}

	info, err := e.info(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("could not ask the Docker engine for its type; mount paths are used unchanged")
		return false
	}

	e.known = true
	e.wslDrives = needsWSLDrivePaths(e.hostOS, info)
	if e.wslDrives {
		log.Info().Msgf("Docker engine %q is a Linux engine outside Docker Desktop: Windows mount paths are translated to %s/<drive>", info.OperatingSystem, wslAutomountRoot)
	}
	return e.wslDrives
}

func hasDriveLetter(p string) bool {
	if len(p) < 2 || p[1] != ':' {
		return false
	}
	c := p[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// wslDrivePath maps a Windows drive-letter path to where a WSL distro sees it:
// C:\ProgramData\x -> /mnt/c/ProgramData/x. Both separators are accepted, since
// the agent joins some paths with "/". Anything else (Linux paths, UNC paths,
// names) comes back unchanged.
func wslDrivePath(p string) string {
	if !hasDriveLetter(p) {
		return p
	}
	rest := strings.ReplaceAll(p[2:], `\`, "/")
	if rest != "" && !strings.HasPrefix(rest, "/") {
		rest = "/" + rest
	}
	return wslAutomountRoot + "/" + strings.ToLower(p[:1]) + rest
}

// wslDriveMountsFor returns mounts with every Windows bind source rewritten
// for a WSL engine. The caller's slice is left untouched.
func wslDriveMountsFor(mounts []mount.Mount) []mount.Mount {
	if len(mounts) == 0 {
		return mounts
	}
	out := make([]mount.Mount, len(mounts))
	copy(out, mounts)
	for i := range out {
		if out[i].Type == mount.TypeBind {
			out[i].Source = wslDrivePath(out[i].Source)
		}
	}
	return out
}

// engineBindSource rewrites one compose bind source for a WSL engine: a
// Windows path directly, a relative one after resolving it against the compose
// file's directory (which the CLI would otherwise turn into a Windows path).
// Named volumes and paths inside the engine stay as they are.
func engineBindSource(src string, composeDir string) (string, bool) {
	switch {
	case hasDriveLetter(src):
		return wslDrivePath(src), true
	case src == "." || src == ".." ||
		strings.HasPrefix(src, "./") || strings.HasPrefix(src, "../") ||
		strings.HasPrefix(src, `.\`) || strings.HasPrefix(src, `..\`):
		dir := strings.ReplaceAll(composeDir, `\`, "/")
		joined := path.Join(dir, strings.ReplaceAll(src, `\`, "/"))
		return wslDrivePath(joined), true
	default:
		return src, false
	}
}

// engineShortVolume rewrites the host side of a short-syntax volume
// ("SRC:DST[:MODE]"). A bare "DST" (anonymous volume) has no host side.
func engineShortVolume(spec string, composeDir string) (string, bool) {
	start := 0
	if hasDriveLetter(spec) {
		start = 2
	}
	i := strings.Index(spec[start:], ":")
	if i < 0 {
		return spec, false
	}
	src, rest := spec[:start+i], spec[start+i:]
	newSrc, changed := engineBindSource(src, composeDir)
	if !changed {
		return spec, false
	}
	return newSrc + rest, true
}

// wslDriveCompose rewrites, in place, every host path in a compose definition
// that the engine itself has to open: service bind mounts (short and long
// syntax) and the files of top-level secrets and configs. Paths the CLI reads
// on the Windows side (env_file, build contexts, extends) stay untouched.
func wslDriveCompose(dockerCompose map[string]interface{}, composeDir string) bool {
	changed := false

	services, _ := dockerCompose["services"].(map[string]interface{})
	for _, serviceInterface := range services {
		service, ok := serviceInterface.(map[string]interface{})
		if !ok {
			continue
		}
		volumes, ok := service["volumes"].([]interface{})
		if !ok {
			continue
		}
		for i, volumeInterface := range volumes {
			switch volume := volumeInterface.(type) {
			case string:
				if rewritten, ok := engineShortVolume(volume, composeDir); ok {
					volumes[i] = rewritten
					changed = true
				}
			case map[string]interface{}:
				volumeType, _ := volume["type"].(string)
				source, _ := volume["source"].(string)
				if volumeType != "bind" || source == "" {
					continue
				}
				if rewritten, ok := engineBindSource(source, composeDir); ok {
					volume["source"] = rewritten
					changed = true
				}
			}
		}
	}

	for _, section := range []string{"secrets", "configs"} {
		entries, _ := dockerCompose[section].(map[string]interface{})
		for _, entryInterface := range entries {
			entry, ok := entryInterface.(map[string]interface{})
			if !ok {
				continue
			}
			file, _ := entry["file"].(string)
			if file == "" {
				continue
			}
			if rewritten, ok := engineBindSource(file, composeDir); ok {
				entry["file"] = rewritten
				changed = true
			}
		}
	}

	return changed
}

// engineComposeFile returns the compose file to hand the CLI: the original,
// or - when the engine needs WSL drive paths and the file binds Windows or
// relative paths - a rewritten copy next to it (written only when its content
// changed). On any error the original is used, so the engine reports the
// problem itself rather than the agent hiding the file.
func (c *Compose) engineComposeFile(ctx context.Context, dockerComposePath string) string {
	if !c.paths.wslDriveMounts(ctx) {
		return dockerComposePath
	}

	raw, err := os.ReadFile(dockerComposePath)
	if err != nil {
		log.Warn().Err(err).Msgf("could not read %s to translate its mount paths", dockerComposePath)
		return dockerComposePath
	}
	var dockerCompose map[string]interface{}
	err = json.Unmarshal(raw, &dockerCompose)
	if err != nil {
		log.Warn().Err(err).Msgf("could not parse %s to translate its mount paths", dockerComposePath)
		return dockerComposePath
	}

	composeDir := filepath.Dir(dockerComposePath)
	if !wslDriveCompose(dockerCompose, composeDir) {
		return dockerComposePath
	}

	rewritten, err := json.Marshal(dockerCompose)
	if err != nil {
		log.Warn().Err(err).Msgf("could not encode the translated %s", dockerComposePath)
		return dockerComposePath
	}

	enginePath := filepath.Join(composeDir, EngineComposeFileName)
	current, err := os.ReadFile(enginePath)
	if err != nil || string(current) != string(rewritten) {
		err = os.WriteFile(enginePath, rewritten, 0644) //nolint:gosec // G306: Docker engine configuration, no secrets, read by the Docker service
		if err != nil {
			log.Warn().Err(err).Msgf("could not write %s", enginePath)
			return dockerComposePath
		}
	}
	return enginePath
}
