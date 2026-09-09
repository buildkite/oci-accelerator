package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildEvictionOrderBulkSortsReverseAndShuffledEntries(t *testing.T) {
	s := &Store{entries: make(map[string]*entry)}
	// Deliberately mix recency, ties, and insertion order. Map traversal must not
	// affect the bulk-built startup index.
	for _, item := range []struct {
		name string
		last int64
	}{{"f", 3}, {"b", 1}, {"e", 2}, {"a", 1}, {"d", 2}, {"c", 3}} {
		meta := testMeta("bulk-"+item.name, "x")
		s.entries[meta.Key] = &entry{meta: meta, last: item.last}
	}

	s.buildEvictionOrderLocked()
	var got []*entry
	for item := s.evictionOrder.Front(); item != nil; item = item.Next() {
		e := item.Value.(*entry)
		if e.eviction != item {
			t.Fatalf("entry %q does not reference its list element", e.meta.Key)
		}
		got = append(got, e)
	}
	if len(got) != len(s.entries) {
		t.Fatalf("ordered entries = %d, want %d", len(got), len(s.entries))
	}
	for i := 1; i < len(got); i++ {
		previous, current := got[i-1], got[i]
		if previous.last > current.last || previous.last == current.last && previous.meta.Key > current.meta.Key {
			t.Fatalf("order[%d:%d] = (%d, %q), (%d, %q)", i-1, i, previous.last, previous.meta.Key, current.last, current.meta.Key)
		}
	}
}

func TestAdmissionEvictionAttemptBudgetBoundary(t *testing.T) {
	requireUnprivilegedTest(t)
	const victims = 8
	const body = "body"
	charge := metadataReservation + int64(len(body))
	s := openTest(t, victims*charge)
	metas := seedEvictionEntries(t, s, victims, body)
	for _, meta := range metas[:victims-1] {
		denyEntryRemoval(t, s.entries[meta.Key].dir)
	}

	f, err := s.Begin(testMeta("budget-boundary-new", body))
	if err != nil {
		t.Fatalf("Begin at exact eviction attempt budget: %v", err)
	}
	if err := f.Abort(); err != nil {
		t.Fatal(err)
	}
	if got := s.Stats(); got.Evictions != 1 || got.Bytes != (victims-1)*charge {
		t.Fatalf("stats after boundary eviction = %+v", got)
	}
	for _, meta := range metas[:victims-1] {
		read(t, s, meta, body)
	}
}

func TestAdmissionEvictionCursorContinuesAfterFailedBatch(t *testing.T) {
	requireUnprivilegedTest(t)
	const body = "body"
	const victims = 9
	charge := metadataReservation + int64(len(body))
	s := openTest(t, victims*charge)
	metas := seedEvictionEntries(t, s, victims, body)
	for _, meta := range metas[:8] {
		denyEntryRemoval(t, s.entries[meta.Key].dir)
	}

	before := s.Stats()
	_, err := s.Begin(testMeta("cursor-retry-first", body))
	assertAdmissionReason(t, err, AdmissionIOError)
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("first Begin did not preserve removal error: %v", err)
	}
	after := s.Stats()
	if after.Evictions != before.Evictions || after.Bytes != before.Bytes || after.Entries != before.Entries {
		t.Fatalf("failed batch changed stored accounting: before=%+v after=%+v", before, after)
	}

	f, err := s.Begin(testMeta("cursor-retry-second", body))
	if err != nil {
		t.Fatalf("second Begin did not continue beyond failed batch: %v", err)
	}
	if err := f.Abort(); err != nil {
		t.Fatal(err)
	}
	if got := s.Stats(); got.Evictions != before.Evictions+1 || got.Bytes != before.Bytes-charge {
		t.Fatalf("stats after cursor retry = %+v", got)
	}
	for _, meta := range metas[:8] {
		read(t, s, meta, body)
	}
}

func TestAdmissionPreflightRefusesMoreThanBudgetWithoutEviction(t *testing.T) {
	const smallBody = "x"
	const victims = 9
	smallCharge := metadataReservation + int64(len(smallBody))
	s := openTest(t, victims*smallCharge)
	metas := seedEvictionEntries(t, s, victims, smallBody)

	largeBody := strings.Repeat("n", int(8*smallCharge+1-metadataReservation))
	before := s.Stats()
	_, err := s.Begin(testMeta("needs-nine-small-victims", largeBody))
	assertAdmissionReason(t, err, AdmissionCapacity)
	after := s.Stats()
	if after.Evictions != before.Evictions || after.Bytes != before.Bytes || after.Entries != before.Entries {
		t.Fatalf("preflight refusal changed accounting: before=%+v after=%+v", before, after)
	}
	for _, meta := range metas {
		read(t, s, meta, smallBody)
	}
}

func TestAdmissionPreflightCursorReachesLargerVictim(t *testing.T) {
	const smallBody = "x"
	const smallVictims = 8
	smallCharge := metadataReservation + int64(len(smallBody))
	largeBody := strings.Repeat("L", int(9*smallCharge+1-metadataReservation))
	largeCharge := metadataReservation + int64(len(largeBody))
	s := openTest(t, smallVictims*smallCharge+largeCharge+smallCharge)
	small := seedEvictionEntries(t, s, smallVictims, smallBody)
	large := testMeta("later-large-victim", largeBody)
	put(t, s, large, largeBody)
	s.setEntryLastLocked(s.entries[large.Key], smallVictims+1)

	before := s.Stats()
	_, err := s.Begin(testMeta("deferred-large-first", largeBody))
	assertAdmissionReason(t, err, AdmissionCapacity)
	if got := s.Stats(); got.Evictions != before.Evictions || got.Bytes != before.Bytes || got.Entries != before.Entries {
		t.Fatalf("deferred batch changed accounting: before=%+v after=%+v", before, got)
	}

	// An unrelated admission that fits must not restart the deferred search.
	fitting, err := s.Begin(testMeta("fitting-small", smallBody))
	if err != nil {
		t.Fatal(err)
	}
	if err := fitting.Abort(); err != nil {
		t.Fatal(err)
	}
	f, err := s.Begin(testMeta("deferred-large-second", largeBody))
	if err != nil {
		t.Fatalf("second Begin did not reach larger victim: %v", err)
	}
	if err := f.Abort(); err != nil {
		t.Fatal(err)
	}
	if got := s.Stats(); got.Evictions != before.Evictions+1 || got.Bytes != before.Bytes-largeCharge {
		t.Fatalf("larger-victim eviction stats = %+v", got)
	}
	for _, meta := range small {
		read(t, s, meta, smallBody)
	}
}

func TestAdmissionEvictionCursorCanBecomePinned(t *testing.T) {
	requireUnprivilegedTest(t)
	const charge = metadataReservation + 4
	s := openTest(t, 17*charge)
	metas := seedEvictionEntries(t, s, 17, "body")
	for _, meta := range metas[:16] {
		denyEntryRemoval(t, s.entries[meta.Key].dir)
	}
	for range 2 {
		_, err := s.Begin(testMeta("before-pin", "body"))
		assertAdmissionReason(t, err, AdmissionIOError)
	}
	r, err := s.Get(context.Background(), metas[15])
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	f, err := s.Begin(testMeta("after-pin", "body"))
	if err != nil {
		t.Fatalf("pinned cursor prevented eviction: %v", err)
	}
	if err := f.Abort(); err != nil {
		t.Fatal(err)
	}
	if st := s.Stats(); st.Readers != 1 || st.Evictions != 1 || st.Bytes != 16*charge {
		t.Fatalf("pin/cursor accounting: %+v", st)
	}
	read(t, s, metas[15], "body")
}

func TestUncappedEvictionCanRemoveMoreThanAdmissionBudget(t *testing.T) {
	const charge = metadataReservation + 1
	s := openTest(t, 9*charge)
	seedEvictionEntries(t, s, 9, "x")
	s.mu.Lock()
	err := s.evictLocked(9*charge, 1, math.MaxInt64, 0)
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if st := s.Stats(); st.Entries != 0 || st.Bytes != 0 || st.Evictions != 9 {
		t.Fatalf("uncapped eviction retained victims: %+v", st)
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
	reopened, err := Open(s.dir, large.Size+metadataReservation)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	read(t, reopened, large, body)
	if st := reopened.Stats(); st.Entries != 1 || st.Bytes != large.Size+metadataReservation {
		t.Fatalf("startup did not retain only the newest object: %+v", st)
	}
}

func seedEvictionEntries(t *testing.T, s *Store, count int, body string) []Metadata {
	t.Helper()
	metas := make([]Metadata, count)
	for i := range metas {
		metas[i] = testMeta(fmt.Sprintf("eviction-victim-%02d", i), body)
		put(t, s, metas[i], body)
		s.setEntryLastLocked(s.entries[metas[i].Key], int64(i+1))
	}
	return metas
}

func denyEntryRemoval(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			return
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Errorf("restore %s permissions: %v", dir, err)
		}
	})
}

func assertAdmissionReason(t *testing.T, err error, reason AdmissionReason) {
	t.Helper()
	var admission *AdmissionError
	if !errors.Is(err, ErrBypass) || !errors.As(err, &admission) || admission.Reason != reason {
		t.Fatalf("Begin error = %v, want admission reason %s", err, reason)
	}
}
