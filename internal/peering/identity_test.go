package peering

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestNodeIDPersistsAcrossRestartAndAddressChange(t *testing.T) {
	dir := bookTestDir(t)
	b, err := OpenBook(dir, "first.test:1")
	if err != nil {
		t.Fatal(err)
	}
	id := b.nodeID
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil || len(raw) != 9 || len(id) != 12 {
		t.Fatalf("node ID = %q, decoded = %x, error = %v", id, raw, err)
	}
	b.Close()
	b, err = OpenBook(dir, "second.test:2")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if b.nodeID != id {
		t.Fatalf("node ID changed from %q to %q", id, b.nodeID)
	}
	other, err := OpenBook(bookTestDir(t), "first.test:1")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if other.nodeID == id {
		t.Fatal("independent state directories share an identity")
	}
}

func TestNodeIDRejectsCorruptOrUnsafeStateWithoutReplacement(t *testing.T) {
	for _, value := range []string{"", "short", "abcdefghijkl=", "abcdefghijk+", "abcdefghijk/", "abcdefghijk\n", strings.Repeat("x", 1000)} {
		t.Run(value[:min(len(value), 16)], func(t *testing.T) {
			dir := bookTestDir(t)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, nodeIDName)
			if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
				t.Fatal(err)
			}
			if b, err := OpenBook(dir, "self.test:1"); err == nil {
				b.Close()
				t.Fatal("accepted invalid ID")
			}
			got, _ := os.ReadFile(path)
			if string(got) != value {
				t.Fatal("replaced corrupt identity")
			}
			// A failed startup must release the lock.
			if err := os.WriteFile(path, []byte("aB3_dE6-gH9j"), 0o600); err != nil {
				t.Fatal(err)
			}
			b, err := OpenBook(dir, "self.test:1")
			if err != nil {
				t.Fatal(err)
			}
			b.Close()
			if err := os.Chmod(path, 0o644); err != nil {
				t.Fatal(err)
			}
			if b, err := OpenBook(dir, "self.test:1"); err == nil {
				b.Close()
				t.Fatal("accepted unsafe ID permissions")
			}
		})
	}
}

func TestNodeIDRetainsPublishedIdentityAfterSyncFailure(t *testing.T) {
	dir := bookTestDir(t)
	lock, err := openBookState(dir)
	if err != nil {
		t.Fatal(err)
	}
	published, err := replaceState(dir, nodeIDName, []byte("aB3_dE6-gH9j"), func(string) error { return syscall.EIO })
	if !published || err == nil {
		t.Fatalf("published = %t, error = %v", published, err)
	}
	lock.close()
	b, err := OpenBook(dir, "self.test:1")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if b.nodeID != "aB3_dE6-gH9j" {
		t.Fatal("regenerated published identity after sync failure")
	}
}
