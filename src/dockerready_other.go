//go:build !windows

package main

// afterDockerReady: nothing waits for Docker on this platform (see
// dockerready_windows.go).
func afterDockerReady() {}
