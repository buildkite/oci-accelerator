package cache

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const metadataReservation int64 = 4096 // capacity charged per entry, including its meta file

const (
	keyHexLen       = 64                            // A key is the lowercase hex of a SHA-256 sum.
	digestPrefix    = "sha256:"                     // Every digest is a SHA-256 digest.
	digestLen       = len(digestPrefix) + keyHexLen // "sha256:" plus 64 hex characters.
	maxMediaTypeLen = 1024                          // Bound on the stored media-type string.
)

// Metadata describes an immutable object representation. The caller supplies a
// lowercase, 64-hex-character Key identifying that representation, including its
// digest and media type. Size must agree with the digest-verified object's size.
// Get requires all fields to match, and Begin refuses an existing valid key even
// when other fields differ. Inconsistent metadata never replaces a valid entry.
type Metadata struct {
	Key, Digest string
	Size        int64
	MediaType   string
}

type diskMetadata struct {
	Metadata
	Committed int64 `json:"committed"`
}

// validMeta reports whether m is a well-formed key for a SHA-256 object: a
// lowercase-hex key, a lowercase "sha256:"-prefixed hex digest, a non-negative
// size, and a bounded media type.
func validMeta(m Metadata) bool {
	if m.Size < 0 || len(m.Key) != keyHexLen || len(m.MediaType) > maxMediaTypeLen {
		return false
	}
	if len(m.Digest) != digestLen || !strings.HasPrefix(m.Digest, digestPrefix) {
		return false
	}

	return isLowerHex(m.Key) && isLowerHex(m.Digest[len(digestPrefix):])
}

func encodeMeta(m Metadata) ([]byte, error) {
	b, e := json.Marshal(diskMetadata{Metadata: m, Committed: time.Now().UnixNano()})
	if e == nil && len(b) > int(metadataReservation) {
		e = errors.New("cache metadata too large")
	}

	return b, e
}

// isLowerHex reports whether s consists solely of lowercase hexadecimal digits.
// The empty string qualifies. Callers bound the length separately.
func isLowerHex(s string) bool {
	if _, err := hex.DecodeString(s); err != nil {
		return false
	}

	return s == strings.ToLower(s)
}
