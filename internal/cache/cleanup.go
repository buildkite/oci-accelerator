package cache

import (
	"errors"
	"os"
)

// retainCleanupLocked transfers an admitted directory's charge to pending
// cleanup. Each record consumes its original entry slot, bounding the map by
// MaxEntries. The caller must already have released any fill reservation.
func (s *Store) retainCleanupLocked(dir string, charge int64) {
	if s.failedCleanups == nil {
		s.failedCleanups = make(map[string]int64)
	}
	s.failedCleanups[dir] = charge
	s.bytes += charge
}

// removeCleanupLocked reclaims recorded capacity only after successful removal.
// Clear the record before reusing a path so a later retry cannot delete new data.
func (s *Store) removeCleanupLocked(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if charge, ok := s.failedCleanups[dir]; ok {
		delete(s.failedCleanups, dir)
		s.bytes -= charge
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
