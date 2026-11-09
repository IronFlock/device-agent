//go:build !windows && !darwin

package trust

import (
	"os"
	"path/filepath"
	"strings"
)

// bundleCandidates are the usual single-file CA bundles, in the order the
// major distributions install them — Go's own list (crypto/x509
// root_linux.go). IronFlockOS and Debian devices land on the first entry; the rest
// cover RPM, SUSE and Alpine hosts.
var bundleCandidates = []string{
	"/etc/ssl/certs/ca-certificates.crt",
	"/etc/pki/tls/certs/ca-bundle.crt",
	"/etc/ssl/ca-bundle.pem",
	"/etc/pki/tls/cacert.pem",
	"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
	"/etc/ssl/cert.pem",
}

// dirCandidates are certificate directories, read in full on top of the
// bundle, as Go does.
var dirCandidates = []string{
	"/etc/ssl/certs",
	"/etc/pki/tls/certs",
}

// hostRootDERs reads the standard locations plus SSL_CERT_FILE and
// SSL_CERT_DIR. Go lets those two REPLACE the standard locations; here they
// are added to them instead, so the result is never smaller than the pool the
// agent's own Go connections verify against, whichever way the service
// environment is set.
func hostRootDERs() ([][]byte, error) {
	ders, err := hostRootsFromFiles(bundleCandidates, dirCandidates)

	var extraDirs []string
	if dirs := os.Getenv("SSL_CERT_DIR"); dirs != "" {
		extraDirs = filepath.SplitList(dirs)
	}
	var extraBundles []string
	if file := strings.TrimSpace(os.Getenv("SSL_CERT_FILE")); file != "" {
		extraBundles = []string{file}
	}
	if extra, extraErr := hostRootsFromFiles(extraBundles, extraDirs); extraErr == nil {
		ders = append(ders, extra...)
		err = nil
	}

	return ders, err
}
