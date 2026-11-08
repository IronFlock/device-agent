package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"reagent/common"
	"reagent/errdefs"
	"reagent/firmware"
	"reagent/messenger"
	"reagent/messenger/topics"
	"reagent/system"
	"reagent/testutil/fakes"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fakeRefresh(t *testing.T, err error) *[]time.Duration {
	t.Helper()
	oldAnnounce := announceFirmware
	announceFirmware = func(context.Context, string, firmware.Inventory) {}
	t.Cleanup(func() { announceFirmware = oldAnnounce })
	var ages []time.Duration
	old := refreshFirmware
	refreshFirmware = func(_ context.Context, maxAge time.Duration, _ string) error {
		ages = append(ages, maxAge)
		return err
	}
	t.Cleanup(func() { refreshFirmware = old })
	return &ages
}

type firmwareInstall struct {
	deviceID, version string
	deadlineLeft      time.Duration
	callCancelled     bool
}

func fakeInstall(t *testing.T, result firmware.Result, err error) *[]firmwareInstall {
	t.Helper()
	var installs []firmwareInstall
	old := installFirmware
	installFirmware = func(ctx context.Context, deviceID, version, _ string, progress func(firmware.Progress)) (firmware.Result, error) {
		deadline, _ := ctx.Deadline()
		installs = append(installs, firmwareInstall{deviceID, version, time.Until(deadline), ctx.Err() != nil})
		progress(firmware.Progress{Stage: "download", CurrentBytes: 5, TotalBytes: 10})
		progress(firmware.Progress{Stage: "install", Status: "device-write", Percent: 40})
		return result, err
	}
	t.Cleanup(func() { installFirmware = old })
	return &installs
}

func installArgs(deviceID, version string) messenger.Result {
	return messenger.Result{
		Details:   systemDetails(),
		Arguments: []interface{}{map[string]interface{}{"device_id": deviceID, "version": version}},
	}
}

func reports(ex *External) []common.Dict {
	var out []common.Dict
	for _, c := range ex.Messenger.(*fakes.Messenger).GetCallCalls() {
		if c.Topic == topics.UpdateDeviceArchitecture {
			out = append(out, c.Args[0].(common.Dict))
		}
	}
	return out
}

func TestFirmwareReportOfUpdates(t *testing.T) {
	inventory := firmware.Inventory{
		Devices: []firmware.Device{{
			ID: "a", Name: "SSD", Version: "1",
			Update: &firmware.Release{
				Version: "2", Summary: "Firmware", Description: "Fixes", Vendor: "ACME", Urgency: "high", Size: 1024, Created: 1700,
				Issues: []string{"CVE-2026-1"}, Locations: []string{"https://cdn.example/a.cab"}, Checksums: []string{"abc"},
			},
		}},
		History: []firmware.HistoryEntry{{DeviceID: "a", Name: "SSD", VersionOld: "0", Version: "1", State: "success", Time: 1600}},
	}
	report := firmwareReport(inventory)

	device := report["devices"].([]common.Dict)[0]
	assert.Equal(t, common.Dict{
		"version": "2", "summary": "Firmware", "description": "Fixes", "vendor": "ACME", "urgency": "high",
		"size": uint64(1024), "created": uint64(1700), "issues": []string{"CVE-2026-1"},
	}, device["update"], "no address and no checksum leaves the device")
	assert.Equal(t, []common.Dict{{
		"device_id": "a", "name": "SSD", "version_old": "0", "version": "1", "state": "success", "time": uint64(1600),
	}}, report["history"])

	t.Run("long release notes are cut", func(t *testing.T) {
		r := firmwareReleaseReport(&firmware.Release{Version: "2", Description: strings.Repeat("ä", firmwareNotesMaxRunes+50)})
		assert.Equal(t, firmwareNotesMaxRunes+1, len([]rune(r["description"].(string))))
	})

	t.Run("nothing to say about updates adds no keys", func(t *testing.T) {
		report := firmwareReport(firmware.Inventory{Devices: []firmware.Device{{ID: "a", Name: "SSD"}}})
		assert.NotContains(t, report, "history")
		assert.NotContains(t, report["devices"].([]common.Dict)[0], "update")
	})
}

func TestFitFirmwareReport(t *testing.T) {
	long := strings.Repeat("x", 500)
	device := func() common.Dict {
		return common.Dict{"id": "a", "name": "x", "update": common.Dict{"version": "2", "description": long}}
	}
	history := make([]common.Dict, 10)
	for i := range history {
		history[i] = common.Dict{"name": strings.Repeat("h", 500)}
	}

	t.Run("a report that fits is kept whole", func(t *testing.T) {
		report := fitFirmwareReport(common.Dict{"devices": []common.Dict{device()}, "history": history})
		assert.Contains(t, report, "history")
	})

	t.Run("the history goes first", func(t *testing.T) {
		devices := make([]common.Dict, 50)
		for i := range devices {
			devices[i] = device()
		}
		report := fitFirmwareReport(common.Dict{"devices": devices, "history": history})
		assert.NotContains(t, report, "history")
		assert.Contains(t, devices[0]["update"], "description")
		assert.LessOrEqual(t, encodedSize(report), firmwareReportMaxBytes)
	})

	t.Run("then the release notes, never the versions", func(t *testing.T) {
		devices := make([]common.Dict, 100)
		for i := range devices {
			devices[i] = device()
		}
		report := fitFirmwareReport(common.Dict{"devices": devices})
		assert.NotContains(t, devices[0]["update"], "description")
		assert.Contains(t, devices[0]["update"], "version")
		assert.LessOrEqual(t, encodedSize(report), firmwareReportMaxBytes)
	})
}

func TestCheckFirmwareUpdates(t *testing.T) {
	inventory := firmware.Inventory{Devices: []firmware.Device{{ID: "a", Name: "SSD"}}}

	t.Run("old metadata is fetched, then the report goes out", func(t *testing.T) {
		useFakeFlockOS(t, true, &fakeFlockOS{})
		withFirmware(t, inventory, nil)
		ages := fakeRefresh(t, nil)
		ex, _ := flockOSExternal(t)
		ex.CheckFirmwareUpdates()
		assert.Equal(t, []time.Duration{firmwareMetadataMaxAge}, *ages)
		assert.Len(t, reports(ex), 1)
	})

	t.Run("no network still reports what fwupd has", func(t *testing.T) {
		useFakeFlockOS(t, true, &fakeFlockOS{})
		withFirmware(t, inventory, nil)
		fakeRefresh(t, errors.New("no route to host"))
		ex, _ := flockOSExternal(t)
		ex.CheckFirmwareUpdates()
		assert.Len(t, reports(ex), 1)
	})

	t.Run("another OS does nothing", func(t *testing.T) {
		useFakeFlockOS(t, false, &fakeFlockOS{})
		ages := fakeRefresh(t, nil)
		ex, _ := flockOSExternal(t)
		ex.CheckFirmwareUpdates()
		assert.Empty(t, *ages)
		assert.Empty(t, reports(ex))
	})
}

func TestFirmwareGoesThroughTheUpdateBase(t *testing.T) {
	const base = "http://appliance:15002/dl"
	inventory := firmware.Inventory{Devices: []firmware.Device{{ID: "a", Name: "SSD", Update: &firmware.Release{Version: "2"}}}}
	useBase := func(ex *External) {
		ex.Config.ReswarmConfig.UpdateURL = base
	}

	t.Run("a check reads the mirror and announces what is on offer", func(t *testing.T) {
		useFakeFlockOS(t, true, &fakeFlockOS{})
		withFirmware(t, inventory, nil)
		var refreshed, announced []string
		oldRefresh, oldAnnounce := refreshFirmware, announceFirmware
		t.Cleanup(func() { refreshFirmware, announceFirmware = oldRefresh, oldAnnounce })
		refreshFirmware = func(_ context.Context, _ time.Duration, mirror string) error {
			refreshed = append(refreshed, mirror)
			return nil
		}
		announceFirmware = func(_ context.Context, mirror string, inv firmware.Inventory) {
			announced = append(announced, mirror)
			assert.Len(t, inv.Devices, 1)
		}
		ex, _ := flockOSExternal(t)
		useBase(ex)
		ex.CheckFirmwareUpdates()
		assert.Equal(t, []string{base + "/lvfs"}, refreshed)
		assert.Equal(t, []string{base + "/lvfs"}, announced)
	})

	t.Run("an install reads the mirror", func(t *testing.T) {
		useFakeFlockOS(t, true, &fakeFlockOS{})
		withFirmware(t, inventory, nil)
		var mirrors []string
		old := installFirmware
		t.Cleanup(func() { installFirmware = old })
		installFirmware = func(_ context.Context, _, _, mirror string, _ func(firmware.Progress)) (firmware.Result, error) {
			mirrors = append(mirrors, mirror)
			return firmware.Result{Version: "2"}, nil
		}
		ex, _ := flockOSExternal(t)
		useBase(ex)
		_, err := ex.installFirmwareUpdateHandler(context.Background(), installArgs("a", "2"))
		require.NoError(t, err)
		assert.Equal(t, []string{base + "/lvfs"}, mirrors)
	})
}

func TestRefreshFirmwareUpdatesHandler(t *testing.T) {
	inventory := firmware.Inventory{Devices: []firmware.Device{
		{ID: "a", Name: "SSD", Update: &firmware.Release{Version: "2"}},
		{ID: "b", Name: "TPM"},
	}}

	t.Run("fetches now and says how many", func(t *testing.T) {
		useFakeFlockOS(t, true, &fakeFlockOS{})
		withFirmware(t, inventory, nil)
		ages := fakeRefresh(t, nil)
		ex, _ := flockOSExternal(t)
		res, err := ex.refreshFirmwareUpdatesHandler(context.Background(), messenger.Result{Details: systemDetails()})
		require.NoError(t, err)
		assert.Equal(t, common.Dict{"updates": 1}, res.Arguments[0])
		assert.Equal(t, []time.Duration{0}, *ages, "whatever its age")
		assert.Len(t, reports(ex), 1)
	})

	t.Run("a failed refresh is the answer", func(t *testing.T) {
		useFakeFlockOS(t, true, &fakeFlockOS{})
		withFirmware(t, inventory, nil)
		fakeRefresh(t, errors.New("no route to host"))
		ex, _ := flockOSExternal(t)
		_, err := ex.refreshFirmwareUpdatesHandler(context.Background(), messenger.Result{Details: systemDetails()})
		require.Error(t, err)
	})

	t.Run("refused without MAINTAIN", func(t *testing.T) {
		useFakeFlockOS(t, true, &fakeFlockOS{})
		ages := fakeRefresh(t, nil)
		ex, _ := flockOSExternal(t)
		details, m := grantPrivilege(false)
		ex.Privilege = newPrivilege(testConfig(), m)
		_, err := ex.refreshFirmwareUpdatesHandler(context.Background(), messenger.Result{Details: details})
		require.Error(t, err)
		assert.True(t, errdefs.IsInsufficientPrivileges(err))
		assert.Empty(t, *ages)
	})

	t.Run("not on another OS", func(t *testing.T) {
		useFakeFlockOS(t, false, &fakeFlockOS{})
		ages := fakeRefresh(t, nil)
		ex, _ := flockOSExternal(t)
		_, err := ex.refreshFirmwareUpdatesHandler(context.Background(), messenger.Result{Details: systemDetails()})
		require.ErrorIs(t, err, system.ErrNotFlockOS)
		assert.Empty(t, *ages)
	})
}

func TestInstallFirmwareUpdateHandler(t *testing.T) {
	inventory := firmware.Inventory{Devices: []firmware.Device{{ID: "a", Name: "SSD"}}}

	t.Run("installs, follows and reports, and leaves the restart to the caller", func(t *testing.T) {
		f := &fakeFlockOS{}
		useFakeFlockOS(t, true, f)
		withFirmware(t, inventory, nil)
		installs := fakeInstall(t, firmware.Result{Version: "2", NeedsReboot: true}, nil)
		ex, logs := flockOSExternal(t)

		res, err := ex.installFirmwareUpdateHandler(context.Background(), installArgs("a", "2"))
		require.NoError(t, err)
		assert.Equal(t, common.Dict{"version": "2", "needs_reboot": true}, res.Arguments[0])
		require.Len(t, *installs, 1)
		assert.Equal(t, "a", (*installs)[0].deviceID)
		assert.Equal(t, "2", (*installs)[0].version)
		assert.Greater(t, (*installs)[0].deadlineLeft, 40*time.Minute)

		published := logs.GetPublishCalls()
		require.Len(t, published, 2)
		assert.Equal(t, "re.mgmt.SER-1.install_firmware_update_progress", string(published[0].Topic))
		assert.Equal(t, common.Dict{"stage": "download", "currentBytes": int64(5), "fileSize": int64(10)}, published[0].Args[0])
		assert.Equal(t, common.Dict{"stage": "install", "status": "device-write", "progressPercent": uint32(40)}, published[1].Args[0])
		assert.Len(t, reports(ex), 1)
		select {
		case <-f.rebooted:
			t.Fatal("the agent restarted the device on its own")
		default:
		}
	})

	t.Run("a dropped connection does not abort the flash", func(t *testing.T) {
		useFakeFlockOS(t, true, &fakeFlockOS{})
		withFirmware(t, inventory, nil)
		installs := fakeInstall(t, firmware.Result{Version: "2"}, nil)
		ex, _ := flockOSExternal(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := ex.installFirmwareUpdateHandler(ctx, installArgs("a", "2"))
		require.NoError(t, err)
		assert.False(t, (*installs)[0].callCancelled)
	})

	t.Run("a failure is the answer, and is reported", func(t *testing.T) {
		useFakeFlockOS(t, true, &fakeFlockOS{})
		withFirmware(t, inventory, nil)
		fakeInstall(t, firmware.Result{}, errors.New("fwupd Install: no space"))
		ex, _ := flockOSExternal(t)
		_, err := ex.installFirmwareUpdateHandler(context.Background(), installArgs("a", "2"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no space")
		assert.Len(t, reports(ex), 1)
	})

	t.Run("refused while another update runs", func(t *testing.T) {
		useFakeFlockOS(t, true, &fakeFlockOS{})
		installs := fakeInstall(t, firmware.Result{}, nil)
		unlock, ok := system.TryLockUpdate()
		require.True(t, ok)
		defer unlock()
		ex, _ := flockOSExternal(t)
		_, err := ex.installFirmwareUpdateHandler(context.Background(), installArgs("a", "2"))
		require.ErrorIs(t, err, system.ErrUpdateRunning)
		assert.Empty(t, *installs)
	})

	t.Run("an install holds off an OS update, and lets go afterwards", func(t *testing.T) {
		useFakeFlockOS(t, true, &fakeFlockOS{})
		withFirmware(t, inventory, nil)
		old := installFirmware
		t.Cleanup(func() { installFirmware = old })
		var lockedDuring bool
		installFirmware = func(context.Context, string, string, string, func(firmware.Progress)) (firmware.Result, error) {
			unlock, ok := system.TryLockUpdate()
			if ok {
				unlock()
			}
			lockedDuring = !ok
			return firmware.Result{Version: "2"}, nil
		}
		ex, _ := flockOSExternal(t)
		_, err := ex.installFirmwareUpdateHandler(context.Background(), installArgs("a", "2"))
		require.NoError(t, err)
		assert.True(t, lockedDuring)
		unlock, ok := system.TryLockUpdate()
		require.True(t, ok)
		unlock()
	})

	for name, args := range map[string]messenger.Result{
		"no version":   installArgs("a", ""),
		"no device id": installArgs("", "2"),
	} {
		t.Run("refused with "+name, func(t *testing.T) {
			useFakeFlockOS(t, true, &fakeFlockOS{})
			installs := fakeInstall(t, firmware.Result{}, nil)
			ex, _ := flockOSExternal(t)
			_, err := ex.installFirmwareUpdateHandler(context.Background(), args)
			require.Error(t, err)
			assert.Empty(t, *installs)
		})
	}

	t.Run("refused without MAINTAIN", func(t *testing.T) {
		useFakeFlockOS(t, true, &fakeFlockOS{})
		installs := fakeInstall(t, firmware.Result{}, nil)
		ex, _ := flockOSExternal(t)
		details, m := grantPrivilege(false)
		ex.Privilege = newPrivilege(testConfig(), m)
		args := installArgs("a", "2")
		args.Details = details
		_, err := ex.installFirmwareUpdateHandler(context.Background(), args)
		require.Error(t, err)
		assert.True(t, errdefs.IsInsufficientPrivileges(err))
		assert.Empty(t, *installs)
	})

	t.Run("not on another OS", func(t *testing.T) {
		useFakeFlockOS(t, false, &fakeFlockOS{})
		installs := fakeInstall(t, firmware.Result{}, nil)
		ex, _ := flockOSExternal(t)
		_, err := ex.installFirmwareUpdateHandler(context.Background(), installArgs("a", "2"))
		require.ErrorIs(t, err, system.ErrNotFlockOS)
		assert.Empty(t, *installs)
	})
}
