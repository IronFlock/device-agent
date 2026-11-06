//go:build !linux && !darwin && !windows

package diskguard

import "errors"

// rootPath is the root filesystem's path.
func rootPath() string { return "/" }

// statPath has no implementation on this platform (none of the agent's
// targets): the watch set stays empty, and the guard never acts.
func statPath(path string) (reading, error) {
	return reading{}, errors.ErrUnsupported
}
