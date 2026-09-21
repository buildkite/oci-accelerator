package cache

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"
)

const evictionAttemptLimit = 8 // Maximum victim-directory removals per Begin.

type AdmissionReason uint8

const (
	AdmissionAccepted AdmissionReason = iota
	AdmissionInvalid
	AdmissionOversized
	AdmissionExisting
	AdmissionFillLimit
	AdmissionCapacity
	AdmissionClosed
	AdmissionIOError
	AdmissionReasonCount
)

type AdmissionError struct {
	Reason AdmissionReason
	Cause  error
}

func (s *Store) begin(meta Metadata) (*Fill, error) {
	if !validMeta(meta) {
		return nil, s.refuse(AdmissionInvalid, nil)
	}
	charge, ok := add(meta.Size, metadataReservation)
	if !ok || charge > s.max {
		return nil, s.refuse(AdmissionOversized, nil)
	}
	// JSON escaping can exceed the meta file limit even for a short raw string.
	// Reject before eviction or staging; Commit still checks its final encoding.
	if _, err := encodeMeta(meta); err != nil {
		return nil, s.refuse(AdmissionInvalid, err)
	}
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		s.admissions[AdmissionClosed]++
		return nil, &AdmissionError{Reason: AdmissionClosed}
	}
	if e, ok := s.entries[meta.Key]; ok && !e.invalid {
		s.admissions[AdmissionExisting]++
		return nil, &AdmissionError{Reason: AdmissionExisting}
	} else if ok && e.pins == 0 && !e.claimed {
		// No reader remains to retry cleanup. Reclaim the invalid entry's slot
		// and bytes on successful removal; otherwise keep them charged.
		e.claimed = true
		s.startOpLocked()
		s.mu.Unlock()
		removeErr := s.removeAll(e.dir)
		s.mu.Lock()
		s.finishOpLocked()
		e.claimed = false
		if removeErr == nil && s.entries[meta.Key] == e && e.pins == 0 {
			s.deleteEntryLocked(e)
			if !e.reclaimed {
				e.reclaimed = true
				s.bytes -= e.charge
			}
		}
	}
	if _, ok := s.fills[meta.Key]; ok {
		s.admissions[AdmissionExisting]++
		return nil, &AdmissionError{Reason: AdmissionExisting}
	}
	if _, ok := s.claims[meta.Key]; ok {
		s.admissions[AdmissionExisting]++
		return nil, &AdmissionError{Reason: AdmissionExisting}
	}
	// Fill concurrency cannot be solved by eviction, so reject before touching
	// useful entries.
	if len(s.fills) >= s.maxFills {
		s.admissions[AdmissionFillLimit]++
		return nil, &AdmissionError{Reason: AdmissionFillLimit}
	}
	// Retry a bounded batch before eviction. Failed or deferred cleanups
	// keep their bytes and slots charged against admission limits.
	s.retryCleanupsLocked()
	if err := s.evictLocked(charge, 1, math.MaxInt64, evictionAttemptLimit); err != nil {
		if errors.Is(err, ErrBypass) {
			s.admissions[AdmissionCapacity]++
			return nil, &AdmissionError{Reason: AdmissionCapacity}
		}
		s.admissions[AdmissionIOError]++
		return nil, &AdmissionError{Reason: AdmissionIOError, Cause: err}
	}
	// Reserve capacity and key ownership before staging without exposing a
	// partially initialized Fill.
	s.claims[meta.Key] = charge
	s.reserved += charge
	// This claim spans every staging step, preventing Close from releasing the
	// process lock in the gaps between individual filesystem calls.
	s.startOpLocked()
	s.mu.Unlock()
	d, err := s.mkdirTemp(s.dir, ".fill-")
	var f *os.File
	if err == nil {
		err = os.Chmod(d, 0o700)
	}
	if err == nil {
		f, err = os.OpenFile(filepath.Join(d, "body"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	}
	if err == nil {
		err = f.Chmod(0o600)
	}
	// Finalize once. If staging failed, perform all close/removal work while the
	// reservation and active operation still protect its resources.
	var cleanupErr, removeErr error
	if err != nil {
		if f != nil {
			cleanupErr = f.Close()
		}
		if d != "" {
			removeErr = s.removeAll(d)
			cleanupErr = errors.Join(cleanupErr, removeErr)
		}
	}
	s.mu.Lock()
	if err != nil || s.closed {
		if err == nil { // Close raced with successful staging; reject and clean it.
			s.mu.Unlock()
			cleanupErr = f.Close()
			removeErr = s.removeAll(d)
			cleanupErr = errors.Join(cleanupErr, removeErr)
			s.mu.Lock()
		}
		delete(s.claims, meta.Key)
		s.reserved -= charge
		if removeErr != nil {
			s.retainCleanupLocked(d, charge)
		}
		s.finishOpLocked()
		if s.closed {
			s.admissions[AdmissionClosed]++
			return nil, &AdmissionError{Reason: AdmissionClosed, Cause: errors.Join(err, cleanupErr)}
		}
		s.admissions[AdmissionIOError]++
		return nil, &AdmissionError{Reason: AdmissionIOError, Cause: errors.Join(err, cleanupErr)}
	}
	fill := &Fill{
		store:     s,
		meta:      meta,
		dir:       d,
		file:      f,
		hash:      sha256.New(),
		charge:    charge,
		deadline:  time.Now().Add(s.fillTimeout),
		timerDone: make(chan struct{}),
		completed: make(chan struct{}),
	}
	s.fills[meta.Key] = fill
	delete(s.claims, meta.Key)
	s.finishOpLocked()
	s.admissions[AdmissionAccepted]++
	fill.timer = time.AfterFunc(s.fillTimeout, func() {
		fill.mu.Lock()
		if !fill.done {
			_ = fill.finish(errFillExpired)
		}
		fill.mu.Unlock()
		fill.timerDoneOnce.Do(func() { close(fill.timerDone) })
	})
	return fill, nil
}

func (s *Store) refuse(reason AdmissionReason, cause error) error {
	s.mu.Lock()
	s.admissions[reason]++
	s.mu.Unlock()
	return &AdmissionError{Reason: reason, Cause: cause}
}

func (e *AdmissionError) Error() string {
	message := "cache admission refused: " + e.Reason.String()
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	return message
}

func (e *AdmissionError) Unwrap() error        { return e.Cause }
func (e *AdmissionError) Is(target error) bool { return target == ErrBypass }

func (r AdmissionReason) String() string {
	names := [...]string{"accepted", "invalid", "oversized", "existing", "fill_limit", "capacity", "closed", "io_error"}
	if int(r) >= len(names) {
		return fmt.Sprintf("unknown(%d)", uint8(r))
	}
	return names[r]
}
