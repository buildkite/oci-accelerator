// Package cachetest provides constructors for cache.Store instances in tests.
// It keeps convenience defaults out of the production cache package's API.
package cachetest

import (
	"time"

	"github.com/buildkite/oci-accelerator/internal/cache"
)

// Open opens a Store at dir with the given byte budget and generous defaults for
// the remaining limits, sparing tests that only care about size from spelling
// out a full cache.Limits.
func Open(dir string, maxBytes int64) (*cache.Store, error) {
	return cache.OpenWithLimits(dir, cache.Limits{
		MaxBytes:    maxBytes,
		MaxFills:    16,
		MaxEntries:  100000,
		FillTimeout: 10 * time.Minute,
	})
}
