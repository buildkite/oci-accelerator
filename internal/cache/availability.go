package cache

import "slices"

// Keys returns the sorted keys of committed entries that have not been
// invalidated. It returns an empty slice once the store is closed.
//
// The result is an availability hint, not a guarantee of a hit. It includes
// entries that a removal currently claims, because a successful removal
// signals Changes and a failed removal leaves the entry in place.
func (s *Store) Keys() []string {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return []string{}
	}
	keys := make([]string, 0, len(s.entries))
	for key, e := range s.entries {
		if !e.invalid {
			keys = append(keys, key)
		}
	}
	s.mu.Unlock()

	slices.Sort(keys)
	return keys
}

// Changes returns the store's single availability notification channel. A
// receive means Keys may have changed since the previous receive. Bursts of
// changes coalesce into one pending notification, so the subscriber must call
// Changes before taking its first Keys snapshot and take a fresh snapshot after
// every receive. Every call returns the same channel, which is never closed.
func (s *Store) Changes() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.changes == nil {
		s.changes = make(chan struct{}, 1)
	}
	return s.changes
}

// notifyChangeLocked records that Keys may have changed without blocking. It
// is a no-op until Changes allocates the channel.
func (s *Store) notifyChangeLocked() {
	if s.changes == nil {
		return
	}
	select {
	case s.changes <- struct{}{}:
	default:
	}
}
