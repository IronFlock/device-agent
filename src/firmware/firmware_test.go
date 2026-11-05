package firmware

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/prop"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func variants(m map[string]interface{}) map[string]dbus.Variant {
	out := map[string]dbus.Variant{}
	for k, v := range m {
		out[k] = dbus.MakeVariant(v)
	}
	return out
}

func TestDecodeDevice(t *testing.T) {
	t.Run("the fields and the flags fwupd sends", func(t *testing.T) {
		d := decodeDevice(variants(map[string]interface{}{
			"DeviceId":          "a45df35ac0e948ee180fe216a5f703f32dda163f",
			"Name":              "System Firmware",
			"Vendor":            "QEMU",
			"Version":           "0.0.0",
			"VersionBootloader": "1.2",
			"Plugin":            "uefi_capsule",
			"Protocol":          "org.uefi.capsule",
			"Guid":              []string{"cb1dbd8c-cfe0-4de4-b8e4-1bd1e4e1a9a0"},
			// internal, updatable, needs-reboot, and bit 2 (registered), not shown
			"Flags":       uint64(1<<0 | 1<<1 | 1<<2 | 1<<8),
			"UpdateState": uint32(2),
		}))
		assert.Equal(t, "System Firmware", d.Name)
		assert.Equal(t, "QEMU", d.Vendor)
		assert.Equal(t, "1.2", d.VersionBootloader)
		assert.Equal(t, []string{"cb1dbd8c-cfe0-4de4-b8e4-1bd1e4e1a9a0"}, d.GUIDs)
		assert.Equal(t, []string{"internal", "updatable", "needs-reboot"}, d.Flags)
		assert.True(t, d.Has("updatable"))
		assert.False(t, d.Has("locked"))
		assert.Equal(t, "success", d.UpdateState)
	})

	t.Run("numbers of either width", func(t *testing.T) {
		d := decodeDevice(variants(map[string]interface{}{"Flags": uint32(1 << 1), "UpdateState": uint64(3)}))
		assert.Equal(t, []string{"updatable"}, d.Flags)
		assert.Equal(t, "failed", d.UpdateState)
	})

	t.Run("missing keys, unknown states and odd types stay empty", func(t *testing.T) {
		d := decodeDevice(variants(map[string]interface{}{"Name": int32(7), "Guid": "not a list", "UpdateState": uint32(99)}))
		assert.Equal(t, Device{GUIDs: []string{}, Flags: []string{}}, d)
	})
}

// --- fwupd on a private bus --------------------------------------------------

type fakeFwupd struct {
	devices []map[string]dbus.Variant
	err     *dbus.Error
	delay   time.Duration
}

func (f *fakeFwupd) GetDevices() ([]map[string]dbus.Variant, *dbus.Error) {
	time.Sleep(f.delay)
	if f.err != nil {
		return nil, f.err
	}
	return f.devices, nil
}

// privateBus starts a dbus-daemon of the test's own and returns its address.
func privateBus(t *testing.T) string {
	t.Helper()
	daemon, err := exec.LookPath("dbus-daemon")
	if err != nil {
		t.Skip("dbus-daemon not installed")
	}
	dir, err := os.MkdirTemp("", "fwbus")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	conf := filepath.Join(dir, "bus.conf")
	require.NoError(t, os.WriteFile(conf, []byte(`<!DOCTYPE busconfig PUBLIC "-//freedesktop//DTD D-Bus Bus Configuration 1.0//EN"
 "http://www.freedesktop.org/standards/dbus/1.0/busconfig.dtd">
<busconfig>
  <type>session</type>
  <listen>unix:dir=`+dir+`</listen>
  <auth>EXTERNAL</auth>
  <policy context="default">
    <allow send_destination="*" eavesdrop="true"/>
    <allow eavesdrop="true"/>
    <allow own="*"/>
  </policy>
</busconfig>
`), 0o600))
	cmd := exec.Command(daemon, "--config-file="+conf, "--print-address", "--nofork", "--nopidfile")
	out, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	addr, err := bufio.NewReader(out).ReadString('\n')
	require.NoError(t, err)
	return strings.TrimSpace(addr)
}

// serve puts fake on the bus at addr as fwupd, with its daemon properties.
func serve(t *testing.T, addr string, fake *fakeFwupd) {
	t.Helper()
	conn, err := dbus.Connect(addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.Export(fake, objectPath, iface))
	_, err = prop.Export(conn, objectPath, prop.Map{iface: {
		"DaemonVersion": {Value: "2.0.20", Emit: prop.EmitFalse},
		"HostProduct":   {Value: "Standard PC (Q35 + ICH9, 2009)", Emit: prop.EmitFalse},
	}})
	require.NoError(t, err)
	reply, err := conn.RequestName(busName, dbus.NameFlagDoNotQueue)
	require.NoError(t, err)
	require.Equal(t, dbus.RequestNameReplyPrimaryOwner, reply)
}

func client(t *testing.T, addr string) *dbus.Conn {
	t.Helper()
	conn, err := dbus.Connect(addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestReadFromFwupd(t *testing.T) {
	t.Run("devices and the daemon's version", func(t *testing.T) {
		addr := privateBus(t)
		serve(t, addr, &fakeFwupd{devices: []map[string]dbus.Variant{
			variants(map[string]interface{}{"DeviceId": "2", "ParentDeviceId": "1", "Name": "Disk", "Version": "4.1", "Flags": uint64(1 << 1)}),
			variants(map[string]interface{}{"DeviceId": "1", "Name": "System Firmware", "Version": "0.0.0", "Flags": uint64(1 << 0)}),
		}})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		inv, err := read(ctx, client(t, addr))
		require.NoError(t, err)
		assert.Equal(t, "2.0.20", inv.FwupdVersion)
		assert.Equal(t, "Standard PC (Q35 + ICH9, 2009)", inv.HostProduct)
		assert.Equal(t, "", inv.HostVendor)
		require.Len(t, inv.Devices, 2)
		assert.Equal(t, "System Firmware", inv.Devices[0].Name, "parents first")
		assert.Equal(t, "Disk", inv.Devices[1].Name)
		assert.Equal(t, []string{"updatable"}, inv.Devices[1].Flags)
	})

	t.Run("no device is an empty inventory, not an error", func(t *testing.T) {
		addr := privateBus(t)
		serve(t, addr, &fakeFwupd{err: dbus.NewError(errNothingToDo, []interface{}{"No detected devices"})})
		inv, err := read(context.Background(), client(t, addr))
		require.NoError(t, err)
		assert.Empty(t, inv.Devices)
		assert.NotNil(t, inv.Devices)
	})

	t.Run("no fwupd on the bus", func(t *testing.T) {
		addr := privateBus(t)
		_, err := read(context.Background(), client(t, addr))
		assert.ErrorIs(t, err, ErrUnavailable)
	})

	t.Run("another fwupd error is returned", func(t *testing.T) {
		addr := privateBus(t)
		serve(t, addr, &fakeFwupd{err: dbus.NewError("org.freedesktop.fwupd.Internal", []interface{}{"boom"})})
		_, err := read(context.Background(), client(t, addr))
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrUnavailable)
		assert.Contains(t, err.Error(), "boom")
	})

	t.Run("a fwupd that does not answer is given up at the deadline", func(t *testing.T) {
		addr := privateBus(t)
		serve(t, addr, &fakeFwupd{delay: 5 * time.Second})
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := read(ctx, client(t, addr))
		require.Error(t, err)
		assert.Less(t, time.Since(start), 2*time.Second)
	})
}
