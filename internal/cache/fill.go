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
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		s.admissions[AdmissionClosed]++
		return nil, &AdmissionError{Reason: AdmissionClosed}
	}
	if e, ok := s.entries[meta.Key]; ok && !e.invalid {
		s.admissions[AdmissionExisting]++
		return nil, &AdmissionError{Reason: AdmissionExisting}
	} else if ok && e.pins == 0 {
		// No reader remains to retry cleanup. Reclaim the invalid entry's slot
		// and bytes on successful removal; otherwise keep them charged.
		if os.RemoveAll(e.dir) == nil {
			delete(s.entries, meta.Key)
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
	d, err := os.MkdirTemp(s.dir, ".fill-")
	if err != nil {
		s.admissions[AdmissionIOError]++
		return nil, &AdmissionError{Reason: AdmissionIOError, Cause: err}
	}
	if err = os.Chmod(d, 0o700); err != nil {
		return nil, s.failAdmissionLocked(d, charge, err)
	}
	f, err := os.OpenFile(filepath.Join(d, "body"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, s.failAdmissionLocked(d, charge, err)
	}
	if err = f.Chmod(0o600); err != nil {
		f.Close()
		return nil, s.failAdmissionLocked(d, charge, err)
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
	s.admissions[AdmissionAccepted]++
	s.reserved += charge
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
func (f *Fill) Commit() error {
	f.mu.Lock()
	defer f.unlockAndStopIfDone()
	if f.done {
		return os.ErrClosed
	}
	if !time.Now().Before(f.deadline) {
		return f.finish(errFillExpired)
	}
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
	b, err := encodeMeta(f.meta)
	if err != nil {
		return f.finish(err)
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
	if err != nil {
		return f.finish(err)
	}
	if err = syncDirectory(f.dir); err != nil {
		return f.finish(err)
	}
	if !time.Now().Before(f.deadline) {
		return f.finish(errFillExpired)
	}
	dest := filepath.Join(f.store.dir, f.meta.Key)
	// Rename cannot replace a non-empty directory. Retire its prior owner
	// before publishing so old readers and cleanup retries cannot remove it.
	if err = f.supersedeTombstone(dest); err != nil {
		return f.finish(err)
	}
	if err = os.Rename(f.dir, dest); err != nil {
		return f.finish(err)
	}
	f.dir = dest
	if err = syncDirectory(f.store.dir); err != nil {
		return f.finish(err)
	}
	if !time.Now().Before(f.deadline) {
		return f.finish(errFillExpired)
	}
	f.store.mu.Lock()
	if !time.Now().Before(f.deadline) {
		f.store.mu.Unlock()
		return f.finish(errFillExpired)
	}
	delete(f.store.fills, f.meta.Key)
	f.store.reserved -= f.charge
	f.store.entries[f.meta.Key] = &entry{
		meta:     f.meta,
		dir:      dest,
		charge:   f.charge,
		last:     time.Now().UnixNano(),
		verified: true, // Just hash-verified during Commit; Readers can skip re-verification.
	}
	f.store.bytes += f.charge
	f.store.fillSuccess++
	f.store.mu.Unlock()
	f.done = true
	close(f.completed)
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
	removeErr := os.RemoveAll(f.dir)
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

// supersedeTombstone removes an invalid entry or failed publication under s.mu.
// Detaching its map entry or cleanup record prevents later removals of the
// replacement path. Pinned bodies retain their charge until their final unpin.
func (f *Fill) supersedeTombstone(dest string) error {
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	e, ok := f.store.entries[f.meta.Key]
	if !ok {
		// Retire any failed publication's cleanup record before reusing dest.
		return f.store.removeCleanupLocked(dest)
	}
	if !e.invalid {
		return errors.New("cache fill destination occupied by a valid entry")
	}
	if err := os.RemoveAll(e.dir); err != nil {
		return err
	}
	delete(f.store.entries, f.meta.Key)
	// Unlinking does not free the body while readers hold its inode open.
	// A detached pinned entry retains its charge until its final unpin.
	if e.pins == 0 && !e.reclaimed {
		e.reclaimed = true
		f.store.bytes -= e.charge
	}
	return nil
}

func (s *Store) refuse(reason AdmissionReason, cause error) error {
	s.mu.Lock()
	s.admissions[reason]++
	s.mu.Unlock()
	return &AdmissionError{Reason: reason, Cause: cause}
}
func syncDirectory(p string) error {
	d, e := os.Open(p)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
