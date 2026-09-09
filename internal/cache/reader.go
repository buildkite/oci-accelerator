package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"sync"
	"time"
)

type Reader struct {
	mu        sync.Mutex
	store     *Store
	entry     *entry
	file      *os.File
	remaining int64
	closed    bool
}

func (s *Store) Get(ctx context.Context, meta Metadata) (*Reader, error) {
	return s.get(ctx, meta, nil, nil)
}

// get looks up a committed entry and returns a Reader that owns a fresh pin.
// An unverified entry is verified once under a single-flight guard: the first
// caller claims e.verifying, later callers wait on that channel and then retry
// the lookup, since the verifier may have invalidated or the entry may have
// been replaced. The callbacks are nil in production and are per-call test
// seams for pausing after verification begins and immediately before a wait.
func (s *Store) get(ctx context.Context, meta Metadata, afterVerificationRead, beforeWait func()) (*Reader, error) {
	if !validMeta(meta) {
		return nil, ErrMiss
	}
	for {
		if ctx.Err() != nil {
			return nil, ErrMiss
		}
		s.mu.Lock()
		e := s.entries[meta.Key]
		if s.closed || e == nil || e.meta != meta || e.invalid {
			s.mu.Unlock()
			return nil, ErrMiss
		}
		need := !e.verified
		if need && e.verifying != nil {
			done := e.verifying
			s.mu.Unlock()
			if beforeWait != nil {
				beforeWait()
			}
			select {
			case <-ctx.Done():
				return nil, ErrMiss
			case <-done:
				// Recheck the entry: a canceled verifier permits takeover;
				// corruption, replacement, or shutdown may instead mean a miss.
				continue
			}
		}
		if need {
			e.verifying = make(chan struct{})
		}
		e.pins++
		s.readers++
		s.mu.Unlock()
		r, err := s.openReader(ctx, e, need, afterVerificationRead)
		if need {
			s.mu.Lock()
			close(e.verifying)
			e.verifying = nil
			s.mu.Unlock()
		}
		return r, err
	}
}

// openReader takes ownership of a pin acquired under s.mu. On success the
// Reader owns that pin; every failure closes the file and releases the pin.
func (s *Store) openReader(ctx context.Context, e *entry, need bool, afterVerificationRead func()) (*Reader, error) {
	meta := e.meta
	f, err := os.Open(e.dir + "/body")
	if err != nil {
		s.invalidate(e)
		return nil, ErrMiss
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() != meta.Size {
		f.Close()
		s.invalidate(e)
		return nil, ErrMiss
	}
	if need {
		if err = verifyBody(ctx, f, meta.Digest, afterVerificationRead); err != nil {
			f.Close()
			// A cancellation leaves the entry intact for a later verifier;
			// any other failure retires this corrupt or unreadable generation.
			if ctx.Err() != nil {
				s.unpin(e)
			} else {
				s.invalidate(e)
			}
			return nil, ErrMiss
		}
		if _, err = f.Seek(0, io.SeekStart); err != nil {
			f.Close()
			s.unpin(e)
			return nil, ErrMiss
		}
	}
	s.mu.Lock()
	// The path may have been replaced since selection. Only the same valid
	// entry can lend its verification state to this open descriptor.
	if s.entries[meta.Key] != e || e.invalid {
		s.mu.Unlock()
		f.Close()
		s.unpin(e)
		return nil, ErrMiss
	}
	e.verified = true
	s.setEntryLastLocked(e, time.Now().UnixNano())
	s.mu.Unlock()
	return &Reader{store: s, entry: e, file: f, remaining: meta.Size}, nil
}

// verifyBody streams r, hashing it, and reports whether its SHA-256 matches
// digest. It returns ctx.Err() if the context is canceled mid-read.
// afterVerificationRead, when non-nil, is a test seam invoked once after the
// first read. It reads r to EOF but does not close or rewind it.
func verifyBody(ctx context.Context, r io.Reader, digest string, afterVerificationRead func()) error {
	h := sha256.New()
	buf := make([]byte, 128*1024)
	var err error
	for {
		if err = ctx.Err(); err != nil {
			break
		}
		var n int
		n, err = r.Read(buf)
		if afterVerificationRead != nil {
			afterVerificationRead()
			afterVerificationRead = nil
		}
		if n > 0 {
			_, _ = h.Write(buf[:n])
		}
		if errors.Is(err, io.EOF) {
			err = nil
			break
		}
		if err != nil {
			break
		}
	}
	if err == nil && "sha256:"+hex.EncodeToString(h.Sum(nil)) != digest {
		err = errors.New("digest mismatch")
	}
	return err
}

func (s *Store) invalidate(e *entry) {
	s.mu.Lock()
	e.invalid = true
	s.mu.Unlock()
	s.unpin(e)
}
func (s *Store) unpin(e *entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.pins--
	s.readers--
	if !e.invalid || e.pins != 0 {
		return
	}
	// Detached entries no longer own their directory path: it may now contain
	// a replacement. Their body charge is retained until this final unpin.
	if cur, ok := s.entries[e.meta.Key]; ok && cur == e {
		if os.RemoveAll(e.dir) != nil {
			return
		}
		s.deleteEntryLocked(e)
	}
	if !e.reclaimed {
		e.reclaimed = true
		s.bytes -= e.charge
	}
}
func (r *Reader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, os.ErrClosed
	}
	n, err := r.file.Read(p)
	r.remaining -= int64(n)
	if err == io.EOF && r.remaining > 0 {
		err = io.ErrUnexpectedEOF
	}
	if err != nil && err != io.EOF {
		// Retire only this generation. Keep its pin and charge until Close,
		// even if a replacement is published before this reader finishes.
		r.store.mu.Lock()
		r.entry.invalid = true
		r.store.mu.Unlock()
	}
	return n, err
}
func (r *Reader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	err := r.file.Close()
	r.store.unpin(r.entry)
	return err
}
