// Package cache provides a private, bounded filesystem cache.
package cache

import (
	"container/list"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Store struct {
	mu                                              sync.Mutex
	admissionMu                                     sync.Mutex
	dir                                             string
	max                                             int64
	maxFills, maxEntries                            int
	fillTimeout                                     time.Duration
	failedCleanups                                  map[string]cleanup
	cleanupQueue                                    list.List
	bytes, reserved                                 int64
	evictions                                       int64
	readers                                         int
	fillSuccess, fillFailure, fillAbort, fillExpiry int64
	admissions                                      [AdmissionReasonCount]int64
	entries                                         map[string]*entry
	fills                                           map[string]*Fill
	claims                                          map[string]int64
	activeOps                                       int
	opsDone                                         *sync.Cond
	removeAll                                       func(string) error
	mkdirTemp                                       func(string, string) (string, error)
	evictionOrder                                   list.List // entries ordered by (last, key)
	// Resume after this (last, key) position even if its entry disappears or pins.
	evictionAfterLast int64
	evictionAfter     string
	lock              *os.File
	closed            bool
}

// completedNotification is a shared, already-closed channel returned for entries
// whose fill has already published; a receive on it proceeds immediately.
var completedNotification = func() <-chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

func OpenWithLimits(dir string, limits Limits) (*Store, error) {
	// Reject an empty directory or out-of-range limits before touching the filesystem.
	if dir == "" || !limits.valid() {
		return nil, ErrInvalidLimits
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

func (s *Store) Get(ctx context.Context, meta Metadata) (*Reader, error) {
	return s.get(ctx, meta, nil, nil)
}

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
// Reader, and call Close again; closing Readers alone does not release the lock.
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

	// Retain the lock if readers were active at the initial check; caller retries.
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
