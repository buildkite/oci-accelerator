package cache

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

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

			r, err := s.openReader(context.Background(), e, need)
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
