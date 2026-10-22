package peering

import (
	"container/list"
	"slices"
)

// maxHintRecords bounds the distinct cache keys one node retains hints for.
const maxHintRecords = 200_000

// hintIndex keeps one record per cache key with unique provider node IDs.
// All peers share a key FIFO. Repeated claims and lookups never move a key,
// and eviction removes the key and all of its providers. Provider counts do
// not consume key slots, so the limit is not a byte-memory bound. The caller
// serializes access and rejects claims from the local node.
type hintIndex struct {
	limit int
	fifo  list.List // cacheKey, oldest admission first
	byKey map[cacheKey][]nodeID
}

// newHintIndex retains at most limit distinct keys. The limit must be positive.
func newHintIndex(limit int) *hintIndex {
	return &hintIndex{limit: limit, byKey: make(map[cacheKey][]nodeID)}
}

// merge adds provider's claims without clearing earlier ones. New keys enter
// in page order, evicting whole keys from the front when the FIFO is full.
func (x *hintIndex) merge(provider nodeID, keys []cacheKey) {
	for _, key := range keys {
		providers, exists := x.byKey[key]
		if !exists {
			if x.fifo.Len() == x.limit {
				oldest := x.fifo.Front()
				delete(x.byKey, oldest.Value.(cacheKey))
				x.fifo.Remove(oldest)
			}
			x.fifo.PushBack(key)
		}
		if !slices.Contains(providers, provider) {
			x.byKey[key] = append(providers, provider)
		}
	}
}

// providers returns an owned, sorted list of the provider IDs for key.
func (x *hintIndex) providers(key cacheKey) []string {
	providers := x.byKey[key]
	if len(providers) == 0 {
		return nil
	}
	ids := make([]string, len(providers))
	for i, provider := range providers {
		ids[i] = provider.String()
	}
	slices.Sort(ids)
	return ids
}

// len returns the number of distinct retained keys.
func (x *hintIndex) len() int { return len(x.byKey) }
