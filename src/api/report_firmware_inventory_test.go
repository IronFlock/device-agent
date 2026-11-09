package api

import (
	"errors"
	"testing"

	"reagent/common"
	"reagent/firmware"
	"reagent/messenger/topics"
	"reagent/testutil/fakes"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReportFirmwareInventory(t *testing.T) {
	inventory := firmware.Inventory{
		FwupdVersion: "2.0.20",
		HostVendor:   "QEMU",
		HostProduct:  "Standard PC (Q35 + ICH9, 2009)",
		Devices: []firmware.Device{
			{
				ID: "a45d", Name: "System Firmware", Vendor: "QEMU", Version: "0.0.0",
				Plugin: "uefi_capsule", GUIDs: []string{"cb1d"}, Flags: []string{"internal", "updatable"},
			},
			{ID: "77ee", ParentID: "a45d", Name: "UEFI dbx", UpdateState: "failed", UpdateError: "no space"},
		},
	}

	t.Run("on IronFlockOS: what fwupd sees, without the GUIDs and the empty fields", func(t *testing.T) {
		useFakeFlockOS(t, true, &fakeFlockOS{})
		budget := withFirmware(t, inventory, nil)
		ex, _ := flockOSExternal(t)

		ex.ReportFirmwareInventory()

		calls := ex.Messenger.(*fakes.Messenger).GetCallCalls()
		require.Len(t, calls, 1)
		assert.Equal(t, topics.UpdateDeviceArchitecture, calls[0].Topic)
		payload := calls[0].Args[0].(common.Dict)
		assert.Equal(t, ex.Config.ReswarmConfig.DeviceKey, payload["device_key"])
		report := payload["firmware_inventory"].(common.Dict)
		assert.Equal(t, "2.0.20", report["fwupd_version"])
		assert.Equal(t, "QEMU", report["host_vendor"])
		assert.Equal(t, "Standard PC (Q35 + ICH9, 2009)", report["host_product"])
		assert.Equal(t, []common.Dict{
			{
				"id": "a45d", "name": "System Firmware", "vendor": "QEMU", "version": "0.0.0",
				"plugin": "uefi_capsule", "flags": []string{"internal", "updatable"},
			},
			{"id": "77ee", "parent_id": "a45d", "name": "UEFI dbx", "update_state": "failed", "update_error": "no space"},
		}, report["devices"])
		assert.Greater(t, *budget, firmwareTimeout/2, "the read has its own time")
	})

	t.Run("a device without firmware to list still reports", func(t *testing.T) {
		useFakeFlockOS(t, true, &fakeFlockOS{})
		withFirmware(t, firmware.Inventory{FwupdVersion: "2.0.20"}, nil)
		ex, _ := flockOSExternal(t)

		ex.ReportFirmwareInventory()

		calls := ex.Messenger.(*fakes.Messenger).GetCallCalls()
		require.Len(t, calls, 1)
		report := calls[0].Args[0].(common.Dict)["firmware_inventory"].(common.Dict)
		assert.Equal(t, []common.Dict{}, report["devices"])
	})

	t.Run("another OS reports nothing and does not ask fwupd", func(t *testing.T) {
		useFakeFlockOS(t, false, &fakeFlockOS{})
		budget := withFirmware(t, inventory, nil)
		ex, _ := flockOSExternal(t)

		ex.ReportFirmwareInventory()

		assert.Empty(t, ex.Messenger.(*fakes.Messenger).GetCallCalls())
		assert.Zero(t, *budget)
	})

	t.Run("a failed read reports nothing, so the platform keeps what it knows", func(t *testing.T) {
		useFakeFlockOS(t, true, &fakeFlockOS{})
		withFirmware(t, firmware.Inventory{}, errors.New("fwupd GetDevices: timeout"))
		ex, _ := flockOSExternal(t)

		ex.ReportFirmwareInventory()

		assert.Empty(t, ex.Messenger.(*fakes.Messenger).GetCallCalls())
	})
}
