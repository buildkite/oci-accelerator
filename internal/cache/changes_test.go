package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestKeysListsSortedCommittedEntriesOnly(t *testing.T) {
	s := openTest(t, 1<<20)
	if keys := s.Keys(); len(keys) != 0 {
		t.Fatalf("empty store keys = %v", keys)
	}
	metas := []Metadata{testMeta("c", "b"), testMeta("a", "b"), testMeta("b", "b")}
	for _, m := range metas {
		put(t, s, m, "b")
	}
	pending := testMeta("pending", "body")
	fill, err := s.Begin(pending)
	if err != nil {
		t.Fatal(err)
	}
	defer fill.Abort()

	want := []string{metas[0].Key, metas[1].Key, metas[2].Key}
	slices.Sort(want)
	if got := s.Keys(); !slices.Equal(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
}

func TestChangesIsCreatedLazily(t *testing.T) {
	s := openTest(t, 1<<20)
	put(t, s, testMeta("a", "1"), "1")
	if s.changes != nil {
		t.Fatal("cache mutation created a notification channel without a subscriber")
	}
	first := s.Changes()
	if second := s.Changes(); first != second {
		t.Fatal("Changes returned a different channel on the second call")
	}
	assertNoChange(t, first)
}

func TestChangesNotifiesOnCommit(t *testing.T) {
	s := openTest(t, 1<<20)
	changes := s.Changes()
	m := testMeta("a", "body")
	fill, err := s.Begin(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(fill, "body"); err != nil {
		t.Fatal(err)
	}
	assertNoChange(t, changes)
	if err := fill.Commit(); err != nil {
		t.Fatal(err)
	}
	assertChange(t, changes)
	if got := s.Keys(); !slices.Equal(got, []string{m.Key}) {
		t.Fatalf("keys after commit = %v", got)
	}
}

func TestChangesIgnoresUnpublishedFills(t *testing.T) {
	s := openTest(t, 1<<20)
	changes := s.Changes()

	aborted, err := s.Begin(testMeta("aborted", "body"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(aborted, "bo"); err != nil {
		t.Fatal(err)
	}
	if err := aborted.Abort(); err != nil {
		t.Fatal(err)
	}
	assertNoChange(t, changes)

	failed, err := s.Begin(testMeta("failed", "body"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(failed, "evil"); err != nil {
		t.Fatal(err)
	}
	if err := failed.Commit(); err == nil {
		t.Fatal("commit with a mismatched digest succeeded")
	}
	assertNoChange(t, changes)

	expired, err := OpenWithLimits(filepath.Join(t.TempDir(), "cache"), Limits{MaxBytes: 1 << 20, MaxFills: 1, MaxEntries: 4, FillTimeout: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer expired.Close()
	expiredChanges := expired.Changes()
	late, err := expired.Begin(testMeta("late", "body"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	_, _ = io.WriteString(late, "body")
	if err := late.Commit(); err == nil {
		t.Fatal("commit after the fill deadline succeeded")
	}

	assertNoChange(t, expiredChanges)
	if keys := s.Keys(); len(keys) != 0 {
		t.Fatalf("unpublished fills appear in keys: %v", keys)
	}
}

func TestChangesNotifiesOnVerificationInvalidation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	bad, good := testMeta("bad", "same"), testMeta("good", "good")
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
	changes := s.Changes()
	if _, err := s.Get(context.Background(), bad); err != ErrMiss {
		t.Fatalf("corrupt get = %v", err)
	}
	assertChange(t, changes)
	if got := s.Keys(); !slices.Equal(got, []string{good.Key}) {
		t.Fatalf("keys after invalidation = %v, want only %s", got, good.Key)
	}
}

func TestChangesNotifiesOnReadFailureInvalidation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	s, err := openDefault(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m := testMeta("short", "body")
	put(t, s, m, "body")
	changes := s.Changes()
	assertNoChange(t, changes)
	// A just-committed entry is already verified, so truncating its body after
	// Get opens it surfaces on Read.
	r, err := s.Get(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(dir, m.Key, "body"), 2); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(r); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("read truncated body = %v", err)
	}
	assertChange(t, changes)
	if keys := s.Keys(); len(keys) != 0 {
		t.Fatalf("invalid entry remains in keys: %v", keys)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	// Removing an already invalid entry does not change the visible set.
	assertNoChange(t, changes)
}

func TestChangesNotifiesOnEviction(t *testing.T) {
	s, err := OpenWithLimits(filepath.Join(t.TempDir(), "cache"), Limits{MaxBytes: 1 << 20, MaxFills: 1, MaxEntries: 1, FillTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	old, replacement := testMeta("old", "1"), testMeta("new", "2")
	put(t, s, old, "1")
	changes := s.Changes()
	f, err := s.Begin(replacement)
	if err != nil {
		t.Fatal(err)
	}
	assertChange(t, changes)
	if keys := s.Keys(); len(keys) != 0 {
		t.Fatalf("evicted entry remains in keys: %v", keys)
	}
	if _, err := io.WriteString(f, "2"); err != nil {
		t.Fatal(err)
	}
	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}
	assertChange(t, changes)
	if got := s.Keys(); !slices.Equal(got, []string{replacement.Key}) {
		t.Fatalf("keys after eviction = %v", got)
	}
}

func TestChangesCoalesceWithoutBlocking(t *testing.T) {
	s := openTest(t, 1<<20)
	changes := s.Changes()
	metas := make([]Metadata, 10)
	for i := range metas {
		metas[i] = testMeta(fmt.Sprint(i), "b")
		put(t, s, metas[i], "b")
	}
	if len(changes) != 1 {
		t.Fatalf("pending notifications = %d, want 1", len(changes))
	}
	assertChange(t, changes)
	assertNoChange(t, changes)
	if got := len(s.Keys()); got != len(metas) {
		t.Fatalf("keys = %d, want %d", got, len(metas))
	}
}

// A subscriber that calls Changes before Keys and rescans after every wakeup
// converges on the final key set even while commits race its first snapshot.
func TestChangesSubscribeBeforeSnapshotObservesConcurrentCommits(t *testing.T) {
	s := openTest(t, 1<<20)
	const writers, perWriter = 4, 25
	start := make(chan struct{})
	var wg sync.WaitGroup
	want := make([]string, 0, writers*perWriter)
	for w := range writers {
		metas := make([]Metadata, perWriter)
		for i := range metas {
			metas[i] = testMeta(fmt.Sprintf("%d-%d", w, i), "b")
			want = append(want, metas[i].Key)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for _, m := range metas {
				f, err := s.Begin(m)
				if err == nil {
					_, err = io.WriteString(f, "b")
				}
				if err == nil {
					err = f.Commit()
				}
				if err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	slices.Sort(want)

	close(start)
	changes := s.Changes()
	seen := s.Keys()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	// Rescan on every wakeup while writers are still committing.
	for running := true; running; {
		select {
		case <-changes:
			seen = s.Keys()
		case <-done:
			running = false
		}
	}
	if t.Failed() {
		return
	}
	// Every commit has returned, so any key still unseen must have left a
	// pending wakeup behind.
	for !slices.Equal(seen, want) {
		select {
		case <-changes:
			seen = s.Keys()
		default:
			t.Fatalf("subscriber saw %d of %d keys without a pending notification", len(seen), len(want))
		}
	}
}

func TestChangesIgnoresRemovalOfInvalidEntries(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	s, err := openDefault(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m := testMeta("short", "body")
	put(t, s, m, "body")
	r, err := s.Get(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(dir, m.Key, "body"), 2); err != nil {
		t.Fatal(err)
	}
	changes := s.Changes()
	if _, err := io.ReadAll(r); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("read truncated body = %v", err)
	}
	assertChange(t, changes)

	// Admitting a replacement while the invalid generation is pinned, and then
	// publishing over it, only announces the replacement.
	f, err := s.Begin(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(f, "body"); err != nil {
		t.Fatal(err)
	}
	assertNoChange(t, changes)
	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}
	assertChange(t, changes)
	// Closing the detached generation's last reader is silent too.
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	assertNoChange(t, changes)
	if got := s.Keys(); !slices.Equal(got, []string{m.Key}) {
		t.Fatalf("keys after replacement = %v", got)
	}
	read(t, s, m, "body")
}

func TestChangesSurvivesClose(t *testing.T) {
	s, err := openDefault(filepath.Join(t.TempDir(), "cache"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	changes := s.Changes()
	put(t, s, testMeta("a", "1"), "1")
	assertChange(t, changes)
	active, err := s.Begin(testMeta("active", "body"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(active, "bo"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Close aborts the active fill without announcing it or closing the channel.
	assertNoChange(t, changes)
	if again := s.Changes(); again != changes {
		t.Fatal("Changes after Close returned a different channel")
	}
}

func assertChange(t *testing.T, changes <-chan struct{}) {
	t.Helper()
	select {
	case _, ok := <-changes:
		if !ok {
			t.Fatal("change notification channel closed")
		}
	default:
		t.Fatal("expected a pending change notification")
	}
}

func assertNoChange(t *testing.T, changes <-chan struct{}) {
	t.Helper()
	select {
	case _, ok := <-changes:
		if !ok {
			t.Fatal("change notification channel closed")
		}
		t.Fatal("unexpected change notification")
	default:
	}
}
