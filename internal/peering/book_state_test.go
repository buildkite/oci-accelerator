package peering

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenBookValidatesSelfBeforeFilesystem(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "created")
	if _, err := OpenBook(dir, "invalid"); err == nil {
		t.Fatal("OpenBook succeeded")
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("directory was touched: %v", err)
	}
}

func TestOpenBookRejectsInvalidState(t *testing.T) {
	tests := map[string]string{
		"corrupt":       `{`,
		"version":       `{"version":2,"peers":[]}`,
		"unknown field": `{"version":1,"peers":[],"extra":1}`,
		"invalid peer":  `{"version":1,"peers":["bad"]}`,
		"duplicate":     `{"version":1,"peers":["a.test:1","a.test:1"]}`,
		"oversized":     strings.Repeat(" ", bookStateMax+1),
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			dir := bookTestDir(t)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, bookStateName), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenBook(dir, "self.test:1"); err == nil {
				t.Fatal("OpenBook succeeded")
			}
		})
	}
}

func TestOpenBookRejectsUnsafeAndUnknownFiles(t *testing.T) {
	for name, setup := range map[string]func(string) error{
		"state permissions": func(dir string) error {
			return os.WriteFile(filepath.Join(dir, bookStateName), []byte(`{"version":1,"peers":[]}`), 0o644)
		},
		"state symlink": func(dir string) error { return os.Symlink("missing", filepath.Join(dir, bookStateName)) },
		"unknown":       func(dir string) error { return os.WriteFile(filepath.Join(dir, "other"), nil, 0o600) },
		"unsafe stage":  func(dir string) error { return os.Mkdir(filepath.Join(dir, ".peers-bad"), 0o700) },
	} {
		t.Run(name, func(t *testing.T) {
			dir := bookTestDir(t)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := setup(dir); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenBook(dir, "self.test:1"); err == nil {
				t.Fatal("OpenBook succeeded")
			}
		})
	}
}

func TestOpenBookCleansInterruptedStage(t *testing.T) {
	dir := bookTestDir(t)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(dir, ".peers-abandoned")
	if err := os.WriteFile(stage, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := OpenBook(dir, "self.test:1")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := os.Lstat(stage); !os.IsNotExist(err) {
		t.Fatalf("stage remains: %v", err)
	}
}

func TestReplaceBookStateRenameFailureCleansStage(t *testing.T) {
	dir := bookTestDir(t)
	if err := os.MkdirAll(filepath.Join(dir, bookStateName), 0o700); err != nil {
		t.Fatal(err)
	}
	published, err := writeBookState(dir, []string{"peer.test:1"})
	if err == nil || published {
		t.Fatalf("rename over directory: published=%t, error=%v", published, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != bookStateName || !entries[0].IsDir() {
		t.Fatalf("failed rename left staging files or replaced destination: %v, %v", entries, err)
	}
}
