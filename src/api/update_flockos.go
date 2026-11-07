package api

import (
	"context"
	"errors"
	"reagent/common"
	"reagent/errdefs"
	"reagent/filesystem"
	"reagent/messenger"
	"reagent/messenger/topics"
	"reagent/release"
	"reagent/safe"
	"reagent/system"
	"regexp"
	"time"

	"github.com/rs/zerolog/log"
)

// osDownloadTimeout bounds one bundle download (several hundred MB).
const osDownloadTimeout = 2 * time.Hour

// slotStatusTimeout bounds the RAUC slot query of get_os_release.
const slotStatusTimeout = 10 * time.Second

// Seams for the tests.
var (
	runsFlockOS     = system.RunsFlockOS
	flockOSOffer    = (*system.System).FlockOSOffer
	downloadFlockOS = (*system.System).DownloadFlockOSUpdate
	installFlockOS  = (*system.System).InstallFlockOSUpdate
	rebootSystem    = (*system.System).Reboot
	flockOSSlots    = system.FlockOSSlots
	rebootDelay     = 2 * time.Second
)

var buildIDTime = regexp.MustCompile(`^(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})Z`)

// buildTime turns BUILD_ID (20261120T095512Z-g1a2b3c4) into 2026-11-20T09:55:12Z.
func buildTime(buildID string) string {
	m := buildIDTime.FindStringSubmatch(buildID)
	if m == nil {
		return ""
	}
	return m[1] + "-" + m[2] + "-" + m[3] + "T" + m[4] + ":" + m[5] + ":" + m[6] + "Z"
}

// flockOSRelease is get_os_release on FlockOS: the running image, the release
// the device's channel offers, and RAUC's slots. The keys of the legacy
// answer (Name, Version, BuildTime) stay; a part that cannot be read carries
// its error instead of failing the whole answer.
func (ex *External) flockOSRelease(ctx context.Context) *messenger.InvokeResult {
	running, offer, err := flockOSOffer(ex.System)
	current := common.Dict{
		"Name":      "FlockOS",
		"Version":   running.ImageVersion,
		"BuildTime": buildTime(running.BuildID),
		"ImageID":   running.ImageID,
		"BuildID":   running.BuildID,
	}
	latest := common.Dict{}
	if err != nil {
		latest["Error"] = err.Error()
	} else {
		latest = common.Dict{
			"Name":            "FlockOS",
			"Version":         offer.Version,
			"BuildTime":       offer.Release.BuildTime,
			"BuildID":         offer.Release.BuildID,
			"Channel":         offer.Channel,
			"UpdateAvailable": offer.Newer,
		}
		if offer.Newer {
			if err := offer.CheckAgent(release.GetVersion()); err != nil {
				latest["UpdateAvailable"] = false
				latest["Error"] = err.Error()
			}
		}
	}
	result := common.Dict{"currentRelease": current, "latestRelease": latest}

	sctx, cancel := context.WithTimeout(ctx, slotStatusTimeout)
	defer cancel()
	booted, primary, slots, err := flockOSSlots(sctx)
	if err != nil {
		result["slotsError"] = err.Error()
	} else {
		list := make([]common.Dict, 0, len(slots))
		for _, s := range slots {
			list = append(list, common.Dict{
				"name": s.Name, "class": s.Class, "bootname": s.Bootname, "state": s.State,
				"boot_status": s.BootStatus, "bundle_version": s.BundleVersion, "installed": s.Installed,
				"image_version": s.ImageVersion, "build_id": s.BuildID,
			})
		}
		result["slots"] = common.Dict{"booted": booted, "primary": primary, "slots": list}
	}
	return &messenger.InvokeResult{Arguments: []interface{}{result}}
}

// performOSUpdateHandler is perform_os_update on FlockOS: download, install
// and reboot into the new slot behind one progress topic. The answer comes
// before the reboot. Elsewhere it refuses: the legacy images update in two
// calls.
func (ex *External) performOSUpdateHandler(ctx context.Context, response messenger.Result) (*messenger.InvokeResult, error) {
	privileged, err := ex.Privilege.Check("MAINTAIN", response.Details)
	if err != nil {
		return nil, err
	}
	if !privileged {
		return nil, errdefs.InsufficientPrivileges(errors.New("insufficient privileges to update the operating system"))
	}
	if !runsFlockOS() {
		return nil, system.ErrNotFlockOS
	}

	topic := topics.Topic(common.BuildPerformOSUpdateProgress(ex.Config.ReswarmConfig.SerialNumber))
	publish := func(p common.Dict) {
		_ = ex.LogMessenger.Publish(topic, []interface{}{p}, nil, nil)
	}

	dctx, cancel := context.WithTimeout(context.Background(), osDownloadTimeout)
	defer cancel()
	path, err := downloadFlockOS(ex.System, dctx, func(dp filesystem.DownloadProgress) {
		publish(common.Dict{"stage": "download", "increment": dp.Increment, "currentBytes": dp.CurrentBytes, "fileSize": dp.TotalFileSize})
	})
	if err != nil {
		return nil, err
	}
	err = installFlockOS(ex.System, func(operation string, percent uint64) {
		publish(common.Dict{"stage": "install", "operationName": operation, "progressPercent": percent})
	})
	if err != nil {
		return nil, err
	}

	publish(common.Dict{"stage": "reboot"})
	log.Info().Msgf("FlockOS update from %s installed; rebooting into the new slot", path)
	safe.Go(func() {
		time.Sleep(rebootDelay)
		if err := rebootSystem(ex.System); err != nil {
			log.Error().Err(err).Msg("failed to reboot into the updated slot")
		}
	})
	return &messenger.InvokeResult{Arguments: []interface{}{common.Dict{"rebooting": true}}}, nil
}
