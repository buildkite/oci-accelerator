package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const evictionAttemptLimit = 8 // Maximum victim-directory removals per Begin.

var errFillExpired = errors.New("cache fill expired")

type Fill struct {
	mu              sync.Mutex
	store           *Store
	meta            Metadata
	dir             string
	file            *os.File
	hash            hash.Hash
	written, charge int64
	done            bool
	deadline        time.Time
	timer           *time.Timer
	timerDone       chan struct{}
	timerDoneOnce   sync.Once
	completed       chan struct{}
}

func (s *Store) Begin(meta Metadata) (*Fill, error) {
	if !validMeta(meta) {
		return nil, s.refuse(AdmissionInvalid, nil)
	}
	charge, ok := add(meta.Size, metadataReservation)
	if !ok || charge > s.max {
		return nil, s.refuse(AdmissionOversized, nil)
	}
	// JSON escaping can exceed the sidecar limit even for a short raw string.
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
func (f *Fill) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.unlockAndStopIfDone()
	if f.done {
		return 0, os.ErrClosed
	}
	if !time.Now().Before(f.deadline) {
		return 0, f.finish(errFillExpired)
	}
	if int64(len(p)) > f.meta.Size-f.written {
		return 0, f.finish(errors.New("cache fill exceeds declared size"))
	}
	n, e := f.file.Write(p)
	if n > 0 {
		_, _ = f.hash.Write(p[:n])
		f.written += int64(n)
	}
	if e != nil {
		return n, f.finish(e)
	}
	return n, e
}

// Commit verifies the written body against the declared size and digest, then
// atomically publishes the staged directory as a committed entry. Any failure,
// including a missed deadline re-checked at each durable step, ends the fill via
// finish. On success the entry is live and followers waiting on it wake up.
func (f *Fill) Commit() error {
	f.mu.Lock()
	defer f.unlockAndStopIfDone()
	if f.done {
		return os.ErrClosed
	}
	if !time.Now().Before(f.deadline) {
		return f.finish(errFillExpired)
	}

	// Verify the body, then flush and close it.
	if f.written != f.meta.Size || "sha256:"+hex.EncodeToString(f.hash.Sum(nil)) != f.meta.Digest {
		return f.finish(errors.New("cache fill verification failed"))
	}
	if err := f.file.Sync(); err != nil {
		return f.finish(err)
	}
	if err := f.file.Close(); err != nil {
		return f.finish(err)
	}
	f.file = nil

	// Write and flush the metadata sidecar, then flush the staging directory.
	if err := f.writeMetaSidecar(); err != nil {
		return f.finish(err)
	}
	if err := syncDirectory(f.dir); err != nil {
		return f.finish(err)
	}
	if !time.Now().Before(f.deadline) {
		return f.finish(errFillExpired)
	}

	// Publish by renaming the staging directory into place, then flush the parent.
	// Rename cannot replace a non-empty directory, so retire any prior owner first
	// so old readers and cleanup retries cannot remove the published path.
	dest := filepath.Join(f.store.dir, f.meta.Key)
	if err := f.supersedeTombstone(dest); err != nil {
		return f.finish(err)
	}
	if err := os.Rename(f.dir, dest); err != nil {
		return f.finish(err)
	}
	f.dir = dest
	if err := syncDirectory(f.store.dir); err != nil {
		return f.finish(err)
	}
	if !time.Now().Before(f.deadline) {
		return f.finish(errFillExpired)
	}

	// Record the entry in the store, converting the reservation into charge.
	if err := f.publishLocked(dest); err != nil {
		return f.finish(err)
	}
	f.done = true
	close(f.completed)
	return nil
}

// writeMetaSidecar writes the encoded metadata to an exclusive meta file in the
// staging directory, propagating the first error across chmod/write/sync/close.
func (f *Fill) writeMetaSidecar() error {
	b, err := encodeMeta(f.meta)
	if err != nil {
		return err
	}
	mf, err := os.OpenFile(filepath.Join(f.dir, "meta"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		err = mf.Chmod(0o600)
	}
	if err == nil {
		_, err = mf.Write(b)
	}
	if err == nil {
		err = mf.Sync()
	}
	if mf != nil {
		if e := mf.Close(); err == nil {
			err = e
		}
	}
	return err
}

// publishLocked records the committed entry under the store lock, re-checking
// the deadline one last time. It converts the fill's reservation into the
// entry's charge. It never returns a non-deadline error.
func (f *Fill) publishLocked(dest string) error {
	f.store.mu.Lock()
	if !time.Now().Before(f.deadline) {
		f.store.mu.Unlock()
		return errFillExpired
	}
	delete(f.store.fills, f.meta.Key)
	f.store.reserved -= f.charge
	e := &entry{
		meta:     f.meta,
		dir:      dest,
		charge:   f.charge,
		last:     time.Now().UnixNano(),
		verified: true, // Just hash-verified during Commit; Readers can skip re-verification.
	}
	f.store.addEntryLocked(e)
	f.store.bytes += f.charge
	f.store.fillSuccess++
	f.store.mu.Unlock()
	return nil
}
func (f *Fill) Abort() error {
	f.mu.Lock()
	defer f.unlockAndStopIfDone()
	if f.done {
		return nil
	}
	return f.finish(nil)
}

func (f *Fill) unlockAndStopIfDone() {
	done := f.done
	f.mu.Unlock()
	if done {
		f.stopTimer()
	}
}

func (f *Fill) stopTimer() {
	if f.timer.Stop() {
		f.timerDoneOnce.Do(func() { close(f.timerDone) })
	}
	<-f.timerDone
}
func (f *Fill) finish(cause error) error {
	if f.file != nil {
		cause = errors.Join(cause, f.file.Close())
		f.file = nil
	}
	f.store.admissionMu.Lock()
	f.store.mu.Lock()
	f.store.startOpLocked()
	f.store.mu.Unlock()
	removeErr := f.store.removeAll(f.dir)
	f.store.mu.Lock()
	f.store.finishOpLocked()
	f.store.mu.Unlock()
	f.store.admissionMu.Unlock()
	cause = errors.Join(cause, removeErr)
	f.store.mu.Lock()
	if cause == nil {
		f.store.fillAbort++
	} else if errors.Is(cause, errFillExpired) {
		f.store.fillExpiry++
	} else {
		f.store.fillFailure++
	}
	delete(f.store.fills, f.meta.Key)
	f.store.reserved -= f.charge
	if removeErr != nil {
		f.store.retainCleanupLocked(f.dir, f.charge)
	}
	f.store.mu.Unlock()
	f.done = true
	close(f.completed)
	return cause
}

// supersedeTombstone claims an invalid entry or failed publication under s.mu,
// removes it outside s.mu, then finalizes its accounting under s.mu.
// Detaching its map entry or cleanup record prevents later removals of the
// replacement path. Pinned bodies retain their charge until their final unpin.
func (f *Fill) supersedeTombstone(dest string) error {
	f.store.admissionMu.Lock()
	defer f.store.admissionMu.Unlock()
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	e, ok := f.store.entries[f.meta.Key]
	if !ok {
		// Retire any failed publication's cleanup record before reusing dest.
		f.store.startOpLocked()
		f.store.mu.Unlock()
		err := f.store.removeAll(dest)
		f.store.mu.Lock()
		f.store.finishOpLocked()
		if err == nil {
			if pending, exists := f.store.failedCleanups[dest]; exists {
				f.store.cleanupQueue.Remove(pending.queued)
				delete(f.store.failedCleanups, dest)
				f.store.bytes -= pending.charge
			}
		}
		return err
	}
	if !e.invalid {
		return errors.New("cache fill destination occupied by a valid entry")
	}
	e.claimed = true
	f.store.startOpLocked()
	f.store.mu.Unlock()
	err := f.store.removeAll(e.dir)
	f.store.mu.Lock()
	f.store.finishOpLocked()
	e.claimed = false
	if err != nil {
		return err
	}
	f.store.deleteEntryLocked(e)
	// Unlinking does not free the body while readers hold its inode open.
	// A detached pinned entry retains its charge until its final unpin.
	if e.pins == 0 && !e.reclaimed {
		e.reclaimed = true
		f.store.bytes -= e.charge
	}
	return nil
}

func syncDirectory(p string) error {
	d, e := os.Open(p)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
