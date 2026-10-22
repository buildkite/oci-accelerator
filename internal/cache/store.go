package cache

import (
	"container/list"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Store is an open cache directory. The caller that opens a Store owns it and
// must Close it.
//
// Capacity is tracked in bytes and entry slots. An object's charge is its Size
// plus metadataReservation. In-progress fills hold their charge in reserved
// until they end, and Commit converts it into committed bytes. Directories awaiting
// deferred cleanup stay charged in bytes until removal succeeds. Every committed
// entry, active fill, and pending cleanup consumes one slot against maxEntries.
//
// Lock order: Fill.mu may briefly take mu to publish or release accounting,
// but mu is never held while taking Fill.mu. Filesystem removal and staging run
// with mu released, so unrelated hits and Stats continue. Claimed generations
// stay charged and cannot be pinned or reused meanwhile.
type Store struct {
	mu                                              sync.Mutex                           // guards all mutable store state below, including entry bookkeeping. ...Locked helpers require it.
	admissionMu                                     sync.Mutex                           // serializes admission and path ownership so key ownership decisions don't race.
	dir                                             string                               // root directory holding the cache on disk.
	max                                             int64                                // maximum total bytes (data plus metadata reservation) the cache may hold.
	maxFills, maxEntries                            int                                  // caps on concurrent in-flight fills and total live entries.
	fillTimeout                                     time.Duration                        // deadline after which a stalled in-progress fill is expired.
	failedCleanups                                  map[string]cleanup                   // temp dirs whose removal failed, keyed by path, awaiting retry.
	cleanupQueue                                    list.List                            // FIFO retry order of failed-cleanup paths.
	bytes, reserved                                 int64                                // bytes charged to published entries, and bytes reserved for in-flight fills.
	evictions                                       int64                                // cumulative count of entries evicted to reclaim space.
	readers                                         int                                  // count of currently open Readers.
	fillSuccess, fillFailure, fillAbort, fillExpiry int64                                // terminal fill outcome counters.
	admissions                                      [AdmissionReasonCount]int64          // per-reason admission decision counters.
	entries                                         map[string]*entry                    // published cache entries indexed by key.
	fills                                           map[string]*Fill                     // in-progress fills indexed by key.
	claims                                          map[string]int64                     // reserved key ownership and byte charge during staging, before a fill publishes.
	activeOps                                       int                                  // count of in-flight filesystem operations running with mu released.
	opsDone                                         *sync.Cond                           // signaled when activeOps reaches zero, so Close can wait for drain.
	removeAll                                       func(string) error                   // injectable os.RemoveAll for tests.
	mkdirTemp                                       func(string, string) (string, error) // injectable os.MkdirTemp for tests.
	evictionOrder                                   list.List                            // entries ordered by (last, key) for eviction victim selection.
	recency                                         int64                                // latest assigned logical access time, never reset during this store's lifetime.
	incarnation                                     [16]byte                             // identifies this open store's key cursors, never persisted.
	evictionAfterLast                               int64                                // resume after this (last, key) position even if its entry disappears or pins.
	evictionAfter                                   string                               // paired with evictionAfterLast.
	lock                                            *os.File                             // held OS file lock enforcing single-process ownership of dir.
	closed                                          bool                                 // true once the store is shutting down and rejecting new work.
}

// completedNotification is a shared, already-closed channel returned for entries
// whose fill has already published. A receive on it proceeds immediately.
var completedNotification = func() <-chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

// OpenWithLimits opens or creates the private cache directory dir, takes its
// exclusive lock, and inventories existing entries, discarding abandoned fills.
// Errors match ErrInvalidConfig, ErrLockConflict, ErrUnsafeState, or
// ErrFilesystem. Filesystem errors also wrap their cause.
func OpenWithLimits(dir string, limits Limits) (*Store, error) {
	// Reject an empty directory or out-of-range limits before touching the filesystem.
	if dir == "" || !limits.valid() {
		return nil, ErrInvalidConfig
	}

	dir = filepath.Clean(dir)
	lf, err := acquireLock(dir)
	if err != nil {
		return nil, err
	}

	// Build the store now that we hold the lock.
	s := &Store{
		dir:         dir,
		max:         limits.MaxBytes,
		maxFills:    limits.MaxFills,
		maxEntries:  limits.MaxEntries,
		fillTimeout: limits.FillTimeout,
		entries:     make(map[string]*entry),
		fills:       make(map[string]*Fill),
		claims:      make(map[string]int64),
		lock:        lf,
		removeAll:   os.RemoveAll,
		mkdirTemp:   os.MkdirTemp,
	}
	rand.Read(s.incarnation[:])

	// Wire the shutdown condition variable to the store mutex.
	s.opsDone = sync.NewCond(&s.mu)

	// Load any entries already on disk into the in-memory index.
	s.mu.Lock()
	err = s.inventory()
	s.mu.Unlock()
	if err != nil {
		s.releaseLock()
		if !errors.Is(err, ErrUnsafeState) {
			err = filesystemError(err)
		}
		return nil, err
	}

	return s, nil
}

// Begin admits meta's key, reserves capacity (evicting if needed), and stages a
// fill. It returns an *AdmissionError, matching ErrBypass, when the object
// cannot be admitted. The caller should then serve the object uncached.
func (s *Store) Begin(meta Metadata) (*Fill, error) {
	return s.begin(meta)
}

// Completion returns a notification for a matching active or already-published
// fill. The channel closes only after the store's terminal state is visible.
func (s *Store) Completion(meta Metadata) (<-chan struct{}, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, false
	}
	if e, ok := s.entries[meta.Key]; ok && e.meta == meta && !e.invalid {
		return completedNotification, true
	}
	if f, ok := s.fills[meta.Key]; ok && f.meta == meta {
		return f.completed, true
	}
	return nil, false
}

// Get pins the committed entry matching meta, verifies its body once if it is
// not yet verified, and returns a Reader that owns the pin. It returns ErrMiss when no
// valid matching entry exists. A body that fails verification becomes a miss.
func (s *Store) Get(ctx context.Context, meta Metadata) (*Reader, error) {
	return s.get(ctx, meta, nil, nil)
}

// Stats returns a point-in-time snapshot of the store's accounting counters.
func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	var cleanupBytes int64
	for _, pending := range s.failedCleanups {
		cleanupBytes += pending.charge
	}
	return Stats{
		Bytes:        s.bytes,
		Reserved:     s.reserved,
		Entries:      len(s.entries),
		Fills:        len(s.fills),
		Pending:      len(s.claims),
		Evictions:    s.evictions,
		Readers:      s.readers,
		FillSuccess:  s.fillSuccess,
		FillFailure:  s.fillFailure,
		FillAbort:    s.fillAbort,
		FillExpiry:   s.fillExpiry,
		Admissions:   s.admissions,
		CleanupBytes: cleanupBytes,
		CleanupSlots: len(s.failedCleanups),
	}
}

// Close permanently rejects new work, aborts active fills, and waits for claimed
// filesystem operations. It returns any abort and lock-release errors.
// It does not wait for readers, including Gets already opening or verifying a
// body, which may still return Readers. If any readers were active at the initial
// check, Close retains the filesystem lock and returns ErrActiveReaders even if
// they drain before it returns. The caller must finish those Gets, close every
// Reader, and call Close again. Closing Readers alone does not release the lock.
func (s *Store) Close() error {

	// Mark the store closed and snapshot the fills and reader count under the lock.
	s.mu.Lock()
	s.closed = true
	fs := make([]*Fill, 0, len(s.fills))
	readers := s.readers // Includes readers of detached entries.
	for _, f := range s.fills {
		fs = append(fs, f)
	}
	s.mu.Unlock()

	// Abort each snapshotted fill, collecting any errors.
	var abortErr error
	for _, f := range fs {
		abortErr = errors.Join(abortErr, f.Abort())
	}

	// Wait for in-flight store operations (e.g. unlocked removals) to finish.
	s.mu.Lock()
	for s.activeOps != 0 {
		s.opsDone.Wait()
	}
	s.mu.Unlock()

	// Retain the lock if readers were active at the initial check. The caller retries.
	if readers != 0 {
		return errors.Join(filesystemError(abortErr), ErrActiveReaders)
	}

	// No readers: release the lock and report abort and release errors together.
	return filesystemError(errors.Join(abortErr, s.releaseLock()))
}

func (s *Store) startOpLocked() { s.activeOps++ }

func (s *Store) finishOpLocked() {
	s.activeOps--
	if s.activeOps == 0 {
		s.opsDone.Broadcast()
	}
}
