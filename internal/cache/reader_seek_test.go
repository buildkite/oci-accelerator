package cache

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestReaderSeekPreservesPinAndCorruptionChecks(t *testing.T) {
	s := openTest(t, 1<<20)
	const body = "0123456789abcdef"
	meta := testMeta("seek", body)
	put(t, s, meta, body)
	r, err := s.Get(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, tc := range []struct {
		offset int64
		whence int
		want   string
	}{
		{-3, io.SeekEnd, "def"},
		{2, io.SeekStart, "23456789abcdef"},
		{-7, io.SeekCurrent, "9abcdef"},
	} {
		if _, err := r.Seek(tc.offset, tc.whence); err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(r)
		if err != nil || string(got) != tc.want || s.Stats().Readers != 1 {
			t.Fatalf("read=%q err=%v readers=%d", got, err, s.Stats().Readers)
		}
	}
	if err := os.Truncate(filepath.Join(s.dir, meta.Key, "body"), 5); err != nil {
		t.Fatal(err)
	}
	if size, err := r.Seek(0, io.SeekEnd); err != nil || size != 16 {
		t.Fatalf("representation size changed after truncation: %d, %v", size, err)
	}
	if _, err := r.Seek(3, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if string(got) != "34" || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated read=%q err=%v", got, err)
	}
	r.Close()
	if _, err := r.Seek(0, io.SeekStart); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed seek=%v", err)
	}
	if _, err := s.Get(context.Background(), meta); !errors.Is(err, ErrMiss) {
		t.Fatalf("corrupt entry remained visible: %v", err)
	}
}
