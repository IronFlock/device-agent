//go:build !linux && !windows

package main

// applyResourceProtection does nothing on development hosts (macOS).
func applyResourceProtection() {}
