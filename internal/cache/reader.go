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
	mu     sync.Mutex
	store  *Store
	entry  *entry
	file   *os.File
	closed bool
}

func (s *Store) Get(ctx context.Context, meta Metadata) (*Reader, error) {
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
		r, err := s.openReader(ctx, e, need)
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
func (s *Store) openReader(ctx context.Context, e *entry, need bool) (*Reader, error) {
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
		h := sha256.New()
		buf := make([]byte, 128*1024)
		for {
			if err = ctx.Err(); err != nil {
				break
			}
			var n int
			n, err = f.Read(buf)
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
		if err == nil && "sha256:"+hex.EncodeToString(h.Sum(nil)) != meta.Digest {
			err = errors.New("digest mismatch")
		}
		if err != nil {
			f.Close()
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
	e.last = time.Now().UnixNano()
	s.mu.Unlock()
	return &Reader{store: s, entry: e, file: f}, nil
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
		delete(s.entries, e.meta.Key)
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
	return r.file.Read(p)
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
