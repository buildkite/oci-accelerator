package cache

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateDirectoryLockAndUnknownState(t *testing.T) {
	s := openTest(t, 1<<20)
	if s2, err := openDefault(s.dir, s.max); err == nil {
		s2.Close()
		t.Fatal("second store opened")
	} else if !errors.Is(err, ErrLockConflict) {
		t.Fatalf("second store error = %v, want lock conflict", err)
	}
	m := testMeta("key", "abcd")
	put(t, s, m, "abcd")
	r, _ := s.Get(context.Background(), m)
	if s.Close() == nil {
		t.Fatal("closed with live reader")
	}
	if s2, err := openDefault(s.dir, s.max); err == nil {
		s2.Close()
		t.Fatal("released lock with reader")
	}
	r.Close()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.dir, "operator-file")
	os.WriteFile(path, []byte("keep"), 0o600)
	if s2, err := openDefault(s.dir, s.max); err == nil {
		s2.Close()
		t.Fatal("unknown state accepted")
	} else if !errors.Is(err, ErrUnsafeState) {
		t.Fatalf("unknown state error = %v, want unsafe state", err)
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, []byte("keep")) {
		t.Fatal("deleted unknown data")
	}
	os.Remove(path)
	os.Chmod(s.dir, 0o755)
	if s2, err := openDefault(s.dir, s.max); err == nil {
		s2.Close()
		t.Fatal("insecure dir accepted")
	}
	os.Chmod(s.dir, 0o700)
	link := filepath.Join(filepath.Dir(s.dir), "link")
	os.Symlink(s.dir, link)
	if s2, err := openDefault(link, s.max); err == nil {
		s2.Close()
		t.Fatal("symlink accepted")
	}
}

func TestOpenErrorCategories(t *testing.T) {
	missingParent := filepath.Join(t.TempDir(), "missing-parent", "cache")
	if _, err := openDefault(missingParent, 1<<20); !errors.Is(err, ErrFilesystem) {
		t.Fatalf("filesystem error = %v", err)
	}
}
