package cache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAdmissionReasonsAreDistinctAndBounded(t *testing.T) {
	s, err := OpenWithLimits(filepath.Join(t.TempDir(), "cache"), Limits{MaxBytes: metadataReservation + 1, MaxFills: 1, MaxEntries: 1, FillTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	invalid := testMeta("invalid", "x")
	invalid.Key = "invalid"
	if _, err := s.Begin(invalid); !errors.Is(err, ErrBypass) {
		t.Fatal(err)
	}
	oversized := testMeta("oversized", "xx")
	if _, err := s.Begin(oversized); !errors.Is(err, ErrBypass) {
		t.Fatal(err)
	}
	accepted := testMeta("accepted", "x")
	f, err := s.Begin(accepted)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Begin(accepted); !errors.Is(err, ErrBypass) {
		t.Fatal(err)
	}
	if _, err := s.Begin(testMeta("limited", "x")); !errors.Is(err, ErrBypass) {
		t.Fatal(err)
	}
	if err := f.Abort(); err != nil {
		t.Fatal(err)
	}
	put(t, s, accepted, "x")
	reader, err := s.Get(context.Background(), accepted)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Begin(testMeta("capacity", "x")); !errors.Is(err, ErrBypass) {
		t.Fatalf("capacity admission = %v", err)
	}
	reader.Close()
	moved := s.dir + ".moved"
	if err := os.Rename(s.dir, moved); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Begin(testMeta("io-error", "x")); !errors.Is(err, ErrBypass) {
		t.Fatalf("I/O admission = %v", err)
	}
	if err := os.Rename(moved, s.dir); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Begin(testMeta("closed", "x")); !errors.Is(err, ErrBypass) {
		t.Fatal(err)
	}
	stats := s.Stats()
	for reason, want := range map[AdmissionReason]int64{AdmissionAccepted: 2, AdmissionInvalid: 1, AdmissionOversized: 1, AdmissionExisting: 1, AdmissionFillLimit: 1, AdmissionCapacity: 1, AdmissionClosed: 1, AdmissionIOError: 1} {
		if got := stats.Admissions[reason]; got != want {
			t.Errorf("admission %s = %d, want %d", reason, got, want)
		}
	}
	if len(stats.Admissions) != int(AdmissionReasonCount) {
		t.Fatalf("admission taxonomy length = %d", len(stats.Admissions))
	}
}

func TestEntrySlotsAndFillConcurrencyAreDistinctLimits(t *testing.T) {
	t.Run("entry slots", func(t *testing.T) {
		s, err := OpenWithLimits(filepath.Join(t.TempDir(), "cache"), Limits{MaxBytes: 1 << 20, MaxFills: 3, MaxEntries: 1, FillTimeout: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		f, err := s.Begin(testMeta("a", "a"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Begin(testMeta("b", "b")); !errors.Is(err, ErrBypass) {
			t.Fatalf("second slot = %v", err)
		}
		f.Abort()
		a, b := testMeta("stored", "s"), testMeta("next", "n")
		put(t, s, a, "s")
		r, err := s.Get(context.Background(), a)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Begin(b); !errors.Is(err, ErrBypass) || s.Stats().Evictions != 0 {
			t.Fatal("pinned entry slot must refuse admission without eviction")
		}
		r.Close()
		put(t, s, b, "n")
		if st := s.Stats(); st.Entries != 1 || st.Evictions != 1 {
			t.Fatalf("entry slot eviction failed: %+v", st)
		}
	})
	t.Run("active fills", func(t *testing.T) {
		s, err := OpenWithLimits(filepath.Join(t.TempDir(), "cache"), Limits{MaxBytes: 1 << 20, MaxFills: 1, MaxEntries: 3, FillTimeout: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		put(t, s, testMeta("stored", "s"), "s")
		f, err := s.Begin(testMeta("a", "a"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Begin(testMeta("b", "b")); !errors.Is(err, ErrBypass) {
			t.Fatalf("second fill = %v", err)
		}
		read(t, s, testMeta("stored", "s"), "s")
		f.Abort()
	})
}
