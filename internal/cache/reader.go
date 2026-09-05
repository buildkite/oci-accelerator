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
	if !validMeta(meta) || ctx.Err() != nil {
		return nil, ErrMiss
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrMiss
	}
	e := s.entries[meta.Key]
	if e == nil || e.meta != meta || e.invalid {
		s.mu.Unlock()
		return nil, ErrMiss
	}
	e.pins++
	s.readers++
	need := !e.verified
	s.mu.Unlock()
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
		s.mu.Lock()
		e.verified = true
		s.mu.Unlock()
	}
	s.mu.Lock()
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
	if e.invalid && e.pins == 0 && os.RemoveAll(e.dir) == nil {
		delete(s.entries, e.meta.Key)
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
