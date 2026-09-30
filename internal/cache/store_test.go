package cache

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestOpenRejectsInvalidConfig(t *testing.T) {
	valid := Limits{MaxBytes: 1 << 20, MaxFills: 1, MaxEntries: 1, FillTimeout: time.Minute}

	// Prove the baseline opens, so each rejection below isolates one field.
	s, err := OpenWithLimits(filepath.Join(t.TempDir(), "cache"), valid)
	if err != nil {
		t.Fatalf("valid limits error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	t.Run("empty dir", func(t *testing.T) {
		// A private working directory would be adoptable if "" became ".".
		wd := t.TempDir()
		if err := os.Chmod(wd, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Chdir(wd)
		if _, err := OpenWithLimits("", valid); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("error = %v, want invalid config", err)
		}
		if _, err := os.Lstat(".lock"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("empty dir touched working directory: %v", err)
		}
	})

	for _, tc := range []struct {
		name   string
		mutate func(*Limits)
	}{
		{"zero max bytes", func(l *Limits) { l.MaxBytes = 0 }},
		{"negative max bytes", func(l *Limits) { l.MaxBytes = -1 }},
		{"zero max fills", func(l *Limits) { l.MaxFills = 0 }},
		{"negative max fills", func(l *Limits) { l.MaxFills = -1 }},
		{"zero max entries", func(l *Limits) { l.MaxEntries = 0 }},
		{"negative max entries", func(l *Limits) { l.MaxEntries = -1 }},
		{"zero fill timeout", func(l *Limits) { l.FillTimeout = 0 }},
		{"negative fill timeout", func(l *Limits) { l.FillTimeout = -time.Second }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limits := valid
			tc.mutate(&limits)
			dir := filepath.Join(t.TempDir(), "cache")
			if _, err := OpenWithLimits(dir, limits); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("error = %v, want invalid config", err)
			}
			if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid limits touched filesystem: %v", err)
			}
		})
	}
}

func TestCompletionTracksMatchingFillTerminalState(t *testing.T) {
	s := openTest(t, 1<<20)
	meta := testMeta("completion", "body")
	f, err := s.Begin(meta)
	if err != nil {
		t.Fatal(err)
	}
	done, ok := s.Completion(meta)
	if !ok {
		t.Fatal("active fill not observed")
	}
	mismatch := meta
	mismatch.MediaType = "different"
	if _, ok := s.Completion(mismatch); ok {
		t.Fatal("mismatching metadata observed")
	}
	select {
	case <-done:
		t.Fatal("active fill reported complete")
	default:
	}
	if _, err := f.Write([]byte("body")); err != nil {
		t.Fatal(err)
	}
	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	default:
		t.Fatal("commit did not notify")
	}
	if published, ok := s.Completion(meta); !ok {
		t.Fatal("published completion not observed")
	} else {
		select {
		case <-published:
		default:
			t.Fatal("published completion was not closed")
		}
	}
}

func TestCompletionNotifiesAbortAndClose(t *testing.T) {
	s := openTest(t, 1<<20)
	first, _ := s.Begin(testMeta("abort-completion", "a"))
	aborted, _ := s.Completion(first.meta)
	if err := first.Abort(); err != nil {
		t.Fatal(err)
	}
	<-aborted

	second, _ := s.Begin(testMeta("close-completion", "b"))
	closed, _ := s.Completion(second.meta)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	<-closed
}

func TestFillReadRestart(t *testing.T) {
	s := openTest(t, 1<<20)
	m := testMeta("registry/repo/accept", "blob bytes")
	f, err := s.Begin(m)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(f, "blob ")
	if _, err := s.Get(context.Background(), m); err != ErrMiss {
		t.Fatal("partial body exposed")
	}
	if _, err := s.Begin(m); !errors.Is(err, ErrBypass) {
		t.Fatal("competing writer admitted")
	}
	io.WriteString(f, "bytes")
	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}
	read(t, s, m, "blob bytes")
	if stats := s.Stats(); stats.Bytes != m.Size+metadataReservation || stats.Reserved != 0 || stats.Entries != 1 {
		t.Fatalf("stats=%+v", stats)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := openDefault(s.dir, s.max)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s2.entries[m.Key].verified {
		t.Fatal("restart skipped digest validation")
	}
	read(t, s2, m, "blob bytes")
	if !s2.entries[m.Key].verified {
		t.Fatal("not marked verified")
	}
	for _, field := range []string{"key", "size", "digest", "media"} {
		x := m
		switch field {
		case "key":
			x.Key = strings.Repeat("f", 64)
		case "size":
			x.Size++
		case "digest":
			x.Digest = "sha256:" + strings.Repeat("f", 64)
		case "media":
			x.MediaType = "text/plain"
		}
		if r, err := s2.Get(context.Background(), x); err != ErrMiss {
			if r != nil {
				r.Close()
			}
			t.Errorf("mismatch %s hit", field)
		}
	}
}
func TestCapacityLRUPinningAndConcurrency(t *testing.T) {
	const charge = metadataReservation + 4
	s := openTest(t, 2*charge)
	a, b, c := testMeta("a", "aaaa"), testMeta("b", "bbbb"), testMeta("c", "cccc")
	put(t, s, a, "aaaa")
	put(t, s, b, "bbbb")
	s.setEntryLastLocked(s.entries[a.Key], 1)
	s.setEntryLastLocked(s.entries[b.Key], 2)
	r, err := s.Get(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	put(t, s, c, "cccc")
	if _, err := s.Get(context.Background(), b); err != ErrMiss {
		t.Fatal("did not evict unpinned LRU")
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if string(got) != "aaaa" {
		t.Fatal("evicted reader")
	}
	if s.Stats().Evictions != 1 {
		t.Fatal("missing eviction")
	}
	r1, _ := s.Get(context.Background(), a)
	r2, _ := s.Get(context.Background(), c)
	if _, err := s.Begin(testMeta("d", "dddd")); !errors.Is(err, ErrBypass) {
		t.Fatal("evicted pinned entries")
	}
	r1.Close()
	r2.Close()
	tooLarge := testMeta("big", "")
	tooLarge.Size = math.MaxInt64
	if _, err := s.Begin(tooLarge); !errors.Is(err, ErrBypass) {
		t.Fatal("overflow admitted")
	}
	s2 := openTest(t, charge)
	results := make(chan *Fill, 20)
	for i := 0; i < 20; i++ {
		go func() {
			f, _ := s2.Begin(a)
			results <- f
		}()
	}
	var admitted []*Fill
	for i := 0; i < 20; i++ {
		if f := <-results; f != nil {
			admitted = append(admitted, f)
		}
	}
	if x := s2.Stats(); x.Reserved != charge || x.Bytes != 0 {
		t.Fatalf("reservation %+v", x)
	}
	if len(admitted) != 1 {
		t.Fatalf("admitted %d competing writers", len(admitted))
	}
	admitted[0].Abort()
	if s2.Stats().Reserved != 0 {
		t.Fatal("admission leak")
	}
}
func TestRestrictiveUmaskAndTinyCapacity(t *testing.T) {
	root := t.TempDir()
	previous := syscall.Umask(0o777)
	defer syscall.Umask(previous)
	s, err := openDefault(filepath.Join(root, "cache"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	m := testMeta("key", "abcd")
	put(t, s, m, "abcd")
	s.Close()
	s, err = openDefault(s.dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Stats().Bytes != 0 {
		t.Fatal("reduced capacity not respected")
	}
	if _, err := s.Begin(m); !errors.Is(err, ErrBypass) {
		t.Fatal("tiny cache admitted blob")
	}
}
