package apps

import (
	"fmt"
	"os"
	"reagent/common"
	"strings"

	"github.com/rs/zerolog/log"
)

func (sm *StateMachine) uninstallApp(payload common.TransitionPayload, app *common.App) error {
	err := sm.removeApp(payload, app)
	if err != nil {
		return err
	}

	err = sm.setState(app, common.UNINSTALLED)
	if err != nil {
		return err
	}

	// Free the app's host ports only now that its containers are gone.
	// Stop/restart keeps reservations so the app gets the same ports back.
	if am := sm.StateObserver.AppManager; am != nil {
		// Tunnels first, while their configs still name the app: leaving them
		// standing kept frpc dialing the just-freed host ports, and their
		// bookkeeping made a later reinstall skip recreating them (the
		// "tunnel already exists" deadlock). Only this stage's: the PROD
		// install and the development run of an app have tunnels of their
		// own, and uninstalling one leaves the other's.
		am.RemoveAppTunnels(payload.AppName, payload.Stage)
		am.hostPorts.ReleaseApp(payload.Stage, payload.AppKey)
	}

	config := sm.Container.GetConfig()

	appsDir := config.CommandLineArguments.AppsDirectory
	// AppName must be lowercased to match the /data bind-mount source created in
	// computeMounts (run_app.go), which uses strings.ToLower(appName). Without
	// this, an app whose name has uppercase letters (e.g. "WireGuard_Easy") would
	// have its data mounted at /apps/<stage>/wireguard_easy but this delete would
	// target /apps/<stage>/WireGuard_Easy — a non-existent path that os.RemoveAll
	// silently ignores, leaking the old state into the next install.
	dataFolderDir := fmt.Sprintf("%s/%s/%s", appsDir, strings.ToLower(string(payload.Stage)), strings.ToLower(payload.AppName))
	err = os.RemoveAll(dataFolderDir) // remove the data directory for the app we just removed
	log.Debug().Msgf("Removed data dir: %s, error: %v", dataFolderDir, err)

	if payload.Stage == common.DEV && payload.DockerCompose == nil {
		buildDir := config.CommandLineArguments.AppsBuildDir
		fileName := payload.AppName + "." + config.CommandLineArguments.CompressedBuildExtension
		buildZipFile := buildDir + "/" + fileName
		err = os.RemoveAll(buildZipFile) // removes the build zip if it exists
		log.Debug().Msgf("Removed build zip file: %s, error: %v", buildZipFile, err)
	}

	if payload.DockerCompose != nil {
		appDir := config.CommandLineArguments.AppsComposeDir + "/" + payload.AppName
		if payload.Stage == common.DEV {
			appDir = config.CommandLineArguments.AppsBuildDir + "/" + payload.AppName
		}

		_, err := os.Stat(appDir)
		if err == nil {
			err = os.RemoveAll(appDir)
			if err != nil {
				return err
			}
		}
	}

	return err
}
