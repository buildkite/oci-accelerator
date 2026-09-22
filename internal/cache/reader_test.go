package cache

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestReaderFailureRetiresOnlyItsGeneration(t *testing.T) {
	for _, mode := range []string{"short", "read-error"} {
		t.Run(mode, func(t *testing.T) {
			s := openTest(t, 1<<20)
			meta := testMeta("read-failure", "abcdef")
			put(t, s, meta, "abcdef")
			r, err := s.Get(context.Background(), meta)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = r.Close() })
			prefix := make([]byte, 2)
			if _, err := io.ReadFull(r, prefix); err != nil || string(prefix) != "ab" {
				t.Fatalf("prefix = %q, %v", prefix, err)
			}
			wantErr, wantBytes := error(io.ErrUnexpectedEOF), "c"
			if mode == "short" {
				if err := os.Truncate(filepath.Join(r.entry.dir, "body"), 3); err != nil {
					t.Fatal(err)
				}
			} else {
				// Force a real descriptor read error after Get has succeeded.
				if err := r.file.Close(); err != nil {
					t.Fatal(err)
				}
				wantErr, wantBytes = os.ErrClosed, ""
			}
			got, err := io.ReadAll(r)
			if !errors.Is(err, wantErr) || string(got) != wantBytes {
				t.Fatalf("failed read = %q, %v; want %q, %v", got, err, wantBytes, wantErr)
			}
			if next, err := s.Get(context.Background(), meta); err != ErrMiss {
				if next != nil {
					_ = next.Close()
				}
				t.Fatalf("Get after read failure = %v", err)
			}
			charge := meta.Size + metadataReservation
			if st := s.Stats(); st.Readers != 1 || st.Bytes != charge {
				t.Fatalf("failure released the reader's pin or charge: %+v", st)
			}
			put(t, s, meta, "abcdef")
			if st := s.Stats(); st.Bytes != 2*charge {
				t.Fatalf("replacement lost old charge: %+v", st)
			}
			if _, err := r.Read(make([]byte, 1)); !errors.Is(err, wantErr) {
				t.Fatalf("repeated failed read = %v", err)
			}
			read(t, s, meta, "abcdef")
			closeErr := r.Close()
			if mode == "read-error" {
				if !errors.Is(closeErr, os.ErrClosed) {
					t.Fatalf("Close injected closed descriptor = %v", closeErr)
				}
			} else if closeErr != nil {
				t.Fatal(closeErr)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if st := s.Stats(); st.Readers != 0 || st.Bytes != charge || st.Entries != 1 {
				t.Fatalf("final unpin disturbed replacement: %+v", st)
			}
			read(t, s, meta, "abcdef")
		})
	}
}

func TestReaderCleanEOFDoesNotInvalidate(t *testing.T) {
	for _, body := range []string{"", "abc"} {
		t.Run(body, func(t *testing.T) {
			s := openTest(t, 1<<20)
			meta := testMeta("clean-eof", body)
			put(t, s, meta, body)
			r, err := s.Get(context.Background(), meta)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if n, err := r.Read(nil); n != 0 || err != nil {
				t.Fatalf("empty read = %d, %v", n, err)
			}
			got, err := io.ReadAll(r)
			if err != nil || string(got) != body {
				t.Fatalf("body = %q, %v", got, err)
			}
			if _, err := r.Read(make([]byte, 1)); err != io.EOF {
				t.Fatalf("repeated EOF = %v", err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Read(make([]byte, 1)); err != os.ErrClosed {
				t.Fatalf("closed reader = %v", err)
			}
			read(t, s, meta, body)
		})
	}
}

func TestOpenReaderRejectsStaleEntry(t *testing.T) {
	for _, mode := range []string{"invalid", "same-metadata", "different-digest"} {
		t.Run(mode, func(t *testing.T) {
			s := openTest(t, 1<<20)
			meta := testMeta("generation", "aaaa")
			put(t, s, meta, "aaaa")

			// Pause at the boundary between Get's locked selection and opening
			// the body. This pin belongs to the pending openReader call.
			s.mu.Lock()
			e := s.entries[meta.Key]
			e.pins++
			s.readers++
			need := !e.verified
			s.mu.Unlock()
			if err := os.Remove(filepath.Join(e.dir, "body")); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Get(context.Background(), meta); err != ErrMiss {
				t.Fatalf("invalidating Get = %v", err)
			}
			next, body := meta, "aaaa"
			if mode == "invalid" {
				if err := os.WriteFile(filepath.Join(e.dir, "body"), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				if mode == "different-digest" {
					next, body = testMeta("generation", "bbbb"), "bbbb"
				}
				put(t, s, next, body)
			}

			r, err := s.openReader(context.Background(), e, need, nil)
			if r != nil {
				r.Close()
				t.Fatal("stale entry returned a reader")
			}
			if err != ErrMiss {
				t.Fatalf("stale openReader = %v, want ErrMiss", err)
			}
			if e.pins != 0 || s.Stats().Readers != 0 {
				t.Fatalf("stale read leaked pin: entry=%d stats=%+v", e.pins, s.Stats())
			}
			if mode != "invalid" {
				read(t, s, next, body)
				if st := s.Stats(); st.Bytes != next.Size+metadataReservation || st.Entries != 1 {
					t.Fatalf("stale reader disturbed replacement: %+v", st)
				}
			}
		})
	}
}
func TestMissingLiveEntryRefillsAndCancelledVerification(t *testing.T) {
	s := openTest(t, 1<<20)
	m := testMeta("key", "abcd")
	put(t, s, m, "abcd")
	if err := os.Remove(filepath.Join(s.dir, m.Key, "body")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(context.Background(), m); err != ErrMiss {
		t.Fatal("missing body hit")
	}
	put(t, s, m, "abcd")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := openDefault(s.dir, s.max)
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
