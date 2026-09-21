package cache

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func awaitBarrier(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func awaitResult[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

func assertHitProgress(t *testing.T, s *Store, meta Metadata, want string) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		r, err := s.Get(context.Background(), meta)
		if err == nil {
			var b []byte
			b, err = io.ReadAll(r)
			if err == nil && string(b) != want {
				err = errors.New("unexpected bytes: " + string(b))
			}
			err = errors.Join(err, r.Close())
		}
		done <- err
	}()
	if err := awaitResult(t, done, "unrelated cache hit"); err != nil {
		t.Fatal(err)
	}
	statsDone := make(chan Stats, 1)
	go func() { statsDone <- s.Stats() }()
	_ = awaitResult(t, statsDone, "Stats")
}

func TestBlockedStagingDoesNotBlockHitOrStatsAndDelaysClose(t *testing.T) {
	s := openTest(t, 1<<20)
	hot := testMeta("hot-during-staging", "body")
	put(t, s, hot, "body")

	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	realMkdirTemp := s.mkdirTemp
	s.mkdirTemp = func(dir, pattern string) (string, error) {
		close(entered)
		<-release
		return realMkdirTemp(dir, pattern)
	}
	beginDone := make(chan error, 1)
	go func() {
		_, err := s.Begin(testMeta("blocked-staging", "body"))
		beginDone <- err
	}()
	awaitBarrier(t, entered, "staging")

	statsDone := make(chan Stats, 1)
	go func() { statsDone <- s.Stats() }()
	if st := awaitResult(t, statsDone, "Stats during staging"); st.Reserved != metadataReservation+4 || st.Entries != 1 || st.Pending != 1 {
		t.Fatalf("staging claim accounting = %+v", st)
	}
	assertHitProgress(t, s, hot, "body")
	closeDone := make(chan error, 1)
	go func() { closeDone <- s.Close() }()
	closed := make(chan struct{})
	go func() {
		for {
			s.mu.Lock()
			isClosed := s.closed
			s.mu.Unlock()
			if isClosed {
				close(closed)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	awaitBarrier(t, closed, "Close to mark store closed")
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned during staging: %v", err)
	default:
	}
	close(release)
	if err := awaitResult(t, beginDone, "blocked Begin shutdown"); err == nil {
		t.Fatal("Begin succeeded after Close")
	} else {
		var admission *AdmissionError
		if !errors.As(err, &admission) || admission.Reason != AdmissionClosed {
			t.Fatalf("Begin = %v, want closed admission", err)
		}
	}
	if err := awaitResult(t, closeDone, "Close after staging release"); err != nil {
		t.Fatal(err)
	}
	if st := s.Stats(); st.Reserved != 0 || st.Fills != 0 || st.CleanupSlots != 0 {
		t.Fatalf("final accounting = %+v", st)
	}
	if _, err := os.Stat(s.dir); err != nil {
		t.Fatal(err)
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

func TestCloseRetainsLockWhileInvalidReaderCleanupBlocked(t *testing.T) {
	s := openTest(t, 1<<20)
	meta := testMeta("invalid-reader-close", "body")
	put(t, s, meta, "body")
	r, err := s.Get(context.Background(), meta)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	r.entry.invalid = true
	s.mu.Unlock()

	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	realRemoveAll := s.removeAll
	target := filepath.Join(s.dir, meta.Key)
	s.removeAll = func(path string) error {
		if path == target {
			close(entered)
			<-release
		}
		return realRemoveAll(path)
	}
	readerDone := make(chan error, 1)
	go func() { readerDone <- r.Close() }()
	awaitBarrier(t, entered, "invalid reader cleanup")
	closeDone := make(chan error, 1)
	go func() { closeDone <- s.Close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("Close released lock during cleanup: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if other, err := OpenWithLimits(s.dir, Limits{MaxBytes: 1 << 20, MaxFills: 1, MaxEntries: 1, FillTimeout: time.Minute}); err == nil {
		_ = other.Close()
		t.Fatal("cache reopened while invalid-reader cleanup was blocked")
	} else if !errors.Is(err, ErrLockConflict) {
		t.Fatalf("reopen = %v, want lock conflict", err)
	}
	close(release)
	if err := awaitResult(t, readerDone, "invalid reader cleanup"); err != nil {
		t.Fatal(err)
	}
	if err := awaitResult(t, closeDone, "Close after invalid reader cleanup"); err != nil {
		t.Fatal(err)
	}
}

func TestPausedDeferredCleanupDoesNotBlockHitsAndDelaysClose(t *testing.T) {
	const failedCharge = metadataReservation
	s := openTest(t, 1<<20)
	hot := testMeta("hot-during-cleanup", "hot bytes")
	put(t, s, hot, "hot bytes")

	realMkdirTemp, realRemoveAll := s.mkdirTemp, s.removeAll
	var failedDir string
	s.mkdirTemp = func(parent, pattern string) (string, error) {
		var err error
		failedDir, err = realMkdirTemp(parent, pattern)
		if err == nil {
			err = os.Mkdir(filepath.Join(failedDir, "body"), 0o700)
		}
		return failedDir, err
	}
	s.removeAll = func(path string) error {
		if path == failedDir {
			return errors.New("retain cleanup")
		}
		return realRemoveAll(path)
	}
	if _, err := s.Begin(testMeta("failed-staging", "")); err == nil {
		t.Fatal("Begin unexpectedly succeeded")
	}

	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	s.mkdirTemp = realMkdirTemp
	s.removeAll = func(path string) error {
		if path == failedDir {
			close(entered)
			<-release
		}
		return realRemoveAll(path)
	}
	beginDone := make(chan error, 1)
	go func() {
		f, err := s.Begin(testMeta("after-cleanup", "x"))
		if f != nil {
			err = errors.Join(err, f.Abort())
		}
		beginDone <- err
	}()
	awaitBarrier(t, entered, "deferred cleanup removal")
	assertHitProgress(t, s, hot, "hot bytes")
	if st := s.Stats(); st.CleanupBytes != failedCharge || st.CleanupSlots != 1 || st.Bytes != failedCharge+metadataReservation+int64(len("hot bytes")) {
		t.Fatalf("paused cleanup accounting = %+v", st)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- s.Close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned during cleanup: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if other, err := openDefault(s.dir, 1<<20); err == nil {
		_ = other.Close()
		t.Fatal("cache reopened during cleanup")
	} else if !errors.Is(err, ErrLockConflict) {
		t.Fatalf("reopen = %v, want lock conflict", err)
	}
	close(release)
	_ = awaitResult(t, beginDone, "admission after cleanup")
	if err := awaitResult(t, closeDone, "Close after cleanup"); err != nil {
		t.Fatal(err)
	}
}

func TestPausedBeginTombstoneCleanupDoesNotBlockHitsAndDelaysClose(t *testing.T) {
	s := openTest(t, 1<<20)
	hot := testMeta("hot-during-supersession", "hot")
	victim := testMeta("paused-tombstone", "old")
	put(t, s, hot, "hot")
	put(t, s, victim, "old")
	r, err := s.Get(context.Background(), victim)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = r.Close()
		}
	})
	s.mu.Lock()
	r.entry.invalid = true
	s.mu.Unlock()
	realRemoveAll := s.removeAll
	s.removeAll = func(string) error { return errors.New("leave tombstone") }
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true

	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	target := filepath.Join(s.dir, victim.Key)
	s.removeAll = func(path string) error {
		if path == target {
			close(entered)
			<-release
		}
		return realRemoveAll(path)
	}
	beginDone := make(chan error, 1)
	fresh := victim
	fresh.Digest = testMeta("unused", "new").Digest
	fresh.Size = int64(len("new"))
	go func() {
		_, err := s.Begin(fresh)
		beginDone <- err
	}()
	awaitBarrier(t, entered, "tombstone supersession")
	assertHitProgress(t, s, hot, "hot")
	wantBytes := 2*metadataReservation + int64(len("hot")+len("old"))
	if st := s.Stats(); st.Bytes != wantBytes || st.Entries != 2 {
		t.Fatalf("paused supersession accounting = %+v", st)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- s.Close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned during supersession: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if other, err := openDefault(s.dir, 1<<20); err == nil {
		_ = other.Close()
		t.Fatal("cache reopened during supersession")
	} else if !errors.Is(err, ErrLockConflict) {
		t.Fatalf("reopen = %v, want lock conflict", err)
	}
	close(release)
	if err := awaitResult(t, beginDone, "superseding Begin shutdown"); err == nil {
		t.Fatal("Begin succeeded after Close")
	}
	if err := awaitResult(t, closeDone, "Close after supersession"); err != nil {
		t.Fatal(err)
	}
}

func TestPausedCommitTombstoneSupersessionDoesNotBlockHitsAndDelaysClose(t *testing.T) {
	s := openTest(t, 1<<20)
	hot := testMeta("hot-during-commit-supersession", "hot bytes")
	victim := testMeta("commit-tombstone", "old bytes")
	put(t, s, hot, "hot bytes")
	held := pinnedTombstone(t, s, victim, "old bytes")
	heldClosed := false
	t.Cleanup(func() {
		if !heldClosed {
			_ = held.Close()
		}
	})

	fresh := victim
	fresh.Digest = testMeta("unused", "new exact bytes").Digest
	fresh.Size = int64(len("new exact bytes"))
	f, err := s.Begin(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(f, "new exact bytes"); err != nil {
		t.Fatal(err)
	}

	entered, release := make(chan struct{}), make(chan struct{})
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
	})
	realRemoveAll := s.removeAll
	target := filepath.Join(s.dir, victim.Key)
	s.removeAll = func(path string) error {
		if path == target {
			close(entered)
			<-release
		}
		return realRemoveAll(path)
	}
	commitDone := make(chan error, 1)
	go func() { commitDone <- f.Commit() }()
	awaitBarrier(t, entered, "Commit tombstone supersession")

	assertHitProgress(t, s, hot, "hot bytes")
	oldCharge := metadataReservation + victim.Size
	freshCharge := metadataReservation + fresh.Size
	hotCharge := metadataReservation + hot.Size
	if st := s.Stats(); st.Bytes != hotCharge+oldCharge || st.Reserved != freshCharge || st.Entries != 2 || st.Fills != 1 || st.Pending != 0 || st.Readers != 1 {
		t.Fatalf("paused Commit accounting = %+v", st)
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- s.Close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned during Commit: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	released = true
	if err := awaitResult(t, commitDone, "Commit after tombstone release"); err != nil {
		t.Fatal(err)
	}
	if err := awaitResult(t, closeDone, "Close after Commit"); !errors.Is(err, ErrActiveReaders) {
		t.Fatalf("Close = %v, want ErrActiveReaders", err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	heldClosed = true
	if st := s.Stats(); st.Bytes != hotCharge+freshCharge || st.Reserved != 0 || st.Entries != 2 || st.Fills != 0 || st.Pending != 0 || st.Readers != 0 {
		t.Fatalf("reclaimed accounting = %+v", st)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openDefault(s.dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	read(t, reopened, fresh, "new exact bytes")
}
