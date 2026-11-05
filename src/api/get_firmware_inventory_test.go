package api

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"reagent/common"
	"reagent/errdefs"
	"reagent/firmware"
	"reagent/messenger"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withFirmware makes readFirmware return inv and err for the test.
func withFirmware(t *testing.T, inv firmware.Inventory, err error) *time.Duration {
	t.Helper()
	var budget time.Duration
	old := readFirmware
	readFirmware = func(ctx context.Context) (firmware.Inventory, error) {
		if deadline, ok := ctx.Deadline(); ok {
			budget = time.Until(deadline)
		}
		return inv, err
	}
	t.Cleanup(func() { readFirmware = old })
	return &budget
}

func TestGetFirmwareInventoryHandler(t *testing.T) {
	t.Run("fwupd's devices, shaped for the platform", func(t *testing.T) {
		budget := withFirmware(t, firmware.Inventory{
			FwupdVersion: "2.0.20",
			HostProduct:  "Standard PC (Q35 + ICH9, 2009)",
			Devices: []firmware.Device{{
				ID: "a45d", Name: "System Firmware", Vendor: "QEMU", Version: "0.0.0",
				Plugin: "uefi_capsule", GUIDs: []string{"cb1d"}, Flags: []string{"internal", "updatable"},
			}},
		}, nil)
		ex := &External{Config: testConfig(), Privilege: priv(t, true)}

		res, err := ex.getFirmwareInventoryHandler(context.Background(), messenger.Result{Details: systemDetails()})
		require.NoError(t, err)
		require.Len(t, res.Arguments, 1)
		dict := res.Arguments[0].(common.Dict)
		assert.Equal(t, true, dict["available"])
		assert.Equal(t, "2.0.20", dict["fwupd_version"])
		assert.Equal(t, "Standard PC (Q35 + ICH9, 2009)", dict["host_product"])
		devices := dict["devices"].([]common.Dict)
		require.Len(t, devices, 1)
		assert.Equal(t, "System Firmware", devices[0]["name"])
		assert.Equal(t, true, devices[0]["updatable"])
		assert.Equal(t, []string{"internal", "updatable"}, devices[0]["flags"])
		assert.Greater(t, *budget, time.Minute, "fwupd gets time to start")
		assert.LessOrEqual(t, *budget, firmwareTimeout)
	})

	t.Run("a device without fwupd answers unavailable, not an error", func(t *testing.T) {
		withFirmware(t, firmware.Inventory{}, fmt.Errorf("%w: no system bus", firmware.ErrUnavailable))
		ex := &External{Config: testConfig(), Privilege: priv(t, true)}

		res, err := ex.getFirmwareInventoryHandler(context.Background(), messenger.Result{Details: systemDetails()})
		require.NoError(t, err)
		dict := res.Arguments[0].(common.Dict)
		assert.Equal(t, false, dict["available"])
		assert.Contains(t, dict["reason"], "no system bus")
	})

	t.Run("an fwupd error is returned", func(t *testing.T) {
		withFirmware(t, firmware.Inventory{}, errors.New("fwupd GetDevices: boom"))
		ex := &External{Config: testConfig(), Privilege: priv(t, true)}

		res, err := ex.getFirmwareInventoryHandler(context.Background(), messenger.Result{Details: systemDetails()})
		require.Error(t, err)
		assert.Nil(t, res)
	})

	t.Run("refused without READ, and fwupd is not asked", func(t *testing.T) {
		asked := false
		old := readFirmware
		readFirmware = func(context.Context) (firmware.Inventory, error) { asked = true; return firmware.Inventory{}, nil }
		t.Cleanup(func() { readFirmware = old })
		details, m := grantPrivilege(false)
		ex := &External{Config: testConfig(), Privilege: newPrivilege(testConfig(), m)}

		res, err := ex.getFirmwareInventoryHandler(context.Background(), messenger.Result{Details: details})
		require.Error(t, err)
		assert.Nil(t, res)
		assert.True(t, errdefs.IsInsufficientPrivileges(err))
		assert.False(t, asked)
		assert.Equal(t, "READ", requestedPrivilege(t, m))
	})
}
