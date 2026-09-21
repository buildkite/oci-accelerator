package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestAdmissionProgressivelyReclaimsMoreThanAttemptBudget(t *testing.T) {
	const smallBody = "x"
	const victims = 9
	smallCharge := metadataReservation + int64(len(smallBody))
	s := openTest(t, victims*smallCharge)
	seedEvictionEntries(t, s, victims, smallBody)

	largeBody := strings.Repeat("n", int(8*smallCharge+1-metadataReservation))
	_, err := s.Begin(testMeta("needs-nine-small-victims", largeBody))
	assertAdmissionReason(t, err, AdmissionCapacity)
	if got := s.Stats(); got.Evictions != 8 || got.Bytes != smallCharge || got.Entries != 1 {
		t.Fatalf("first reclamation batch stats = %+v", got)
	}

	meta := testMeta("needs-nine-small-victims", largeBody)
	f, err := s.Begin(meta)
	if err != nil {
		t.Fatalf("second Begin: %v", err)
	}
	if _, err := io.WriteString(f, largeBody); err != nil {
		t.Fatal(err)
	}
	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}
	read(t, s, meta, largeBody)
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
	if got := s.Stats(); got.Evictions != before.Evictions+smallVictims || got.Bytes != before.Bytes-smallVictims*smallCharge || got.Entries != before.Entries-smallVictims {
		t.Fatalf("first progressive batch accounting: before=%+v after=%+v", before, got)
	}

	// Refill the free space with older small candidates. If fitting admissions
	// reset the cursor, the next request spends its whole budget on these instead
	// of resuming at the larger victim.
	for i := range smallVictims {
		meta := testMeta(fmt.Sprintf("replacement-small-%02d", i), smallBody)
		put(t, s, meta, smallBody)
		s.setEntryLastLocked(s.entries[meta.Key], 0)
	}
	f, err := s.Begin(testMeta("deferred-large-second", largeBody))
	if err != nil {
		t.Fatalf("second Begin did not reach larger victim: %v", err)
	}
	if err := f.Abort(); err != nil {
		t.Fatal(err)
	}
	if got := s.Stats(); got.Evictions != before.Evictions+smallVictims+1 || got.Bytes != before.Bytes-largeCharge {
		t.Fatalf("larger-victim eviction stats = %+v", got)
	}
	for _, meta := range small {
		if _, err := s.Get(context.Background(), meta); err != ErrMiss {
			t.Fatalf("evicted small victim Get = %v, want ErrMiss", err)
		}
	}
}

func TestAdmissionGloballyImpossibleWithPinnedEntryDoesNotEvict(t *testing.T) {
	const body = "x"
	const victims = 3
	charge := metadataReservation + int64(len(body))
	s := openTest(t, victims*charge)
	metas := seedEvictionEntries(t, s, victims, body)
	r, err := s.Get(context.Background(), metas[1])
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	before := s.Stats()
	largeBody := strings.Repeat("p", int(victims*charge-metadataReservation))
	_, err = s.Begin(testMeta("pinned-prevents-global-fit", largeBody))
	assertAdmissionReason(t, err, AdmissionCapacity)
	if got := s.Stats(); got.Evictions != before.Evictions || got.Bytes != before.Bytes || got.Entries != before.Entries {
		t.Fatalf("globally impossible admission changed accounting: before=%+v after=%+v", before, got)
	}
}

func TestAdmissionPartialEvictionFailuresReportIOErrorAndProgress(t *testing.T) {
	requireUnprivilegedTest(t)
	const body = "x"
	const victims = 11
	charge := metadataReservation + int64(len(body))
	s := openTest(t, victims*charge)
	metas := seedEvictionEntries(t, s, victims, body)
	for _, meta := range metas[:2] {
		denyEntryRemoval(t, s.entries[meta.Key].dir)
	}
	largeBody := strings.Repeat("m", int(8*charge+1-metadataReservation))

	_, err := s.Begin(testMeta("mixed-removal-failures", largeBody))
	assertAdmissionReason(t, err, AdmissionIOError)
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("Begin did not preserve first removal error: %v", err)
	}
	if got := s.Stats(); got.Evictions != 6 || got.Bytes != 5*charge || got.Entries != 5 {
		t.Fatalf("partial failure accounting = %+v", got)
	}

	f, err := s.Begin(testMeta("mixed-removal-failures", largeBody))
	if err != nil {
		t.Fatalf("subsequent Begin did not make progress: %v", err)
	}
	if err := f.Abort(); err != nil {
		t.Fatal(err)
	}
	if got := s.Stats(); got.Evictions != 9 || got.Bytes != 2*charge || got.Entries != 2 {
		t.Fatalf("subsequent progress accounting = %+v", got)
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
	err := s.evictAtStartupLocked(9*charge, 1, math.MaxInt64)
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if st := s.Stats(); st.Entries != 0 || st.Bytes != 0 || st.Evictions != 9 {
		t.Fatalf("uncapped eviction retained victims: %+v", st)
	}
}

func TestEvictionRechecksVictimPinsAfterBlockedRemoval(t *testing.T) {
	const charge = metadataReservation + 1
	s := openTest(t, 2*charge)
	metas := seedEvictionEntries(t, s, 2, "x")

	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	realRemoveAll := s.removeAll
	firstDir := filepath.Join(s.dir, metas[0].Key)
	s.removeAll = func(path string) error {
		if path == firstDir {
			close(entered)
			<-release
		}
		return realRemoveAll(path)
	}
	result := make(chan error, 1)
	go func() {
		_, err := s.Begin(testMeta("needs-both-victims", "ab"))
		result <- err
	}()
	awaitBarrier(t, entered, "first victim removal")
	type getResult struct {
		r   *Reader
		err error
	}
	getDone := make(chan getResult, 1)
	go func() {
		r, err := s.Get(context.Background(), metas[1])
		getDone <- getResult{r, err}
	}()
	var r *Reader
	t.Cleanup(func() {
		if r != nil {
			_ = r.Close()
		}
	})
	got := awaitResult(t, getDone, "pinning second victim")
	r = got.r
	if got.err != nil {
		t.Fatal(got.err)
	}
	close(release)
	select {
	case err := <-result:
		assertAdmissionReason(t, err, AdmissionCapacity)
	case <-time.After(2 * time.Second):
		t.Fatal("Begin remained blocked")
	}
	if st := s.Stats(); st.Bytes != charge || st.Entries != 1 || st.Readers != 1 || st.Reserved != 0 || st.Pending != 0 {
		t.Fatalf("capacity bypass accounting = %+v", st)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEvictionSkipsVictimHitAndClosedDuringBlockedRemoval(t *testing.T) {
	for _, clock := range []string{"advanced", "repeated"} {
		t.Run(clock, func(t *testing.T) {
			const charge = metadataReservation + 1
			s := openTest(t, 3*charge)
			metas := seedEvictionEntries(t, s, 3, "x")
			originalLast := s.entries[metas[1].Key].last

			entered, release := make(chan struct{}), make(chan struct{})
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
			})
			realRemoveAll := s.removeAll
			firstDir := filepath.Join(s.dir, metas[0].Key)
			s.removeAll = func(path string) error {
				if path == firstDir {
					close(entered)
					<-release
				}
				return realRemoveAll(path)
			}
			result := make(chan error, 1)
			go func() {
				// Two removals are needed, but the third candidate can replace the
				// second if it becomes hot while the first removal is blocked.
				f, err := s.Begin(testMeta("needs-two-cold-victims", "ab"))
				if f != nil {
					err = errors.Join(err, f.Abort())
				}
				result <- err
			}()
			awaitBarrier(t, entered, "first victim removal")
			// This reads exact bytes and closes the reader before releasing eviction.
			// Pin checks alone cannot protect this now-unpinned, recently hit entry.
			assertHitProgress(t, s, metas[1], "x")
			if clock == "repeated" {
				// Model a clock that returned the same timestamp on this access.
				s.mu.Lock()
				s.setEntryLastLocked(s.entries[metas[1].Key], originalLast)
				s.mu.Unlock()
			}
			close(release)
			if err := awaitResult(t, result, "admission after concurrent hit"); err != nil {
				t.Fatal(err)
			}
			read(t, s, metas[1], "x")
			for _, meta := range []Metadata{metas[0], metas[2]} {
				if r, err := s.Get(context.Background(), meta); err != ErrMiss {
					if r != nil {
						_ = r.Close()
					}
					t.Fatalf("cold victim Get = %v, want ErrMiss", err)
				}
			}
			if st := s.Stats(); st.Evictions != 2 || st.Bytes != charge || st.Entries != 1 || st.Readers != 0 || st.Reserved != 0 || st.Pending != 0 {
				t.Fatalf("concurrent hit accounting = %+v", st)
			}
		})
	}
}

func TestPinnedEntryRefusalDoesNotEvictUsefulEntries(t *testing.T) {
	const charge = metadataReservation + 1
	s := openTest(t, 2*charge)
	a, b := testMeta("a", "a"), testMeta("b", "b")
	put(t, s, a, "a")
	put(t, s, b, "b")
	r, err := s.Get(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	tooLarge := testMeta("large", strings.Repeat("x", int(charge)))
	if _, err := s.Begin(tooLarge); !errors.Is(err, ErrBypass) {
		t.Fatalf("admission = %v", err)
	}
	read(t, s, b, "b")
	if got := s.Stats().Evictions; got != 0 {
		t.Fatalf("evictions = %d", got)
	}
}

func TestFailedAdmissionDoesNotEvictInsufficientVictims(t *testing.T) {
	s := openTest(t, 3*metadataReservation)
	a := testMeta("old", "a")
	body := strings.Repeat("b", int(metadataReservation)-1)
	b := testMeta("new", body)
	put(t, s, a, "a")
	put(t, s, b, body)
	s.setEntryLastLocked(s.entries[a.Key], 1)
	s.setEntryLastLocked(s.entries[b.Key], 3)
	if err := s.evictAtStartupLocked(2*metadataReservation, 0, 2); err != ErrBypass {
		t.Fatalf("eviction without enough eligible space = %v", err)
	}
	read(t, s, a, "a")
	read(t, s, b, body)
	if s.Stats().Evictions != 0 {
		t.Fatal("failed admission evicted useful data")
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
