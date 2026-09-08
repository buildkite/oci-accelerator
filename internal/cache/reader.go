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
	if !e.invalid || e.pins != 0 {
		return
	}
	// After Change A a re-admitted fill can commit a new entry onto this key,
	// which shares the same on-disk directory path. Only remove the directory
	// when the map still points at this tombstone: otherwise a superseding
	// Commit already renamed the new entry onto that path (and detached this
	// tombstone), so removing it would destroy live content. When we do still
	// own the slot, remove the directory and drop the map entry. The charge is
	// reclaimed at most once across unpin, Begin (C1), and a superseding Commit,
	// guarded by e.reclaimed; a new entry accounts for its own charge at commit.
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
