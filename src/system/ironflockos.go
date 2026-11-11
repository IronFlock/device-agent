package system

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os/exec"
	"path"
	"reagent/filesystem"
	"reagent/osupdate"
	"reagent/release"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/rs/zerolog/log"
)

// OS updates of IronFlockOS images (IronFlockOS WP-2.4): the signed catalog below the
// update base names the release of the device's channel; the bundle is
// downloaded to osupdate.CacheDir, checked against the catalog, and installed
// by RAUC, which checks its signature and compatible. Devices that do not run
// IronFlockOS keep the legacy path (GetOSUpdate, InstallOSUpdate).

// ErrNotIronFlockOS: the device does not run an IronFlockOS image.
var ErrNotIronFlockOS = errors.New("this device does not run IronFlockOS")

// ErrNoNewerRelease: the channel offers nothing newer than the running image.
var ErrNoNewerRelease = errors.New("no newer IronFlockOS release for this device's channel")

// osUpdateMu lets one IronFlockOS update operation run at a time: a second
// download would remove the first one's partial file, and RAUC installs one
// bundle at a time.
var osUpdateMu sync.Mutex

// ErrOSUpdateRunning: another download or install is running.
var ErrOSUpdateRunning = errors.New("an OS update is already running on this device")

// ErrUpdateRunning is returned when a firmware update cannot start because an
// OS or firmware update is running.
var ErrUpdateRunning = errors.New("an update is already running on this device")

// TryLockUpdate claims the device for a firmware update, which excludes OS
// updates and other firmware updates, as both can end in a restart.
func TryLockUpdate() (unlock func(), ok bool) {
	if !osUpdateMu.TryLock() {
		return nil, false
	}
	return osUpdateMu.Unlock, true
}

// Seams for the tests.
var (
	readRunning = osupdate.ReadRunning
	openCatalog = osupdate.OpenCatalog
	fetchSmallC = fetchSmall
	cacheDir    = osupdate.CacheDir
	installRauc = raucInstallBundle
)

// RunsIronFlockOS reports whether the device runs an IronFlockOS image.
func RunsIronFlockOS() bool {
	r, err := readRunning()
	return err == nil && r.IsIronFlockOS()
}

// IronFlockOSOffer reads the running image and the catalog, and returns the
// release the device's channel offers for it.
func (sys *System) IronFlockOSOffer() (osupdate.Running, osupdate.Offer, error) {
	running, err := readRunning()
	if err != nil {
		return osupdate.Running{}, osupdate.Offer{}, err
	}
	if !running.IsIronFlockOS() {
		return running, osupdate.Offer{}, ErrNotIronFlockOS
	}
	url := sys.updateBaseURL() + "/" + osupdate.CatalogPath
	data, status, err := fetchSmallC(url)
	if err != nil {
		return running, osupdate.Offer{}, err
	}
	if status != http.StatusOK {
		return running, osupdate.Offer{}, fmt.Errorf("%s returned status %d", url, status)
	}
	catalog, keyID, err := openCatalog(data)
	if err != nil {
		return running, osupdate.Offer{}, fmt.Errorf("refusing %s: %w", url, err)
	}
	log.Debug().Str("key_id", keyID).Str("issued_at", catalog.IssuedAt).Msg("IronFlockOS catalog signature verified")
	offer, err := catalog.Offer(running, GetEnvironment(sys.config))
	return running, offer, err
}

// offerToInstall is IronFlockOSOffer for an update: a newer release this agent
// may install.
func (sys *System) offerToInstall() (osupdate.Offer, error) {
	running, offer, err := sys.IronFlockOSOffer()
	if err != nil {
		return osupdate.Offer{}, err
	}
	if !offer.Newer {
		return offer, fmt.Errorf("%w: it offers %s, the device runs %s", ErrNoNewerRelease, offer.Version, running.ImageVersion)
	}
	if err := offer.CheckAgent(release.GetVersion()); err != nil {
		return offer, err
	}
	return offer, nil
}

// DownloadIronFlockOSUpdate downloads the bundle of the offered release, or
// returns the one already downloaded.
func (sys *System) DownloadIronFlockOSUpdate(ctx context.Context, progress func(filesystem.DownloadProgress)) (string, error) {
	if !osUpdateMu.TryLock() {
		return "", ErrOSUpdateRunning
	}
	defer osUpdateMu.Unlock()
	offer, err := sys.offerToInstall()
	if err != nil {
		return "", err
	}
	if path := osupdate.Downloaded(offer.Bundle, cacheDir); path != "" {
		log.Info().Msgf("IronFlockOS %s is downloaded already: %s", offer.Version, path)
		return path, nil
	}
	// From the update base the catalog came from (an appliance's mirror, or
	// the cloud), not from the cloud url the signed catalog names.
	bundle := offer.Bundle
	if bundle.URL, err = osupdate.BundleURL(sys.updateBaseURL(), offer.Version, offer.Bundle); err != nil {
		return "", err
	}
	log.Info().Msgf("downloading IronFlockOS %s (%s) from %s", offer.Version, offer.Channel, bundle.URL)
	return osupdate.Download(ctx, bundle, cacheDir, progress)
}

// InstallIronFlockOSUpdate installs the downloaded bundle of the offered
// release into the other slot. RAUC's errors are returned. After a successful
// install the bundle is removed to free its space on /data; after a failed one
// it stays, so that a retry does not download it again.
func (sys *System) InstallIronFlockOSUpdate(progress func(operationName string, progressPercent uint64)) error {
	if !osUpdateMu.TryLock() {
		return ErrOSUpdateRunning
	}
	defer osUpdateMu.Unlock()
	offer, err := sys.offerToInstall()
	if err != nil {
		return err
	}
	path := osupdate.Downloaded(offer.Bundle, cacheDir)
	if path == "" {
		return fmt.Errorf("IronFlockOS %s is not downloaded: download_os_update first", offer.Version)
	}
	log.Info().Msgf("installing IronFlockOS %s from %s", offer.Version, path)
	if err := installRauc(path, progress); err != nil {
		return fmt.Errorf("RAUC: %w", err)
	}
	// The install stands even when the bundle cannot be removed: an error
	// here must not keep the device from rebooting into the new slot. The
	// next download removes what is left.
	if err := osupdate.Clean(cacheDir, ""); err != nil {
		log.Warn().Err(err).Msgf("could not remove the installed IronFlockOS bundle from %s", cacheDir)
	}
	return nil
}

// RemoveLeftoverIronFlockOSBundles removes the bundles no install needs any
// more: all but the one of the newer release the device's channel offers,
// which a download may have fetched before the agent restarted. It is for the
// bundles installs left behind before the agent removed them after the
// install. It reads the catalog to tell which bundle is still needed and keeps
// them all when it cannot; a download or install that is running is left
// alone. Errors are logged, never returned. Not IronFlockOS: nothing to do.
func (sys *System) RemoveLeftoverIronFlockOSBundles() {
	if !RunsIronFlockOS() {
		return
	}
	_, offer, err := sys.IronFlockOSOffer()
	if err != nil {
		log.Info().Err(err).Msg("keeping downloaded IronFlockOS bundles: the catalog cannot tell which are still needed")
		return
	}
	keep := ""
	if offer.Newer {
		keep = path.Base(offer.Bundle.URL)
	}
	if !osUpdateMu.TryLock() {
		return
	}
	defer osUpdateMu.Unlock()
	if err := osupdate.Clean(cacheDir, keep); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Warn().Err(err).Msgf("could not remove leftover IronFlockOS bundles from %s", cacheDir)
	}
}

// --- RAUC: the health gate and the slots ------------------------------------

// raucCall calls a RAUC D-Bus method on a connection of its own, bounded by
// ctx.
func raucCall(ctx context.Context, method string, args []interface{}, out ...interface{}) error {
	conn, err := dbus.ConnectSystemBus(dbus.WithContext(ctx))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	return conn.Object(raucDBusInterface, raucDBusObjectPath).CallWithContext(ctx, method, 0, args...).Store(out...)
}

// MarkIronFlockOSSlotGood marks the slot the device booted from good: the
// health gate of an update (IronFlockOS master plan, section 3.5, step 6). The
// agent calls it once it is connected to the platform and Docker runs; a
// slot that never gets here is not marked, and the bootloader falls back to
// the previous slot. Not IronFlockOS: nothing to do.
func MarkIronFlockOSSlotGood(ctx context.Context) error {
	if !RunsIronFlockOS() {
		return nil
	}
	var slot, message string
	if err := raucCall(ctx, raucDBusMethodMark, []interface{}{"good", "booted"}, &slot, &message); err != nil {
		return fmt.Errorf("RAUC mark good: %w", err)
	}
	log.Info().Msgf("RAUC: %s (%s)", message, slot)
	return nil
}

// Slot is one RAUC slot as the platform shows it.
type Slot struct {
	Name          string
	Class         string
	Bootname      string
	State         string // booted, active, inactive
	BootStatus    string // good, bad
	BundleVersion string
	Installed     string
	// ImageVersion and BuildID are the IMAGE_VERSION and BUILD_ID of the
	// image the root slot holds, from its os-release; "" when it holds none
	// or it cannot be read. RAUC knows the version of what it installed
	// only (BundleVersion): a slot written by flashing the image has none.
	ImageVersion string
	BuildID      string
}

// IronFlockOSSlots returns RAUC's slots and the names of the slot booted and
// of the primary (the next boot), e.g. rootfs.0 and rootfs.1; primary is ""
// when the bootloader backend cannot tell.
func IronFlockOSSlots(ctx context.Context) (booted string, primary string, slots []Slot, err error) {
	var raw []struct {
		Name   string
		Status map[string]dbus.Variant
	}
	if err := raucCall(ctx, raucDBusMethodSlot, nil, &raw); err != nil {
		return "", "", nil, fmt.Errorf("RAUC slot status: %w", err)
	}
	for _, s := range raw {
		slot := Slot{
			Name:          s.Name,
			Class:         variantString(s.Status, "class"),
			Bootname:      variantString(s.Status, "bootname"),
			State:         variantString(s.Status, "state"),
			BootStatus:    variantString(s.Status, "boot-status"),
			BundleVersion: variantString(s.Status, "bundle.version"),
			Installed:     variantString(s.Status, "installed.timestamp"),
		}
		if slot.State == "booted" {
			booted = slot.Name
		}
		slotImage(ctx, &slot, variantString(s.Status, "type"), variantString(s.Status, "device"))
		slots = append(slots, slot)
	}
	// Not every bootloader backend reports the primary; the slots stand
	// without it.
	if err := raucCall(ctx, raucDBusMethodPrimary, nil, &primary); err != nil {
		log.Debug().Err(err).Msg("RAUC primary")
		primary = ""
	}
	return booted, primary, slots, nil
}

// --- The image a slot holds -------------------------------------------------

// slotReadTimeout bounds the read of one slot's os-release.
const slotReadTimeout = 5 * time.Second

// slotOSReleasePath is where an IronFlockOS image keeps its os-release (IronFlockOS
// build/customize.sh); /etc/os-release links to it.
const slotOSReleasePath = "/usr/lib/os-release"

// slotOSReleaseMax caps what is kept of a slot's os-release: a few hundred
// bytes on every image.
const slotOSReleaseMax = 64 << 10

// readSlotOSRelease is a seam for the tests.
var readSlotOSRelease = debugfsOSRelease

// slotImages remembers per slot name the image read from it and the RAUC
// status it was read at. A slot's content changes only when RAUC writes it,
// and every install changes its status (marked bad when it starts, a new
// installed timestamp when it ends), so the slot is read again then.
var slotImages = struct {
	sync.Mutex
	m map[string]slotImageEntry
}{m: map[string]slotImageEntry{}}

type slotImageEntry struct {
	at      string
	running osupdate.Running
}

// slotImage fills in ImageVersion and BuildID of a root slot (one with a
// bootname): for the booted slot from the running os-release, for another
// ext4 slot from the os-release on its device. A slot that cannot be read
// keeps them empty: the slot status stands without them.
func slotImage(ctx context.Context, slot *Slot, fsType, device string) {
	if slot.Bootname == "" {
		return
	}
	if slot.State == "booted" {
		if r, err := readRunning(); err == nil {
			slot.ImageVersion, slot.BuildID = r.ImageVersion, r.BuildID
		}
		return
	}
	if fsType != "ext4" || !strings.HasPrefix(device, "/dev/") {
		return
	}
	at := strings.Join([]string{device, slot.BootStatus, slot.Installed, slot.BundleVersion}, "\x00")
	slotImages.Lock()
	entry, ok := slotImages.m[slot.Name]
	slotImages.Unlock()
	if !ok || entry.at != at {
		data, err := readSlotOSRelease(ctx, device)
		if err != nil {
			log.Debug().Err(err).Str("slot", slot.Name).Msg("could not read the slot's os-release")
			return
		}
		fields := osupdate.ParseOSRelease(data)
		entry = slotImageEntry{at: at, running: osupdate.Running{
			ID: fields["ID"], ImageID: fields["IMAGE_ID"], ImageVersion: fields["IMAGE_VERSION"], BuildID: fields["BUILD_ID"],
		}}
		// An empty slot (a file system without the file, or none) has
		// nothing to remember: it is read again next time.
		if !entry.running.IsIronFlockOS() {
			return
		}
		slotImages.Lock()
		slotImages.m[slot.Name] = entry
		slotImages.Unlock()
	}
	slot.ImageVersion, slot.BuildID = entry.running.ImageVersion, entry.running.BuildID
}

// debugfsOSRelease reads a slot's os-release straight from its ext4 device
// with debugfs (e2fsprogs, on every IronFlockOS image), read-only and without
// mounting it: a mounted slot would make RAUC refuse to install into it.
func debugfsOSRelease(ctx context.Context, device string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, slotReadTimeout)
	defer cancel()
	out := &cappedBuffer{max: slotOSReleaseMax}
	// -c opens the file system read-only without reading its bitmaps; the
	// device comes from RAUC's system.conf.
	cmd := exec.CommandContext(ctx, "debugfs", "-c", "-R", "cat "+slotOSReleasePath, device) //nolint:gosec // G204: fixed command, device from RAUC's slot status
	cmd.Stdout = out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("debugfs %s: %w", device, err)
	}
	return out.Bytes(), nil
}

// cappedBuffer keeps the first max bytes written to it and drops the rest.
type cappedBuffer struct {
	bytes.Buffer
	max int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - c.Len(); room > 0 {
		if len(p) > room {
			c.Buffer.Write(p[:room])
		} else {
			c.Buffer.Write(p)
		}
	}
	return len(p), nil
}

func variantString(m map[string]dbus.Variant, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, _ := v.Value().(string)
	return s
}

// markGoodTimeout bounds the health gate's RAUC call.
const markGoodTimeout = 30 * time.Second

// MarkIronFlockOSSlotGoodNow is MarkIronFlockOSSlotGood with its own deadline, for
// the agent's connect handler: errors are logged, never returned.
func MarkIronFlockOSSlotGoodNow() {
	ctx, cancel := context.WithTimeout(context.Background(), markGoodTimeout)
	defer cancel()
	if err := MarkIronFlockOSSlotGood(ctx); err != nil {
		log.Error().Err(err).Msg("could not mark the booted IronFlockOS slot good")
	}
}
