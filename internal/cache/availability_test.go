package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestKeysAndChangesSignalOnlySuccessfulPublication(t *testing.T) {
	s := openTest(t, 1<<20)
	changes := s.Changes()
	if s.Changes() != changes {
		t.Fatal("Changes returned a different channel on the second call")
	}
	if keys := s.Keys(); keys == nil || len(keys) != 0 {
		t.Fatalf("empty store Keys = %#v", keys)
	}

	aborted := testMeta("aborted", "body")
	f, err := s.Begin(aborted)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(f, "body"); err != nil {
		t.Fatal(err)
	}
	if err := f.Abort(); err != nil {
		t.Fatal(err)
	}
	assertNoChange(t, changes, "aborted fill")

	failed := testMeta("failed", "body")
	f, err = s.Begin(failed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(f, "evil"); err != nil {
		t.Fatal(err)
	}
	if err := f.Commit(); err == nil {
		t.Fatal("Commit accepted a digest mismatch")
	}
	assertNoChange(t, changes, "failed fill")
	assertKeys(t, s)

	published := testMeta("published", "body")
	put(t, s, published, "body")
	assertChange(t, changes, "successful publication")
	assertKeys(t, s, published)

	read(t, s, published, "body")
	if r, err := s.Get(context.Background(), failed); err != ErrMiss {
		if r != nil {
			_ = r.Close()
		}
		t.Fatalf("Get failed key = %v", err)
	}
	if _, err := s.Begin(published); !errors.Is(err, ErrBypass) {
		t.Fatalf("duplicate Begin = %v", err)
	}
	assertNoChange(t, changes, "reads, misses, and rejected admission")
}

func TestChangesCoalesceWithoutLosingLaterWake(t *testing.T) {
	s := openTest(t, 1<<20)
	changes := s.Changes()
	a, b, c := testMeta("a", "1"), testMeta("b", "2"), testMeta("c", "3")

	put(t, s, a, "1")
	put(t, s, b, "2")
	assertChange(t, changes, "burst of publications")
	assertNoChange(t, changes, "coalesced burst")
	assertKeys(t, s, a, b)

	put(t, s, c, "3")
	assertChange(t, changes, "publication after drained wake")
	assertKeys(t, s, a, b, c)
}

func TestChangesConsumerConvergesOnConcurrentPublications(t *testing.T) {
	s := openTest(t, 1<<20)
	const n = 16 // openDefault's MaxFills, so every concurrent Begin is admitted.
	metas := make([]Metadata, n)
	for i := range metas {
		metas[i] = testMeta(fmt.Sprintf("key-%d", i), fmt.Sprintf("body-%d", i))
	}

	// Subscribe before the first snapshot, then snapshot after every wake.
	changes := s.Changes()
	converged := make(chan []string, 1)
	go func() {
		deadline := time.After(5 * time.Second)
		for {
			if keys := s.Keys(); len(keys) == n {
				converged <- keys
				return
			}
			select {
			case <-changes:
			case <-deadline:
				converged <- s.Keys()
				return
			}
		}
	}()

	errs := make(chan error, n)
	for i, m := range metas {
		go func() {
			f, err := s.Begin(m)
			if err == nil {
				_, err = io.WriteString(f, fmt.Sprintf("body-%d", i))
				if err == nil {
					err = f.Commit()
				} else {
					err = errors.Join(err, f.Abort())
				}
			}
			errs <- err
		}()
	}
	for range metas {
		if err := awaitResult(t, errs, "concurrent publication"); err != nil {
			t.Fatal(err)
		}
	}
	if keys := <-converged; len(keys) != n {
		t.Fatalf("consumer saw %d of %d keys after its last wake", len(keys), n)
	}
}

func TestChangesSignalEvictionAndKeepClaimedEntries(t *testing.T) {
	s := openTest(t, metadataReservation+4)
	changes := s.Changes()
	old, replacement := testMeta("old", "aaaa"), testMeta("new", "bbbb")
	put(t, s, old, "aaaa")
	assertChange(t, changes, "first publication")

	// A failed eviction removal keeps the victim and reports no change.
	var claimedKeys []string
	s.removeAll = func(string) error {
		claimedKeys = s.Keys()
		return os.ErrPermission
	}
	if _, err := s.Begin(replacement); !errors.Is(err, ErrBypass) {
		t.Fatalf("Begin with failed eviction = %v", err)
	}
	if !slices.Equal(claimedKeys, []string{old.Key}) {
		t.Fatalf("Keys during claimed removal = %v", claimedKeys)
	}
	assertNoChange(t, changes, "failed eviction")
	assertKeys(t, s, old)

	s.removeAll = os.RemoveAll
	put(t, s, replacement, "bbbb")
	assertChange(t, changes, "eviction and publication")
	assertKeys(t, s, replacement)
	if st := s.Stats(); st.Evictions != 1 {
		t.Fatalf("eviction count = %d", st.Evictions)
	}
}

func TestChangesSignalReaderInvalidationAndDeletion(t *testing.T) {
	s := openTest(t, 1<<20)
	meta, other := testMeta("broken", "abcdef"), testMeta("other", "x")
	put(t, s, meta, "abcdef")
	put(t, s, other, "x")
	changes := s.Changes()

	r, err := s.Get(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	assertNoChange(t, changes, "Get hit")

	// Force a descriptor read error while the reader still pins the entry.
	if err := r.file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("failed read = %v", err)
	}
	assertChange(t, changes, "read invalidation")
	assertKeys(t, s, other)
	if st := s.Stats(); st.Entries != 2 {
		t.Fatalf("invalid pinned entry left the index early: %+v", st)
	}

	if _, err := r.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("repeated failed read = %v", err)
	}
	assertNoChange(t, changes, "repeated invalidation")

	if err := r.Close(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Close = %v", err)
	}
	assertNoChange(t, changes, "deletion of an already-invalid entry")
	if st := s.Stats(); st.Entries != 1 {
		t.Fatalf("final unpin kept invalid entry: %+v", st)
	}
	assertKeys(t, s, other)
}

func TestKeysAcrossReopenVerificationAndClose(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	s, err := openDefault(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	a, b := testMeta("a", "aaaa"), testMeta("b", "bbbb")
	put(t, s, a, "aaaa")
	put(t, s, b, "bbbb")
	changes := s.Changes()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	assertChange(t, changes, "Close")
	assertKeys(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	assertNoChange(t, changes, "repeated Close")

	// Startup inventory is visible through the initial snapshot alone.
	s, err = openDefault(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	changes = s.Changes()
	assertNoChange(t, changes, "startup inventory")
	assertKeys(t, s, a, b)

	// Reopened entries verify on first Get, and corruption retires them.
	if err := os.WriteFile(filepath.Join(dir, a.Key, "body"), []byte("xxxx"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r, err := s.Get(context.Background(), a); err != ErrMiss {
		if r != nil {
			_ = r.Close()
		}
		t.Fatalf("corrupt Get = %v", err)
	}
	assertChange(t, changes, "verification invalidation")
	assertNoChange(t, changes, "coalesced invalidation and deletion")
	assertKeys(t, s, b)
	read(t, s, b, "bbbb")
	assertNoChange(t, changes, "first verified read")

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	assertChange(t, changes, "Close after reopen")
	assertKeys(t, s)
}

// assertChange requires a pending notification. Notifications are sent before
// the mutating call returns, so no wait is needed.
func assertChange(t *testing.T, changes <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-changes:
	default:
		t.Fatalf("no change notification after %s", what)
	}
}

func assertNoChange(t *testing.T, changes <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-changes:
		t.Fatalf("unexpected change notification after %s", what)
	default:
	}
}

func assertKeys(t *testing.T, s *Store, metas ...Metadata) {
	t.Helper()
	want := make([]string, 0, len(metas))
	for _, m := range metas {
		want = append(want, m.Key)
	}
	slices.Sort(want)
	if got := s.Keys(); got == nil || !slices.Equal(got, want) {
		t.Fatalf("Keys = %#v, want %#v", got, want)
	}
}
