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
	"reagent/messenger/topics"
	"reagent/osupdate"
	"reagent/system"
	"reagent/testutil/fakes"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeIronFlockOS replaces the system calls of the IronFlockOS handlers.
type fakeIronFlockOS struct {
	mu          sync.Mutex
	offerErr    error
	downloadErr error
	installErr  error
	slotsErr    error
	runningErr  error
	steps       []string
	rebooted    chan struct{}
}

func useFakeIronFlockOS(t *testing.T, ironFlockOS bool, f *fakeIronFlockOS) {
	t.Helper()
	f.rebooted = make(chan struct{}, 1)
	old := struct {
		runs     func() bool
		offer    func(*system.System) (osupdate.Running, osupdate.Offer, error)
		download func(*system.System, context.Context, func(filesystem.DownloadProgress)) (string, error)
		install  func(*system.System, func(string, uint64)) error
		reboot   func(*system.System) error
		slots    func(context.Context) (string, string, []system.Slot, error)
		running  func() (osupdate.Running, error)
		delay    time.Duration
	}{runsIronFlockOS, ironFlockOSOffer, downloadIronFlockOS, installIronFlockOS, rebootSystem, ironFlockOSSlots, readRunning, rebootDelay}
	t.Cleanup(func() {
		runsIronFlockOS, ironFlockOSOffer, downloadIronFlockOS, installIronFlockOS = old.runs, old.offer, old.download, old.install
		rebootSystem, ironFlockOSSlots, readRunning, rebootDelay = old.reboot, old.slots, old.running, old.delay
	})
	rebootDelay = 0
	runsIronFlockOS = func() bool { return ironFlockOS }
	readRunning = func() (osupdate.Running, error) {
		if f.runningErr != nil {
			return osupdate.Running{}, f.runningErr
		}
		if !ironFlockOS {
			return osupdate.Running{ID: "debian"}, nil
		}
		return osupdate.Running{ID: "ironflockos", ImageID: "ironflockos-amd64-efi-base", ImageVersion: "1.0.0", BuildID: "20261120T095512Z-g1a2b3c4"}, nil
	}
	ironFlockOSOffer = func(*system.System) (osupdate.Running, osupdate.Offer, error) {
		running := osupdate.Running{ID: "ironflockos", ImageID: "ironflockos-amd64-efi-base", ImageVersion: "1.0.0", BuildID: "20261120T095512Z-g1a2b3c4"}
		if f.offerErr != nil {
			return running, osupdate.Offer{}, f.offerErr
		}
		return running, osupdate.Offer{Channel: "test", Version: "1.0.1", Newer: true,
			Release: osupdate.Release{BuildID: "20261130T141003Z-g5d6e7f8", BuildTime: "2026-11-30T14:10:03Z"}}, nil
	}
	downloadIronFlockOS = func(_ *system.System, _ context.Context, p func(filesystem.DownloadProgress)) (string, error) {
		f.step("download")
		if f.downloadErr != nil {
			return "", f.downloadErr
		}
		p(filesystem.DownloadProgress{Increment: 10, CurrentBytes: 10, TotalFileSize: 10})
		return "/var/cache/ironflockos/ironflockos-amd64-efi-base-1.0.1.raucb", nil
	}
	installIronFlockOS = func(_ *system.System, p func(string, uint64)) error {
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
	ironFlockOSSlots = func(context.Context) (string, string, []system.Slot, error) {
		if f.slotsErr != nil {
			return "", "", nil, f.slotsErr
		}
		return "rootfs.0", "rootfs.0", []system.Slot{
			{Name: "rootfs.0", Class: "rootfs", Bootname: "A", State: "booted", BootStatus: "good", BundleVersion: "1.0.0"},
			{Name: "rootfs.1", Class: "rootfs", Bootname: "B", State: "inactive", BootStatus: "good", ImageVersion: "0.9.0", BuildID: "20261107T122204Z-gef21459"},
		}, nil
	}
}

func (f *fakeIronFlockOS) step(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps = append(f.steps, s)
}

func (f *fakeIronFlockOS) done() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.steps...)
}

func ironFlockOSExternal(t *testing.T) (*External, *fakes.Messenger) {
	t.Helper()
	logs := fakes.NewMessenger()
	cfg := testConfig()
	cfg.ReswarmConfig.SerialNumber = "SER-1"
	sys := system.New(cfg, nil)
	return &External{Config: cfg, Privilege: priv(t, true), Messenger: fakes.NewMessenger(), LogMessenger: logs, System: &sys}, logs
}

// reportedOSRelease waits for the report the agent sends in the background.
func reportedOSRelease(t *testing.T, ex *External) common.Dict {
	t.Helper()
	m := ex.Messenger.(*fakes.Messenger)
	require.Eventually(t, func() bool { return len(m.GetCallCalls()) > 0 }, 2*time.Second, 5*time.Millisecond)
	call := m.GetCallCalls()[0]
	assert.Equal(t, topics.UpdateDeviceArchitecture, call.Topic)
	return call.Args[0].(common.Dict)["os_release"].(common.Dict)
}

func TestBuildTime(t *testing.T) {
	assert.Equal(t, "2026-11-20T09:55:12Z", buildTime("20261120T095512Z-g1a2b3c4"))
	assert.Equal(t, "", buildTime("v1.0.0"))
}

func TestGetOSReleaseOnIronFlockOS(t *testing.T) {
	t.Run("current, latest and the slots", func(t *testing.T) {
		useFakeIronFlockOS(t, true, &fakeIronFlockOS{})
		ex, _ := ironFlockOSExternal(t)
		res, err := ex.getOSReleaseHandler(context.Background(), messenger.Result{Details: systemDetails()})
		require.NoError(t, err)
		dict := res.Arguments[0].(common.Dict)
		current := dict["currentRelease"].(common.Dict)
		assert.Equal(t, "IronFlockOS", current["Name"])
		assert.Equal(t, "1.0.0", current["Version"])
		assert.Equal(t, "2026-11-20T09:55:12Z", current["BuildTime"])
		assert.Equal(t, "ironflockos-amd64-efi-base", current["ImageID"])
		latest := dict["latestRelease"].(common.Dict)
		assert.Equal(t, "1.0.1", latest["Version"])
		assert.Equal(t, "test", latest["Channel"])
		assert.Equal(t, true, latest["UpdateAvailable"])
		slots := dict["slots"].(common.Dict)
		assert.Equal(t, "rootfs.0", slots["booted"])
		require.Len(t, slots["slots"], 2)
		// A flashed slot: no bundle version from RAUC, the image's own.
		b := slots["slots"].([]common.Dict)[1]
		assert.Equal(t, "", b["bundle_version"])
		assert.Equal(t, "0.9.0", b["image_version"])
		assert.Equal(t, "20261107T122204Z-gef21459", b["build_id"])
	})

	t.Run("a catalog or RAUC that fails leaves its error, not a failed call", func(t *testing.T) {
		useFakeIronFlockOS(t, true, &fakeIronFlockOS{offerErr: errors.New("refusing the catalog"), slotsErr: errors.New("no RAUC")})
		ex, _ := ironFlockOSExternal(t)
		res, err := ex.getOSReleaseHandler(context.Background(), messenger.Result{Details: systemDetails()})
		require.NoError(t, err)
		dict := res.Arguments[0].(common.Dict)
		assert.Equal(t, "1.0.0", dict["currentRelease"].(common.Dict)["Version"])
		assert.Contains(t, dict["latestRelease"].(common.Dict)["Error"], "refusing the catalog")
		assert.Contains(t, dict["slotsError"], "no RAUC")
	})
}

func TestOSReleaseReport(t *testing.T) {
	t.Run("IronFlockOS: the running image and the slots, no release offered", func(t *testing.T) {
		useFakeIronFlockOS(t, true, &fakeIronFlockOS{offerErr: errors.New("must not be asked")})
		report, ok := OSReleaseReport(context.Background())
		require.True(t, ok)
		assert.Equal(t, "1.0.0", report["currentRelease"].(common.Dict)["Version"])
		assert.Equal(t, "ironflockos-amd64-efi-base", report["currentRelease"].(common.Dict)["ImageID"])
		assert.Equal(t, "rootfs.0", report["slots"].(common.Dict)["booted"])
		require.Len(t, report["slots"].(common.Dict)["slots"], 2)
		assert.NotContains(t, report, "latestRelease")
	})

	t.Run("RAUC that fails leaves its error beside the running image", func(t *testing.T) {
		useFakeIronFlockOS(t, true, &fakeIronFlockOS{slotsErr: errors.New("no RAUC")})
		report, ok := OSReleaseReport(context.Background())
		require.True(t, ok)
		assert.Equal(t, "1.0.0", report["currentRelease"].(common.Dict)["Version"])
		assert.Contains(t, report["slotsError"], "no RAUC")
		assert.NotContains(t, report, "slots")
	})

	t.Run("another OS reports nothing to show, which clears what was known", func(t *testing.T) {
		useFakeIronFlockOS(t, false, &fakeIronFlockOS{})
		report, ok := OSReleaseReport(context.Background())
		require.True(t, ok)
		assert.Empty(t, report)
	})

	t.Run("no os-release (Windows) leaves the report out", func(t *testing.T) {
		useFakeIronFlockOS(t, true, &fakeIronFlockOS{runningErr: errors.New("no such file")})
		_, ok := OSReleaseReport(context.Background())
		assert.False(t, ok)
	})
}

func TestFailedInstallReportsTheSlots(t *testing.T) {
	t.Run("perform_os_update", func(t *testing.T) {
		useFakeIronFlockOS(t, true, &fakeIronFlockOS{installErr: errors.New("RAUC: signature verification failed")})
		ex, _ := ironFlockOSExternal(t)
		_, err := ex.performOSUpdateHandler(context.Background(), messenger.Result{Details: systemDetails()})
		require.Error(t, err)
		report := reportedOSRelease(t, ex)
		assert.Equal(t, "rootfs.0", report["slots"].(common.Dict)["booted"])
	})

	t.Run("install_os_update", func(t *testing.T) {
		useFakeIronFlockOS(t, true, &fakeIronFlockOS{installErr: errors.New("RAUC: Compatible mismatch")})
		ex, _ := ironFlockOSExternal(t)
		_, err := ex.installOSUpdateHandler(context.Background(), messenger.Result{Details: systemDetails()})
		require.Error(t, err)
		reportedOSRelease(t, ex)
	})

	t.Run("a failed download changes no slot and reports nothing", func(t *testing.T) {
		useFakeIronFlockOS(t, true, &fakeIronFlockOS{downloadErr: errors.New("no network")})
		ex, _ := ironFlockOSExternal(t)
		_, err := ex.performOSUpdateHandler(context.Background(), messenger.Result{Details: systemDetails()})
		require.Error(t, err)
		time.Sleep(20 * time.Millisecond)
		assert.Empty(t, ex.Messenger.(*fakes.Messenger).GetCallCalls())
	})
}

func TestDownloadAndInstallOnIronFlockOS(t *testing.T) {
	t.Run("download returns the bundle's path, with progress", func(t *testing.T) {
		useFakeIronFlockOS(t, true, &fakeIronFlockOS{})
		ex, logs := ironFlockOSExternal(t)
		res, err := ex.downloadOSUpdateHandler(context.Background(), messenger.Result{Details: systemDetails()})
		require.NoError(t, err)
		assert.Equal(t, "/var/cache/ironflockos/ironflockos-amd64-efi-base-1.0.1.raucb", res.Arguments[0].(common.Dict)["path"])
		assert.Equal(t, 1, logs.GetPublishCount())
	})

	t.Run("RAUC's error reaches the caller", func(t *testing.T) {
		useFakeIronFlockOS(t, true, &fakeIronFlockOS{installErr: errors.New("RAUC: Compatible mismatch")})
		ex, _ := ironFlockOSExternal(t)
		_, err := ex.installOSUpdateHandler(context.Background(), messenger.Result{Details: systemDetails()})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Compatible mismatch")
		reportedOSRelease(t, ex)
	})
}

func TestPerformOSUpdate(t *testing.T) {
	t.Run("download, install, then reboot after the answer", func(t *testing.T) {
		f := &fakeIronFlockOS{}
		useFakeIronFlockOS(t, true, f)
		ex, logs := ironFlockOSExternal(t)
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
		f := &fakeIronFlockOS{installErr: errors.New("RAUC: signature")}
		useFakeIronFlockOS(t, true, f)
		ex, _ := ironFlockOSExternal(t)
		_, err := ex.performOSUpdateHandler(context.Background(), messenger.Result{Details: systemDetails()})
		require.Error(t, err)
		reportedOSRelease(t, ex)
		time.Sleep(50 * time.Millisecond)
		assert.Equal(t, []string{"download", "install"}, f.done())
	})

	t.Run("refused off IronFlockOS", func(t *testing.T) {
		f := &fakeIronFlockOS{}
		useFakeIronFlockOS(t, false, f)
		ex, _ := ironFlockOSExternal(t)
		_, err := ex.performOSUpdateHandler(context.Background(), messenger.Result{Details: systemDetails()})
		assert.ErrorIs(t, err, system.ErrNotIronFlockOS)
		assert.Empty(t, f.done())
	})

	t.Run("refused without MAINTAIN", func(t *testing.T) {
		f := &fakeIronFlockOS{}
		useFakeIronFlockOS(t, true, f)
		details, m := grantPrivilege(false)
		ex, _ := ironFlockOSExternal(t)
		ex.Privilege = newPrivilege(testConfig(), m)
		_, err := ex.performOSUpdateHandler(context.Background(), messenger.Result{Details: details})
		require.Error(t, err)
		assert.True(t, errdefs.IsInsufficientPrivileges(err))
		assert.Equal(t, "MAINTAIN", requestedPrivilege(t, m))
		assert.Empty(t, f.done())
	})
}
