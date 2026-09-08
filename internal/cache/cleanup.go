package cache

import (
	"container/list"
	"errors"
	"os"
)

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
		if err := s.removeCleanupLocked(next.Value.(string)); err != nil {
			s.cleanupQueue.MoveToBack(next)
		}
	}
}

// removeCleanupLocked reclaims recorded capacity only after successful removal.
// Clear the record before reusing a path so a later retry cannot delete new data.
func (s *Store) removeCleanupLocked(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if pending, ok := s.failedCleanups[dir]; ok {
		s.cleanupQueue.Remove(pending.queued)
		delete(s.failedCleanups, dir)
		s.bytes -= pending.charge
	}
	return nil
}

// failAdmissionLocked cleans up a staging directory that never became a Fill.
// Admission has already checked capacity under s.mu, so it can retain one slot.
func (s *Store) failAdmissionLocked(dir string, charge int64, cause error) error {
	if err := os.RemoveAll(dir); err != nil {
		s.retainCleanupLocked(dir, charge)
		cause = errors.Join(cause, err)
	}
	s.admissions[AdmissionIOError]++
	return &AdmissionError{Reason: AdmissionIOError, Cause: cause}
}
