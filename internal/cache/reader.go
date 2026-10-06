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

// Reader streams one pinned, verified body. The pin keeps the entry's body and
// charge alive, even if the entry is evicted or replaced, until Close.
type Reader struct {
	mu        sync.Mutex // guards the file position and closed flag.
	store     *Store
	entry     *entry
	file      *os.File
	remaining int64
	closed    bool
}

// Read streams the body. Reaching EOF before the declared Size returns
// io.ErrUnexpectedEOF, and any read error invalidates the entry.
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
		r.store.invalidateLocked(r.entry)
		r.store.mu.Unlock()
	}

	return n, err
}

// Seek moves within the pinned, verified file without changing its ownership.
func (r *Reader) Seek(offset int64, whence int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return 0, os.ErrClosed
	}

	// HTTP range sizing must use the verified representation length, not a
	// possibly changed physical EOF after the reader was opened.
	if whence == io.SeekEnd {
		offset += r.entry.meta.Size
		whence = io.SeekStart
	}
	position, err := r.file.Seek(offset, whence)
	if err == nil {
		r.remaining = r.entry.meta.Size - position
	}
	return position, err
}

// Close releases the pin exactly once. Later calls return nil.
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

// get looks up a committed entry and returns a Reader that owns a fresh pin.
// An unverified entry is verified once under a single-flight guard: the first
// caller claims e.verifying, later callers wait on that channel and then retry
// the lookup, since the verifier may have invalidated or the entry may have
// been replaced. The callbacks are nil in production and are per-call test
// seams for pausing after verification begins and immediately before a wait.
func (s *Store) get(ctx context.Context, meta Metadata, afterVerificationRead, beforeWait func()) (*Reader, error) {
	// Malformed keys can never name a committed entry, so skip the lookup.
	if !validMeta(meta) {
		return nil, ErrMiss
	}
	for {
		// Abandon the lookup if the caller has already given up.
		if ctx.Err() != nil {
			return nil, ErrMiss
		}

		// Select a live entry that still matches the requested generation.
		s.mu.Lock()
		e := s.entries[meta.Key]
		if s.closed || e == nil || e.meta != meta || e.invalid || e.claimed {
			s.mu.Unlock()
			return nil, ErrMiss
		}

		// A verification is already in flight: wait for it, then retry the lookup.
		needsVerification := !e.verified
		if needsVerification && e.verifying != nil {
			done := e.verifying
			s.mu.Unlock()
			if beforeWait != nil {
				beforeWait()
			}
			select {
			// Hit when the caller cancels before the verifier finishes.
			case <-ctx.Done():
				return nil, ErrMiss

			// Hit when the verifier closes the channel, signaling it is done.
			case <-done:
				// Recheck the entry: a canceled verifier permits takeover, but
				// corruption, replacement, or shutdown may instead mean a miss.
				continue
			}
		}

		// Claim the verification so concurrent callers wait rather than duplicate it.
		if needsVerification {
			e.verifying = make(chan struct{})
		}

		// Take a pin under the lock so the entry survives the open below.
		e.pins++
		s.readers++
		s.mu.Unlock()

		// Open (and, if claimed, verify) the body outside the lock.
		r, err := s.openReader(ctx, e, needsVerification, afterVerificationRead)

		// Release the single-flight claim, waking any callers that were waiting.
		if needsVerification {
			s.mu.Lock()
			close(e.verifying)
			e.verifying = nil
			s.mu.Unlock()
		}
		return r, err
	}
}

// openReader takes ownership of a pin acquired under s.mu. On success the
// Reader owns that pin. Every failure closes the file and releases the pin.
func (s *Store) openReader(ctx context.Context, e *entry, needsVerification bool, afterVerificationRead func()) (*Reader, error) {
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

	if needsVerification {
		if err = verifyBody(ctx, f, meta.Digest, afterVerificationRead); err != nil {
			f.Close()
			// A cancellation leaves the entry intact for a later verifier, while
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
	s.invalidateLocked(e)
	s.mu.Unlock()
	s.unpin(e)
}

func (s *Store) unpin(e *entry) {
	s.mu.Lock()

	cleanup := e.invalid && e.pins == 1
	if cleanup {
		// Cover the admission-serialization wait as well as removal. Close must
		// not observe the last reader gone before this generation is settled.
		s.startOpLocked()
	}

	e.pins--
	s.readers--

	if !cleanup {
		s.mu.Unlock()
		return
	}

	s.mu.Unlock()

	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()

	s.mu.Lock()
	defer func() {
		s.finishOpLocked()
		s.mu.Unlock()
	}()

	// Detached entries no longer own their directory path: it may now contain
	// a replacement. Their body charge is retained until this final unpin.
	if cur, ok := s.entries[e.meta.Key]; ok && cur == e {
		e.claimed = true
		s.mu.Unlock()

		err := s.removeAll(e.dir)

		s.mu.Lock()
		e.claimed = false
		if err != nil {
			return
		}
		s.deleteEntryLocked(e)
	}

	if !e.reclaimed {
		e.reclaimed = true
		s.bytes -= e.charge
	}
}
