package peering

import (
	"bytes"
	"slices"
)

// reconcile announces hints for keys added since the previous inventory and
// forgets local hints for keys that disappeared, without withdrawals.
// Comparing successive inventories, rather than the bounded index, stops
// unrelated changes from reannouncing a hint that FIFO eviction forgot.
func (a *ProviderHints) reconcile(keys []string) {
	current := parseInventory(keys)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.applyInventory(current)
}

// restoreInventory reconciles restored hints with the inventory at startup.
// It forgets local hints for absent keys and announces hints for present
// keys without a retained local hint. Retained local hints keep their seen
// sets and forwarding progress. The inventory becomes the baseline.
func (a *ProviderHints) restoreInventory(keys []string) {
	current := parseInventory(keys)
	a.mu.Lock()
	defer a.mu.Unlock()
	for e := a.index.fifo.Front(); e != nil; {
		h := e.Value.(*hint)
		e = e.Next()
		if _, present := slices.BinarySearchFunc(current, h.key, compareCacheKeys); h.provider == a.self && !present && a.index.forget(h.key) {
			a.revision++
		}
	}
	a.applyInventory(current)
}

// applyInventory requires mu.
func (a *ProviderHints) applyInventory(current []cacheKey) {
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
			order = compareCacheKeys(previous[i], current[j])
		}
		switch {
		case order < 0:
			if a.index.forget(previous[i]) {
				a.revision++
			}
			i++
		case order > 0:
			if a.index.find(a.self, current[j]) == nil {
				a.index.announce(current[j], now.Add(a.jitter(announceCadence)))
				a.revision++
			}
			j++
		default:
			i++
			j++
		}
	}
	a.baseline = current
}

// parseInventory returns the valid keys sorted and unique.
func parseInventory(keys []string) []cacheKey {
	current := make([]cacheKey, 0, len(keys))
	for _, text := range keys {
		if key, ok := parseCacheKey(text); ok {
			current = append(current, key)
		}
	}
	slices.SortFunc(current, compareCacheKeys)
	return slices.Compact(current)
}

func compareCacheKeys(x, y cacheKey) int { return bytes.Compare(x[:], y[:]) }
