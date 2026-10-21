package cache

import (
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"testing/synctest"
	"time"
)

func TestRecencyAfterRestartWithFutureCommitTimes(t *testing.T) {
	const body = "x"
	const charge = metadataReservation + 1
	s := openTest(t, 3*charge)
	metas := []Metadata{testMeta("recency-a", body), testMeta("recency-b", body), testMeta("recency-c", body)}
	sort.Slice(metas, func(i, j int) bool { return metas[i].Key < metas[j].Key })
	for _, meta := range metas {
		put(t, s, meta, body)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Model a restart after the wall clock has rolled back. Equal commit times
	// also force startup to use key order. Even the largest disk timestamp must
	// leave room for subsequent live accesses to advance.
	for _, meta := range metas {
		raw, err := json.Marshal(diskMetadata{Metadata: meta, Committed: math.MaxInt64})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s.dir, meta.Key, "meta"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reopened, err := openDefault(s.dir, s.max)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var keys []string
	for item := reopened.evictionOrder.Front(); item != nil; item = item.Next() {
		keys = append(keys, item.Value.(*entry).meta.Key)
	}
	for i, meta := range metas {
		if keys[i] != meta.Key {
			t.Fatalf("startup order = %v, want ascending keys", keys)
		}
	}
	cursor := reopened.entries[metas[2].Key].last
	read(t, reopened, metas[0], body)
	if last := reopened.entries[metas[0].Key].last; last <= cursor {
		t.Fatalf("hit recency = %d, must advance beyond startup cursor %d", last, cursor)
	}
	put(t, reopened, testMeta("recency-replacement", body), body)
	// The hit must protect the smallest key, not evict it after the rollback.
	read(t, reopened, metas[0], body)
	read(t, reopened, metas[2], body)
	if got := reopened.Stats(); got.Evictions != 1 || got.Entries != 3 || got.Bytes != 3*charge {
		t.Fatalf("post-rollback eviction accounting = %+v", got)
	}
}

func TestRecencyAdvancesForEqualClockHitsAndConcurrentFills(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := openTest(t, 1<<20)
		const body = "x"
		metas := []Metadata{testMeta("equal-a", body), testMeta("equal-b", body), testMeta("equal-c", body)}
		sort.Slice(metas, func(i, j int) bool { return metas[i].Key < metas[j].Key })
		start := time.Now()
		// Reserve fills before publishing or hitting anything. Recency belongs to
		// publication, not admission, and cannot be reserved by an earlier Begin.
		fills := make([]*Fill, len(metas))
		for i, meta := range metas {
			var err error
			fills[i], err = s.Begin(meta)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(fills[i], body); err != nil {
				t.Fatal(err)
			}
		}
		if err := fills[2].Commit(); err != nil {
			t.Fatal(err)
		}
		cursor := s.entries[metas[2].Key].last
		results := make(chan error, 2)
		for _, fill := range fills[:2] {
			go func() { results <- fill.Commit() }()
		}
		for range 2 {
			if err := <-results; err != nil {
				t.Fatal(err)
			}
		}
		a, b := s.entries[metas[0].Key].last, s.entries[metas[1].Key].last
		if a <= cursor || b <= cursor || a == b {
			t.Fatalf("concurrent publication recencies = %d, %d, must be distinct and beyond %d", a, b, cursor)
		}
		cursor = max(a, b)
		// The smallest key must advance past every earlier position even when
		// every operation samples exactly the same wall-clock timestamp.
		for range 2 {
			read(t, s, metas[0], body)
			last := s.entries[metas[0].Key].last
			if last <= cursor {
				t.Fatalf("hit recency = %d, must advance beyond %d", last, cursor)
			}
			cursor = last
		}
		if !time.Now().Equal(start) {
			t.Fatal("test did not exercise equal wall-clock timestamps")
		}
	})
}
