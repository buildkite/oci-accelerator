package cache

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// pinnedTombstone invalidates an entry by unlinking its body and attempting
// another Get. The returned reader still pins and can read the original inode.
func pinnedTombstone(t *testing.T, s *Store, m Metadata, body string) *Reader {
	t.Helper()
	put(t, s, m, body)
	held, err := s.Get(context.Background(), m)
	if err != nil {
		t.Fatalf("initial Get: %v", err)
	}
	if err := os.Remove(filepath.Join(s.dir, m.Key, "body")); err != nil {
		t.Fatalf("remove body: %v", err)
	}
	if _, err := s.Get(context.Background(), m); err != ErrMiss {
		t.Fatalf("invalidating Get = %v, want ErrMiss", err)
	}
	e := s.entries[m.Key]
	if e == nil || !e.invalid || e.pins != 1 {
		t.Fatalf("expected invalid pinned tombstone, got %+v", e)
	}
	return held
}

// Replacing an invalid entry preserves old readers and releases the old charge
// only after those readers close.
func TestBeginReadmitsPinnedInvalidEntry(t *testing.T) {
	s := openTest(t, 1<<20)
	m := testMeta("readmit", "original")
	held := pinnedTombstone(t, s, m, "original")

	fresh := m
	fresh.Digest = testMeta("readmit", "replacement").Digest
	fresh.Size = int64(len("replacement"))

	f, err := s.Begin(fresh)
	if err != nil {
		t.Fatalf("Begin on pinned tombstone = %v, want success", err)
	}
	if _, err := io.WriteString(f, "replacement"); err != nil {
		t.Fatal(err)
	}
	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}

	got, err := io.ReadAll(held)
	if err != nil {
		t.Fatalf("held reader read: %v", err)
	}
	if string(got) != "original" {
		t.Fatalf("held reader returned %q, want original bytes", got)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}

	if e := s.entries[fresh.Key]; e == nil || e.invalid {
		t.Fatalf("new entry missing or invalid after old reader closed: %+v", e)
	}
	wantBytes := fresh.Size + metadataReservation
	if st := s.Stats(); st.Bytes != wantBytes {
		t.Fatalf("Stats.Bytes = %d, want %d (new charge only)", st.Bytes, wantBytes)
	}
	read(t, s, fresh, "replacement")
}

// Fresh readers see the replacement immediately; closing an old reader must
// neither remove the new directory nor subtract the new entry's charge.
func TestReadmitIndexIntegrityAcrossOverwrite(t *testing.T) {
	s := openTest(t, 1<<20)
	m := testMeta("integrity", "original")
	held := pinnedTombstone(t, s, m, "original")

	fresh := m
	fresh.Digest = testMeta("integrity", "brandnew").Digest
	fresh.Size = int64(len("brandnew"))

	f, err := s.Begin(fresh)
	if err != nil {
		t.Fatalf("Begin = %v", err)
	}
	newEntry := (*entry)(nil)
	if _, err := io.WriteString(f, "brandnew"); err != nil {
		t.Fatal(err)
	}
	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}

	newEntry = s.entries[fresh.Key]
	if newEntry == nil || newEntry.invalid {
		t.Fatalf("map does not point at committed new entry: %+v", newEntry)
	}
	read(t, s, fresh, "brandnew")

	// Unlinked bodies still occupy space until their final readers close.
	wantBytes := fresh.Size + metadataReservation
	withOld := wantBytes + m.Size + metadataReservation
	if st := s.Stats(); st.Bytes != withOld {
		t.Fatalf("Stats.Bytes before old-reader close = %d, want %d", st.Bytes, withOld)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if got := s.entries[fresh.Key]; got != newEntry {
		t.Fatalf("closing old reader disturbed the new entry: %+v", got)
	}
	if st := s.Stats(); st.Bytes != wantBytes {
		t.Fatalf("Stats.Bytes after old-reader close = %d, want %d (no double subtract)", st.Bytes, wantBytes)
	}
	read(t, s, fresh, "brandnew")
}

func TestDetachedReadersRetainCapacity(t *testing.T) {
	const charge = metadataReservation + 4
	s := openTest(t, 2*charge)
	m := testMeta("retained", "aaaa")
	held := pinnedTombstone(t, s, m, "aaaa")
	defer held.Close()
	put(t, s, m, "aaaa")
	current, err := s.Get(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	next := testMeta("next", "bbbb")
	f, err := s.Begin(next)
	if f != nil {
		f.Abort()
	}
	var admission *AdmissionError
	if !errors.As(err, &admission) || admission.Reason != AdmissionCapacity {
		t.Fatalf("admission with detached and current readers = %v", err)
	}
	if st := s.Stats(); st.Bytes != 2*charge || st.Readers != 2 || st.Evictions != 0 {
		t.Fatalf("detached reader lost accounting: %+v", st)
	}
	got, err := io.ReadAll(held)
	if err != nil || string(got) != "aaaa" {
		t.Fatalf("detached body = %q, %v", got, err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if st := s.Stats(); st.Bytes != charge || st.Readers != 1 {
		t.Fatalf("detached reader did not reclaim exactly once: %+v", st)
	}
	put(t, s, next, "bbbb")
	read(t, s, m, "aaaa")
	read(t, s, next, "bbbb")
}

func TestAbortedReplacementRetainsDetachedCharge(t *testing.T) {
	s := openTest(t, 1<<20)
	m := testMeta("aborted-replacement", "aaaa")
	held := pinnedTombstone(t, s, m, "aaaa")
	defer held.Close()
	f, err := s.Begin(m)
	if err != nil {
		t.Fatal(err)
	}
	// Stop at the publication boundary after detaching the old directory.
	if err := f.supersedeTombstone(filepath.Join(s.dir, m.Key)); err != nil {
		t.Fatal(err)
	}
	if err := f.Abort(); err != nil {
		t.Fatal(err)
	}
	if st := s.Stats(); st.Bytes != m.Size+metadataReservation || st.Reserved != 0 || st.Entries != 0 {
		t.Fatalf("aborted replacement dropped detached charge: %+v", st)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if st := s.Stats(); st.Bytes != 0 || st.Readers != 0 {
		t.Fatalf("detached charge not reclaimed: %+v", st)
	}
}

func TestCloseWaitsForDetachedReader(t *testing.T) {
	s := openTest(t, 1<<20)
	m := testMeta("close-detached", "aaaa")
	held := pinnedTombstone(t, s, m, "aaaa")
	defer held.Close()
	put(t, s, m, "aaaa")
	if err := s.Close(); err == nil {
		t.Fatal("Close succeeded with a detached reader")
	}
	if second, err := Open(s.dir, s.max); err == nil {
		second.Close()
		t.Fatal("lock released with a detached reader")
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(s.dir, s.max)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	read(t, second, m, "aaaa")
}

// A pinned invalid entry must not hide its replacement fill from Completion.
func TestCompletionReportsActiveFillOverTombstone(t *testing.T) {
	s := openTest(t, 1<<20)
	m := testMeta("completion-tomb", "original")
	held := pinnedTombstone(t, s, m, "original")
	defer held.Close()

	fresh := m
	fresh.Digest = testMeta("completion-tomb", "second").Digest
	fresh.Size = int64(len("second"))

	f, err := s.Begin(fresh)
	if err != nil {
		t.Fatalf("Begin = %v", err)
	}
	ch, ok := s.Completion(fresh)
	if !ok || ch == nil {
		t.Fatalf("Completion during active re-fill = (%v, %v), want active channel", ch, ok)
	}
	select {
	case <-ch:
		t.Fatal("active fill reported complete")
	default:
	}
	if _, err := io.WriteString(f, "second"); err != nil {
		t.Fatal(err)
	}
	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
	default:
		t.Fatal("commit did not notify Completion waiter")
	}
}

// Deny directory removal on the final unpin, then restore permissions so Begin
// can reclaim the retained entry before staging its replacement.
func TestBeginReclaimsUnpinnedTombstone(t *testing.T) {
	requireUnprivilegedTest(t)
	s := openTest(t, 1<<20)
	m := testMeta("reclaim", "original")

	put(t, s, m, "original")
	baseline := s.Stats().Bytes // one live entry's charge
	tombstoneCharge := m.Size + metadataReservation
	held, err := s.Get(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.dir, m.Key, "body")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(context.Background(), m); err != ErrMiss {
		t.Fatalf("invalidating Get = %v", err)
	}

	// Deny removal at the final unpin so the tombstone stays mapped with pins==0.
	if err := os.Chmod(s.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	e := s.entries[m.Key]
	if e == nil || !e.invalid || e.pins != 0 {
		t.Fatalf("expected mapped unpinned tombstone after denied unpin removal, got %+v", e)
	}
	if st := s.Stats(); st.Bytes != baseline {
		t.Fatalf("Stats.Bytes after denied unpin = %d, want %d (charge still held)", st.Bytes, baseline)
	}
	if baseline != tombstoneCharge {
		t.Fatalf("baseline %d != tombstone charge %d", baseline, tombstoneCharge)
	}

	// Restore write permission; Begin must now reclaim the invalid entry.
	if err := os.Chmod(s.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fresh := m
	fresh.Digest = testMeta("reclaim", "renewed").Digest
	fresh.Size = int64(len("renewed"))
	f, err := s.Begin(fresh)
	if err != nil {
		t.Fatalf("Begin after unpinned tombstone = %v", err)
	}
	if s.entries[m.Key] != nil {
		t.Fatal("Begin did not detach the invalid entry before staging the fill")
	}
	if _, err := io.WriteString(f, "renewed"); err != nil {
		t.Fatal(err)
	}
	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}
	wantBytes := fresh.Size + metadataReservation
	if st := s.Stats(); st.Bytes != wantBytes {
		t.Fatalf("Stats.Bytes = %d, want %d (baseline was %d; tombstone must not leak)", st.Bytes, wantBytes, baseline)
	}
	read(t, s, fresh, "renewed")
}

// Exercise concurrent close and replacement publication repeatedly: an old
// reader's final unpin must never remove the newly published directory.
func TestReadmitCommitReaderCloseRace(t *testing.T) {
	for i := 0; i < 200; i++ {
		s, err := Open(filepath.Join(t.TempDir(), "cache"), 1<<20)
		if err != nil {
			t.Fatalf("iter %d: open = %v", i, err)
		}
		m := testMeta("race-readmit", "original")
		held := pinnedTombstone(t, s, m, "original")

		fresh := m
		fresh.Digest = testMeta("race-readmit", "replacement").Digest
		fresh.Size = int64(len("replacement"))

		f, err := s.Begin(fresh)
		if err != nil {
			t.Fatalf("iter %d: Begin = %v", i, err)
		}
		if _, err := io.WriteString(f, "replacement"); err != nil {
			t.Fatalf("iter %d: write = %v", i, err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		start := make(chan struct{})
		var commitErr error
		go func() {
			defer wg.Done()
			<-start
			commitErr = f.Commit()
		}()
		go func() {
			defer wg.Done()
			<-start
			_ = held.Close()
		}()
		close(start)
		wg.Wait()

		if commitErr != nil {
			t.Fatalf("iter %d: Commit = %v", i, commitErr)
		}
		// The committed entry must be intact regardless of the interleaving.
		read(t, s, fresh, "replacement")
		wantBytes := fresh.Size + metadataReservation
		if st := s.Stats(); st.Bytes != wantBytes {
			t.Fatalf("iter %d: Stats.Bytes = %d, want %d", i, st.Bytes, wantBytes)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("iter %d: Close = %v", i, err)
		}
	}
}
