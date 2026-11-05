package api

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"reagent/common"
	"reagent/errdefs"
	"reagent/filesystem"
	"reagent/messenger"
	"reagent/osupdate"
	"reagent/system"
	"reagent/testutil/fakes"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeFlockOS replaces the system calls of the FlockOS handlers.
type fakeFlockOS struct {
	mu          sync.Mutex
	offerErr    error
	downloadErr error
	installErr  error
	slotsErr    error
	steps       []string
	rebooted    chan struct{}
}

func useFakeFlockOS(t *testing.T, flockOS bool, f *fakeFlockOS) {
	t.Helper()
	f.rebooted = make(chan struct{}, 1)
	old := struct {
		runs     func() bool
		offer    func(*system.System) (osupdate.Running, osupdate.Offer, error)
		download func(*system.System, context.Context, func(filesystem.DownloadProgress)) (string, error)
		install  func(*system.System, func(string, uint64)) error
		reboot   func(*system.System) error
		slots    func(context.Context) (string, string, []system.Slot, error)
		delay    time.Duration
	}{runsFlockOS, flockOSOffer, downloadFlockOS, installFlockOS, rebootSystem, flockOSSlots, rebootDelay}
	t.Cleanup(func() {
		runsFlockOS, flockOSOffer, downloadFlockOS, installFlockOS = old.runs, old.offer, old.download, old.install
		rebootSystem, flockOSSlots, rebootDelay = old.reboot, old.slots, old.delay
	})
	rebootDelay = 0
	runsFlockOS = func() bool { return flockOS }
	flockOSOffer = func(*system.System) (osupdate.Running, osupdate.Offer, error) {
		running := osupdate.Running{ID: "flockos", ImageID: "flockos-amd64-efi-base", ImageVersion: "1.0.0", BuildID: "20261120T095512Z-g1a2b3c4"}
		if f.offerErr != nil {
			return running, osupdate.Offer{}, f.offerErr
		}
		return running, osupdate.Offer{Channel: "test", Version: "1.0.1", Newer: true,
			Release: osupdate.Release{BuildID: "20261130T141003Z-g5d6e7f8", BuildTime: "2026-11-30T14:10:03Z"}}, nil
	}
	downloadFlockOS = func(_ *system.System, _ context.Context, p func(filesystem.DownloadProgress)) (string, error) {
		f.step("download")
		if f.downloadErr != nil {
			return "", f.downloadErr
		}
		p(filesystem.DownloadProgress{Increment: 10, CurrentBytes: 10, TotalFileSize: 10})
		return "/var/cache/flockos/flockos-amd64-efi-base-1.0.1.raucb", nil
	}
	installFlockOS = func(_ *system.System, p func(string, uint64)) error {
		f.step("install")
		if f.installErr != nil {
			return f.installErr
		}
		p("Installing", 100)
		return nil
	}
	rebootSystem = func(*system.System) error {
		f.step("reboot")
		f.rebooted <- struct{}{}
		return nil
	}
	flockOSSlots = func(context.Context) (string, string, []system.Slot, error) {
		if f.slotsErr != nil {
			return "", "", nil, f.slotsErr
		}
		return "rootfs.0", "rootfs.0", []system.Slot{
			{Name: "rootfs.0", Class: "rootfs", Bootname: "A", State: "booted", BootStatus: "good", BundleVersion: "1.0.0"},
			{Name: "rootfs.1", Class: "rootfs", Bootname: "B", State: "inactive", BootStatus: "good"},
		}, nil
	}
}

func (f *fakeFlockOS) step(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps = append(f.steps, s)
}

func (f *fakeFlockOS) done() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.steps...)
}

func flockOSExternal(t *testing.T) (*External, *fakes.Messenger) {
	t.Helper()
	logs := fakes.NewMessenger()
	cfg := testConfig()
	cfg.ReswarmConfig.SerialNumber = "SER-1"
	return &External{Config: cfg, Privilege: priv(t, true), LogMessenger: logs, System: &system.System{}}, logs
}

func TestBuildTime(t *testing.T) {
	assert.Equal(t, "2026-11-20T09:55:12Z", buildTime("20261120T095512Z-g1a2b3c4"))
	assert.Equal(t, "", buildTime("v1.0.0"))
}

func TestGetOSReleaseOnFlockOS(t *testing.T) {
	t.Run("current, latest and the slots", func(t *testing.T) {
		useFakeFlockOS(t, true, &fakeFlockOS{})
		ex, _ := flockOSExternal(t)
		res, err := ex.getOSReleaseHandler(context.Background(), messenger.Result{Details: systemDetails()})
		require.NoError(t, err)
		dict := res.Arguments[0].(common.Dict)
		current := dict["currentRelease"].(common.Dict)
		assert.Equal(t, "FlockOS", current["Name"])
		assert.Equal(t, "1.0.0", current["Version"])
		assert.Equal(t, "2026-11-20T09:55:12Z", current["BuildTime"])
		assert.Equal(t, "flockos-amd64-efi-base", current["ImageID"])
		latest := dict["latestRelease"].(common.Dict)
		assert.Equal(t, "1.0.1", latest["Version"])
		assert.Equal(t, "test", latest["Channel"])
		assert.Equal(t, true, latest["UpdateAvailable"])
		slots := dict["slots"].(common.Dict)
		assert.Equal(t, "rootfs.0", slots["booted"])
		assert.Len(t, slots["slots"], 2)
	})

	t.Run("a catalog or RAUC that fails leaves its error, not a failed call", func(t *testing.T) {
		useFakeFlockOS(t, true, &fakeFlockOS{offerErr: errors.New("refusing the catalog"), slotsErr: errors.New("no RAUC")})
		ex, _ := flockOSExternal(t)
		res, err := ex.getOSReleaseHandler(context.Background(), messenger.Result{Details: systemDetails()})
		require.NoError(t, err)
		dict := res.Arguments[0].(common.Dict)
		assert.Equal(t, "1.0.0", dict["currentRelease"].(common.Dict)["Version"])
		assert.Contains(t, dict["latestRelease"].(common.Dict)["Error"], "refusing the catalog")
		assert.Contains(t, dict["slotsError"], "no RAUC")
	})
}

func TestDownloadAndInstallOnFlockOS(t *testing.T) {
	t.Run("download returns the bundle's path, with progress", func(t *testing.T) {
		useFakeFlockOS(t, true, &fakeFlockOS{})
		ex, logs := flockOSExternal(t)
		res, err := ex.downloadOSUpdateHandler(context.Background(), messenger.Result{Details: systemDetails()})
		require.NoError(t, err)
		assert.Equal(t, "/var/cache/flockos/flockos-amd64-efi-base-1.0.1.raucb", res.Arguments[0].(common.Dict)["path"])
		assert.Equal(t, 1, logs.GetPublishCount())
	})

	t.Run("RAUC's error reaches the caller", func(t *testing.T) {
		useFakeFlockOS(t, true, &fakeFlockOS{installErr: errors.New("RAUC: Compatible mismatch")})
		ex, _ := flockOSExternal(t)
		_, err := ex.installOSUpdateHandler(context.Background(), messenger.Result{Details: systemDetails()})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Compatible mismatch")
	})
}

func TestPerformOSUpdate(t *testing.T) {
	t.Run("download, install, then reboot after the answer", func(t *testing.T) {
		f := &fakeFlockOS{}
		useFakeFlockOS(t, true, f)
		ex, logs := flockOSExternal(t)
		res, err := ex.performOSUpdateHandler(context.Background(), messenger.Result{Details: systemDetails()})
		require.NoError(t, err)
		assert.Equal(t, true, res.Arguments[0].(common.Dict)["rebooting"])
		select {
		case <-f.rebooted:
		case <-time.After(5 * time.Second):
			t.Fatal("no reboot")
		}
		assert.Equal(t, []string{"download", "install", "reboot"}, f.done())
		var stages []interface{}
		for _, c := range logs.GetPublishCalls() {
			assert.Equal(t, "re.mgmt.SER-1.perform_os_update_progress", string(c.Topic))
			stages = append(stages, c.Args[0].(common.Dict)["stage"])
		}
		assert.Equal(t, []interface{}{"download", "install", "reboot"}, stages)
	})

	t.Run("a failed install does not reboot", func(t *testing.T) {
		f := &fakeFlockOS{installErr: errors.New("RAUC: signature")}
		useFakeFlockOS(t, true, f)
		ex, _ := flockOSExternal(t)
		_, err := ex.performOSUpdateHandler(context.Background(), messenger.Result{Details: systemDetails()})
		require.Error(t, err)
		time.Sleep(50 * time.Millisecond)
		assert.Equal(t, []string{"download", "install"}, f.done())
	})

	t.Run("refused off FlockOS", func(t *testing.T) {
		f := &fakeFlockOS{}
		useFakeFlockOS(t, false, f)
		ex, _ := flockOSExternal(t)
		_, err := ex.performOSUpdateHandler(context.Background(), messenger.Result{Details: systemDetails()})
		assert.ErrorIs(t, err, system.ErrNotFlockOS)
		assert.Empty(t, f.done())
	})

	t.Run("refused without MAINTAIN", func(t *testing.T) {
		f := &fakeFlockOS{}
		useFakeFlockOS(t, true, f)
		details, m := grantPrivilege(false)
		ex, _ := flockOSExternal(t)
		ex.Privilege = newPrivilege(testConfig(), m)
		_, err := ex.performOSUpdateHandler(context.Background(), messenger.Result{Details: details})
		require.Error(t, err)
		assert.True(t, errdefs.IsInsufficientPrivileges(err))
		assert.Equal(t, "MAINTAIN", requestedPrivilege(t, m))
		assert.Empty(t, f.done())
	})
}
