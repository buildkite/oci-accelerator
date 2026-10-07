package cache

import "slices"

// Keys returns the sorted keys of committed entries that are not invalid. It
// excludes active fills, which may still fail or be aborted. The list is
// advisory: Get can still miss a listed key, for example after Close or when
// first verification after a restart finds a corrupt body.
func (s *Store) Keys() []string {
	s.mu.Lock()
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

// Changes returns the store's single change notification channel, creating it
// on first use so stores without a subscriber pay nothing. It receives after
// the set reported by Keys may have changed. Every call returns the same
// channel, so only one goroutine may receive from it. Notifications coalesce
// into one pending wakeup and carry no event history, so the subscriber should
// call Changes before Keys and rescan after each receive. The channel is never
// closed, including by Close, so the subscriber must stop on its own context.
func (s *Store) Changes() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.changes == nil {
		s.changes = make(chan struct{}, 1)
	}
	return s.changes
}

// invalidateLocked retires e from lookups. Only the generation that still owns
// its key is visible to Keys, so retiring a detached one changes nothing.
func (s *Store) invalidateLocked(e *entry) {
	if e.invalid {
		return
	}
	e.invalid = true
	if s.entries[e.meta.Key] == e {
		s.notifyChangeLocked()
	}
}

// notifyChangeLocked never blocks: a full buffer already holds a wakeup, and a
// nil channel means nobody has subscribed.
func (s *Store) notifyChangeLocked() {
	select {
	case s.changes <- struct{}{}:
	default:
	}
}
