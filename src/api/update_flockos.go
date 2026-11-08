package api

import (
	"context"
	"errors"
	"reagent/common"
	"reagent/errdefs"
	"reagent/filesystem"
	"reagent/messenger"
	"reagent/messenger/topics"
	"reagent/osupdate"
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
	readRunning     = osupdate.ReadRunning
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
	result := common.Dict{"currentRelease": flockOSCurrent(running), "latestRelease": latest}
	addFlockOSSlots(ctx, result)
	return &messenger.InvokeResult{Arguments: []interface{}{result}}
}

// flockOSCurrent is the running image as get_os_release names it.
func flockOSCurrent(running osupdate.Running) common.Dict {
	return common.Dict{
		"Name":      "FlockOS",
		"Version":   running.ImageVersion,
		"BuildTime": buildTime(running.BuildID),
		"ImageID":   running.ImageID,
		"BuildID":   running.BuildID,
	}
}

// addFlockOSSlots adds RAUC's slots to an answer, or why they cannot be read.
func addFlockOSSlots(ctx context.Context, result common.Dict) {
	sctx, cancel := context.WithTimeout(ctx, slotStatusTimeout)
	defer cancel()
	booted, primary, slots, err := flockOSSlots(sctx)
	if err != nil {
		result["slotsError"] = err.Error()
		return
	}
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

// osReleaseReportTimeout bounds reading the slots and the report's call.
const osReleaseReportTimeout = time.Minute

// DeviceReport is the report of what this device is and runs.
func (ex *External) DeviceReport() common.Dict {
	sysInfo := release.GetSystemInfo()
	payload := common.Dict{
		"swarm_key":     ex.Config.ReswarmConfig.SwarmKey,
		"device_key":    ex.Config.ReswarmConfig.DeviceKey,
		"architecture":  sysInfo.Arch + sysInfo.Variant,
		"os":            sysInfo.DetailedOS,
		"arch":          sysInfo.Arch,
		"variant":       sysInfo.Variant,
		"agent_version": release.GetVersion(),
	}
	// The FlockOS image the device runs: an appliance mirrors the OS updates
	// of the images its devices run. A small file read, no blocking.
	if imageID, ok := osupdate.ImageIDReport(readRunning()); ok {
		payload["os_image_id"] = imageID
	}
	return payload
}

// ReportOSRelease tells the platform what this FlockOS device runs, which
// Studio shows from the device list store, online or offline. Sent after a
// connect, and after a failed install, which changes the slots without one.
// Errors are logged, never returned: the report is for display only.
func (ex *External) ReportOSRelease() {
	ctx, cancel := context.WithTimeout(context.Background(), osReleaseReportTimeout)
	defer cancel()
	osRelease, ok := OSReleaseReport(ctx)
	if !ok {
		return
	}
	payload := ex.DeviceReport()
	payload["os_release"] = osRelease
	if _, err := ex.Messenger.Call(ctx, topics.UpdateDeviceArchitecture, []interface{}{payload}, nil, nil, nil); err != nil {
		log.Error().Err(err).Msg("failed to report the OS release to the backend")
	}
}

// OSReleaseReport is what the agent reports to the platform as os_release: on
// FlockOS the running image and RAUC's slots as get_os_release names them. The
// release to update to is not part of it: Studio compares this with the
// catalog itself. An empty Dict on any other OS, and ok false when the
// os-release cannot be read (Windows), so that the report leaves the key out
// and the platform keeps what it knows.
func OSReleaseReport(ctx context.Context) (report common.Dict, ok bool) {
	running, err := readRunning()
	if err != nil {
		return nil, false
	}
	if !running.IsFlockOS() {
		return common.Dict{}, true
	}
	report = common.Dict{"currentRelease": flockOSCurrent(running)}
	addFlockOSSlots(ctx, report)
	return report, true
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
		// RAUC marked the slot bad when it started, and nothing reconnects.
		safe.Go(ex.ReportOSRelease)
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
