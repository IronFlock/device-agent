package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reagent/common"
	"reagent/errdefs"
	"reagent/firmware"
	"reagent/messenger"
	"reagent/messenger/topics"
	"reagent/system"
	"time"

	"github.com/rs/zerolog/log"
)

const (
	// firmwareRefreshTimeout bounds a metadata refresh.
	firmwareRefreshTimeout = 2 * time.Minute
	// firmwareInstallTimeout bounds a download and flash. An SSD or a dock
	// can take many minutes.
	firmwareInstallTimeout = 45 * time.Minute
	// firmwareCheckInterval is how often a running agent looks for updates,
	// and firmwareMetadataMaxAge how old the metadata may get before a connect
	// fetches it again. fwupd's own timer does this daily; the image masks it.
	firmwareCheckInterval  = 24 * time.Hour
	firmwareMetadataMaxAge = 20 * time.Hour

	// firmwareReportMaxBytes keeps a report under what the platform stores:
	// f_update_device_architecture ignores more than 32768 characters of
	// jsonb text, which is longer than the JSON sent here.
	firmwareReportMaxBytes = 28 * 1024
	firmwareNotesMaxRunes  = 600
	firmwareIssuesMax      = 10
)

// The tests replace these.
var (
	snapshotFirmware = firmware.Snapshot
	refreshFirmware  = firmware.Refresh
	installFirmware  = firmware.Install
	tryLockUpdate    = system.TryLockUpdate
)

// firmwareReleaseReport is the update offered for a device, as the platform
// stores it: no download address and no checksum, which stay on the device.
func firmwareReleaseReport(r *firmware.Release) common.Dict {
	report := common.Dict{"version": r.Version}
	notes := []rune(r.Description)
	if len(notes) > firmwareNotesMaxRunes {
		notes = append(notes[:firmwareNotesMaxRunes], '…')
	}
	for key, value := range map[string]string{
		"summary":     r.Summary,
		"description": string(notes),
		"vendor":      r.Vendor,
		"urgency":     r.Urgency,
	} {
		if value != "" {
			report[key] = value
		}
	}
	if r.Size > 0 {
		report["size"] = r.Size
	}
	if r.Created > 0 {
		report["created"] = r.Created
	}
	if len(r.Issues) > firmwareIssuesMax {
		report["issues"] = r.Issues[:firmwareIssuesMax]
	} else if len(r.Issues) > 0 {
		report["issues"] = r.Issues
	}
	return report
}

func firmwareHistoryReport(entries []firmware.HistoryEntry) []common.Dict {
	history := make([]common.Dict, 0, len(entries))
	for _, e := range entries {
		entry := common.Dict{"device_id": e.DeviceID, "name": e.Name, "time": e.Time}
		for key, value := range map[string]string{
			"version_old": e.VersionOld,
			"version":     e.Version,
			"state":       e.State,
			"error":       e.Error,
		} {
			if value != "" {
				entry[key] = value
			}
		}
		history = append(history, entry)
	}
	return history
}

func encodedSize(v interface{}) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b)
}

// fitFirmwareReport gives up what matters least until the report fits: one
// over the platform's bound would be dropped whole.
func fitFirmwareReport(report common.Dict) common.Dict {
	if encodedSize(report) <= firmwareReportMaxBytes {
		return report
	}
	delete(report, "history")
	if encodedSize(report) <= firmwareReportMaxBytes {
		return report
	}
	if devices, ok := report["devices"].([]common.Dict); ok {
		for _, device := range devices {
			if update, ok := device["update"].(common.Dict); ok {
				delete(update, "description")
			}
		}
	}
	return report
}

// reportFirmware reads fwupd and sends the report, and returns what it read.
func (ex *External) reportFirmware() (firmware.Inventory, error) {
	readCtx, cancelRead := context.WithTimeout(context.Background(), firmwareTimeout)
	defer cancelRead()
	inventory, err := snapshotFirmware(readCtx)
	if err != nil {
		return firmware.Inventory{}, err
	}
	payload := ex.DeviceReport()
	payload["firmware_inventory"] = fitFirmwareReport(firmwareReport(inventory))
	callCtx, cancelCall := context.WithTimeout(context.Background(), firmwareReportCallTimeout)
	defer cancelCall()
	_, err = ex.Messenger.Call(callCtx, topics.UpdateDeviceArchitecture, []interface{}{payload}, nil, nil, nil)
	return inventory, err
}

// CheckFirmwareUpdates fetches fwupd's metadata if it is old, then reports.
// A failed refresh is logged and the report goes out with what fwupd has.
func (ex *External) CheckFirmwareUpdates() {
	if !runsFlockOS() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), firmwareRefreshTimeout)
	defer cancel()
	if err := refreshFirmware(ctx, firmwareMetadataMaxAge); err != nil {
		log.Warn().Err(err).Msg("failed to refresh the firmware metadata")
	}
	ex.ReportFirmwareInventory()
}

// WatchFirmwareUpdates checks once a day, for devices that stay connected.
func (ex *External) WatchFirmwareUpdates() {
	if !runsFlockOS() {
		return
	}
	ticker := time.NewTicker(firmwareCheckInterval)
	defer ticker.Stop()
	for range ticker.C {
		ex.CheckFirmwareUpdates()
	}
}

func countFirmwareUpdates(inventory firmware.Inventory) int {
	count := 0
	for _, d := range inventory.Devices {
		if d.Update != nil {
			count++
		}
	}
	return count
}

// refreshFirmwareUpdatesHandler is refresh_firmware_updates: fetch the
// metadata now and report. What it found reaches Studio through the device
// list store; the answer only says how many updates there are.
func (ex *External) refreshFirmwareUpdatesHandler(ctx context.Context, response messenger.Result) (*messenger.InvokeResult, error) {
	privileged, err := ex.Privilege.Check("MAINTAIN", response.Details)
	if err != nil {
		return nil, err
	}
	if !privileged {
		return nil, errdefs.InsufficientPrivileges(errors.New("insufficient privileges to check for firmware updates"))
	}
	if !runsFlockOS() {
		return nil, system.ErrNotFlockOS
	}
	refreshCtx, cancel := context.WithTimeout(ctx, firmwareRefreshTimeout)
	defer cancel()
	if err := refreshFirmware(refreshCtx, 0); err != nil {
		return nil, err
	}
	inventory, err := ex.reportFirmware()
	if err != nil {
		return nil, err
	}
	return &messenger.InvokeResult{
		Arguments: []interface{}{common.Dict{"updates": countFirmwareUpdates(inventory)}},
	}, nil
}

func firmwareProgressEvent(p firmware.Progress) common.Dict {
	if p.Stage == "download" {
		return common.Dict{"stage": p.Stage, "currentBytes": p.CurrentBytes, "fileSize": p.TotalBytes}
	}
	return common.Dict{"stage": p.Stage, "status": p.Status, "progressPercent": p.Percent}
}

// installFirmwareUpdateHandler is install_firmware_update: {device_id,
// version}, a version fwupd offers for that device. It answers when fwupd is
// done, with needs_reboot if the next restart applies the firmware; the agent
// does not restart the device for it. Progress goes to
// install_firmware_update_progress. It shares the lock of the OS update, and
// its report goes out on failure too, as the platform shows the error.
func (ex *External) installFirmwareUpdateHandler(ctx context.Context, response messenger.Result) (*messenger.InvokeResult, error) {
	privileged, err := ex.Privilege.Check("MAINTAIN", response.Details)
	if err != nil {
		return nil, err
	}
	if !privileged {
		return nil, errdefs.InsufficientPrivileges(errors.New("insufficient privileges to install firmware"))
	}
	if !runsFlockOS() {
		return nil, system.ErrNotFlockOS
	}
	args, err := firstArgDict(response.Arguments)
	if err != nil {
		return nil, err
	}
	deviceID, _ := args["device_id"].(string)
	version, _ := args["version"].(string)
	if deviceID == "" || version == "" {
		return nil, fmt.Errorf("device_id and version are required")
	}

	unlock, ok := tryLockUpdate()
	if !ok {
		return nil, system.ErrUpdateRunning
	}
	defer unlock()

	topic := topics.Topic(common.BuildInstallFirmwareUpdateProgress(ex.Config.ReswarmConfig.SerialNumber))
	progress := func(p firmware.Progress) {
		_ = ex.LogMessenger.Publish(topic, []interface{}{firmwareProgressEvent(p)}, nil, nil)
	}

	// Not the call's context: a dropped connection must not abort a flash.
	installCtx, cancel := context.WithTimeout(context.Background(), firmwareInstallTimeout)
	defer cancel()
	log.Info().Msgf("installing firmware %s on device %s", version, deviceID)
	result, err := installFirmware(installCtx, deviceID, version, progress)
	ex.ReportFirmwareInventory()
	if err != nil {
		return nil, err
	}
	return &messenger.InvokeResult{
		Arguments: []interface{}{common.Dict{"version": result.Version, "needs_reboot": result.NeedsReboot}},
	}, nil
}
