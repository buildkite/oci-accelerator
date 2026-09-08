package cache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
)

func TestEvictionFailureAdmissionCause(t *testing.T) {
	requireUnprivilegedTest(t)
	for _, mode := range []string{"all-fail", "later-victim-succeeds", "pinned"} {
		t.Run(mode, func(t *testing.T) {
			const charge = metadataReservation + 4
			s := openTest(t, 2*charge)
			old, next := testMeta("old", "body"), testMeta("next", "body")
			put(t, s, old, "body")
			put(t, s, next, "body")
			for _, meta := range []Metadata{old, next} {
				if mode == "pinned" {
					r, err := s.Get(context.Background(), meta)
					if err != nil {
						t.Fatal(err)
					}
					defer r.Close()
				}
				if mode == "later-victim-succeeds" && meta == next {
					continue
				}
				dir := s.entries[meta.Key].dir
				if err := os.Chmod(dir, 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Chmod(dir, 0o700); err != nil {
						t.Error(err)
					}
				})
			}
			s.entries[old.Key].last, s.entries[next.Key].last = 1, 2
			want := s.Stats()
			f, err := s.Begin(testMeta("new", "body"))
			if mode == "later-victim-succeeds" {
				if err != nil {
					t.Fatalf("later eviction should admit despite first failure: %v", err)
				}
				if err := f.Abort(); err != nil {
					t.Fatal(err)
				}
				if st := s.Stats(); st.Evictions != 1 || st.Bytes != charge || st.Admissions[AdmissionIOError] != 0 {
					t.Fatalf("successful fallback eviction: %+v", st)
				}
				read(t, s, old, "body")
				return
			}
			if f != nil {
				_ = f.Abort()
				t.Fatal("admitted without reclaiming capacity")
			}
			var admission *AdmissionError
			if !errors.Is(err, ErrBypass) || !errors.As(err, &admission) {
				t.Fatalf("Begin = %v", err)
			}
			if mode == "all-fail" {
				var pathErr *os.PathError
				if admission.Reason != AdmissionIOError || !errors.Is(err, os.ErrPermission) || !errors.As(err, &pathErr) || !strings.HasPrefix(pathErr.Path, s.entries[old.Key].dir) {
					t.Fatalf("lost first eviction cause: %v", err)
				}
				want.Admissions[AdmissionIOError]++
			} else {
				if admission.Reason != AdmissionCapacity || admission.Cause != nil {
					t.Fatalf("pinned capacity misclassified: %v", err)
				}
				want.Admissions[AdmissionCapacity]++
			}
			if got := s.Stats(); got != want {
				t.Fatalf("failed eviction changed accounting: got %+v, want %+v", got, want)
			}
			read(t, s, old, "body")
			read(t, s, next, "body")
		})
	}
}

func TestSizeMismatchDoesNotReplaceValidEntry(t *testing.T) {
	s := openTest(t, 1<<20)
	meta := testMeta("immutable", "body")
	put(t, s, meta, "body")
	for _, size := range []int64{meta.Size - 1, meta.Size + 1} {
		mismatch := meta
		mismatch.Size = size
		want := s.Stats()
		want.Admissions[AdmissionExisting]++
		if r, err := s.Get(context.Background(), mismatch); err != ErrMiss {
			if r != nil {
				_ = r.Close()
			}
			t.Fatalf("Get mismatching size = %v", err)
		}
		if _, ok := s.Completion(mismatch); ok {
			t.Fatal("mismatching entry reported complete")
		}
		f, err := s.Begin(mismatch)
		if f != nil {
			_ = f.Abort()
			t.Fatal("mismatching size admitted over a valid entry")
		}
		var admission *AdmissionError
		if !errors.Is(err, ErrBypass) || !errors.As(err, &admission) || admission.Reason != AdmissionExisting || admission.Cause != nil {
			t.Fatalf("Begin mismatching size = %v", err)
		}
		if got := s.Stats(); got != want {
			t.Fatalf("mismatch changed cache accounting: got %+v, want %+v", got, want)
		}
		read(t, s, meta, "body")
	}
}

func TestAdmissionErrorPreservesCause(t *testing.T) {
	pathErr := &os.PathError{Op: "mkdir", Path: "cache/.fill-test", Err: os.ErrPermission}
	for _, cause := range []error{nil, pathErr, fmt.Errorf("create staging directory: %w", pathErr)} {
		t.Run(fmt.Sprint(cause), func(t *testing.T) {
			admission := &AdmissionError{Reason: AdmissionIOError, Cause: cause}
			err := fmt.Errorf("begin fill: %w", admission)
			if !errors.Is(err, ErrBypass) {
				t.Fatal("lost ErrBypass classification")
			}
			var got *AdmissionError
			if !errors.As(err, &got) || got != admission {
				t.Fatal("lost admission error identity")
			}
			if errors.Is(err, ErrMiss) {
				t.Fatal("admission unexpectedly matches ErrMiss")
			}
			if cause == nil {
				if admission.Error() != "cache admission refused: io_error" || errors.Unwrap(admission) != nil {
					t.Fatalf("cause-free admission = %q, unwrap = %v", admission.Error(), errors.Unwrap(admission))
				}
				return
			}
			if !errors.Is(err, cause) || !errors.Is(err, os.ErrPermission) {
				t.Fatalf("underlying cause is not inspectable: %v", err)
			}
			var gotPath *os.PathError
			if !errors.As(err, &gotPath) || gotPath != pathErr {
				t.Fatal("underlying PathError is not inspectable")
			}
			if !strings.Contains(admission.Error(), cause.Error()) {
				t.Fatalf("message omits cause: %q", admission.Error())
			}
		})
	}
}

func TestBeginIOErrorPreservesFilesystemCause(t *testing.T) {
	s := openTest(t, 1<<20)
	moved := s.dir + ".moved"
	if err := os.Rename(s.dir, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Rename(moved, s.dir); err != nil {
			t.Error(err)
		}
	})
	_, err := s.Begin(testMeta("missing-dir", "body"))
	var admission *AdmissionError
	if !errors.As(err, &admission) || admission.Reason != AdmissionIOError || admission.Cause == nil {
		t.Fatalf("Begin = %v, want I/O admission error with cause", err)
	}
	var pathErr *os.PathError
	if !errors.Is(err, ErrBypass) || !errors.Is(err, os.ErrNotExist) || !errors.As(err, &pathErr) {
		t.Fatalf("filesystem cause not inspectable: %v", err)
	}
	if !strings.Contains(err.Error(), admission.Cause.Error()) {
		t.Fatalf("message omits filesystem cause: %v", err)
	}
}

func TestAdmissionChecksEncodedMetadataBeforeEviction(t *testing.T) {
	for _, size := range []int{int(metadataReservation) - 1, int(metadataReservation), int(metadataReservation) + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := openTest(t, metadataReservation+4)
				old := testMeta("useful", "body")
				put(t, s, old, "body")
				meta := testMeta("escaped", "body")
				prefix := `application/octet-stream; x="` + strings.Repeat("<", 600)
				meta.MediaType = prefix + `"`
				base, err := encodeMeta(meta)
				if err != nil {
					t.Fatal(err)
				}
				meta.MediaType = prefix + strings.Repeat("a", size-len(base)) + `"`
				if !validMeta(meta) {
					t.Fatal("fixture exceeds raw metadata limits")
				}
				raw, _ := encodeMeta(meta)
				if len(raw) != size {
					t.Fatalf("encoded size = %d, want %d", len(raw), size)
				}
				if size <= int(metadataReservation) {
					put(t, s, meta, "body")
					read(t, s, meta, "body")
					info, err := os.Stat(filepath.Join(s.dir, meta.Key, "meta"))
					if err != nil || info.Size() != int64(size) {
						t.Fatalf("published sidecar = %v, %v", info, err)
					}
					return
				}
				want := s.Stats()
				want.Admissions[AdmissionInvalid]++
				f, err := s.Begin(meta)
				if f != nil {
					f.Abort()
					t.Fatal("oversized metadata admitted")
				}
				var admission *AdmissionError
				if !errors.Is(err, ErrBypass) || !errors.As(err, &admission) || admission.Reason != AdmissionInvalid || admission.Cause == nil {
					t.Fatalf("Begin = %v, want invalid admission with cause", err)
				}
				if got := s.Stats(); got != want {
					t.Fatalf("rejected metadata changed cache: got %+v, want %+v", got, want)
				}
				read(t, s, old, "body")
				staging, err := filepath.Glob(filepath.Join(s.dir, ".fill-*"))
				if err != nil || len(staging) != 0 {
					t.Fatalf("rejected metadata staged files: %v, %v", staging, err)
				}
			})
		})
	}
}
