package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
)

func TestCloseReportsAllAbortErrorsAndPreservesReaderLock(t *testing.T) {
	requireUnprivilegedTest(t)
	for _, activeReader := range []bool{false, true} {
		t.Run(fmt.Sprintf("reader=%t", activeReader), func(t *testing.T) {
			s := openTest(t, 1<<20)
			meta := testMeta("stored", "body")
			put(t, s, meta, "body")
			var reader *Reader
			if activeReader {
				var err error
				reader, err = s.Get(context.Background(), meta)
				if err != nil {
					t.Fatal(err)
				}
				defer reader.Close()
			}
			fills := make([]*Fill, 3)
			for i := range fills {
				f, err := s.Begin(testMeta(fmt.Sprintf("shutdown-%d", i), "body"))
				if err != nil {
					t.Fatal(err)
				}
				fills[i] = f
				if _, err := io.WriteString(f, "body"); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chmod(fills[0].dir, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(fills[0].dir, 0o700) })
			if err := fills[1].file.Close(); err != nil {
				t.Fatal(err)
			}
			err := s.Close()
			if !errors.Is(err, os.ErrPermission) || !errors.Is(err, os.ErrClosed) {
				t.Fatalf("Close lost an abort failure: %v", err)
			}
			if strings.Contains(err.Error(), "cache has active readers") != activeReader {
				t.Fatalf("Close reader error = %v, reader=%t", err, activeReader)
			}
			if st := s.Stats(); st.Fills != 0 || st.Reserved != 0 || st.FillFailure != 2 || st.FillAbort != 1 || st.CleanupSlots != 1 || st.CleanupBytes != metadataReservation+4 {
				t.Fatalf("Close did not finish every fill: %+v", st)
			}
			for _, f := range fills {
				select {
				case <-f.completed:
				default:
					t.Fatal("Close did not notify a fill waiter")
				}
			}
			// Restore cleanup permissions so subsequent Open failures can only
			// reflect lock ownership, not unreadable abandoned directories.
			if err := os.Chmod(fills[0].dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if activeReader {
				if err := reader.Close(); err != nil {
					t.Fatal(err)
				}
				other, err := Open(s.dir, s.max)
				if other != nil {
					other.Close()
				}
				if !errors.Is(err, syscall.EWOULDBLOCK) {
					t.Fatalf("closing reader released lock before Close retry: %v", err)
				}
				if err := s.Close(); err != nil {
					t.Fatalf("retry Close: %v", err)
				}
			}
			other, err := Open(s.dir, s.max)
			if err != nil {
				t.Fatalf("shutdown retained lock without readers: %v", err)
			}
			defer other.Close()
			read(t, other, meta, "body")
		})
	}
}

func TestCloseReportsUnlockAndDescriptorCloseErrors(t *testing.T) {
	s := openTest(t, 1<<20)
	if err := s.lock.Close(); err != nil {
		t.Fatal(err)
	}
	err := s.Close()
	if !errors.Is(err, syscall.EBADF) || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Close lost unlock or descriptor-close failure: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("repeated Close = %v", err)
	}
}

func TestCloseReportsMultipleFailuresInOneFill(t *testing.T) {
	requireUnprivilegedTest(t)
	s := openTest(t, 1<<20)
	f, err := s.Begin(testMeta("double-failure", "body"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.dir, 0o700) })
	err = s.Close()
	if !errors.Is(err, os.ErrClosed) || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("Close lost file-close or directory-removal failure: %v", err)
	}
	if st := s.Stats(); st.FillFailure != 1 || st.Fills != 0 || st.Reserved != 0 || st.CleanupSlots != 1 || st.CleanupBytes != metadataReservation+4 {
		t.Fatalf("failed cleanup accounting: %+v", st)
	}
}
