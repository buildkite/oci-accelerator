package cache

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
	"testing/synctest"
	"time"
)

func TestKeysEmptyStore(t *testing.T) {
	s := openTest(t, 1<<20)
	assertKeys(t, s, nil)
}

func TestKeysListsCommittedEntriesByRecency(t *testing.T) {
	s := openTest(t, 1<<20)
	metas := []Metadata{testMeta("c", "b"), testMeta("a", "b"), testMeta("b", "b")}
	sort.Slice(metas, func(i, j int) bool { return metas[i].Key > metas[j].Key })
	for _, m := range metas {
		put(t, s, m, "b")
	}
	want := []string{metas[0].Key, metas[1].Key, metas[2].Key}
	assertKeys(t, s, want)
	read(t, s, metas[0], "b")
	assertKeys(t, s, []string{metas[1].Key, metas[2].Key, metas[0].Key})
}

func TestKeysExcludesActiveFill(t *testing.T) {
	s := openTest(t, 1<<20)
	committed := testMeta("committed", "body")
	put(t, s, committed, "body")
	fill, err := s.Begin(testMeta("pending", "body"))
	if err != nil {
		t.Fatal(err)
	}
	defer fill.Abort()
	if _, err := io.WriteString(fill, "bo"); err != nil {
		t.Fatal(err)
	}
	assertKeys(t, s, []string{committed.Key})
}

func TestKeysExcludesAbortedFill(t *testing.T) {
	s := openTest(t, 1<<20)
	fill, err := s.Begin(testMeta("aborted", "body"))
	if err != nil {
		t.Fatal(err)
	}
	if err := fill.Abort(); err != nil {
		t.Fatal(err)
	}
	assertKeys(t, s, nil)
}

func TestKeysExcludesFailedFill(t *testing.T) {
	s := openTest(t, 1<<20)
	digest, err := s.Begin(testMeta("digest", "body"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(digest, "evil"); err != nil {
		t.Fatal(err)
	}
	if err := digest.Commit(); err == nil {
		t.Fatal("commit with a mismatched digest succeeded")
	}
	size, err := s.Begin(testMeta("size", "body"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(size, "bo"); err != nil {
		t.Fatal(err)
	}
	if err := size.Commit(); err == nil {
		t.Fatal("commit with a short body succeeded")
	}
	assertKeys(t, s, nil)
}

func TestKeysExcludesInvalidEntryAwaitingCleanup(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	bad, good := testMeta("bad", "same"), testMeta("good", "good")
	bad.RequestKey = testMeta("bad request", "").Key
	s, err := openDefault(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	put(t, s, bad, "same")
	put(t, s, good, "good")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopened entries are unverified, so the first Get checks the digest.
	if err := os.WriteFile(filepath.Join(dir, bad.Key, "body"), []byte("evil"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err = openDefault(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// A failed removal leaves the invalid entry indexed.
	s.removeAll = func(string) error { return errors.New("leave tombstone") }
	if _, err := s.Get(context.Background(), bad); err != ErrMiss {
		t.Fatalf("corrupt get = %v", err)
	}
	s.mu.Lock()
	e, indexed := s.entries[bad.Key]
	s.mu.Unlock()
	if !indexed || !e.invalid {
		t.Fatal("corrupt entry is not an indexed tombstone")
	}
	assertKeys(t, s, []string{good.Key})
	assertLookup(t, s, bad.RequestKey, Metadata{}, ErrMiss)
}

func TestKeysExcludesEntryClaimedByEviction(t *testing.T) {
	s, err := OpenWithLimits(filepath.Join(t.TempDir(), "cache"), Limits{MaxBytes: 1 << 20, MaxFills: 1, MaxEntries: 1, FillTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	old, replacement := testMeta("old", "1"), testMeta("new", "2")
	old.RequestKey = testMeta("evicting request", "").Key
	put(t, s, old, "1")

	entered, release := make(chan struct{}), make(chan struct{})
	// Deferred after Close so a failing assertion releases the removal
	// before Close waits for it.
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	realRemoveAll := s.removeAll
	target := filepath.Join(s.dir, old.Key)
	s.removeAll = func(path string) error {
		if path == target {
			close(entered)
			<-release
		}
		return realRemoveAll(path)
	}
	admitted := make(chan error, 1)
	go func() {
		fill, err := s.Begin(replacement)
		if err == nil {
			err = fill.Abort()
		}
		admitted <- err
	}()
	awaitBarrier(t, entered, "eviction removal")
	s.mu.Lock()
	e, indexed := s.entries[old.Key]
	s.mu.Unlock()
	if !indexed || !e.claimed {
		t.Fatal("evicting entry is not an indexed claim")
	}
	assertKeys(t, s, nil)
	assertLookup(t, s, old.RequestKey, Metadata{}, ErrMiss)
	close(release)
	if err := awaitResult(t, admitted, "admission after eviction"); err != nil {
		t.Fatal(err)
	}
}

func TestKeysExcludesEvictedEntry(t *testing.T) {
	s, err := OpenWithLimits(filepath.Join(t.TempDir(), "cache"), Limits{MaxBytes: 1 << 20, MaxFills: 1, MaxEntries: 1, FillTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	old, replacement := testMeta("old", "1"), testMeta("new", "2")
	put(t, s, old, "1")
	put(t, s, replacement, "2")
	assertKeys(t, s, []string{replacement.Key})
}

func TestKeysIncludesRecoveredEntries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "cache")
		s, err := openDefault(dir, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		a, b := testMeta("a", "1"), testMeta("b", "2")
		// Equal persisted commit times are recovered in key order.
		put(t, s, a, "1")
		put(t, s, b, "2")
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s, err = openDefault(dir, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		want := []string{a.Key, b.Key}
		slices.Sort(want)
		assertKeys(t, s, want)
	})
}

func TestKeyPageBoundaries(t *testing.T) {
	s := openTest(t, 1<<20)
	a, b, c := testMeta("a", "x"), testMeta("b", "x"), testMeta("c", "x")
	for _, meta := range []Metadata{c, a, b} {
		put(t, s, meta, "x")
	}
	for _, limit := range []int{1, 2, 3, 4} {
		var after KeyPosition
		var got []string
		for page := 0; ; page++ {
			keys, next, err := s.KeyPage(after, limit)
			if err != nil || len(keys) > limit || page > 3 {
				t.Fatalf("limit %d: page %d = %v, %+v, %v", limit, page, keys, next, err)
			}
			got = append(got, keys...)
			if next == (KeyPosition{}) {
				break
			}
			if len(keys) != limit || next.Key != keys[len(keys)-1] {
				t.Fatalf("limit %d: incomplete nonterminal page %v, %+v", limit, keys, next)
			}
			after = next
		}
		if !slices.Equal(got, []string{c.Key, a.Key, b.Key}) {
			t.Fatalf("limit %d: keys = %v", limit, got)
		}
	}
	for _, limit := range []int{0, -1} {
		if _, _, err := s.KeyPage(KeyPosition{}, limit); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("limit %d: error = %v", limit, err)
		}
	}
}

func TestKeyPageLiveMovement(t *testing.T) {
	for _, moved := range []int{0, 1, 2} {
		t.Run([]string{"earlier key", "boundary key", "unlisted key"}[moved], func(t *testing.T) {
			s := openTest(t, 1<<20)
			metas := []Metadata{testMeta("a", "x"), testMeta("b", "x"), testMeta("c", "x"), testMeta("d", "x")}
			for _, meta := range metas {
				put(t, s, meta, "x")
			}
			keys, after, err := s.KeyPage(KeyPosition{}, 2)
			if err != nil || !slices.Equal(keys, []string{metas[0].Key, metas[1].Key}) || after == (KeyPosition{}) {
				t.Fatalf("first page = %v, %+v, %v", keys, after, err)
			}
			read(t, s, metas[moved], "x")
			newest := testMeta("new publication", "x")
			put(t, s, newest, "x")
			want := []string{metas[2].Key, metas[3].Key}
			if moved == 2 {
				want = []string{metas[3].Key}
			}
			want = append(want, metas[moved].Key, newest.Key)
			keys, next, err := s.KeyPage(after, 4)
			if err != nil || next != (KeyPosition{}) || !slices.Equal(keys, want) {
				t.Fatalf("live continuation = %v, %+v, %v, want %v", keys, next, err, want)
			}
		})
	}
}

func TestKeyPageEvictedBoundaryAndRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	limits := Limits{MaxBytes: 1 << 20, MaxFills: 1, MaxEntries: 2, FillTimeout: time.Minute}
	s, err := OpenWithLimits(dir, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, b, c := testMeta("a", "x"), testMeta("b", "x"), testMeta("c", "x")
	put(t, s, a, "x")
	put(t, s, b, "x")
	keys, after, err := s.KeyPage(KeyPosition{}, 1)
	if err != nil || !slices.Equal(keys, []string{a.Key}) || after == (KeyPosition{}) {
		t.Fatalf("first page = %v, %+v, %v", keys, after, err)
	}
	put(t, s, c, "x") // Evicts the cursor's entry.
	keys, next, err := s.KeyPage(after, 2)
	if err != nil || next != (KeyPosition{}) || !slices.Equal(keys, []string{b.Key, c.Key}) {
		t.Fatalf("evicted boundary continuation = %v, %+v, %v", keys, next, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenWithLimits(dir, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, _, err := reopened.KeyPage(after, 2); !errors.Is(err, ErrKeyCursor) {
		t.Fatalf("old store cursor = %v, want ErrKeyCursor", err)
	}
	keys, _, err = reopened.KeyPage(KeyPosition{}, 2)
	if err != nil || len(keys) != 2 || !slices.Contains(keys, b.Key) || !slices.Contains(keys, c.Key) {
		t.Fatalf("fresh listing = %v, %v", keys, err)
	}
}

func TestKeysAfterClose(t *testing.T) {
	s, err := openDefault(filepath.Join(t.TempDir(), "cache"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	put(t, s, testMeta("a", "1"), "1")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	assertKeys(t, s, nil)
}

func assertKeys(t *testing.T, s *Store, want []string) {
	t.Helper()
	got, next, err := s.KeyPage(KeyPosition{}, 100)
	if err != nil || next != (KeyPosition{}) {
		t.Fatalf("single page = next %+v, error %v", next, err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
}
