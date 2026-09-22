package cache

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"testing"
	"time"
)

func testMeta(key, body string) Metadata {
	return Metadata{Key: fmt.Sprintf("%x", sha256.Sum256([]byte(key))), Digest: fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(body))), Size: int64(len(body)), MediaType: "application/octet-stream"}
}

func openTest(t *testing.T, limit int64) *Store {
	t.Helper()
	s, err := openDefault(filepath.Join(t.TempDir(), "cache"), limit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

// openDefault opens a Store with a generous set of non-size limits, mirroring
// cachetest.Open for internal tests that cannot import the cachetest package
// without creating an import cycle.
func openDefault(dir string, maxBytes int64) (*Store, error) {
	return OpenWithLimits(dir, Limits{MaxBytes: maxBytes, MaxFills: 16, MaxEntries: 100000, FillTimeout: 10 * time.Minute})
}

func put(t *testing.T, s *Store, m Metadata, body string) {
	t.Helper()
	f, err := s.Begin(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(f, body); err != nil {
		t.Fatal(err)
	}
	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, s *Store, m Metadata, want string) {
	t.Helper()
	r, err := s.Get(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	closeErr := r.Close()
	if err != nil || string(got) != want {
		t.Fatalf("body=%q err=%v", got, err)
	}
	if closeErr != nil {
		t.Fatalf("close reader: %v", closeErr)
	}
}

func assertHitProgress(t *testing.T, s *Store, meta Metadata, want string) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		r, err := s.Get(context.Background(), meta)
		if err == nil {
			var b []byte
			b, err = io.ReadAll(r)
			if err == nil && string(b) != want {
				err = errors.New("unexpected bytes: " + string(b))
			}
			err = errors.Join(err, r.Close())
		}
		done <- err
	}()
	if err := awaitResult(t, done, "unrelated cache hit"); err != nil {
		t.Fatal(err)
	}
	statsDone := make(chan Stats, 1)
	go func() { statsDone <- s.Stats() }()
	_ = awaitResult(t, statsDone, "Stats")
}

func awaitBarrier(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func awaitResult[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}
