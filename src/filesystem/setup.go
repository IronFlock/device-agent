package filesystem

import (
	"os"
	"path/filepath"
	"reagent/config"
)

func InitDirectories(cliArgs *config.CommandLineArguments) error {
	err := os.MkdirAll(cliArgs.AppsDirectory, os.ModePerm)
	if err != nil {
		if !os.IsExist(err) {
			return err
		}
	}
	err = os.MkdirAll(cliArgs.AppsBuildDir, os.ModePerm)
	if err != nil {
		if !os.IsExist(err) {
			return err
		}
	}
	err = os.MkdirAll(cliArgs.AppsComposeDir, os.ModePerm)
	if err != nil {
		if !os.IsExist(err) {
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
