// Package osupdate is the device side of FlockOS updates (FlockOS master
// plan, section 3.5; WP-2.4): which image the device runs (os-release), the
// signed release catalog the release pipeline publishes (dl/flockos/
// manifest.json, FlockOS ADR-0009 and docs/manifest.schema.json), and the
// download of an update bundle. RAUC installs the bundle and checks its
// signature and compatible itself (system.InstallBundle).
//
// Nothing here runs at start or before the connection: the platform asks
// (get_os_release, download_os_update, install_os_update,
// perform_os_update), and every network call carries the caller's context.
package osupdate

import (
	"os"
	"strings"
)

// Running is the image this device runs, from its os-release.
type Running struct {
	ID           string // ID: flockos on FlockOS
	ImageID      string // IMAGE_ID: flockos-<target>-<variant>[-rt], the RAUC compatible
	ImageVersion string // IMAGE_VERSION: the FlockOS version
	BuildID      string // BUILD_ID: <UTC build time>Z-g<commit>
	PrettyName   string
}

// IsFlockOS reports whether the device runs a FlockOS image that RAUC
// updates: ID flockos with an image id and version.
func (r Running) IsFlockOS() bool {
	return r.ID == "flockos" && r.ImageID != "" && r.ImageVersion != ""
}

// osReleasePaths are read in order (os-release(5)).
var osReleasePaths = []string{"/etc/os-release", "/usr/lib/os-release"}

// ReadRunning reads the running image from os-release.
func ReadRunning() (Running, error) {
	var lastErr error
	for _, path := range osReleasePaths {
		data, err := os.ReadFile(path)
		if err != nil {
			lastErr = err
			continue
		}
		fields := ParseOSRelease(data)
		return Running{
			ID:           fields["ID"],
			ImageID:      fields["IMAGE_ID"],
			ImageVersion: fields["IMAGE_VERSION"],
			BuildID:      fields["BUILD_ID"],
			PrettyName:   fields["PRETTY_NAME"],
		}, nil
	}
	return Running{}, lastErr
}

// ParseOSRelease reads os-release(5) data: KEY=value lines, the value
// unquoted or in double or single quotes, backslash escapes inside double
// quotes. Comments, blank lines and lines without a key are skipped; a value
// may itself contain '='.
func ParseOSRelease(data []byte) map[string]string {
	fields := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || strings.ContainsAny(key, " \t\"'") {
			continue
		}
		fields[key] = unquote(strings.TrimSpace(value))
	}
	return fields
}

func unquote(v string) string {
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		return v[1 : len(v)-1]
	}
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		var b strings.Builder
		inner := v[1 : len(v)-1]
		for i := 0; i < len(inner); i++ {
			if inner[i] == '\\' && i+1 < len(inner) {
				i++
			}
			b.WriteByte(inner[i])
		}
		return b.String()
	}
	return v
}
