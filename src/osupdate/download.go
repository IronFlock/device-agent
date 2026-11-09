package osupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reagent/filesystem"
	"strings"
	"time"
)

// CacheDir holds the one bundle a device downloads: on IronFlockOS a directory
// of /data, so a download survives a reboot and never fills a root slot.
const CacheDir = "/var/cache/flockos"

// stallTimeout gives up a download that delivers no byte for this long.
var stallTimeout = 60 * time.Second

// BundleURL is where a device downloads the bundle of version: below its own
// update base, at <base>/flockos/<version>/<file>, where <file> is the last
// path segment of the catalog's url. The catalog's urls name the cloud copy
// (instance-registry.ironflock.com/dl/flockos/<version>/<file>), and the
// catalog is signed, so no mirror can rewrite them; a device behind an
// appliance reaches only the appliance, whose mirror serves the same tree
// next to the catalog it read. On the cloud the two urls are the same. Size
// and SHA-256 still come from the signed catalog, so the host serving the
// bytes does not matter to their integrity.
func BundleURL(base, version string, b Bundle) (string, error) {
	base = strings.TrimRight(base, "/")
	if base == "" {
		return "", errors.New("no update base to download the bundle from")
	}
	if !versionRe.MatchString(version) {
		return "", fmt.Errorf("release version %q is not a version", version)
	}
	u, err := url.Parse(b.URL)
	if err != nil {
		return "", fmt.Errorf("bundle url %q: %w", b.URL, err)
	}
	name := path.Base(u.Path)
	if !strings.HasSuffix(name, ".raucb") || name == ".raucb" || strings.ContainsAny(name, `\`) {
		return "", fmt.Errorf("bundle url %q does not name a .raucb file", b.URL)
	}
	return base + "/flockos/" + version + "/" + name, nil
}

// Download fetches the bundle into dir: streamed to <name>.part, checked for
// size and SHA-256, then renamed to <name>. Other bundles and partial
// downloads in dir are removed first: there is one update at a time. ctx
// ends the download; so does a minute without data.
func Download(ctx context.Context, b Bundle, dir string, progress func(filesystem.DownloadProgress)) (string, error) {
	name := path.Base(b.URL)
	if !strings.HasSuffix(name, ".raucb") || strings.ContainsAny(name, `/\`) || name == ".raucb" {
		return "", fmt.Errorf("bundle url %q does not name a .raucb file", b.URL)
	}
	if !sha256Hex.MatchString(b.SHA256) || b.Size <= 0 {
		return "", errors.New("the bundle has no sha256 and size to check against")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := clean(dir); err != nil {
		return "", err
	}
	dest := filepath.Join(dir, name)
	part := dest + ".part"

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.URL, nil)
	if err != nil {
		return "", err
	}
	resp, err := downloadClient().Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s returned status %d", b.URL, resp.StatusCode)
	}

	f, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(part)
		}
	}()

	stall := time.AfterFunc(stallTimeout, cancel)
	defer stall.Stop()
	hash := sha256.New()
	var got uint64
	buf := make([]byte, 1<<20)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			stall.Reset(stallTimeout)
			if _, err := f.Write(buf[:n]); err != nil {
				return "", err
			}
			hash.Write(buf[:n])
			got += uint64(n)
			if got > uint64(b.Size) {
				return "", fmt.Errorf("%s is larger than the catalog's %d bytes", b.URL, b.Size)
			}
			if progress != nil {
				progress(filesystem.DownloadProgress{FilePath: dest, Increment: uint64(n), CurrentBytes: got, TotalFileSize: uint64(b.Size)})
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			if ctx.Err() != nil && errors.Is(readErr, context.Canceled) {
				return "", fmt.Errorf("download of %s stopped: %w", b.URL, readErr)
			}
			return "", readErr
		}
	}
	if got != uint64(b.Size) {
		return "", fmt.Errorf("%s has %d bytes, the catalog says %d", b.URL, got, b.Size)
	}
	if sum := hex.EncodeToString(hash.Sum(nil)); sum != b.SHA256 {
		return "", fmt.Errorf("%s has sha256 %s, the catalog says %s", b.URL, sum, b.SHA256)
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(part, dest); err != nil {
		return "", err
	}
	ok = true
	return dest, nil
}

// Downloaded returns the bundle in dir that matches b (same name, size and
// SHA-256), or "".
func Downloaded(b Bundle, dir string) string {
	dest := filepath.Join(dir, path.Base(b.URL))
	info, err := os.Stat(dest)
	if err != nil || info.Size() != b.Size {
		return ""
	}
	f, err := os.Open(dest)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return ""
	}
	if hex.EncodeToString(hash.Sum(nil)) != b.SHA256 {
		return ""
	}
	return dest
}

// clean removes bundles and partial downloads from dir.
func clean(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".raucb") || strings.HasSuffix(e.Name(), ".raucb.part") {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// downloadClient is the agent's download transport (proxy from the
// environment, bounded connect and handshake) without an overall timeout: a
// bundle takes minutes; the context and the stall timer end it.
func downloadClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			DisableKeepAlives:     true,
		},
	}
}
