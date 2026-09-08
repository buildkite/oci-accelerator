// Package cache provides a private, bounded filesystem cache.
package cache

import (
	"container/list"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	return [...]string{"accepted", "invalid", "oversized", "existing", "fill_limit", "capacity", "closed", "io_error"}[r]
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
	lock                                            *os.File
	closed                                          bool
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
	s := &Store{dir: dir, max: limits.MaxBytes, maxFills: limits.MaxFills, maxEntries: limits.MaxEntries, fillTimeout: limits.FillTimeout, entries: make(map[string]*entry), fills: make(map[string]*Fill), lock: lf}
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

func (s *Store) inventory() error {
	directory, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	for {
		des, readErr := directory.ReadDir(64)
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		for _, de := range des {
			n := de.Name()
			if n == ".lock" {
				continue
			}
			if strings.HasPrefix(n, ".fill-") && de.IsDir() {
				if err := os.RemoveAll(filepath.Join(s.dir, n)); err != nil {
					return err
				}
				continue
			}
			if len(n) != 64 || !de.IsDir() {
				return fmt.Errorf("unrecognized cache state %q", n)
			}
			if _, err := hex.DecodeString(n); err != nil || n != strings.ToLower(n) {
				return fmt.Errorf("unrecognized cache state %q", n)
			}
			p := filepath.Join(s.dir, n)
			pi, err := os.Lstat(p)
			if err != nil || pi.Mode().Perm() != 0o700 || !owned(pi) {
				return errors.New("insecure cache entry")
			}
			d, err := os.Open(p)
			if err != nil {
				return err
			}
			children, readErr := d.ReadDir(3)
			d.Close()
			if readErr != nil && readErr != io.EOF {
				return readErr
			}
			for _, child := range children {
				if child.Name() != "body" && child.Name() != "meta" {
					return fmt.Errorf("unrecognized state in cache entry %q", n)
				}
			}
			mi, err := os.Lstat(filepath.Join(p, "meta"))
			if err == nil && (!mi.Mode().IsRegular() || mi.Mode().Perm() != 0o600 || !owned(mi)) {
				return errors.New("insecure cache metadata")
			}
			var raw []byte
			if err == nil && mi.Size() <= metadataReservation {
				var f *os.File
				f, err = os.Open(filepath.Join(p, "meta"))
				if err == nil {
					raw, err = io.ReadAll(io.LimitReader(f, metadataReservation+1))
					f.Close()
				}
			}
			if err != nil || len(raw) > int(metadataReservation) {
				if err := os.RemoveAll(p); err != nil {
					return err
				}
				continue
			}
			var dm diskMetadata
			if json.Unmarshal(raw, &dm) != nil || !validMeta(dm.Metadata) || dm.Key != n {
				if err := os.RemoveAll(p); err != nil {
					return err
				}
				continue
			}
			bi, err := os.Lstat(filepath.Join(p, "body"))
			if err != nil || !bi.Mode().IsRegular() || bi.Mode().Perm() != 0o600 || !owned(bi) || bi.Size() != dm.Size {
				if err := os.RemoveAll(p); err != nil {
					return err
				}
				continue
			}
			charge, ok := add(dm.Size, metadataReservation)
			if !ok {
				return errors.New("cache entry size overflow")
			}
			// Keep the startup index bounded even when capacity was reduced.
			if charge > s.max || !s.evictLocked(charge, 1, dm.Committed) {
				if err := os.RemoveAll(p); err != nil {
					return err
				}
				continue
			}
			s.entries[n] = &entry{meta: dm.Metadata, dir: p, charge: charge, last: dm.Committed}
			s.bytes += charge
		}
		if readErr == io.EOF {
			break
		}
	}
	return nil
}
func add(a, b int64) (int64, bool) {
	if a < 0 || b < 0 || a > int64(^uint64(0)>>1)-b {
		return 0, false
	}
	return a + b, true
}

// before prevents an older startup candidate from evicting newer entries.
func (s *Store) evictLocked(need int64, needSlots int, before int64) bool {
	fits := func() bool {
		return need <= s.max-s.bytes-s.reserved && len(s.entries)+len(s.fills)+len(s.failedCleanups)+needSlots <= s.maxEntries
	}
	if fits() {
		return true
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
		return false
	}
	sort.Slice(es, func(i, j int) bool { return es[i].last < es[j].last })
	for _, e := range es {
		if fits() {
			return true
		}
		if os.RemoveAll(e.dir) == nil {
			delete(s.entries, e.meta.Key)
			// Eviction only selects unpinned entries, so their charge is free.
			e.reclaimed = true
			s.bytes -= e.charge
			s.evictions++
		}
	}
	return fits()
}

func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	var cleanupBytes int64
	for _, pending := range s.failedCleanups {
		cleanupBytes += pending.charge
	}
	return Stats{Bytes: s.bytes, Reserved: s.reserved, Entries: len(s.entries), Fills: len(s.fills), Evictions: s.evictions, Readers: s.readers, FillSuccess: s.fillSuccess, FillFailure: s.fillFailure, FillAbort: s.fillAbort, FillExpiry: s.fillExpiry, Admissions: s.admissions, CleanupBytes: cleanupBytes, CleanupSlots: len(s.failedCleanups)}
}
func (s *Store) Close() error {
	s.mu.Lock()
	s.closed = true
	fs := make([]*Fill, 0, len(s.fills))
	readers := s.readers // Includes readers of detached entries.
	for _, f := range s.fills {
		fs = append(fs, f)
	}
	s.mu.Unlock()
	for _, f := range fs {
		_ = f.Abort()
	}
	if readers != 0 {
		return errors.New("cache has active readers")
	}
	return s.releaseLock()
}
func (s *Store) releaseLock() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return nil
	}
	_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
	err := s.lock.Close()
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
