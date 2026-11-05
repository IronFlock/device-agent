// Package firmware reads the device's firmware inventory from fwupd, over its
// D-Bus API (org.freedesktop.fwupd on the system bus). Read-only: FlockOS
// ships boot firmware with its OS releases, and component firmware through
// fwupd starts as an inventory the platform shows (FlockOS ADR-0009). Nothing
// here runs before the connection: Read is called per request, opens its own
// bus connection and gives up after its context's deadline.
package firmware

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/godbus/dbus/v5"
)

const (
	busName    = "org.freedesktop.fwupd"
	objectPath = dbus.ObjectPath("/")
	iface      = "org.freedesktop.fwupd"

	// fwupd answers GetDevices with this error when it has no device.
	errNothingToDo = "org.freedesktop.fwupd.NothingToDo"
	// The bus has no fwupd: not installed, or not activatable.
	errServiceUnknown = "org.freedesktop.DBus.Error.ServiceUnknown"
)

// ErrUnavailable is returned when the device has no fwupd to ask: no system
// bus (Windows, macOS, a container) or no fwupd on it.
var ErrUnavailable = errors.New("fwupd is not available on this device")

// Device is one device of fwupd's inventory.
type Device struct {
	ID                string
	ParentID          string
	Name              string
	Summary           string
	Vendor            string
	Version           string
	VersionLowest     string
	VersionBootloader string
	Plugin            string
	Protocol          string
	GUIDs             []string
	Flags             []string
	UpdateState       string
	UpdateError       string
}

// Inventory is what fwupd reports for the whole device.
type Inventory struct {
	FwupdVersion string
	HostVendor   string
	HostProduct  string
	Devices      []Device
}

// Read asks fwupd on the system bus for its devices. fwupd is D-Bus
// activated and probes its devices when it starts, so the first call can take
// a while; ctx bounds it.
func Read(ctx context.Context) (Inventory, error) {
	conn, err := dbus.ConnectSystemBus(dbus.WithContext(ctx))
	if err != nil {
		return Inventory{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer func() { _ = conn.Close() }()
	return read(ctx, conn)
}

// read asks fwupd on conn.
func read(ctx context.Context, conn *dbus.Conn) (Inventory, error) {
	obj := conn.Object(busName, objectPath)

	var raw []map[string]dbus.Variant
	err := obj.CallWithContext(ctx, iface+".GetDevices", 0).Store(&raw)
	var dbusErr dbus.Error
	switch {
	case err == nil:
	case errors.As(err, &dbusErr) && dbusErr.Name == errNothingToDo:
		raw = nil
	case errors.As(err, &dbusErr) && dbusErr.Name == errServiceUnknown:
		return Inventory{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	default:
		return Inventory{}, fmt.Errorf("fwupd GetDevices: %w", err)
	}

	inv := Inventory{
		FwupdVersion: property(ctx, obj, "DaemonVersion"),
		HostVendor:   property(ctx, obj, "HostVendor"),
		HostProduct:  property(ctx, obj, "HostProduct"),
		Devices:      make([]Device, 0, len(raw)),
	}
	for _, d := range raw {
		inv.Devices = append(inv.Devices, decodeDevice(d))
	}
	// Parents before their children, then by name: a stable order for the
	// platform's view.
	sort.SliceStable(inv.Devices, func(i, j int) bool {
		a, b := inv.Devices[i], inv.Devices[j]
		if (a.ParentID == "") != (b.ParentID == "") {
			return a.ParentID == ""
		}
		return a.Name < b.Name
	})
	return inv, nil
}

// property reads a string property of fwupd's daemon object; "" when it is
// missing (older fwupd) or not a string.
func property(ctx context.Context, obj dbus.BusObject, name string) string {
	var v dbus.Variant
	if err := obj.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, iface, name).Store(&v); err != nil {
		return ""
	}
	s, _ := v.Value().(string)
	return s
}

// Device flags (FWUPD_DEVICE_FLAG_*, libfwupd/fwupd-enums.h of fwupd 2.0)
// that the platform shows. Other bits are left out.
var flagNames = []struct {
	bit  uint
	name string
}{
	{0, "internal"},
	{1, "updatable"},
	{3, "require-ac"},
	{4, "locked"},
	{5, "supported"},
	{6, "needs-bootloader"},
	{8, "needs-reboot"},
	{17, "needs-shutdown"},
	{37, "updatable-hidden"},
	{49, "emulated"},
}

// UpdateState values (FWUPD_UPDATE_STATE_*): the result of the last update.
var updateStates = []string{"", "pending", "success", "failed", "needs-reboot", "failed-transient"}

// decodeDevice turns one a{sv} of GetDevices into a Device. Missing keys
// and values of an unexpected type stay empty.
func decodeDevice(d map[string]dbus.Variant) Device {
	dev := Device{
		ID:                str(d, "DeviceId"),
		ParentID:          str(d, "ParentDeviceId"),
		Name:              str(d, "Name"),
		Summary:           str(d, "Summary"),
		Vendor:            str(d, "Vendor"),
		Version:           str(d, "Version"),
		VersionLowest:     str(d, "VersionLowest"),
		VersionBootloader: str(d, "VersionBootloader"),
		Plugin:            str(d, "Plugin"),
		Protocol:          str(d, "Protocol"),
		GUIDs:             strs(d, "Guid"),
		UpdateError:       str(d, "UpdateError"),
	}
	flags := uint64Of(d, "Flags")
	dev.Flags = []string{}
	for _, f := range flagNames {
		if flags&(1<<f.bit) != 0 {
			dev.Flags = append(dev.Flags, f.name)
		}
	}
	if s := uint64Of(d, "UpdateState"); s < uint64(len(updateStates)) {
		dev.UpdateState = updateStates[s]
	}
	return dev
}

func str(d map[string]dbus.Variant, key string) string {
	v, ok := d[key]
	if !ok {
		return ""
	}
	s, _ := v.Value().(string)
	return s
}

func strs(d map[string]dbus.Variant, key string) []string {
	v, ok := d[key]
	if !ok {
		return []string{}
	}
	s, ok := v.Value().([]string)
	if !ok {
		return []string{}
	}
	return s
}

// uint64Of reads an unsigned number, whatever width fwupd sends it in.
func uint64Of(d map[string]dbus.Variant, key string) uint64 {
	v, ok := d[key]
	if !ok {
		return 0
	}
	switch n := v.Value().(type) {
	case uint64:
		return n
	case uint32:
		return uint64(n)
	case uint16:
		return uint64(n)
	case byte:
		return uint64(n)
	}
	return 0
}

// Has reports whether the device has the named flag.
func (d Device) Has(flag string) bool {
	for _, f := range d.Flags {
		if f == flag {
			return true
		}
	}
	return false
}
