package peering

import "encoding/hex"

const (
	nodeIDLen   = 12
	cacheKeyLen = 32
)

// nodeID is a validated node identity in fixed-size form, so hint records do
// not allocate a string for every provider.
type nodeID [nodeIDLen]byte

// cacheKey is a decoded cache key. Its text form is exactly 64 lowercase hex
// digits.
type cacheKey [cacheKeyLen]byte

// parseNodeID converts a node ID in text form and reports whether it is valid.
func parseNodeID(s string) (nodeID, bool) {
	var id nodeID
	if !validNodeID(s) {
		return id, false
	}
	copy(id[:], s)
	return id, true
}

// String returns the node ID in its text form.
func (id nodeID) String() string { return string(id[:]) }

// parseCacheKey converts a cache key in text form and reports whether it is
// valid. Uppercase hex digits are rejected so every key has one text form.
func parseCacheKey(s string) (cacheKey, bool) {
	var key cacheKey
	if len(s) != 2*cacheKeyLen {
		return key, false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return key, false
		}
	}
	_, err := hex.Decode(key[:], []byte(s))
	return key, err == nil
}

// String returns the cache key as 64 lowercase hex digits.
func (k cacheKey) String() string { return hex.EncodeToString(k[:]) }
