package cache

import "container/list"

const cleanupRetryLimit = 8 // Maximum background cleanup attempts per admission.

type cleanup struct {
	charge int64
	queued *list.Element
}

// retainCleanupLocked transfers an admitted directory's charge to pending
// cleanup. Each record consumes its original entry slot, bounding the map by
// MaxEntries. The caller must already have released any fill reservation.
func (s *Store) retainCleanupLocked(dir string, charge int64) {
	if s.failedCleanups == nil {
		s.failedCleanups = make(map[string]cleanup)
	}
	s.failedCleanups[dir] = cleanup{charge: charge, queued: s.cleanupQueue.PushBack(dir)}
	s.bytes += charge
}

// retryCleanupsLocked rotates failures so later directories get a turn. Limit
// attempts even when more cleanup could avoid eviction or admission refusal.
// The queue contains exactly the charged records, with no stale path retries.
func (s *Store) retryCleanupsLocked() {
	for range min(cleanupRetryLimit, s.cleanupQueue.Len()) {
		next := s.cleanupQueue.Front()
		path := next.Value.(string)
		s.startOpLocked()
		s.mu.Unlock()
		err := s.removeAll(path)
		s.mu.Lock()
		s.finishOpLocked()
		if err != nil {
			s.cleanupQueue.MoveToBack(next)
		} else if pending, ok := s.failedCleanups[path]; ok {
			s.cleanupQueue.Remove(pending.queued)
			delete(s.failedCleanups, path)
			s.bytes -= pending.charge
		}
	}
}
