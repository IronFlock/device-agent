package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reagent/common"
	"reagent/errdefs"
	"reagent/filesystem"
	"reagent/messenger"
	"reagent/messenger/topics"
	"reagent/system"
	"strings"
)

func (ex *External) getOSReleaseHandler(ctx context.Context, response messenger.Result) (*messenger.InvokeResult, error) {
	privileged, err := ex.Privilege.Check("READ", response.Details)
	if err != nil {
		return nil, err
	}

	if !privileged {
		return nil, errdefs.InsufficientPrivileges(errors.New("insufficient privileges to get os release data"))
	}

	// current release information
	osReleaseCurrent, err := system.GetOSReleaseCurrent()
	if err != nil {
		if os.IsNotExist(err) {
			return &messenger.InvokeResult{
				Arguments: []interface{}{},
			}, nil
		}
		return nil, err
	}
	osReleaseVersionSplit := strings.Split(osReleaseCurrent["VERSION"], "-")
	osReleaseVersion := ""
	osReleaseBuildTime := ""
	if len(osReleaseVersionSplit) == 3 {
		osReleaseVersion = strings.Trim(osReleaseVersionSplit[0], "v")
		osReleaseBuildTime = osReleaseVersionSplit[2]
	}
	currentOSRelease := system.OSRelease{
		Name:      osReleaseCurrent["Name"],
		Version:   osReleaseVersion,
		BuildTime: osReleaseBuildTime,
	}

	// latest release information
	osReleaseLatest, err := system.GetOSReleaseLatest()
	if err != nil {
		// Systems that do not have /etc/os-release can return an empty value
		if os.IsNotExist(err) {
			return &messenger.InvokeResult{
				Arguments: []interface{}{},
			}, nil
		}
		return nil, err
	}

	latestOSRelease := system.OSRelease{
		Name:      fmt.Sprint(osReleaseLatest[""]),
		Version:   fmt.Sprint(osReleaseLatest["version"]),
		BuildTime: fmt.Sprint(osReleaseLatest["buildtime"]),
	}

	// merge both
	osrelease := common.Dict{
		"currentRelease": common.Dict{
			"Name":      currentOSRelease.Name,
			"Version":   currentOSRelease.Version,
			"BuildTime": currentOSRelease.BuildTime,
		},
		"latestRelease": common.Dict{
			"Name":      latestOSRelease.Name,
			"Version":   latestOSRelease.Version,
			"BuildTime": latestOSRelease.BuildTime,
		},
	}

	return &messenger.InvokeResult{
		Arguments: []interface{}{osrelease},
	}, nil

}

func (ex *External) downloadOSUpdateHandler(ctx context.Context, response messenger.Result) (*messenger.InvokeResult, error) {
	privileged, err := ex.Privilege.Check("MAINTAIN", response.Details)
	if err != nil {
		return nil, err
	}

	if !privileged {
		return nil, errdefs.InsufficientPrivileges(errors.New("insufficient privileges to update reswarmos"))
	}

	// prepare callback monitoring progress of download
	progressCallback := func(dp filesystem.DownloadProgress) {
		progress := common.Dict{
			"increment":    dp.Increment,
			"currentBytes": dp.CurrentBytes,
			"fileSize":     dp.TotalFileSize,
		}

		serialNumber := ex.Config.ReswarmConfig.SerialNumber
		topic := common.BuildDownloadOSUpdateProgress(serialNumber)
		_ = ex.LogMessenger.Publish(topics.Topic(topic), []interface{}{progress}, nil, nil)
	}

	// start downloading...
	err = system.GetOSUpdate(progressCallback)
	if err != nil {
		return nil, err
	}

	return &messenger.InvokeResult{}, nil
}

func (ex *External) installOSUpdateHandler(ctx context.Context, response messenger.Result) (*messenger.InvokeResult, error) {
	privileged, err := ex.Privilege.Check("MAINTAIN", response.Details)
	if err != nil {
		return nil, err
	}

	if !privileged {
		return nil, errdefs.InsufficientPrivileges(errors.New("insufficient privileges to update reswarmos"))
	}

	// prepare callback monitoring progress of OS installation
	progressCallback := func(operationName string, progressPercent uint64) {
		progress := common.Dict{
			"operationName":   operationName,
			"progressPercent": progressPercent,
		}

		serialNumber := ex.Config.ReswarmConfig.SerialNumber
		topic := common.BuildInstallOSUpdateProgress(serialNumber)
		_ = ex.LogMessenger.Publish(topics.Topic(topic), []interface{}{progress}, nil, nil)
	}

	// start installing OS bundle...
	err = system.InstallOSUpdate(progressCallback)
	if err != nil {
		return nil, err
	}

	return &messenger.InvokeResult{}, nil
}
