package firmware

import (
	"context"
	"crypto/sha1" //nolint:gosec // fwupd's metadata can name only a SHA-1; it verifies the payload itself as well
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/rs/zerolog/log"
)

// Updates through fwupd (lane 3 of IronFlockOS ADR-0007). fwupd only flashes: its
// client downloads the metadata and the firmware and hands them over as file
// descriptors, so this package does what fwupdmgr does (libfwupd/fwupd-client.c
// of fwupd 2.0). fwupd checks the signed metadata, and that the firmware
// belongs to the device (OnlyTrusted, on by default).

const (
	// fwupd's answers when it has nothing to offer for a device.
	errNotSupported = "org.freedesktop.fwupd.NotSupported"
	errNotFound     = "org.freedesktop.fwupd.NotFound"

	// FWUPD_REMOTE_KIND_DOWNLOAD: a remote whose metadata is fetched.
	remoteKindDownload = 1

	maxMetadataBytes  = 128 << 20
	maxSignatureBytes = 4 << 20
	maxFirmwareBytes  = 1 << 30

	historyLength    = 10
	progressInterval = time.Second
	userAgent        = "ironflock-device-agent"

	// lvfsRemote is the id of fwupd's remote for LVFS, the one remote the
	// update base carries.
	lvfsRemote = "lvfs"
	// mirrorDir is LVFS below the update base: the cloud's download proxy or
	// an appliance's mirror.
	mirrorDir    = "lvfs"
	metadataFile = "firmware.xml.zst"
	// lvfsFirmwareBase is what a bare file name in LVFS's metadata is relative
	// to (FirmwareBaseURI of fwupd's lvfs.conf).
	lvfsFirmwareBase = "https://fwupd.org/downloads"
	announceTimeout  = 10 * time.Second
)

// ErrNotOffered is returned when fwupd does not offer the requested version
// for the device, which also covers a request made on an outdated report.
var ErrNotOffered = errors.New("fwupd does not offer this firmware version for the device")

var (
	// TempDir holds the downloads until fwupd has read them. On /data: a
	// firmware file can be larger than the tmpfs is.
	TempDir = "/var/cache/flockos"

	// httpClient follows https only, and is bounded by each call's context.
	httpClient = &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return fmt.Errorf("refusing a redirect to %s", req.URL.Scheme)
		}
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		return nil
	}}
)

// MirrorURL is where the update base serves LVFS, "" for a base that is not
// an http(s) URL.
func MirrorURL(base string) string {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return ""
	}
	return strings.TrimRight(base, "/") + "/" + mirrorDir
}

// source is a place to read a file from. Only the update base may be plain
// http, as an appliance without a domain is: what comes from it is checked
// against fwupd's signed metadata, not trusted for its transport.
type source struct {
	url   string
	plain bool
}

func (s source) check() error {
	u, err := url.Parse(s.url)
	if err != nil {
		return err
	}
	if u.Scheme != "https" && !(s.plain && u.Scheme == "http") {
		return fmt.Errorf("refusing to download over %q: %s", u.Scheme, s.url)
	}
	return nil
}

// Release is a firmware version fwupd offers for a device.
type Release struct {
	Version     string
	Summary     string
	Description string
	Vendor      string
	Urgency     string
	Size        uint64
	Created     uint64
	Issues      []string
	Locations   []string
	Checksums   []string
}

// HistoryEntry is one firmware update fwupd remembers.
type HistoryEntry struct {
	DeviceID   string
	Name       string
	VersionOld string
	Version    string
	State      string
	Error      string
	Time       uint64
}

// Progress is what an install is doing. Stage is download or install.
type Progress struct {
	Stage        string
	CurrentBytes int64
	TotalBytes   int64
	Status       string
	Percent      uint32
}

// Result is how an install ended. NeedsReboot: the firmware is staged and the
// next restart applies it.
type Result struct {
	Version     string
	NeedsReboot bool
}

// fileName is the release's file, the last part of its first location.
func (r *Release) fileName() string {
	if len(r.Locations) == 0 {
		return ""
	}
	location := r.Locations[0]
	if i := strings.IndexAny(location, "?#"); i >= 0 {
		location = location[:i]
	}
	location = location[strings.LastIndexByte(location, '/')+1:]
	return location
}

// sources lists where the release can be downloaded from: the update base,
// then what the metadata names. LVFS's metadata has bare file names as well
// as URLs.
func (r *Release) sources(mirror string) []source {
	var all []source
	if file := r.fileName(); mirror != "" && file != "" {
		all = append(all, source{url: mirror + "/" + url.PathEscape(file), plain: true})
	}
	for _, location := range r.Locations {
		if !strings.Contains(location, "://") {
			location = lvfsFirmwareBase + "/" + url.PathEscape(location)
		}
		all = append(all, source{url: location})
	}
	return all
}

// FWUPD_RELEASE_URGENCY_*.
var urgencies = []string{"", "low", "medium", "high", "critical"}

// FWUPD_STATUS_*.
var statuses = []string{
	"", "idle", "loading", "decompressing", "device-restart", "device-write", "device-verify", "scheduling",
	"downloading", "device-read", "device-erase", "waiting-for-auth", "device-busy", "shutdown", "waiting-for-user",
}

var markup = regexp.MustCompile(`<[^>]*>`)

// plainText turns the markup of a release description into one line of text.
func plainText(s string) string {
	return strings.Join(strings.Fields(html.UnescapeString(markup.ReplaceAllString(s, " "))), " ")
}

// decodeRelease turns one a{sv} of GetUpgrades into a Release.
func decodeRelease(d map[string]dbus.Variant) Release {
	r := Release{
		Version:     str(d, "Version"),
		Summary:     str(d, "Summary"),
		Description: plainText(str(d, "Description")),
		Vendor:      str(d, "Vendor"),
		Size:        uint64Of(d, "Size"),
		Created:     uint64Of(d, "Created"),
		Issues:      strs(d, "Issues"),
		Locations:   strs(d, "Locations"),
	}
	if uri := str(d, "Uri"); uri != "" && len(r.Locations) == 0 {
		r.Locations = []string{uri}
	}
	if u := uint64Of(d, "Urgency"); u < uint64(len(urgencies)) {
		r.Urgency = urgencies[u]
	}
	if sums := str(d, "Checksum"); sums != "" {
		r.Checksums = strings.Split(sums, ",")
	}
	return r
}

// bestChecksum picks the strongest checksum of a release, with its hash.
func bestChecksum(sums []string) (string, hash.Hash, error) {
	for _, candidate := range []struct {
		hexLen int
		new    func() hash.Hash
	}{{128, sha512.New}, {64, sha256.New}, {40, sha1.New}} { //nolint:gosec // see the import
		for _, sum := range sums {
			if len(sum) == candidate.hexLen {
				return strings.ToLower(sum), candidate.new(), nil
			}
		}
	}
	return "", nil, errors.New("the release has no checksum to verify it by")
}

// nothingOffered reports whether a GetUpgrades error means no update.
func nothingOffered(err error) bool {
	var dbusErr dbus.Error
	if !errors.As(err, &dbusErr) {
		return false
	}
	return dbusErr.Name == errNothingToDo || dbusErr.Name == errNotSupported || dbusErr.Name == errNotFound
}

// upgrades lists the versions fwupd offers for a device, the newest first.
func upgrades(ctx context.Context, obj dbus.BusObject, deviceID string) ([]Release, error) {
	var raw []map[string]dbus.Variant
	err := obj.CallWithContext(ctx, iface+".GetUpgrades", 0, deviceID).Store(&raw)
	if err != nil {
		if nothingOffered(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("fwupd GetUpgrades: %w", err)
	}
	releases := make([]Release, 0, len(raw))
	for _, d := range raw {
		releases = append(releases, decodeRelease(d))
	}
	return releases, nil
}

// history lists the updates fwupd remembers, the latest first.
func history(ctx context.Context, obj dbus.BusObject) ([]HistoryEntry, error) {
	var raw []map[string]dbus.Variant
	err := obj.CallWithContext(ctx, iface+".GetHistory", 0).Store(&raw)
	if err != nil {
		if nothingOffered(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("fwupd GetHistory: %w", err)
	}
	entries := make([]HistoryEntry, 0, len(raw))
	for _, d := range raw {
		entry := HistoryEntry{
			DeviceID:   str(d, "DeviceId"),
			Name:       str(d, "Name"),
			VersionOld: str(d, "Version"),
			Error:      str(d, "UpdateError"),
			Time:       uint64Of(d, "Modified"),
		}
		if entry.Time == 0 {
			entry.Time = uint64Of(d, "Created")
		}
		if s := uint64Of(d, "UpdateState"); s < uint64(len(updateStates)) {
			entry.State = updateStates[s]
		}
		if v, ok := d["Release"]; ok {
			if releases, ok := v.Value().([]map[string]dbus.Variant); ok && len(releases) > 0 {
				entry.Version = str(releases[0], "Version")
			}
		}
		entries = append(entries, entry)
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Time > entries[j].Time })
	if len(entries) > historyLength {
		entries = entries[:historyLength]
	}
	return entries, nil
}

// Snapshot is Read, with the newest version fwupd offers for each device and
// its update history. What fwupd cannot say about updates is logged and left
// out: the inventory still counts.
func Snapshot(ctx context.Context) (Inventory, error) {
	conn, err := dbus.ConnectSystemBus(dbus.WithContext(ctx))
	if err != nil {
		return Inventory{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer func() { _ = conn.Close() }()
	return snapshot(ctx, conn)
}

func snapshot(ctx context.Context, conn *dbus.Conn) (Inventory, error) {
	inv, err := read(ctx, conn)
	if err != nil {
		return Inventory{}, err
	}
	obj := conn.Object(busName, objectPath)
	for i := range inv.Devices {
		device := &inv.Devices[i]
		if !device.Has("updatable") {
			continue
		}
		releases, err := upgrades(ctx, obj, device.ID)
		if err != nil {
			log.Warn().Err(err).Msgf("no firmware updates listed for %s", device.Name)
			continue
		}
		if len(releases) > 0 {
			device.Update = &releases[0]
		}
	}
	if inv.History, err = history(ctx, obj); err != nil {
		log.Warn().Err(err).Msg("no firmware update history")
	}
	return inv, nil
}

// fetch writes url to w, up to limit bytes. onBytes follows the download.
func fetch(ctx context.Context, src source, limit int64, w io.Writer, onBytes func(current, total int64)) error {
	if err := src.check(); err != nil {
		return err
	}
	rawURL := src.url
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", rawURL, resp.Status)
	}
	if onBytes != nil {
		w = &countingWriter{w: w, total: resp.ContentLength, onBytes: onBytes}
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return fmt.Errorf("%s is larger than %d bytes", rawURL, limit)
	}
	return nil
}

type countingWriter struct {
	w       io.Writer
	current int64
	total   int64
	onBytes func(current, total int64)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.current += int64(n)
	c.onBytes(c.current, c.total)
	return n, err
}

// tempFile is an unnamed file of the download directory: it goes away with
// its descriptor, also if the agent dies during an install.
func tempFile() (*os.File, error) {
	if err := os.MkdirAll(TempDir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(TempDir, "firmware-*")
	if err != nil {
		return nil, err
	}
	if err := os.Remove(f.Name()); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// fetchToFile downloads url into a temporary file, rewound for reading.
func fetchToFile(ctx context.Context, src source, limit int64, onBytes func(current, total int64)) (*os.File, error) {
	f, err := tempFile()
	if err != nil {
		return nil, err
	}
	if err := fetch(ctx, src, limit, f, onBytes); err != nil {
		_ = f.Close()
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// Refresh brings fwupd's metadata up to date, for the enabled remotes last
// fetched more than maxAge ago (0: all of them). fwupd checks the signature.
// The IronFlockOS image has no fwupd-refresh timer: this is what replaces it.
// mirror is MirrorURL of the update base; LVFS itself is the fallback, so
// that a device behind an appliance without the mirror keeps working.
func Refresh(ctx context.Context, maxAge time.Duration, mirror string) error {
	conn, err := dbus.ConnectSystemBus(dbus.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer func() { _ = conn.Close() }()
	return refresh(ctx, conn, maxAge, mirror)
}

func refresh(ctx context.Context, conn *dbus.Conn, maxAge time.Duration, mirror string) error {
	obj := conn.Object(busName, objectPath)
	var remotes []map[string]dbus.Variant
	if err := obj.CallWithContext(ctx, iface+".GetRemotes", 0).Store(&remotes); err != nil {
		var dbusErr dbus.Error
		if errors.As(err, &dbusErr) && dbusErr.Name == errServiceUnknown {
			return fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		return fmt.Errorf("fwupd GetRemotes: %w", err)
	}
	var failed []error
	for _, remote := range remotes {
		enabled, _ := variantBool(remote, "Enabled")
		if !enabled || uint64Of(remote, "Type") != remoteKindDownload || str(remote, "Uri") == "" {
			continue
		}
		if age := uint64Of(remote, "ModificationTime"); maxAge > 0 && age > 0 &&
			time.Since(time.Unix(int64(age), 0)) < maxAge {
			continue
		}
		if err := refreshRemote(ctx, obj, remote, mirror); err != nil {
			failed = append(failed, fmt.Errorf("remote %s: %w", str(remote, "RemoteId"), err))
		}
	}
	return errors.Join(failed...)
}

func variantBool(d map[string]dbus.Variant, key string) (bool, bool) {
	v, ok := d[key]
	if !ok {
		return false, false
	}
	b, ok := v.Value().(bool)
	return b, ok
}

// refreshRemote takes the metadata from the first source that gives fwupd
// something it accepts.
func refreshRemote(ctx context.Context, obj dbus.BusObject, remote map[string]dbus.Variant, mirror string) error {
	sources := []source{{url: str(remote, "Uri")}}
	if mirror != "" && str(remote, "RemoteId") == lvfsRemote {
		sources = append([]source{{url: mirror + "/" + metadataFile, plain: true}}, sources...)
	}
	var failed []error
	for _, src := range sources {
		err := refreshFrom(ctx, obj, remote, src)
		if err == nil {
			return nil
		}
		log.Warn().Err(err).Msgf("no firmware metadata from %s", src.url)
		failed = append(failed, err)
	}
	return errors.Join(failed...)
}

// refreshFrom fetches the signature first: an unchanged one means the
// metadata is unchanged, and it is a fraction of the size.
func refreshFrom(ctx context.Context, obj dbus.BusObject, remote map[string]dbus.Variant, src source) error {
	signature, err := fetchToFile(ctx, source{url: src.url + ".jcat", plain: src.plain}, maxSignatureBytes, nil)
	if err != nil {
		return err
	}
	defer func() { _ = signature.Close() }()

	if known := strings.ToLower(str(remote, "Checksum")); known != "" {
		same, err := matchesChecksum(signature, known)
		if err != nil {
			return err
		}
		if same {
			return nil
		}
	}

	metadata, err := fetchToFile(ctx, src, maxMetadataBytes, nil)
	if err != nil {
		return err
	}
	defer func() { _ = metadata.Close() }()

	call := obj.CallWithContext(ctx, iface+".UpdateMetadata", 0, str(remote, "RemoteId"),
		dbus.UnixFD(metadata.Fd()), dbus.UnixFD(signature.Fd()))
	if call.Err != nil {
		return fmt.Errorf("fwupd UpdateMetadata: %w", call.Err)
	}
	return nil
}

// matchesChecksum compares f to a checksum of the kind its length names, and
// leaves f rewound.
func matchesChecksum(f *os.File, want string) (bool, error) {
	_, h, err := bestChecksum([]string{want})
	if err != nil {
		return false, nil
	}
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == want, nil
}

// Announce tells the update base which firmware files this device was
// offered, so that an appliance holds them before anyone installs them. It is
// a HEAD for each file; the answer does not matter, and neither does a failure.
func Announce(ctx context.Context, mirror string, inv Inventory) {
	if mirror == "" {
		return
	}
	seen := map[string]bool{}
	for _, device := range inv.Devices {
		if device.Update == nil {
			continue
		}
		file := device.Update.fileName()
		if file == "" || seen[file] {
			continue
		}
		seen[file] = true
		if err := announce(ctx, source{url: mirror + "/" + url.PathEscape(file), plain: true}); err != nil {
			log.Debug().Err(err).Msgf("could not announce firmware %s", file)
		}
	}
}

func announce(ctx context.Context, src source) error {
	if err := src.check(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, announceTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, src.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// Install downloads the given version of a device's firmware and has fwupd
// install it. The version must be one fwupd offers: nothing but the device's
// id and the version comes from the caller, and the file and its checksum
// come from fwupd's signed metadata; mirror is MirrorURL of the update base.
// Firmware has no A/B slot, so ctx should not be tied to the caller's
// connection.
func Install(ctx context.Context, deviceID, version, mirror string, progress func(Progress)) (Result, error) {
	conn, err := dbus.ConnectSystemBus(dbus.WithContext(ctx))
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer func() { _ = conn.Close() }()
	return install(ctx, conn, deviceID, version, mirror, progress)
}

func install(ctx context.Context, conn *dbus.Conn, deviceID, version, mirror string, progress func(Progress)) (Result, error) {
	obj := conn.Object(busName, objectPath)
	releases, err := upgrades(ctx, obj, deviceID)
	if err != nil {
		return Result{}, err
	}
	var release *Release
	for i := range releases {
		if releases[i].Version == version {
			release = &releases[i]
			break
		}
	}
	if release == nil {
		return Result{}, ErrNotOffered
	}

	f, err := downloadRelease(ctx, release, mirror, progress)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = f.Close() }()

	done := make(chan struct{})
	defer close(done)
	go followInstall(ctx, obj, done, progress)

	options := map[string]dbus.Variant{
		"reason":        dbus.MakeVariant("user-action"),
		"filename":      dbus.MakeVariant("firmware.cab"),
		"install-flags": dbus.MakeVariant(uint64(0)),
	}
	if call := obj.CallWithContext(ctx, iface+".Install", 0, deviceID, dbus.UnixFD(f.Fd()), options); call.Err != nil {
		return Result{}, fmt.Errorf("fwupd Install: %w", call.Err)
	}

	result := Result{Version: version}
	inv, err := read(ctx, conn)
	if err != nil {
		log.Warn().Err(err).Msg("could not read the firmware state after the install")
		return result, nil
	}
	for _, d := range inv.Devices {
		if d.ID == deviceID {
			result.NeedsReboot = d.Has("needs-reboot") || d.Has("needs-shutdown") || d.UpdateState == "needs-reboot"
		}
	}
	return result, nil
}

// downloadRelease fetches the firmware from the first source that serves it
// and verifies it against the checksum of the signed metadata.
func downloadRelease(ctx context.Context, release *Release, mirror string, progress func(Progress)) (*os.File, error) {
	if len(release.Locations) == 0 {
		return nil, errors.New("the release names no location to download it from")
	}
	want, h, err := bestChecksum(release.Checksums)
	if err != nil {
		return nil, err
	}
	var last time.Time
	onBytes := func(current, total int64) {
		if time.Since(last) < progressInterval {
			return
		}
		last = time.Now()
		if total <= 0 {
			total = int64(release.Size)
		}
		progress(Progress{Stage: "download", CurrentBytes: current, TotalBytes: total})
	}
	var failed []error
	for _, src := range release.sources(mirror) {
		f, err := fetchToFile(ctx, src, maxFirmwareBytes, onBytes)
		if err != nil {
			failed = append(failed, err)
			continue
		}
		h.Reset()
		if _, err := io.Copy(h, f); err != nil {
			_ = f.Close()
			return nil, err
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != want {
			_ = f.Close()
			// A mirror's copy can be bad and LVFS's still good.
			failed = append(failed, fmt.Errorf("firmware checksum mismatch from %s: expected %s, got %s", src.url, want, got))
			continue
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			_ = f.Close()
			return nil, err
		}
		return f, nil
	}
	return nil, fmt.Errorf("the firmware could not be downloaded: %w", errors.Join(failed...))
}

// followInstall reports fwupd's status and percentage while Install runs.
func followInstall(ctx context.Context, obj dbus.BusObject, done <-chan struct{}, progress func(Progress)) {
	ticker := time.NewTicker(progressInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			status := uint32Property(ctx, obj, "Status")
			p := Progress{Stage: "install", Percent: uint32Property(ctx, obj, "Percentage")}
			if int(status) < len(statuses) {
				p.Status = statuses[status]
			}
			progress(p)
		}
	}
}

func uint32Property(ctx context.Context, obj dbus.BusObject, name string) uint32 {
	var v dbus.Variant
	if err := obj.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, iface, name).Store(&v); err != nil {
		return 0
	}
	n, _ := v.Value().(uint32)
	return n
}
