package cache

import (
	"encoding/hex"
	"errors"
	"hash"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var errFillExpired = errors.New("cache fill expired")

// Fill stages one admitted object until Commit publishes it or it ends
// otherwise. A fill that is aborted, misses its deadline, fails verification,
// or cannot be written is discarded; a partial object is never published.
type Fill struct {
	mu              sync.Mutex // guards this fill's state; see Store for lock order.
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

// Write appends p to the staged body, hashing it and ending the fill if it
// would exceed the declared Size.
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

	// Write and flush the meta file, then flush the staging directory.
	if err := f.writeMetaFile(); err != nil {
		return f.finish(err)
	}
	if err := syncDirectory(f.dir); err != nil {
		return f.finish(err)
	}
	if !time.Now().Before(f.deadline) {
		return f.finish(errFillExpired)
	}

	// Publish by renaming the staging directory into place, then flush the parent.
	// Staging lives in the store directory, so the rename stays on one filesystem.
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

// Abort discards the fill without publishing it. It is a no-op once the fill
// has ended.
func (f *Fill) Abort() error {
	f.mu.Lock()
	defer f.unlockAndStopIfDone()
	if f.done {
		return nil
	}
	return f.finish(nil)
}

// writeMetaFile writes the encoded metadata to an exclusive meta file in the
// staging directory, propagating the first error across chmod/write/sync/close.
func (f *Fill) writeMetaFile() error {
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

func syncDirectory(p string) error {
	d, e := os.Open(p)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
