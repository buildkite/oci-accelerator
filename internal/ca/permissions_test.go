//go:build unix

package ca

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReopenRejectsUnsafeParent(t *testing.T) {
	for _, mode := range []os.FileMode{0o720, 0o702, 0o777} {
		t.Run(mode.String(), func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, "ca")
			a, err := Open(dir, 1)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(parent, mode); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(dir, 1); err == nil || !strings.Contains(err.Error(), "CA parent") {
				t.Fatalf("reopen with writable parent: %v", err)
			}
			if err := os.Chmod(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(dir, 1)
			if err != nil {
				t.Fatal(err)
			}
			if reopened.Fingerprint() != a.Fingerprint() {
				t.Fatal("rejected reopen replaced CA")
			}
		})
	}
}

func TestReopenRejectsSymlinkParent(t *testing.T) {
	parent := t.TempDir()
	if _, err := Open(filepath.Join(parent, "ca"), 1); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "parent")
	if err := os.Symlink(parent, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(link, "ca"), 1); err == nil || !strings.Contains(err.Error(), "CA parent") {
		t.Fatalf("reopen through symlink parent: %v", err)
	}
}

func TestReopenRejectsForeignOwnership(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to change fixture ownership and read foreign-owned files")
	}
	for _, target := range []string{"parent", "directory", certName, keyName} {
		t.Run(target, func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, "ca")
			a, err := Open(dir, 1)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, target)
			switch target {
			case "parent":
				path = parent
			case "directory":
				path = dir
			}
			if err := os.Chown(path, 1, -1); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(dir, 1); err == nil || !strings.Contains(err.Error(), "owned by the current user") {
				t.Fatalf("reopen with foreign-owned %s: %v", target, err)
			}
			// Exercise the losing initializer's adoption path too.
			if err := initialize(dir); err == nil {
				t.Fatalf("initializer adopted foreign-owned %s", target)
			}
			if err := os.Chown(path, 0, -1); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(dir, 1)
			if err != nil {
				t.Fatal(err)
			}
			if reopened.Fingerprint() != a.Fingerprint() {
				t.Fatal("rejected reopen replaced CA")
			}
		})
	}
}
