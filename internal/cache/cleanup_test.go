package cache

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// abandonFill leaves a real failed cleanup, either in staging or at the
// renamed-but-unindexed publication boundary, without a running fill timer.
func abandonFill(t *testing.T, s *Store, meta Metadata, published bool) string {
	t.Helper()
	f, err := s.Begin(meta)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(f, "body"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	if published {
		dest := filepath.Join(s.dir, meta.Key)
		err = os.Rename(f.dir, dest)
		if err == nil {
			f.dir = dest
		}
	}
	dir := f.dir
	f.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := f.Abort(); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("Abort with denied cleanup = %v, want permission error", err)
	}
	return dir
}

func TestAdmissionRetriesFailedCleanup(t *testing.T) {
	const charge = metadataReservation + 4
	for _, limit := range []string{"slots", "bytes"} {
		for _, state := range []string{"staging", "published", "removed"} {
			t.Run(limit+"/"+state, func(t *testing.T) {
				limits := Limits{MaxBytes: 1 << 20, MaxEntries: 1, MaxFills: 1, FillTimeout: time.Minute}
				if limit == "bytes" {
					limits.MaxBytes, limits.MaxEntries = charge, 2
				}
				s, err := OpenWithLimits(filepath.Join(t.TempDir(), "cache"), limits)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = s.Close() })
				meta := testMeta("cleanup", "body")
				dir := abandonFill(t, s, meta, state == "published")
				for i := 0; i < 3; i++ {
					f, err := s.Begin(meta)
					if f != nil {
						f.Abort()
					}
					var admission *AdmissionError
					if !errors.As(err, &admission) || admission.Reason != AdmissionCapacity {
						t.Fatalf("admission while cleanup fails = %v", err)
					}
					if st := s.Stats(); st.Bytes != charge || st.CleanupBytes != charge || st.CleanupSlots != 1 || st.Fills != 0 || st.Reserved != 0 {
						t.Fatalf("retry changed outstanding charge: %+v", st)
					}
				}
				if err := os.Chmod(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if state == "removed" {
					if err := os.RemoveAll(dir); err != nil {
						t.Fatal(err)
					}
				}
				f, err := s.Begin(meta)
				if err != nil {
					t.Fatalf("admission after recovery = %v", err)
				}
				defer f.Abort()
				if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("orphan retained after retry: %v", err)
				}
				if st := s.Stats(); st.CleanupBytes != 0 || st.CleanupSlots != 0 || st.Bytes != 0 || st.Reserved != charge {
					t.Fatalf("retry did not reclaim capacity exactly once: %+v", st)
				}
				if _, err := io.WriteString(f, "body"); err != nil {
					t.Fatal(err)
				}
				if err := f.Commit(); err != nil {
					t.Fatal(err)
				}
				read(t, s, meta, "body")
			})
		}
	}
}

func TestCommitRetiresCleanupBeforeReusingPath(t *testing.T) {
	const charge = metadataReservation + 4
	s := openTest(t, 3*charge)
	meta := testMeta("reused-cleanup", "body")
	dir := abandonFill(t, s, meta, true)
	f, err := s.Begin(meta) // There is room for a refill even though cleanup still fails.
	if err != nil {
		t.Fatal(err)
	}
	defer f.Abort()
	if st := s.Stats(); st.CleanupSlots != 1 || st.CleanupBytes != charge || st.Reserved != charge {
		t.Fatalf("refill lost failed cleanup accounting: %+v", st)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(f, "body"); err != nil {
		t.Fatal(err)
	}
	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}
	if st := s.Stats(); st.CleanupSlots != 0 || st.CleanupBytes != 0 || st.Bytes != charge {
		t.Fatalf("commit retained old cleanup record: %+v", st)
	}
	put(t, s, testMeta("other", "body"), "body")
	read(t, s, meta, "body") // Another admission must not delete the replacement.
}

func TestConcurrentCleanupRetriesReclaimOnce(t *testing.T) {
	const charge = metadataReservation + 4
	s := openTest(t, charge)
	meta := testMeta("concurrent-cleanup", "body")
	dir := abandonFill(t, s, meta, false)
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan *Fill, 8)
	for i := 0; i < cap(results); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f, _ := s.Begin(meta)
			results <- f
		}()
	}
	wg.Wait()
	close(results)
	admitted := 0
	for f := range results {
		if f != nil {
			admitted++
			defer f.Abort()
		}
	}
	if st := s.Stats(); admitted != 1 || st.Bytes != 0 || st.Reserved != charge || st.CleanupSlots != 0 || st.CleanupBytes != 0 {
		t.Fatalf("concurrent retry: admitted=%d stats=%+v", admitted, st)
	}
}

func TestFailedAdmissionCleanupIsRetained(t *testing.T) {
	s := openTest(t, metadataReservation)
	dir, err := os.MkdirTemp(s.dir, ".fill-")
	if err != nil {
		t.Fatal(err)
	}
	// Deny unlink of the directory, as can happen after MkdirTemp succeeds
	// but before admission finishes configuring its files and permissions.
	if err := os.Chmod(s.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(s.dir, 0o700) })
	cause := errors.New("admission setup failed")
	s.mu.Lock()
	err = s.failAdmissionLocked(dir, metadataReservation, cause)
	s.mu.Unlock()
	var admission *AdmissionError
	if !errors.As(err, &admission) || admission.Reason != AdmissionIOError || !errors.Is(admission.Cause, cause) || !errors.Is(admission.Cause, os.ErrPermission) {
		t.Fatalf("lost setup or cleanup failure: %v", err)
	}
	if st := s.Stats(); st.CleanupSlots != 1 || st.CleanupBytes != metadataReservation || st.Bytes != metadataReservation {
		t.Fatalf("unadmitted directory lost its charge: %+v", st)
	}
	if err := os.Chmod(s.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	put(t, s, testMeta("empty", ""), "")
	if st := s.Stats(); st.CleanupSlots != 0 || st.CleanupBytes != 0 || st.Bytes != metadataReservation {
		t.Fatalf("admission cleanup not reclaimed: %+v", st)
	}
}
