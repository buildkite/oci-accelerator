package peering

import (
	"encoding/base64"
	"encoding/binary"

	"github.com/buildkite/oci-accelerator/internal/cache"
)

// Cursors are fixed-size, versioned, URL-safe tokens. They are neither secret
// nor signed. Authenticated peers may select any position in their listing.
const keysCursorBytes = 1 + 16 + 8 + cacheKeyLen

func encodeKeysCursor(position cache.KeyPosition) (string, error) {
	key, ok := parseCacheKey(position.Key)
	if !ok || position.Last <= 0 || position.Incarnation == ([16]byte{}) {
		return "", errInvalidKeysPage
	}
	var raw [keysCursorBytes]byte
	raw[0] = 1
	copy(raw[1:17], position.Incarnation[:])
	binary.BigEndian.PutUint64(raw[17:25], uint64(position.Last))
	copy(raw[25:], key[:])
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func decodeKeysCursor(token string) (cache.KeyPosition, error) {
	if len(token) != base64.RawURLEncoding.EncodedLen(keysCursorBytes) {
		return cache.KeyPosition{}, errInvalidKeysPage
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(raw) != keysCursorBytes || raw[0] != 1 {
		return cache.KeyPosition{}, errInvalidKeysPage
	}
	var position cache.KeyPosition
	copy(position.Incarnation[:], raw[1:17])
	position.Last = int64(binary.BigEndian.Uint64(raw[17:25]))
	var key cacheKey
	copy(key[:], raw[25:])
	position.Key = key.String()
	if position.Last <= 0 || position.Incarnation == ([16]byte{}) {
		return cache.KeyPosition{}, errInvalidKeysPage
	}
	return position, nil
}
