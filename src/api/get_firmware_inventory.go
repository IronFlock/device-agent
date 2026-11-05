package api

import (
	"context"
	"errors"
	"reagent/common"
	"reagent/errdefs"
	"reagent/firmware"
	"reagent/messenger"
	"time"
)

// firmwareTimeout bounds one inventory read. fwupd is D-Bus activated and
// probes its devices when it starts, so the first read after boot is slow.
const firmwareTimeout = 90 * time.Second

// readFirmware is firmware.Read; the tests replace it.
var readFirmware = firmware.Read

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
