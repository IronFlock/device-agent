package filesystem

import (
	"os"
	"path/filepath"
	"reagent/common"
	"reagent/config"
	"strings"
)

// privateAppsDirMode keeps other host users out of the directories that hold
// app secrets: the per-app /data dirs under the stage dirs (whose env/ mirror
// carries APP_AUTH_SECRET) and the compose/build dirs (whose .env-compose does
// too). Only the agent and the Docker daemon, both root, use them on the host,
// and a container sees its own bind mount whatever the dirs above it allow, so
// the files inside can stay readable for a container that runs as any user.
// The apps dir itself is left alone: on FlockOS it is also Docker's data-root.
// On Windows Chmod only touches the read-only attribute; the installer's ACLs
// protect the apps dir there.
const privateAppsDirMode os.FileMode = 0700

func InitDirectories(cliArgs *config.CommandLineArguments) error {
	err := os.MkdirAll(cliArgs.AppsDirectory, os.ModePerm)
	if err != nil {
		if !os.IsExist(err) {
			return err
		}
	}

	privateDirs := []string{
		cliArgs.AppsBuildDir,
		cliArgs.AppsComposeDir,
		filepath.Join(cliArgs.AppsDirectory, strings.ToLower(string(common.PROD))),
		filepath.Join(cliArgs.AppsDirectory, strings.ToLower(string(common.DEV))),
	}
	for _, dir := range privateDirs {
		// MkdirAll leaves an existing dir's mode alone: Chmod narrows the ones
		// older agents created open.
		if err := os.MkdirAll(dir, privateAppsDirMode); err != nil {
			return err
		}
		if err := os.Chmod(dir, privateAppsDirMode); err != nil {
			return err
		}
	}

	err = os.MkdirAll(cliArgs.AppsSharedDir, os.ModePerm)
	if err != nil {
		if !os.IsExist(err) {
			return err
		}
	}

	err = os.MkdirAll(cliArgs.DownloadDir, os.ModePerm)
	if err != nil {
		if !os.IsExist(err) {
			return err
		}
	}

	// frpc opens its log file itself and does not create the folder
	// (/var/log/ironflock in the current layout).
	if cliArgs.FrpcLogFileLocation != "" {
		err = os.MkdirAll(filepath.Dir(cliArgs.FrpcLogFileLocation), 0o755)
		if err != nil && !os.IsExist(err) {
			return err
		}
	}

	return nil
}
