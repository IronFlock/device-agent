//go:build darwin

package trust

import (
	"context"
	"os/exec"
)

// hostRootDERs exports the macOS trust store. Only a developer machine ever
// runs the agent here, so shelling out to `security` is preferred over
// binding the Security framework.
func hostRootDERs() ([][]byte, error) {
	keychains := []string{
		"/System/Library/Keychains/SystemRootCertificates.keychain",
		"/Library/Keychains/System.keychain",
	}

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	var ders [][]byte
	for _, keychain := range keychains {
		out, err := exec.CommandContext(ctx, "security", "find-certificate", "-a", "-p", keychain).Output() //nolint:gosec // G204: macOS security tool with the fixed system keychain paths
		if err != nil {
			continue
		}
		ders = append(ders, decodePEMCertificates(out)...)
	}

	if len(ders) == 0 {
		return nil, errNoHostStore
	}
	return ders, nil
}
