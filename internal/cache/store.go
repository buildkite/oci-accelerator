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
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const metadataReservation int64 = 4096 // capacity charged per entry, including its sidecar

var (
	ErrMiss   = errors.New("cache miss")
	ErrBypass = errors.New("cache bypass")
)

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

func (r AdmissionReason) String() string {
	names := [...]string{"accepted", "invalid", "oversized", "existing", "fill_limit", "capacity", "closed", "io_error"}
	if int(r) >= len(names) {
		return fmt.Sprintf("unknown(%d)", uint8(r))
	}
	return names[r]
}

type AdmissionError struct {
	Reason AdmissionReason
	Cause  error
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
	Entries, Fills                                  int
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
	// Charges are reclaimed once, after directory removal and the final unpin.
	reclaimed bool
}

type Store struct {
	mu                                              sync.Mutex
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

func (l Limits) valid() bool {
	return l.MaxBytes > 0 && l.MaxFills > 0 && l.MaxEntries > 0 && l.FillTimeout > 0
}

func Open(dir string, maxBytes int64) (*Store, error) {
	return OpenWithLimits(dir, Limits{MaxBytes: maxBytes, MaxFills: 16, MaxEntries: 100000, FillTimeout: 10 * time.Minute})
}

func OpenWithLimits(dir string, limits Limits) (*Store, error) {
	if dir == "" || !limits.valid() {
		return nil, errors.New("cache directory and capacity are required")
	}
	dir = filepath.Clean(dir)
	if err := ensurePrivateDir(dir); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(dir, ".lock")
	if li, err := os.Lstat(lockPath); err == nil {
		if !li.Mode().IsRegular() || li.Mode().Perm() != 0o600 || !owned(li) {
			return nil, errors.New("cache lock has insecure type, ownership, or permissions")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lf.Close()
		return nil, fmt.Errorf("cache is already open: %w", err)
	}
	if err = lf.Chmod(0o600); err != nil {
		lf.Close()
		return nil, err
	}
	s := &Store{
		dir:         dir,
		max:         limits.MaxBytes,
		maxFills:    limits.MaxFills,
		maxEntries:  limits.MaxEntries,
		fillTimeout: limits.FillTimeout,
		entries:     make(map[string]*entry),
		fills:       make(map[string]*Fill),
		lock:        lf,
	}
	if err := s.inventory(); err != nil {
		s.releaseLock()
		return nil, err
	}
	return s, nil
}

func ensurePrivateDir(dir string) error {
	parentInfo, parentErr := os.Lstat(filepath.Dir(dir))
	if parentErr != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 || parentInfo.Mode().Perm()&0o022 != 0 || !owned(parentInfo) {
		return errors.New("cache parent must be an owner-controlled non-symlink directory")
	}
	i, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err = os.Mkdir(dir, 0o700); err != nil {
			return err
		}
		if err = os.Chmod(dir, 0o700); err != nil {
			return err
		}
		i, err = os.Lstat(dir)
	}
	if err != nil || !i.IsDir() || i.Mode()&os.ModeSymlink != 0 || i.Mode().Perm() != 0o700 || !owned(i) {
		return errors.New("cache directory must be owned, non-symlink, and mode 0700")
	}
	return nil
}
func owned(i os.FileInfo) bool {
	st, ok := i.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}

func validMeta(m Metadata) bool {
	if m.Size < 0 || len(m.Key) != 64 || len(m.Digest) != 71 || !strings.HasPrefix(m.Digest, "sha256:") || len(m.MediaType) > 1024 {
		return false
	}
	_, e1 := hex.DecodeString(m.Key)
	_, e2 := hex.DecodeString(m.Digest[7:])
	return e1 == nil && e2 == nil && m.Key == strings.ToLower(m.Key) && m.Digest == strings.ToLower(m.Digest)
}

func add(a, b int64) (int64, bool) {
	if a < 0 || b < 0 || a > int64(^uint64(0)>>1)-b {
		return 0, false
	}
	return a + b, true
}

// before prevents an older startup candidate from evicting newer entries.
// maxAttempts bounds admission eviction; zero leaves startup eviction uncapped.
// ErrBypass means insufficient eligible capacity in this batch. If removal
// failures prevent admission, return the first filesystem error.
func (s *Store) evictLocked(need int64, needSlots int, before int64, maxAttempts int) error {
	fits := func() bool {
		return need <= s.max-s.bytes-s.reserved && len(s.entries)+len(s.fills)+len(s.failedCleanups)+needSlots <= s.maxEntries
	}
	if fits() {
		// A fitting admission must not reset another request's deferred batch.
		return nil
	}
	var es []*entry
	available := s.max - s.bytes - s.reserved
	availableSlots := s.maxEntries - len(s.entries) - len(s.fills) - len(s.failedCleanups)
	for _, e := range s.entries {
		if e.pins == 0 && e.last <= before {
			es = append(es, e)
			available += e.charge
			availableSlots++
		}
	}
	if available < need || availableSlots < needSlots {
		return ErrBypass
	}
	sort.Slice(es, func(i, j int) bool {
		if es[i].last == es[j].last {
			return es[i].meta.Key < es[j].meta.Key
		}
		return es[i].last < es[j].last
	})
	start, count := 0, len(es)
	if maxAttempts > 0 {
		count = min(count, maxAttempts)
		if s.evictionAfter != "" {
			for i, e := range es {
				if e.last > s.evictionAfterLast || e.last == s.evictionAfterLast && e.meta.Key > s.evictionAfter {
					start = i
					break
				}
			}
		}
		// Do not remove useful entries when even a fully successful batch
		// cannot admit the fill. Resume later so blocked or small victims
		// cannot permanently hide a usable victim beyond this batch.
		available = s.max - s.bytes - s.reserved
		availableSlots = s.maxEntries - len(s.entries) - len(s.fills) - len(s.failedCleanups)
		for i := range count {
			available += es[(start+i)%len(es)].charge
			availableSlots++
		}
		last := es[(start+count-1)%len(es)]
		s.evictionAfterLast, s.evictionAfter = last.last, last.meta.Key
		if available < need || availableSlots < needSlots {
			return ErrBypass
		}
	}
	var removeErr error
	for i := range count {
		if fits() {
			s.evictionAfter = ""
			return nil
		}
		e := es[(start+i)%len(es)]
		if err := os.RemoveAll(e.dir); err == nil {
			delete(s.entries, e.meta.Key)
			// Eviction only selects unpinned entries, so their charge is free.
			e.reclaimed = true
			s.bytes -= e.charge
			s.evictions++
		} else if removeErr == nil {
			removeErr = err
		}
	}
	if fits() {
		s.evictionAfter = ""
		return nil
	}
	return removeErr
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

// Close permanently rejects new work and attempts to abort every active fill,
// returning any abort and lock-release errors. It does not wait for readers,
// including Gets already opening or verifying a body, which may still return
// Readers. If any were active at shutdown's initial check, Close retains the
// filesystem lock and returns an active-reader error. The caller must finish
// those Gets, close all Readers, and call Close again to release the lock;
// closing the Readers alone does not release it.
func (s *Store) Close() error {
	s.mu.Lock()
	s.closed = true
	fs := make([]*Fill, 0, len(s.fills))
	readers := s.readers // Includes readers of detached entries.
	for _, f := range s.fills {
		fs = append(fs, f)
	}
	s.mu.Unlock()
	var abortErr error
	for _, f := range fs {
		abortErr = errors.Join(abortErr, f.Abort())
	}
	if readers != 0 {
		return errors.Join(abortErr, errors.New("cache has active readers"))
	}
	return errors.Join(abortErr, s.releaseLock())
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
