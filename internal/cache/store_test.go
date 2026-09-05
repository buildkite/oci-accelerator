package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func testMeta(key, body string) Metadata {
	return Metadata{Key: fmt.Sprintf("%x", sha256.Sum256([]byte(key))), Digest: fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(body))), Size: int64(len(body)), MediaType: "application/octet-stream"}
}
func openTest(t *testing.T, limit int64) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "cache"), limit)
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
	r.Close()
	if err != nil || string(got) != want {
		t.Fatalf("body=%q err=%v", got, err)
	}
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
	if _, err := s.Begin(m); err != ErrBypass {
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
	s2, err := Open(s.dir, s.max)
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
func TestFillFailuresNeverPublish(t *testing.T) {
	for _, mode := range []string{"short", "digest", "overlong", "write", "sync", "metadata", "abort", "close", "disk-full"} {
		t.Run(mode, func(t *testing.T) {
			s := openTest(t, 1<<20)
			m := testMeta("key", "abcd")
			f, err := s.Begin(m)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "short":
				io.WriteString(f, "ab")
			case "digest":
				io.WriteString(f, "xxxx")
			case "overlong":
				if _, err = io.WriteString(f, "abcde"); err == nil {
					t.Fatal("overflow accepted")
				}
			case "write":
				f.file.Close()
				if _, err = io.WriteString(f, "abcd"); err == nil {
					t.Fatal("closed write accepted")
				}
			case "sync":
				io.WriteString(f, "abcd")
				f.file.Close()
			case "metadata":
				io.WriteString(f, "abcd")
				os.Mkdir(filepath.Join(f.dir, "meta"), 0o700)
			case "abort":
				io.WriteString(f, "ab")
				f.Abort()
			case "close":
				io.WriteString(f, "ab")
				s.Close()
			case "disk-full":
				f.file.Close()
				f.file, err = os.OpenFile("/dev/full", os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = io.WriteString(f, "abcd"); err == nil {
					t.Fatal("disk-full write accepted")
				}
			}
			if err := f.Commit(); err == nil {
				t.Fatal("invalid fill committed")
			}
			f.Abort()
			if stats := s.Stats(); stats.Entries != 0 || stats.Fills != 0 || stats.Reserved != 0 || stats.Bytes != 0 {
				t.Fatalf("leaked %+v", stats)
			}
			names, err := os.ReadDir(s.dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(names) != 1 || names[0].Name() != ".lock" {
				t.Fatal("abandoned temp")
			}
		})
	}
}
func TestCapacityLRUPinningAndConcurrency(t *testing.T) {
	const charge = metadataReservation + 4
	s := openTest(t, 2*charge)
	a, b, c := testMeta("a", "aaaa"), testMeta("b", "bbbb"), testMeta("c", "cccc")
	put(t, s, a, "aaaa")
	put(t, s, b, "bbbb")
	s.entries[a.Key].last = 1
	s.entries[b.Key].last = 2
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
	if _, err := s.Begin(testMeta("d", "dddd")); err != ErrBypass {
		t.Fatal("evicted pinned entries")
	}
	r1.Close()
	r2.Close()
	tooLarge := testMeta("big", "")
	tooLarge.Size = math.MaxInt64
	if _, err := s.Begin(tooLarge); err != ErrBypass {
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
func TestRecoveryAndCorruption(t *testing.T) {
	for _, mode := range []string{"body-missing", "short", "corrupt", "meta-missing", "meta-invalid", "meta-huge", "temporary"} {
		t.Run(mode, func(t *testing.T) {
			s := openTest(t, 1<<20)
			m := testMeta("key", "abcd")
			put(t, s, m, "abcd")
			s.Close()
			d := filepath.Join(s.dir, m.Key)
			switch mode {
			case "body-missing":
				os.Remove(filepath.Join(d, "body"))
			case "short":
				os.Truncate(filepath.Join(d, "body"), 2)
			case "corrupt":
				os.WriteFile(filepath.Join(d, "body"), []byte("xxxx"), 0o600)
			case "meta-missing":
				os.Remove(filepath.Join(d, "meta"))
			case "meta-invalid":
				os.WriteFile(filepath.Join(d, "meta"), []byte("invalid"), 0o600)
			case "meta-huge":
				os.Truncate(filepath.Join(d, "meta"), 1<<30)
			case "temporary":
				os.Rename(d, filepath.Join(s.dir, ".fill-abandoned"))
			}
			s2, err := Open(s.dir, s.max)
			if err != nil {
				t.Fatal(err)
			}
			defer s2.Close()
			if r, err := s2.Get(context.Background(), m); err != ErrMiss {
				if r != nil {
					r.Close()
				}
				t.Fatal("bad entry hit")
			}
			put(t, s2, m, "abcd")
			read(t, s2, m, "abcd")
		})
	}
}
func TestMissingLiveEntryRefillsAndCancelledVerification(t *testing.T) {
	s := openTest(t, 1<<20)
	m := testMeta("key", "abcd")
	put(t, s, m, "abcd")
	os.Remove(filepath.Join(s.dir, m.Key, "body"))
	if _, err := s.Get(context.Background(), m); err != ErrMiss {
		t.Fatal("missing body hit")
	}
	put(t, s, m, "abcd")
	s.Close()
	s2, err := Open(s.dir, s.max)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s2.Get(ctx, m); err != ErrMiss {
		t.Fatal("cancelled hit")
	}
	read(t, s2, m, "abcd")
}
func TestPrivateDirectoryLockAndUnknownState(t *testing.T) {
	s := openTest(t, 1<<20)
	if s2, err := Open(s.dir, s.max); err == nil {
		s2.Close()
		t.Fatal("second store opened")
	}
	m := testMeta("key", "abcd")
	put(t, s, m, "abcd")
	r, _ := s.Get(context.Background(), m)
	if s.Close() == nil {
		t.Fatal("closed with live reader")
	}
	if s2, err := Open(s.dir, s.max); err == nil {
		s2.Close()
		t.Fatal("released lock with reader")
	}
	r.Close()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.dir, "operator-file")
	os.WriteFile(path, []byte("keep"), 0o600)
	if s2, err := Open(s.dir, s.max); err == nil {
		s2.Close()
		t.Fatal("unknown state accepted")
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, []byte("keep")) {
		t.Fatal("deleted unknown data")
	}
	os.Remove(path)
	os.Chmod(s.dir, 0o755)
	if s2, err := Open(s.dir, s.max); err == nil {
		s2.Close()
		t.Fatal("insecure dir accepted")
	}
	os.Chmod(s.dir, 0o700)
	link := filepath.Join(filepath.Dir(s.dir), "link")
	os.Symlink(s.dir, link)
	if s2, err := Open(link, s.max); err == nil {
		s2.Close()
		t.Fatal("symlink accepted")
	}
}
func TestRestrictiveUmaskAndTinyCapacity(t *testing.T) {
	root := t.TempDir()
	previous := syscall.Umask(0o777)
	defer syscall.Umask(previous)
	s, err := Open(filepath.Join(root, "cache"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	m := testMeta("key", "abcd")
	put(t, s, m, "abcd")
	s.Close()
	s, err = Open(s.dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Stats().Bytes != 0 {
		t.Fatal("reduced capacity not respected")
	}
	if _, err := s.Begin(m); err != ErrBypass {
		t.Fatal("tiny cache admitted blob")
	}
}

func TestRestartKeepsNewestCandidateRegardlessOfInventoryOrder(t *testing.T) {
	s := openTest(t, 1<<20)
	for _, key := range []string{"a", "b", "c"} {
		put(t, s, testMeta(key, "abcd"), "abcd")
	}
	s.Close()
	d, err := os.Open(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	names, err := d.ReadDir(-1)
	d.Close()
	if err != nil {
		t.Fatal(err)
	}
	var newest Metadata
	stamp := int64(100)
	for _, name := range names {
		if !name.IsDir() {
			continue
		}
		path := filepath.Join(s.dir, name.Name(), "meta")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var meta diskMetadata
		if err := json.Unmarshal(raw, &meta); err != nil {
			t.Fatal(err)
		}
		meta.Committed = stamp
		if stamp == 100 {
			newest = meta.Metadata
		}
		stamp--
		raw, _ = json.Marshal(meta)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s2, err := Open(s.dir, metadataReservation+4)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	read(t, s2, newest, "abcd")
	if s2.Stats().Entries != 1 {
		t.Fatal("capacity exceeded")
	}
}

func TestFailedAdmissionDoesNotEvictInsufficientVictims(t *testing.T) {
	s := openTest(t, 3*metadataReservation)
	a := testMeta("old", "a")
	body := strings.Repeat("b", int(metadataReservation)-1)
	b := testMeta("new", body)
	put(t, s, a, "a")
	put(t, s, b, body)
	s.entries[a.Key].last = 1
	s.entries[b.Key].last = 3
	if s.evictLocked(2*metadataReservation, 2) {
		t.Fatal("admitted without enough eligible space")
	}
	read(t, s, a, "a")
	read(t, s, b, body)
	if s.Stats().Evictions != 0 {
		t.Fatal("failed admission evicted useful data")
	}
}
