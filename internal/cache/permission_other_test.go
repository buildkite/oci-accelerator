//go:build !linux

package cache

// Ambient capabilities are Linux-specific.
func clearAmbientCapabilities() error { return nil }
