package peering

import (
	"bytes"
	"slices"
)

// reconcile originates hints for keys added since the previous inventory and
// forgets local hints for keys that disappeared, without withdrawals.
// Comparing successive inventories, rather than the bounded index, stops
// unrelated changes from reannouncing a hint that FIFO eviction forgot.
func (a *Availability) reconcile(keys []string) {
	current := make([]cacheKey, 0, len(keys))
	for _, text := range keys {
		if key, ok := parseCacheKey(text); ok {
			current = append(current, key)
		}
	}
	slices.SortFunc(current, func(x, y cacheKey) int { return bytes.Compare(x[:], y[:]) })
	current = slices.Compact(current)

	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	previous := a.baseline
	i, j := 0, 0
	for i < len(previous) || j < len(current) {
		order := 0
		switch {
		case i == len(previous):
			order = 1
		case j == len(current):
			order = -1
		default:
			order = bytes.Compare(previous[i][:], current[j][:])
		}
		switch {
		case order < 0:
			a.index.forget(previous[i])
			i++
		case order > 0:
			a.index.originate(current[j], now.Add(a.jitter(availabilityCadence)))
			j++
		default:
			i++
			j++
		}
	}
	a.baseline = current
}
