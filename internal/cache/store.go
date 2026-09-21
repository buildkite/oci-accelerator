// Package cache provides a private, bounded filesystem cache.
package cache

import (
	"container/list"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const metadataReservation int64 = 4096 // capacity charged per entry, including its meta file

var (
	ErrMiss          = errors.New("cache miss")
	ErrBypass        = errors.New("cache bypass")
	ErrInvalidLimits = errors.New("invalid cache limits")
	ErrLockConflict  = errors.New("cache lock conflict")
	ErrUnsafeState   = errors.New("unsafe cache state")
	ErrFilesystem    = errors.New("cache filesystem failure")
	ErrActiveReaders = errors.New("cache has active readers")
)

func filesystemError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrFilesystem, err)
}

var completedNotification = func() <-chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

// Metadata describes an immutable object representation. The caller supplies a
// lowercase, 64-hex-character Key identifying that representation, including its
// digest and media type. Size must agree with the digest-verified object's size.
// Get requires all fields to match; Begin refuses an existing valid key even
// when other fields differ. Inconsistent metadata never replaces a valid entry.
type Metadata struct {
	Key, Digest string
	Size        int64
	MediaType   string
}
type Stats struct {
	Bytes, Reserved                                 int64
	Entries, Fills, Pending                         int
	Evictions                                       int64
	Readers                                         int
	FillSuccess, FillFailure, FillAbort, FillExpiry int64
	Admissions                                      [AdmissionReasonCount]int64
	CleanupBytes                                    int64 // Included in Bytes until removal succeeds.
	CleanupSlots                                    int   // Charged against MaxEntries.
}

type diskMetadata struct {
	Metadata
	Committed int64 `json:"committed"`
}
type entry struct {
	meta      Metadata
	dir       string
	charge    int64
	last      int64
	pins      int
	verified  bool
	verifying chan struct{} // Non-nil while one Get verifies; guarded by Store.mu.
	invalid   bool
	claimed   bool // A synchronous removal owns this generation.
	// Charges are reclaimed once, after directory removal and the final unpin.
	reclaimed bool
	eviction  *list.Element
}

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

type Limits struct {
	MaxBytes    int64
	MaxFills    int
	MaxEntries  int
	FillTimeout time.Duration
}

func (l Limits) valid() bool {
	return l.MaxBytes > 0 && l.MaxFills > 0 && l.MaxEntries > 0 && l.FillTimeout > 0
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

func OpenWithLimits(dir string, limits Limits) (*Store, error) {
	// Reject an empty directory or out-of-range limits before touching the filesystem.
	if dir == "" || !limits.valid() {
		return nil, ErrInvalidLimits
	}

	// Ensure the cache directory is a private, this-user-owned 0700 directory.
	dir = filepath.Clean(dir)
	if err := ensurePrivateDir(dir); err != nil {
		return nil, err
	}

	// Reject a pre-existing lock file that isn't a regular, 0600, owned file.
	lockPath := filepath.Join(dir, ".lock")
	if li, err := os.Lstat(lockPath); err == nil {
		if !li.Mode().IsRegular() || li.Mode().Perm() != 0o600 || !owned(li) {
			return nil, ErrUnsafeState
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, filesystemError(err)
	}

	// Open (creating if absent) the lock file.
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, filesystemError(err)
	}

	// Take an exclusive, non-blocking flock; a contended lock means another process owns this cache.
	if err = syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lf.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("%w: %w", ErrLockConflict, err)
		}
		return nil, filesystemError(err)
	}

	// Re-assert 0600 on the now-owned lock file in case it predated us with looser bits.
	if err = lf.Chmod(0o600); err != nil {
		lf.Close()
		return nil, filesystemError(err)
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

func ensurePrivateDir(dir string) error {
	// The parent need not be exactly 0700, but must be a real directory owned by
	// this user with no group- or other-write bit that would let another account
	// swap the cache directory underneath us.
	parentInfo, parentErr := os.Lstat(filepath.Dir(dir))
	parentPrivate := parentErr == nil && isRealDir(parentInfo) && parentInfo.Mode().Perm()&0o022 == 0 && owned(parentInfo)
	if !parentPrivate {
		if parentErr != nil {
			return filesystemError(parentErr)
		}
		return ErrUnsafeState
	}
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err = os.Mkdir(dir, 0o700); err != nil {
			return filesystemError(err)
		}
		if err = os.Chmod(dir, 0o700); err != nil {
			return filesystemError(err)
		}
		info, err = os.Lstat(dir)
	}
	// The cache directory itself must be owned by this user and exactly 0700.
	dirPrivate := err == nil && isRealDir(info) && info.Mode().Perm() == 0o700 && owned(info)
	if !dirPrivate {
		if err != nil {
			return filesystemError(err)
		}
		return ErrUnsafeState
	}
	return nil
}

// isRealDir reports whether info describes a directory that is not a symlink.
func isRealDir(info os.FileInfo) bool {
	return info.IsDir() && info.Mode()&os.ModeSymlink == 0
}

func owned(i os.FileInfo) bool {
	st, ok := i.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}

const (
	keyHexLen       = 64                            // A key is the lowercase hex of a SHA-256 sum.
	digestPrefix    = "sha256:"                     // Every digest is a SHA-256 digest.
	digestLen       = len(digestPrefix) + keyHexLen // "sha256:" plus 64 hex characters.
	maxMediaTypeLen = 1024                          // Bound on the stored media-type string.
)

// isLowerHex reports whether s consists solely of lowercase hexadecimal digits.
// The empty string qualifies; callers bound the length separately.
func isLowerHex(s string) bool {
	if _, err := hex.DecodeString(s); err != nil {
		return false
	}
	return s == strings.ToLower(s)
}

// validMeta reports whether m is a well-formed key for a SHA-256 object: a
// lowercase-hex key, a lowercase "sha256:"-prefixed hex digest, a non-negative
// size, and a bounded media type.
func validMeta(m Metadata) bool {
	if m.Size < 0 || len(m.Key) != keyHexLen || len(m.MediaType) > maxMediaTypeLen {
		return false
	}
	if len(m.Digest) != digestLen || !strings.HasPrefix(m.Digest, digestPrefix) {
		return false
	}
	return isLowerHex(m.Key) && isLowerHex(m.Digest[len(digestPrefix):])
}

func add(a, b int64) (int64, bool) {
	if a < 0 || b < 0 || a > int64(^uint64(0)>>1)-b {
		return 0, false
	}
	return a + b, true
}

// freeCapacityLocked reports the currently unused bytes and entry slots.
func (s *Store) freeCapacityLocked() (bytes int64, slots int) {
	return s.max - s.bytes - s.reserved, s.maxEntries - len(s.entries) - len(s.fills) - len(s.claims) - len(s.failedCleanups)
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

// Close rejects new work, aborts every active fill, and releases the filesystem
// lock once no readers remain. With readers still active it keeps the lock and
// returns ErrActiveReaders; the caller must drain them and call Close again.
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
func (s *Store) releaseLock() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
	err := errors.Join(unlockErr, s.lock.Close())
	s.lock = nil
	return err
}

func encodeMeta(m Metadata) ([]byte, error) {
	b, e := json.Marshal(diskMetadata{Metadata: m, Committed: time.Now().UnixNano()})
	if e == nil && len(b) > int(metadataReservation) {
		e = errors.New("cache metadata too large")
	}
	return b, e
}
