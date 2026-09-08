package cache

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// pinnedTombstone stores meta, opens a still-held Reader that pins the entry,
// then removes the body file and triggers a second Get. That second Get fails
// os.Open, marks the entry invalid, and unpins only its own pin, leaving an
// invalid+mapped entry that the first Reader keeps pinned. The held Reader's
// file descriptor still points at the original inode, so it continues to return
// the original bytes even though the path was unlinked.
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

// Test 1: Begin re-admits an invalidated-but-pinned entry. Previously Begin
// returned AdmissionExisting because the tombstone remained mapped. After the
// fix Begin succeeds, the held Reader still returns the OLD bytes, and after the
// new fill commits Stats.Bytes equals exactly the new entry's charge.
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

// Test 2: index integrity across the commit-overwrite. After re-admit+commit
// while the old reader is still held, s.entries[key] must already point at the
// new entry and a fresh Get must return the new bytes. Dropping the old reader
// must not double-subtract or delete the new entry.
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

	// The superseding Commit detaches the tombstone and reclaims its charge under
	// s.mu, so only the new entry's charge is accounted even while the old reader
	// is still pinned. Closing that reader must not subtract the charge again.
	wantBytes := fresh.Size + metadataReservation
	if st := s.Stats(); st.Bytes != wantBytes {
		t.Fatalf("Stats.Bytes before old-reader close = %d, want %d (new charge only)", st.Bytes, wantBytes)
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

// Test 3: Completion consistency. While the tombstone is pinned and a re-Begin
// is active for the same key, Completion must report the active new fill
// (non-nil channel, ok==true), not treat the key as absent.
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

// Test 4: C1 reclamation, genuinely exercising the Begin removal branch. To keep
// an unpinned invalid tombstone mapped past the last unpin, the store directory
// is made read-only before the final Close so unpin's os.RemoveAll fails and
// leaves the tombstone (invalid, pins==0) in the map. Restoring write permission
// lets the subsequent Begin's C1 retry succeed, dropping the slot and reclaiming
// the charge so Stats returns to baseline before the new fill's own charge.
//
// Limitation: a tombstone whose removal fails permanently (never removable) is
// not portably reproducible; that fall-through relies on Change A plus
// identity-aware unpin/eviction, covered by the pinned-tombstone tests above.
func TestBeginReclaimsUnpinnedTombstone(t *testing.T) {
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

	// Restore write permission; Begin's C1 retry must now reclaim the tombstone.
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
		t.Fatal("C1 did not detach the tombstone before staging the fill")
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

// Test 5 (regression): closing the old reader concurrently with the superseding
// Commit must never destroy the new entry's directory. The tombstone shares the
// new entry's on-disk path, so if Commit did not detach the tombstone from the
// map before renaming (leaving a window where unpin/eviction could RemoveAll the
// destination), the closing reader could delete the freshly published content.
// supersedeTombstone closes that window structurally by detaching under s.mu
// before the rename; this exercises the interleaving as a stress/regression
// guard. Run under -race to surface any reintroduced timing window.
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
