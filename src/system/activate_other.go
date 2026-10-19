//go:build !windows && !linux

package system

// maybeActivateAgentUpdate is a no-op here (macOS): downloads are kept, nothing
// activates them.
func (system *System) maybeActivateAgentUpdate(result UpdateResult) {
}
