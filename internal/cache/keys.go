package cache

import "errors"

// KeyPosition identifies a position in one open store's live recency order.
// Last is a logical position, not a wall-clock timestamp. The zero value starts
// a listing and marks its end. Positions do not survive reopening the store.
type KeyPosition struct {
	Incarnation [16]byte
	Last        int64
	Key         string
}

// ErrKeyCursor means a continuation is malformed or belongs to another store
// lifetime. A caller must not silently restart a partial listing.
var ErrKeyCursor = errors.New("invalid cache key cursor")

// KeyPage copies up to limit eligible committed keys in LRU-to-MRU order,
// strictly after after. limit must be positive. A nonzero next identifies the
// last returned key when more eligible keys follow. No listing session or
// entry pointers are retained between calls. Pages are live, not a snapshot:
// accesses can move keys later and make them appear again on a later page.
// Invalid entries, removal claims, active fills, and a closed store are omitted.
// Listed keys remain advisory because a later Get may still miss.
func (s *Store) KeyPage(after KeyPosition, limit int) ([]string, KeyPosition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 {
		return nil, KeyPosition{}, ErrInvalidConfig
	}
	if after != (KeyPosition{}) && (after.Incarnation != s.incarnation || after.Last <= 0 || len(after.Key) != keyHexLen || !isLowerHex(after.Key)) {
		return nil, KeyPosition{}, ErrKeyCursor
	}
	if s.closed {
		return nil, KeyPosition{}, nil
	}

	start := s.evictionOrder.Front()

	// The usual continuation starts directly after the unchanged boundary.
	// If it moved or disappeared, find the first position beyond its old tuple.
	if e := s.entries[after.Key]; e != nil && e.last == after.Last {
		start = e.eviction.Next()
	}

	keys := make([]string, 0, min(limit, len(s.entries)))
	var last KeyPosition
	for item := start; item != nil; item = item.Next() {
		e := item.Value.(*entry)
		if e.invalid || e.claimed || e.last < after.Last || e.last == after.Last && e.meta.Key <= after.Key {
			continue
		}
		if len(keys) == limit {
			return keys, last, nil
		}

		keys = append(keys, e.meta.Key)
		last = KeyPosition{Incarnation: s.incarnation, Last: e.last, Key: e.meta.Key}
	}

	return keys, KeyPosition{}, nil
}
