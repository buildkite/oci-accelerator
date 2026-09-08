package cache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadInventoryEntryOutcomesWithoutRemoval(t *testing.T) {
	meta := testMeta("inventory-loader", "body")
	const committed = int64(12345)

	makeEntry := func(t *testing.T) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), meta.Key)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "body"), []byte("body"), 0o600); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(diskMetadata{Metadata: meta, Committed: committed})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "meta"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("load", func(t *testing.T) {
		path := makeEntry(t)
		e, err := loadInventoryEntry(path, meta.Key)
		if err != nil || e == nil {
			t.Fatalf("load = %+v, %v", e, err)
		}
		if e.meta != meta || e.dir != path || e.charge != meta.Size+metadataReservation || e.last != committed || e.verified {
			t.Fatalf("loaded entry = %+v", e)
		}
	})

	t.Run("discard", func(t *testing.T) {
		path := makeEntry(t)
		if err := os.WriteFile(filepath.Join(path, "meta"), []byte("invalid"), 0o600); err != nil {
			t.Fatal(err)
		}
		e, err := loadInventoryEntry(path, meta.Key)
		if err != nil || e != nil {
			t.Fatalf("load corrupt = %+v, %v", e, err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("loader removed discardable entry: %v", err)
		}
	})

	t.Run("refuse", func(t *testing.T) {
		path := makeEntry(t)
		if err := os.WriteFile(filepath.Join(path, "unknown"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if e, err := loadInventoryEntry(path, meta.Key); err == nil || e != nil {
			t.Fatalf("load unknown state = %+v, %v", e, err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("loader removed refused entry: %v", err)
		}
	})
}
