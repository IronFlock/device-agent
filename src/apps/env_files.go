package apps

import (
	"fmt"
	"os"
	"path/filepath"
	"reagent/common"
	"reagent/config"
	"strings"

	"github.com/rs/zerolog/log"
)

// appEnvFilesHostDir is the host directory whose files appear as
// /data/env/<NAME>.txt inside the app's containers: single-container apps
// bind-mount its parent to /data (computeMounts), compose services get it
// mounted read-only at /data/env (addComposeEnvFilesMount). Because it is a
// bind mount, file updates are visible to RUNNING containers immediately —
// the delivery channel for values that change after container start, like the
// cloud port of an instance tcp/udp tunnel.
func appEnvFilesHostDir(cfg *config.Config, stage common.Stage, appName string) string {
	return filepath.Join(cfg.CommandLineArguments.AppsDirectory, strings.ToLower(string(stage)), strings.ToLower(appName), "env")
}

// The env mirror is readable by every user: an app's container may run as any
// user, and the SDKs read the mirror file-first to pick up values that change
// while the app runs. Other host users are kept out one level up, by the
// private stage dirs (filesystem.InitDirectories).
const (
	envDirMode  os.FileMode = 0755
	envFileMode os.FileMode = 0644
)

// ensureEnvDir creates the env mirror dir and opens up a dir and files that an
// older agent made root-only (0700/0600): MkdirAll and WriteFile keep the mode
// of what already exists, and a file the agent no longer rewrites would stay
// unreadable for good.
//
// It first drops a DEVICE_SECRET.txt an older agent left behind: the device's
// realm1 credential is no longer injected, and must not be opened up with the
// rest.
func ensureEnvDir(envDir string) error {
	if err := os.MkdirAll(envDir, envDirMode); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(envDir, "DEVICE_SECRET.txt")); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Chmod(envDir, envDirMode); err != nil {
		return err
	}

	entries, err := os.ReadDir(envDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Type().IsRegular() {
			if err := os.Chmod(filepath.Join(envDir, entry.Name()), envFileMode); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeEnvValue writes {name}.txt; the Chmod undoes a restrictive umask.
func writeEnvValue(envDir string, name string, value string) error {
	filePath := filepath.Join(envDir, name+".txt")
	if err := os.WriteFile(filePath, []byte(value), envFileMode); err != nil {
		return err
	}
	return os.Chmod(filePath, envFileMode)
}

// refreshRemotePortEnvFiles live-updates the tunnel-port env files of an app:
// {NAME}.txt (the tunnel's remote port) and {NAME}_CLOUD.txt (the
// internet-facing cloud port of an instance tcp/udp tunnel) for every port
// rule, under the canonical REMOTE_PORT_FOR_<port> name plus the rule's
// custom remote_port_environment name when one is declared. Called from
// syncPortState — which runs on exactly the sync the backend triggers after
// allocating a cloud port — so running containers see fresh values within
// seconds, without a restart. A cloud port that disappeared (forwarding off)
// removes the file, flipping SDK URL composition back to the LAN fallback.
func refreshRemotePortEnvFiles(cfg *config.Config, stage common.Stage, appName string, rules []common.PortForwardRule) {
	envDir := appEnvFilesHostDir(cfg, stage, appName)

	relevant := false
	for _, rule := range rules {
		if rule.RemotePort > 0 || rule.CloudRemotePort > 0 {
			relevant = true
			break
		}
	}

	if relevant {
		if err := ensureEnvDir(envDir); err != nil {
			log.Debug().Err(err).Str("dir", envDir).Msg("Failed to create env files dir")
			return
		}
	} else if _, err := os.Stat(envDir); err != nil {
		// Nothing to write, and no existing dir that could hold stale cloud
		// files to remove: don't create empty env dirs.
		return
	}

	for _, rule := range rules {
		names := []string{fmt.Sprintf("REMOTE_PORT_FOR_%d", rule.Port)}
		if rule.RemotePortEnvironment != "" {
			names = append(names, rule.RemotePortEnvironment)
		}

		for _, name := range names {
			if rule.RemotePort > 0 {
				writeEnvFile(envDir, name, fmt.Sprintf("%d", rule.RemotePort))
			}

			cloudName := name + "_CLOUD"
			if rule.CloudRemotePort > 0 {
				writeEnvFile(envDir, cloudName, fmt.Sprintf("%d", rule.CloudRemotePort))
			} else {
				removeEnvFile(envDir, cloudName)
			}
		}
	}
}

func writeEnvFile(envDir string, name string, value string) {
	if err := writeEnvValue(envDir, name, value); err != nil {
		log.Debug().Err(err).Str("dir", envDir).Str("name", name).Msg("Failed to write env file")
	}
}

func removeEnvFile(envDir string, name string) {
	filePath := filepath.Join(envDir, name+".txt")
	err := os.Remove(filePath)
	if err != nil && !os.IsNotExist(err) {
		log.Debug().Err(err).Str("file", filePath).Msg("Failed to remove env file")
	}
}

// refreshAppCredentialEnvFiles live-updates an app's per-app WAMP credential
// files (APP_AUTH_ID.txt / APP_AUTH_SECRET.txt) in the /data/env bind mount.
//
// The bind mount is the ONLY channel that reaches a container without
// recreating it — an env-var change alone never triggers a recreate
// (containerNetworkConfigOutdated only reacts to network/host-port drift). A
// credential rotated while the app is running therefore lands in the file
// immediately; the SDK picks it up on its next connection attempt, because
// both SDKs resolve injected values file-first, env-second.
//
// No-op when the credential cannot be derived (per-device key not fetched
// yet): existing files are left INTACT rather than blanked, so a backend
// outage can never invalidate a running app's credential.
func refreshAppCredentialEnvFiles(cfg *config.Config, appCredKey string, payload common.TransitionPayload) {
	envDir := appEnvFilesHostDir(cfg, payload.Stage, payload.AppName)

	authID, secret := appCredential(cfg, appCredKey, payload)
	if authID == "" || secret == "" {
		return
	}

	if err := ensureEnvDir(envDir); err != nil {
		log.Debug().Err(err).Str("dir", envDir).Msg("Failed to create env files dir")
		return
	}

	writeEnvFile(envDir, "APP_AUTH_ID", authID)
	writeEnvFile(envDir, "APP_AUTH_SECRET", secret)
}
