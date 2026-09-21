package cache

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
func TestRecoveryAndCorruption(t *testing.T) {
	for _, mode := range []string{"body-missing", "short", "corrupt", "meta-missing", "meta-invalid", "meta-huge", "temporary"} {
		t.Run(mode, func(t *testing.T) {
			s := openTest(t, 1<<20)
			m := testMeta("key", "abcd")
			put(t, s, m, "abcd")
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			d := filepath.Join(s.dir, m.Key)
			switch mode {
			case "body-missing":
				if err := os.Remove(filepath.Join(d, "body")); err != nil {
					t.Fatal(err)
				}
			case "short":
				if err := os.Truncate(filepath.Join(d, "body"), 2); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := os.WriteFile(filepath.Join(d, "body"), []byte("xxxx"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "meta-missing":
				if err := os.Remove(filepath.Join(d, "meta")); err != nil {
					t.Fatal(err)
				}
			case "meta-invalid":
				if err := os.WriteFile(filepath.Join(d, "meta"), []byte("invalid"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "meta-huge":
				if err := os.Truncate(filepath.Join(d, "meta"), 1<<30); err != nil {
					t.Fatal(err)
				}
			case "temporary":
				if err := os.Rename(d, filepath.Join(s.dir, ".fill-abandoned")); err != nil {
					t.Fatal(err)
				}
			}
			s2, err := openDefault(s.dir, s.max)
			if err != nil {
				t.Fatal(err)
			}
			defer s2.Close()
			if r, err := s2.Get(context.Background(), m); err != ErrMiss {
				if r != nil {
					r.Close()
				}
				t.Fatal("bad entry hit")
			}
			put(t, s2, m, "abcd")
			read(t, s2, m, "abcd")
		})
	}
}
func TestRestartKeepsNewestCandidateRegardlessOfInventoryOrder(t *testing.T) {
	s := openTest(t, 1<<20)
	for _, key := range []string{"a", "b", "c"} {
		put(t, s, testMeta(key, "abcd"), "abcd")
	}
	s.Close()
	d, err := os.Open(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	names, err := d.ReadDir(-1)
	d.Close()
	if err != nil {
		t.Fatal(err)
	}
	var newest Metadata
	stamp := int64(100)
	for _, name := range names {
		if !name.IsDir() {
			continue
		}
		path := filepath.Join(s.dir, name.Name(), "meta")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var meta diskMetadata
		if err := json.Unmarshal(raw, &meta); err != nil {
			t.Fatal(err)
		}
		meta.Committed = stamp
		if stamp == 100 {
			newest = meta.Metadata
		}
		stamp--
		raw, _ = json.Marshal(meta)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s2, err := openDefault(s.dir, metadataReservation+4)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	read(t, s2, newest, "abcd")
	if s2.Stats().Entries != 1 {
		t.Fatal("capacity exceeded")
	}
}

func TestRestartReducedMaxEntriesKeepsNewest(t *testing.T) {
	s := openTest(t, 1<<20)
	old, newest := testMeta("old", "abcd"), testMeta("newest", "abcd")
	put(t, s, old, "abcd")
	put(t, s, newest, "abcd")
	s.Close()
	for m, stamp := range map[Metadata]int64{old: 1, newest: 2} {
		path := filepath.Join(s.dir, m.Key, "meta")
		var dm diskMetadata
		raw, _ := os.ReadFile(path)
		if err := json.Unmarshal(raw, &dm); err != nil {
			t.Fatal(err)
		}
		dm.Committed = stamp
		raw, _ = json.Marshal(dm)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s2, err := OpenWithLimits(s.dir, Limits{MaxBytes: 1 << 20, MaxFills: 1, MaxEntries: 1, FillTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	read(t, s2, newest, "abcd")
	if _, err := s2.Get(context.Background(), old); err != ErrMiss {
		t.Fatalf("old entry = %v", err)
	}
}

func TestStartupRetainsNewestLargeCandidate(t *testing.T) {
	s := openTest(t, 1<<20)
	metas := seedEvictionEntries(t, s, 10, "x")
	body := strings.Repeat("L", int(9*(metadataReservation+1)+1-metadataReservation))
	large := testMeta("large-startup", body)
	put(t, s, large, body)
	metas = append(metas, large)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// The newest object must survive regardless of directory traversal order.
	for i, meta := range metas {
		raw, err := json.Marshal(diskMetadata{Metadata: meta, Committed: int64(i + 1)})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s.dir, meta.Key, "meta"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reopened, err := openDefault(s.dir, large.Size+metadataReservation)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	read(t, reopened, large, body)
	if st := reopened.Stats(); st.Entries != 1 || st.Bytes != large.Size+metadataReservation {
		t.Fatalf("startup did not retain only the newest object: %+v", st)
	}
}
