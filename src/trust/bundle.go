// Package trust materializes the CA bundle that app containers verify TLS
// against.
//
// An appliance in domain mode serves its endpoints with a certificate signed
// by the customer's internal CA. The device's own OS trusts that CA — it is
// installed as part of the corporate-certificate migration — so the agent,
// frpc and dockerd all connect happily. An app container does not: it carries
// whatever CA bundle its base image shipped with, and no base image ships a
// customer-internal root. Every SDK inside such a container therefore fails
// the handshake against wss://ws.<appliance domain> and reconnects forever,
// while the device around it looks perfectly online (tls-sf015, 2026-09-10:
// both TRUMPF apps sat in a two-second reconnect loop for hours; the packet
// capture showed each connection abandoned right after the server's
// certificate).
//
// The fix is to hand containers the device's OWN trust: this package exports
// the host root store, folds in the CA chain the device endpoint actually
// presents, and writes a single PEM bundle the agent bind-mounts into every
// app container.
//
// Exporting the whole host store rather than only the private CA is
// deliberate. The environment variables that point a runtime at a bundle
// (SSL_CERT_FILE, REQUESTS_CA_BUNDLE) REPLACE the default store instead of
// adding to it, so a bundle holding only the corporate root would break every
// public HTTPS call an app makes.
package trust

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// dirName sits under the APPS directory, not the agent directory, on
	// purpose: on Windows the agent dir is ACL-locked to SYSTEM and
	// Administrators (see servicecmd_windows.go) while Docker Desktop
	// resolves bind mounts in the interactive user's context. A bundle under
	// the agent dir would be unreadable to the very containers that need it;
	// the apps dir carries the explicit Users:Modify grant and inherits it.
	dirName  = "certs"
	fileName = "ca-bundle.crt"

	// ContainerDir is where the bundle's directory is mounted inside an app
	// container, ContainerPath the bundle itself. Containers get the
	// directory rather than the file so that rebuilding the bundle (write to
	// a temp file, then rename) is visible after a restart instead of
	// stranding the container on a deleted inode.
	ContainerDir  = "/etc/ironflock/certs"
	ContainerPath = ContainerDir + "/" + fileName

	// probeTimeout bounds the endpoint TLS probe. It runs during agent
	// startup, so it must fail fast on an offline device rather than delay
	// every app.
	probeTimeout = 5 * time.Second
)

// Enabled reports whether this device should hand its apps a CA bundle.
//
// Only devices attached to an appliance domain qualify, which is exactly the
// private-CA case the bundle exists for. It also keeps the cloud fleet
// untouched: because SSL_CERT_FILE replaces a container's own store, a device
// whose OS roots are older than its apps' base images would otherwise lose
// public-TLS coverage it has today.
func Enabled(applianceDomain string) bool {
	return strings.TrimSpace(applianceDomain) != ""
}

// HostDir returns the device-side directory holding the bundle.
func HostDir(appsDirectory string) string {
	return filepath.Join(appsDirectory, dirName)
}

// HostPath returns the device-side path of the bundle itself.
func HostPath(appsDirectory string) string {
	return filepath.Join(HostDir(appsDirectory), fileName)
}

// Available reports whether a usable bundle is on disk. Callers mount only
// what exists, so a device that never managed to build one behaves exactly as
// it did before this package.
func Available(appsDirectory string) bool {
	info, err := os.Stat(HostPath(appsDirectory))
	return err == nil && !info.IsDir() && info.Size() > 0
}

// Remove deletes a previously written bundle, for a device that is no longer
// attached to an appliance domain. Callers keep mounting what exists, so
// leaving a stale bundle behind would pin apps to a trust set nobody
// refreshes.
func Remove(appsDirectory string) error {
	err := os.Remove(HostPath(appsDirectory))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Build writes the bundle for appsDirectory and returns its path.
//
// endpointURL is the device endpoint (wss://...). Its verified chain is folded
// in so the appliance CA is present even when the host store cannot be
// enumerated, but a probe failure is never fatal: an offline device still gets
// a bundle from its host store, which on a correctly provisioned device
// already contains the corporate root.
func Build(appsDirectory, endpointURL string) (string, error) {
	hostRoots, hostErr := hostRootDERs()
	endpointCAs, endpointErr := endpointCADERs(endpointURL)

	bundle := buildPEM(hostRoots, endpointCAs)
	if len(bundle) == 0 {
		return "", fmt.Errorf("no usable CA certificates found (host store: %v; endpoint probe: %v)", hostErr, endpointErr)
	}

	path, err := writeBundle(appsDirectory, bundle)
	if err != nil {
		return "", err
	}
	return path, nil
}

// buildPEM turns raw DER inputs into a stable PEM bundle: trust anchors only
// (see isTrustAnchor), expired ones dropped, deduplicated, and ordered by
// fingerprint so that two runs over the same trust set produce byte-identical
// output (which lets writeBundle skip a needless rewrite).
func buildPEM(derSets ...[][]byte) []byte {
	type entry struct {
		fingerprint [32]byte
		der         []byte
	}

	seen := make(map[[32]byte]struct{})
	entries := make([]entry, 0, 256)
	now := time.Now()

	for _, ders := range derSets {
		for _, der := range ders {
			cert, err := x509.ParseCertificate(der)
			if err != nil || !isTrustAnchor(cert) || !cert.NotAfter.After(now) {
				continue
			}

			fingerprint := sha256.Sum256(der)
			if _, duplicate := seen[fingerprint]; duplicate {
				continue
			}
			seen[fingerprint] = struct{}{}
			entries = append(entries, entry{fingerprint: fingerprint, der: der})
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		return bytes.Compare(entries[i].fingerprint[:], entries[j].fingerprint[:]) < 0
	})

	var out []byte
	for _, e := range entries {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: e.der})...)
	}
	return out
}

// isTrustAnchor reports whether cert can anchor a chain: a CA certificate, or
// a self-signed one that is not marked as a CA. Go (and OpenSSL) accept the
// latter as a root, and it is how a v1 corporate root or an operator's
// explicitly trusted self-signed server certificate sits in a store; dropping
// it would leave the bundle trusting less than the device itself does. A leaf
// issued by someone else is not an anchor and stays out.
func isTrustAnchor(cert *x509.Certificate) bool {
	if cert.IsCA {
		return true
	}
	return bytes.Equal(cert.RawSubject, cert.RawIssuer) &&
		cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature) == nil
}

// writeBundle publishes the bundle atomically and world-readable: an app
// container runs as whatever UID its image declares, and an unreadable bundle
// is indistinguishable from the failure this package exists to fix.
func writeBundle(appsDirectory string, bundle []byte) (string, error) {
	return writeFileAtomic(HostDir(appsDirectory), fileName, bundle)
}

// writeFileAtomic writes data to dir/name with mode 0644 via a temp file and a
// rename, and leaves an unchanged file alone.
func writeFileAtomic(dir, name string, data []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil && !os.IsExist(err) {
		return "", err
	}

	path := filepath.Join(dir, name)
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, data) {
		return path, nil
	}

	tmp, err := os.CreateTemp(dir, name+".*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	return path, nil
}

// endpointCADERs returns the CA certificates of the chain the device endpoint
// presents, as verified by the host's own trust store. Returning the VERIFIED
// chain rather than whatever the server sent is what makes this safe to fold
// into the bundle: the OS has already decided it trusts these.
func endpointCADERs(endpointURL string) ([][]byte, error) {
	address, err := tlsAddress(endpointURL)
	if err != nil || address == "" {
		return nil, err
	}

	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: probeTimeout}, "tcp", address, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	state := conn.ConnectionState()
	if len(state.VerifiedChains) > 0 && len(state.VerifiedChains[0]) > 1 {
		chain := state.VerifiedChains[0][1:]
		ders := make([][]byte, 0, len(chain))
		for _, cert := range chain {
			ders = append(ders, cert.Raw)
		}
		return ders, nil
	}

	// Platform verifiers (Windows) do not always populate VerifiedChains.
	// The handshake succeeded, so the OS vouched for the chain the peer
	// sent; buildPEM keeps only the CA certificates out of it.
	if len(state.PeerCertificates) > 1 {
		peers := state.PeerCertificates[1:]
		ders := make([][]byte, 0, len(peers))
		for _, cert := range peers {
			ders = append(ders, cert.Raw)
		}
		return ders, nil
	}

	return nil, nil
}

// tlsAddress reduces a device endpoint URL to a host:port to probe, or ""
// when the endpoint does not use TLS at all (a LAN appliance on plain ws://,
// or a local dev setup — neither has a private CA to distribute).
func tlsAddress(endpointURL string) (string, error) {
	trimmed := strings.TrimSpace(endpointURL)
	if trimmed == "" {
		return "", nil
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "wss" && parsed.Scheme != "https" {
		return "", nil
	}

	host := parsed.Hostname()
	if host == "" {
		return "", nil
	}

	port := parsed.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(host, port), nil
}

// decodePEMCertificates pulls every CERTIFICATE block out of a PEM blob. Used
// by the file-based host stores; non-certificate blocks (a stray key, a trust
// comment header) are skipped rather than treated as an error.
func decodePEMCertificates(data []byte) [][]byte {
	var ders [][]byte
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return ders
		}
		if block.Type == "CERTIFICATE" {
			ders = append(ders, block.Bytes)
		}
	}
}

// hostRootsFromFiles reads certificates the way Go's own system pool does on
// Linux (crypto/x509 root_unix.go), so the bundle trusts at least what the
// agent's WAMP connection trusts: the first bundle file in bundles that can be
// read, PLUS every file in every directory in dirs. A root an administrator
// dropped into /etc/ssl/certs without running update-ca-certificates is
// therefore included, just as Go includes it. Like Go, directory entries are
// not filtered by extension (PEM decoding skips what is not a certificate)
// and a symlink to a file in the same directory, which is how the hashed
// names point at the real files, is not read twice.
//
// Platform-neutral so tests can point it at fixture paths.
func hostRootsFromFiles(bundles, dirs []string) ([][]byte, error) {
	var ders [][]byte

	for _, path := range bundles {
		data, err := os.ReadFile(path) //nolint:gosec // G703: paths of the platform's CA bundle locations
		if err != nil {
			continue
		}
		ders = append(ders, decodePEMCertificates(data)...)
		break
	}

	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}

		for _, entry := range entries {
			if entry.IsDir() || isSameDirSymlink(dir, entry) {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, entry.Name())) //nolint:gosec // G703: paths of the platform's CA bundle locations
			if err != nil {
				continue
			}
			ders = append(ders, decodePEMCertificates(data)...)
		}
	}

	if len(ders) == 0 {
		return nil, errNoHostStore
	}
	return ders, nil
}

// isSameDirSymlink mirrors Go's readUniqueDirectoryEntries: a symlink whose
// target has no directory component points into the same directory.
func isSameDirSymlink(dir string, entry os.DirEntry) bool {
	if entry.Type()&os.ModeSymlink == 0 {
		return false
	}
	target, err := os.Readlink(filepath.Join(dir, entry.Name()))
	return err == nil && !strings.Contains(target, "/")
}

var errNoHostStore = errors.New("no host CA store could be read")
