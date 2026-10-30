package peering

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestOpenBookPeerLimit(t *testing.T) {
	for _, count := range []int{32, 33} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			peers := make(map[string]bookRecord, count)
			for i := range count {
				peers[fmt.Sprintf("p%02d.test:1", i)] = bookRecord{NodeID: testNodeID(i), LastSuccess: time.Now()}
			}
			raw, err := json.Marshal(bookState{Version: 3, Peers: peers})
			if err != nil {
				t.Fatal(err)
			}

			dir := bookTestDir(t)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, bookStateName)
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}

			book, err := OpenBook(context.Background(), dir, "self.test:1", nil, nil)
			if book != nil {
				defer book.Close()
			}
			if count > 32 {
				if err == nil {
					t.Fatal("OpenBook accepted oversized membership")
				}
				got, readErr := os.ReadFile(path)
				if readErr != nil || string(got) != string(raw) {
					t.Fatalf("rejected state changed: %q, %v", got, readErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := book.Snapshot().Peers; len(got) != count {
				t.Fatalf("peer count = %d, want %d", len(got), count)
			}
		})
	}
}

func TestOpenBookRejectsInvalidState(t *testing.T) {
	tests := map[string]string{
		"corrupt":                `{`,
		"version":                `{"version":4,"peers":{}}`,
		"v1":                     `{"version":1,"peers":["a.test:1","b.test:2"]}`,
		"v2":                     `{"version":2,"peers":{"a.test:1":"000000000001"}}`,
		"unidentified v3":        `{"version":3,"peers":{"a.test:1":{"node_id":"","last_success":"2026-01-01T00:00:00Z"}}}`,
		"missing timestamp":      `{"version":3,"peers":{"a.test:1":{"node_id":"000000000001"}}}`,
		"zero timestamp":         `{"version":3,"peers":{"a.test:1":{"node_id":"000000000001","last_success":"0001-01-01T00:00:00Z"}}}`,
		"unknown record field":   `{"version":3,"peers":{"a.test:1":{"node_id":"000000000001","last_success":"2026-01-01T00:00:00Z","other":""}}}`,
		"unknown field":          `{"version":3,"peers":{},"extra":1}`,
		"invalid peer":           `{"version":3,"peers":{"bad":{"node_id":"000000000001","last_success":"2026-01-01T00:00:00Z"}}}`,
		"noncanonical peer":      `{"version":3,"peers":{"A.TEST:1":{"node_id":"000000000001","last_success":"2026-01-01T00:00:00Z"}}}`,
		"invalid node ID":        `{"version":3,"peers":{"a.test:1":{"node_id":"short","last_success":"2026-01-01T00:00:00Z"}}}`,
		"null node ID":           `{"version":3,"peers":{"a.test:1":{"node_id":null,"last_success":"2026-01-01T00:00:00Z"}}}`,
		"duplicate address":      `{"version":3,"peers":{"a.test:1":{"node_id":"000000000001","last_success":"2026-01-01T00:00:00Z"},"a.test:1":{"node_id":"000000000002","last_success":"2026-01-01T00:00:00Z"}}}`,
		"duplicate node ID":      `{"version":3,"peers":{"a.test:1":{"node_id":"000000000001","last_success":"2026-01-01T00:00:00Z"},"b.test:1":{"node_id":"000000000001","last_success":"2026-01-01T00:00:00Z"}}}`,
		"duplicate record field": `{"version":3,"peers":{"a.test:1":{"node_id":"000000000001","node_id":"000000000002","last_success":"2026-01-01T00:00:00Z"}}}`,
		"duplicate version":      `{"version":3,"version":3,"peers":{}}`,
		"duplicate peers":        `{"version":3,"peers":{},"peers":{}}`,
		"null peers":             `{"version":3,"peers":null}`,
		"trailing object":        `{"version":3,"peers":{}} {}`,
		"oversized":              strings.Repeat(" ", bookStateMax+1),
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

			raw, err := os.ReadFile(filepath.Join(dir, bookStateName))
			if err != nil || string(raw) != content {
				t.Fatalf("rejected state changed: %q, %v", raw, err)
			}
		})
	}
}

func TestOpenBookRejectsUnsafeAndUnknownFiles(t *testing.T) {
	for name, setup := range map[string]func(string) error{
		"state permissions": func(dir string) error {
			path := filepath.Join(dir, bookStateName)
			if err := os.WriteFile(path, []byte(`{"version":3,"peers":{}}`), 0o600); err != nil {
				return err
			}
			return os.Chmod(path, 0o644)
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
