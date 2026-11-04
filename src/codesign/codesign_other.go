//go:build !windows

package codesign

// platformVerify is a no-op off Windows: Authenticode is Windows-only, and the
// agent binary that runs on Linux/macOS is trusted via other means (systemd
// packaging, GCS TLS + SHA-256). Never reached anyway, since a real pinned
// root is only meaningful for the Windows service self-update path.
func platformVerify(path string) error {
	return nil
}

// ReportSelf does nothing off Windows.
func ReportSelf() {}
