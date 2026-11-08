package api

import (
	"context"
	"errors"
	"reagent/common"
	"reagent/errdefs"
	"reagent/firmware"
	"reagent/messenger"
	"reagent/messenger/topics"
	"time"

	"github.com/rs/zerolog/log"
)

// firmwareTimeout bounds one inventory read. fwupd is D-Bus activated and
// probes its devices when it starts, so the first read after boot is slow.
const firmwareTimeout = 90 * time.Second

// firmwareReportCallTimeout bounds the report's call, after the read.
const firmwareReportCallTimeout = 30 * time.Second

// readFirmware is firmware.Read; the tests replace it.
var readFirmware = firmware.Read

// ReportFirmwareInventory tells the platform which firmware this FlockOS
// device runs, which Studio shows from the device list store, online or
// offline. Sent after a connect. Not FlockOS: nothing to report. Errors are
// logged, never returned: the report is for display only, and the platform
// keeps what it knows.
func (ex *External) ReportFirmwareInventory() {
	if !runsFlockOS() {
		return
	}
	readCtx, cancelRead := context.WithTimeout(context.Background(), firmwareTimeout)
	defer cancelRead()
	inventory, err := readFirmware(readCtx)
	if err != nil {
		log.Error().Err(err).Msg("failed to read the firmware inventory")
		return
	}
	payload := ex.DeviceReport()
	payload["firmware_inventory"] = firmwareReport(inventory)
	callCtx, cancelCall := context.WithTimeout(context.Background(), firmwareReportCallTimeout)
	defer cancelCall()
	if _, err := ex.Messenger.Call(callCtx, topics.UpdateDeviceArchitecture, []interface{}{payload}, nil, nil, nil); err != nil {
		log.Error().Err(err).Msg("failed to report the firmware inventory to the backend")
	}
}

// firmwareReport is the inventory as the platform stores it. It travels with
// the device list, so it stays small: empty fields are left out, and the
// GUIDs, which only fwupd needs to find updates, stay on the device.
func firmwareReport(inventory firmware.Inventory) common.Dict {
	devices := make([]common.Dict, 0, len(inventory.Devices))
	for _, d := range inventory.Devices {
		device := common.Dict{"id": d.ID, "name": d.Name}
		for key, value := range map[string]string{
			"parent_id":          d.ParentID,
			"summary":            d.Summary,
			"vendor":             d.Vendor,
			"version":            d.Version,
			"version_lowest":     d.VersionLowest,
			"version_bootloader": d.VersionBootloader,
			"plugin":             d.Plugin,
			"protocol":           d.Protocol,
			"update_state":       d.UpdateState,
			"update_error":       d.UpdateError,
		} {
			if value != "" {
				device[key] = value
			}
		}
		if len(d.Flags) > 0 {
			device["flags"] = d.Flags
		}
		devices = append(devices, device)
	}
	return common.Dict{
		"fwupd_version": inventory.FwupdVersion,
		"host_vendor":   inventory.HostVendor,
		"host_product":  inventory.HostProduct,
		"devices":       devices,
	}
}

// getFirmwareInventoryHandler answers get_firmware_inventory: fwupd's devices
// and their firmware versions, read-only (FlockOS ADR-0009). A device without
// fwupd (another Linux, Windows) answers available false with the reason,
// not an error, so the platform can say so.
func (ex *External) getFirmwareInventoryHandler(ctx context.Context, response messenger.Result) (*messenger.InvokeResult, error) {
	privileged, err := ex.Privilege.Check("READ", response.Details)
	if err != nil {
		return nil, err
	}

	if !privileged {
		return nil, errdefs.InsufficientPrivileges(errors.New("insufficient privileges to get the firmware inventory"))
	}

	ctx, cancel := context.WithTimeout(ctx, firmwareTimeout)
	defer cancel()

	inventory, err := readFirmware(ctx)
	if errors.Is(err, firmware.ErrUnavailable) {
		return &messenger.InvokeResult{
			Arguments: []interface{}{common.Dict{"available": false, "reason": err.Error()}},
		}, nil
	}
	if err != nil {
		return nil, err
	}

	devices := make([]common.Dict, 0, len(inventory.Devices))
	for _, d := range inventory.Devices {
		devices = append(devices, common.Dict{
			"id":                 d.ID,
			"parent_id":          d.ParentID,
			"name":               d.Name,
			"summary":            d.Summary,
			"vendor":             d.Vendor,
			"version":            d.Version,
			"version_lowest":     d.VersionLowest,
			"version_bootloader": d.VersionBootloader,
			"plugin":             d.Plugin,
			"protocol":           d.Protocol,
			"guids":              d.GUIDs,
			"flags":              d.Flags,
			"updatable":          d.Has("updatable"),
			"update_state":       d.UpdateState,
			"update_error":       d.UpdateError,
		})
	}

	return &messenger.InvokeResult{
		Arguments: []interface{}{common.Dict{
			"available":     true,
			"fwupd_version": inventory.FwupdVersion,
			"host_vendor":   inventory.HostVendor,
			"host_product":  inventory.HostProduct,
			"devices":       devices,
		}},
	}, nil
}
