package peering

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestOpenBookValidatesSelfBeforeFilesystem(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "created")
	if _, err := OpenBook(context.Background(), dir, "invalid", nil, nil); err == nil {
		t.Fatal("OpenBook succeeded")
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("directory was touched: %v", err)
	}
}

func TestOpenBookRejectsInvalidState(t *testing.T) {
	tests := map[string]string{
		"corrupt":              `{`,
		"version":              `{"version":4,"peers":{}}`,
		"v3 missing timestamp": `{"version":3,"peers":{"a.test:1":{"node_id":"000000000001"}}}`,
		"v3 duplicate address": `{"version":3,"peers":{"a.test:1":{"node_id":""},"a.test:1":{"node_id":""}}}`,
		"v3 unknown field":     `{"version":3,"peers":{"a.test:1":{"other":""}}}`,
		"unknown field":        `{"version":2,"peers":{},"extra":1}`,
		"invalid peer":         `{"version":2,"peers":{"bad":"000000000001"}}`,
		"invalid node ID":      `{"version":2,"peers":{"a.test:1":"short"}}`,
		"null node ID":         `{"version":2,"peers":{"a.test:1":null}}`,
		"duplicate address":    `{"version":2,"peers":{"a.test:1":"000000000001","a.test:1":"000000000002"}}`,
		"duplicate node ID":    `{"version":2,"peers":{"a.test:1":"000000000001","b.test:1":"000000000001"}}`,
		"v1 invalid peer":      `{"version":1,"peers":["bad"]}`,
		"v1 duplicate":         `{"version":1,"peers":["a.test:1","a.test:1"]}`,
		"oversized":            strings.Repeat(" ", bookStateMax+1),
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
			if _, err := OpenBook(context.Background(), dir, "self.test:1", nil, nil); err == nil {
				t.Fatal("OpenBook succeeded")
			}
		})
	}
}

func TestOpenBookMigratesVersion1AddressesToCandidates(t *testing.T) {
	dir := bookTestDir(t)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, bookStateName), []byte(`{"version":1,"peers":["a.test:1","b.test:2"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := OpenBook(context.Background(), dir, "self.test:1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if got, err := loadBookState(dir); err != nil || !reflect.DeepEqual(got, map[string]string{"a.test:1": "", "b.test:2": ""}) {
		t.Fatalf("migrated candidates = %v, %v", got, err)
	}
	if got := b.Snapshot().Peers; len(got) != 0 {
		t.Fatalf("version 1 addresses exported with identities: %v", got)
	}
}

func TestOpenBookRejectsUnsafeAndUnknownFiles(t *testing.T) {
	for name, setup := range map[string]func(string) error{
		"state permissions": func(dir string) error {
			return os.WriteFile(filepath.Join(dir, bookStateName), []byte(`{"version":2,"peers":{}}`), 0o644)
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
			if _, err := OpenBook(context.Background(), dir, "self.test:1", nil, nil); err == nil {
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
	b, err := OpenBook(context.Background(), dir, "self.test:1", nil, nil)
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
	published, err := writeBookState(dir, map[string]bookRecord{"peer.test:1": {NodeID: testNodeID(1)}})
	if err == nil || published {
		t.Fatalf("rename over directory: published=%t, error=%v", published, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != bookStateName || !entries[0].IsDir() {
		t.Fatalf("failed rename left staging files or replaced destination: %v, %v", entries, err)
	}
}
