package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Fill struct {
	mu              sync.Mutex
	store           *Store
	meta            Metadata
	dir             string
	file            *os.File
	hash            hash.Hash
	written, charge int64
	done            bool
	deadline        time.Time
	timer           *time.Timer
	timerDone       chan struct{}
	timerDoneOnce   sync.Once
	completed       chan struct{}
}

func (s *Store) Begin(meta Metadata) (*Fill, error) {
	if !validMeta(meta) {
		return nil, ErrBypass
	}
	charge, ok := add(meta.Size, metadataReservation)
	if !ok || charge > s.max {
		return nil, ErrBypass
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrBypass
	}
	if _, ok := s.entries[meta.Key]; ok {
		return nil, ErrBypass
	}
	if _, ok := s.fills[meta.Key]; ok {
		return nil, ErrBypass
	}
	// Fill concurrency cannot be solved by eviction, so reject before touching
	// useful entries.
	if len(s.fills) >= s.maxFills {
		return nil, ErrBypass
	}
	if !s.evictLocked(charge, 1, math.MaxInt64) {
		return nil, ErrBypass
	}
	d, err := os.MkdirTemp(s.dir, ".fill-")
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(d, 0o700); err != nil {
		os.RemoveAll(d)
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(d, "body"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		os.RemoveAll(d)
		return nil, err
	}
	if err = f.Chmod(0o600); err != nil {
		f.Close()
		os.RemoveAll(d)
		return nil, err
	}
	fill := &Fill{store: s, meta: meta, dir: d, file: f, hash: sha256.New(), charge: charge, deadline: time.Now().Add(s.fillTimeout), timerDone: make(chan struct{}), completed: make(chan struct{})}
	s.fills[meta.Key] = fill
	s.reserved += charge
	fill.timer = time.AfterFunc(s.fillTimeout, func() {
		fill.mu.Lock()
		if !fill.done {
			_ = fill.finish(errors.New("cache fill expired"))
		}
		fill.mu.Unlock()
		fill.timerDoneOnce.Do(func() { close(fill.timerDone) })
	})
	return fill, nil
}
func (f *Fill) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.unlockAndStopIfDone()
	if f.done {
		return 0, os.ErrClosed
	}
	if !time.Now().Before(f.deadline) {
		return 0, f.finish(errors.New("cache fill expired"))
	}
	if int64(len(p)) > f.meta.Size-f.written {
		return 0, f.finish(errors.New("cache fill exceeds declared size"))
	}
	n, e := f.file.Write(p)
	if n > 0 {
		_, _ = f.hash.Write(p[:n])
		f.written += int64(n)
	}
	if e != nil {
		return n, f.finish(e)
	}
	return n, e
}
func (f *Fill) Commit() error {
	f.mu.Lock()
	defer f.unlockAndStopIfDone()
	if f.done {
		return os.ErrClosed
	}
	if !time.Now().Before(f.deadline) {
		return f.finish(errors.New("cache fill expired"))
	}
	if f.written != f.meta.Size || "sha256:"+hex.EncodeToString(f.hash.Sum(nil)) != f.meta.Digest {
		return f.finish(errors.New("cache fill verification failed"))
	}
	if err := f.file.Sync(); err != nil {
		return f.finish(err)
	}
	if err := f.file.Close(); err != nil {
		return f.finish(err)
	}
	f.file = nil
	b, err := encodeMeta(f.meta)
	if err != nil {
		return f.finish(err)
	}
	mf, err := os.OpenFile(filepath.Join(f.dir, "meta"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		err = mf.Chmod(0o600)
	}
	if err == nil {
		_, err = mf.Write(b)
	}
	if err == nil {
		err = mf.Sync()
	}
	if mf != nil {
		if e := mf.Close(); err == nil {
			err = e
		}
	}
	if err != nil {
		return f.finish(err)
	}
	if err = syncDirectory(f.dir); err != nil {
		return f.finish(err)
	}
	if !time.Now().Before(f.deadline) {
		return f.finish(errors.New("cache fill expired"))
	}
	dest := filepath.Join(f.store.dir, f.meta.Key)
	if err = os.Rename(f.dir, dest); err != nil {
		return f.finish(err)
	}
	f.dir = dest
	if err = syncDirectory(f.store.dir); err != nil {
		return f.finish(err)
	}
	if !time.Now().Before(f.deadline) {
		return f.finish(errors.New("cache fill expired"))
	}
	f.store.mu.Lock()
	if !time.Now().Before(f.deadline) {
		f.store.mu.Unlock()
		return f.finish(errors.New("cache fill expired"))
	}
	delete(f.store.fills, f.meta.Key)
	f.store.reserved -= f.charge
	f.store.entries[f.meta.Key] = &entry{meta: f.meta, dir: dest, charge: f.charge, last: time.Now().UnixNano(), verified: true}
	f.store.bytes += f.charge
	f.store.mu.Unlock()
	f.done = true
	close(f.completed)
	return nil
}
func (f *Fill) Abort() error {
	f.mu.Lock()
	defer f.unlockAndStopIfDone()
	if f.done {
		return nil
	}
	return f.finish(nil)
}

func (f *Fill) unlockAndStopIfDone() {
	done := f.done
	f.mu.Unlock()
	if done {
		f.stopTimer()
	}
}

func (f *Fill) stopTimer() {
	if f.timer.Stop() {
		f.timerDoneOnce.Do(func() { close(f.timerDone) })
	}
	<-f.timerDone
}
func (f *Fill) finish(cause error) error {
	if f.file != nil {
		if e := f.file.Close(); cause == nil {
			cause = e
		}
		f.file = nil
	}
	removeErr := os.RemoveAll(f.dir)
	if cause == nil {
		cause = removeErr
	}
	f.store.mu.Lock()
	delete(f.store.fills, f.meta.Key)
	f.store.reserved -= f.charge
	// Failed cleanup must not allow the occupied space to be admitted again.
	if removeErr != nil {
		f.store.bytes += f.charge
		f.store.failedSlots++
	}
	f.store.mu.Unlock()
	f.done = true
	close(f.completed)
	return cause
}
func syncDirectory(p string) error {
	d, e := os.Open(p)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
