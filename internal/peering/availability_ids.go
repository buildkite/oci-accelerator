package peering

import (
	"bytes"
	"encoding/hex"
	"slices"
)

const (
	nodeIDLen   = 12
	cacheKeyLen = 32
)

// nodeID is a validated node identity in fixed-size form, so hint records do
// not allocate a string for every seen ID.
type nodeID [nodeIDLen]byte

// cacheKey is a decoded cache key. Its text form is 64 lowercase hex digits.
type cacheKey [cacheKeyLen]byte

func parseNodeID(s string) (nodeID, bool) {
	var id nodeID
	if !validNodeID(s) {
		return id, false
	}
	copy(id[:], s)
	return id, true
}

func (id nodeID) String() string { return string(id[:]) }

func parseCacheKey(s string) (cacheKey, bool) {
	var key cacheKey
	if len(s) != 2*cacheKeyLen {
		return key, false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return key, false
		}
	}
	_, err := hex.Decode(key[:], []byte(s))
	return key, err == nil
}

func (k cacheKey) String() string { return hex.EncodeToString(k[:]) }

func compareNodeIDs(a, b nodeID) int { return bytes.Compare(a[:], b[:]) }

// sortedUnique reports whether ids are strictly ascending.
func sortedUnique(ids []nodeID) bool {
	for i := 1; i < len(ids); i++ {
		if compareNodeIDs(ids[i-1], ids[i]) >= 0 {
			return false
		}
	}
	return true
}

func containsNodeID(ids []nodeID, id nodeID) bool {
	_, found := slices.BinarySearchFunc(ids, id, compareNodeIDs)
	return found
}

// countMissing returns how many IDs in b are absent from a. Both are sorted
// and unique.
func countMissing(a, b []nodeID) int {
	missing, i := 0, 0
	for _, id := range b {
		for i < len(a) && compareNodeIDs(a[i], id) < 0 {
			i++
		}
		if i == len(a) || a[i] != id {
			missing++
		}
	}
	return missing
}

// unionNodeIDs merges two sorted, unique sets into a new, exactly sized
// sorted, unique set, so accounting by length matches its allocation.
func unionNodeIDs(a, b []nodeID) []nodeID {
	union := make([]nodeID, 0, len(a)+countMissing(a, b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch c := compareNodeIDs(a[i], b[j]); {
		case c < 0:
			union = append(union, a[i])
			i++
		case c > 0:
			union = append(union, b[j])
			j++
		default:
			union = append(union, a[i])
			i++
			j++
		}
	}
	union = append(union, a[i:]...)
	return append(union, b[j:]...)
}
