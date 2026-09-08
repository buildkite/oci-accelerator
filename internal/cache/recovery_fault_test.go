package cache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const crashChildMode = "CACHE_CRASH_CHILD_MODE"

func TestCacheCrashChild(t *testing.T) {
	mode := os.Getenv(crashChildMode)
	if mode == "" {
		t.Skip("subprocess helper")
	}
	dir, ready := os.Getenv("CACHE_CRASH_DIR"), os.Getenv("CACHE_CRASH_READY")
	body := []byte("completed bytes survive exactly")
	meta := testMeta("bounded4c-complete", string(body))
	s, err := Open(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}

	switch mode {
	case "active-real-fill":
		f, err := s.Begin(testMeta("bounded4c-partial", "partial body"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte("partial")); err != nil {
			t.Fatal(err)
		}
	case "committed-real-api":
		f, err := s.Begin(meta)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(body); err != nil {
			t.Fatal(err)
		}
		if err := f.Commit(); err != nil {
			t.Fatal(err)
		}
	case "staged-incomplete-published-pair":
		// Staged boundary: this is equivalent disk state, not an interrupted Commit.
		dest := filepath.Join(dir, meta.Key)
		if err := os.Mkdir(dest, 0o700); err != nil {
			t.Fatal(err)
		}
		writeSyncedRecoveryFile(t, filepath.Join(dest, "body"), body[:len(body)-1])
		raw, err := encodeMeta(meta)
		if err != nil {
			t.Fatal(err)
		}
		writeSyncedRecoveryFile(t, filepath.Join(dest, "meta"), raw)
	case "staged-complete-renamed-unindexed":
		// Staged boundary: synced body/meta and rename, but no in-memory indexing.
		tmp, err := os.MkdirTemp(dir, ".fill-")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(tmp, 0o700); err != nil {
			t.Fatal(err)
		}
		writeSyncedRecoveryFile(t, filepath.Join(tmp, "body"), body)
		raw, err := encodeMeta(meta)
		if err != nil {
			t.Fatal(err)
		}
		writeSyncedRecoveryFile(t, filepath.Join(tmp, "meta"), raw)
		if err := syncDirectory(tmp); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(dir, meta.Key)); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown child mode %q", mode)
	}
	if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func writeSyncedRecoveryFile(t *testing.T, path string, body []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		_, err = f.Write(body)
	}
	if err == nil {
		err = f.Sync()
	}
	if f != nil {
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestProcessCrashRecovery(t *testing.T) {
	body := "completed bytes survive exactly"
	complete := testMeta("bounded4c-complete", body)
	partial := testMeta("bounded4c-partial", "partial body")
	cases := []struct {
		mode  string
		query Metadata
		hit   bool
	}{
		{"active-real-fill", partial, false},
		{"committed-real-api", complete, true},
		{"staged-incomplete-published-pair", complete, false},
		{"staged-complete-renamed-unindexed", complete, true},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "cache")
			ready := filepath.Join(filepath.Dir(dir), "ready")
			cmd := exec.Command(os.Args[0], "-test.run=^TestCacheCrashChild$")
			cmd.Env = append(os.Environ(), crashChildMode+"="+tc.mode, "CACHE_CRASH_DIR="+dir, "CACHE_CRASH_READY="+ready)
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			defer func() {
				if !waited {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			}()
			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, err := os.Stat(ready); err == nil {
					break
				}
				if time.Now().After(deadline) {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
					waited = true
					t.Fatalf("child did not reach publication boundary; output:\n%s", output.String())
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err == nil {
				waited = true
				t.Fatal("killed child exited successfully")
			}
			waited = true

			s, err := Open(dir, 1<<20) // proves the killed process released its flock.
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			fills, err := filepath.Glob(filepath.Join(dir, ".fill-*"))
			if err != nil || len(fills) != 0 {
				t.Fatalf("temporary fills after recovery: %v, %v", fills, err)
			}
			r, err := s.Get(context.Background(), tc.query)
			if !tc.hit {
				if err != ErrMiss {
					t.Fatalf("incomplete boundary Get = %v", err)
				}
				if st := s.Stats(); st.Entries != 0 || st.Fills != 0 || st.Reserved != 0 || st.Bytes != 0 {
					t.Fatalf("failed recovery stats = %+v", st)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(r)
			_ = r.Close()
			if err != nil || string(got) != body {
				t.Fatalf("recovered bytes = %q, %v", got, err)
			}
			if st := s.Stats(); st.Entries != 1 || st.Fills != 0 || st.Reserved != 0 {
				t.Fatalf("successful recovery stats = %+v", st)
			}
		})
	}
}

// assertBeginAdmitsPinnedTombstone checks refill admission while an invalid
// entry remains reader-pinned. Its isolated store preserves the caller's
// capacity and eviction accounting.
func assertBeginAdmitsPinnedTombstone(t *testing.T) {
	t.Helper()
	s := openTest(t, 1<<20)
	corrupt := testMeta("corrupt-window", "same")
	put(t, s, corrupt, "same")
	held, err := s.Get(context.Background(), corrupt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.dir, corrupt.Key, "body")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(context.Background(), corrupt); err != ErrMiss {
		t.Fatalf("invalidating Get during pinned window = %v", err)
	}
	if e := s.entries[corrupt.Key]; e == nil || !e.invalid || e.pins != 1 {
		t.Fatalf("expected invalid pinned tombstone, got %+v", e)
	}
	refill, err := s.Begin(corrupt)
	if err != nil {
		t.Fatalf("Begin over invalid pinned tombstone = %v, want admission", err)
	}
	if err := refill.Abort(); err != nil {
		t.Fatal(err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, corrupt.Key)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tombstone not reclaimed after abort and unpin: %v", err)
	}
}

func TestConcurrentRestartCorruption(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	bad, valid := testMeta("bad", "same"), testMeta("valid", "good")
	s, err := OpenWithLimits(dir, Limits{MaxBytes: 1 << 20, MaxFills: 4, MaxEntries: 2, FillTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	put(t, s, bad, "same")
	put(t, s, valid, "good")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, bad.Key, "body"), []byte("evil"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err = OpenWithLimits(dir, Limits{MaxBytes: 1 << 20, MaxFills: 4, MaxEntries: 2, FillTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	storeClosed := false
	defer func() {
		if !storeClosed {
			_ = s.Close()
		}
	}()

	const n = 24
	start := make(chan struct{})
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			<-start
			r, err := s.Get(context.Background(), bad)
			if r != nil {
				_ = r.Close()
			}
			results <- err
		}()
	}
	close(start)
	for i := 0; i < n; i++ {
		if err := <-results; err != ErrMiss {
			t.Fatalf("corrupt reader %d = %v", i, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, bad.Key)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("corrupt entry not cleaned after final unpin: %v", err)
	}

	assertBeginAdmitsPinnedTombstone(t)

	other := testMeta("other", "stay")
	put(t, s, other, "stay")
	otherReader, err := s.Get(context.Background(), other)
	if err != nil {
		t.Fatal(err)
	}

	type readResult struct {
		body string
		err  error
	}
	validResults := make(chan readResult, n)
	var wg sync.WaitGroup
	gate := make(chan struct{})
	released := false
	otherClosed := false
	defer func() {
		if !released {
			close(gate)
			wg.Wait()
		}
		if !otherClosed {
			_ = otherReader.Close()
		}
	}()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.Get(context.Background(), valid)
			if err != nil {
				validResults <- readResult{err: err}
				return
			}
			body, readErr := io.ReadAll(r)
			validResults <- readResult{body: string(body), err: readErr}
			<-gate
			_ = r.Close()
		}()
	}
	var validErr error
	for i := 0; i < n; i++ {
		result := <-validResults
		if result.err != nil || result.body != "good" {
			validErr = errors.Join(validErr, errors.New("valid reader returned incorrect bytes"), result.err)
		}
	}
	if validErr != nil {
		close(gate)
		released = true
		wg.Wait()
		_ = otherReader.Close()
		otherClosed = true
		t.Fatal(validErr)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Get(canceled, valid); err != ErrMiss {
		t.Fatalf("canceled Get = %v", err)
	}
	next := testMeta("next", "next")
	if _, err := s.Begin(next); !errors.Is(err, ErrBypass) {
		t.Fatalf("admission with all entry slots pinned = %v", err)
	}
	close(gate)
	released = true
	wg.Wait()
	if err := otherReader.Close(); err != nil {
		t.Fatal(err)
	}
	otherClosed = true
	put(t, s, next, "next")
	read(t, s, next, "next")
	if st := s.Stats(); st.Fills != 0 || st.Reserved != 0 {
		t.Fatalf("final stats = %+v", st)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("final close (leaked reader pin or fill): %v", err)
	}
	storeClosed = true
}

// Err is first checked at Get ingress, then between verification reads. Block
// the third check, after one hash chunk, to hold the elected verifier.
type verificationBarrierContext struct {
	context.Context
	checks  int
	arrived chan<- struct{}
	release <-chan struct{}
}

func (c *verificationBarrierContext) Err() error {
	c.checks++
	if c.checks == 3 {
		c.arrived <- struct{}{}
		<-c.release
	}
	return c.Context.Err()
}

type waiterContext struct {
	context.Context
	waiting chan<- struct{}
	once    sync.Once
	checks  int
}

func (c *waiterContext) Err() error {
	c.checks++
	return c.Context.Err()
}

func (c *waiterContext) Done() <-chan struct{} {
	c.once.Do(func() { c.waiting <- struct{}{} })
	return c.Context.Done()
}

type getResult struct {
	body string
	err  error
}

func awaitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func awaitGet(t *testing.T, ch <-chan getResult, what string) getResult {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return getResult{}
	}
}

func asyncGet(s *Store, ctx context.Context, m Metadata) <-chan getResult {
	result := make(chan getResult, 1)
	go func() {
		r, err := s.Get(ctx, m)
		if err == nil {
			body, readErr := io.ReadAll(r)
			closeErr := r.Close()
			err = errors.Join(readErr, closeErr)
			result <- getResult{body: string(body), err: err}
			return
		}
		result <- getResult{err: err}
	}()
	return result
}

func openUnverifiedTestEntry(t *testing.T, key string) (*Store, Metadata, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cache")
	body := string(bytes.Repeat([]byte("verified"), 128*1024))
	m := testMeta(key, body)
	s, err := Open(dir, 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	put(t, s, m, body)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	return s, m, body
}

func TestFirstVerificationSharedByWaiters(t *testing.T) {
	s, m, body := openUnverifiedTestEntry(t, "verification-shared")
	defer s.Close()
	arrived, release := make(chan struct{}, 1), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	owner := asyncGet(s, &verificationBarrierContext{Context: context.Background(), arrived: arrived, release: release}, m)
	awaitSignal(t, arrived, "owner verification")
	const clients = 8
	waiting := make(chan struct{}, clients)
	contexts := make([]*waiterContext, clients)
	results := make([]<-chan getResult, clients)
	for i := range clients {
		contexts[i] = &waiterContext{Context: context.Background(), waiting: waiting}
		results[i] = asyncGet(s, contexts[i], m)
	}
	for range clients {
		awaitSignal(t, waiting, "shared waiter")
	}
	if st := s.Stats(); st.Readers != 1 {
		t.Fatalf("verification readers = %d, want one owner", st.Readers)
	}
	releaseOnce.Do(func() { close(release) })
	if got := awaitGet(t, owner, "owner"); got.err != nil || got.body != body {
		t.Fatalf("owner = (%d bytes, %v)", len(got.body), got.err)
	}
	for i, result := range results {
		if got := awaitGet(t, result, "shared waiter"); got.err != nil || got.body != body {
			t.Fatalf("waiter = (%d bytes, %v)", len(got.body), got.err)
		}
		// Only ingress and the post-notification recheck; hashing calls Err
		// again for each body chunk and must not occur in these waiters.
		if contexts[i].checks != 2 {
			t.Errorf("waiter %d context checks = %d, want 2 (no hashing)", i, contexts[i].checks)
		}
	}
	if st := s.Stats(); st.Readers != 0 {
		t.Fatalf("verification leaked pins: %+v", st)
	}
}

func TestFirstVerificationWaiterCancellation(t *testing.T) {
	s, m, body := openUnverifiedTestEntry(t, "verification-waiter-cancel")
	defer s.Close()
	arrived, release := make(chan struct{}, 1), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	owner := asyncGet(s, &verificationBarrierContext{Context: context.Background(), arrived: arrived, release: release}, m)
	awaitSignal(t, arrived, "owner verification")
	waiting := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	waiter := asyncGet(s, &waiterContext{Context: ctx, waiting: waiting}, m)
	awaitSignal(t, waiting, "waiter selection")
	if st := s.Stats(); st.Readers != 1 {
		t.Fatalf("concurrent verification readers = %d, want one owner", st.Readers)
	}
	cancel()
	if got := awaitGet(t, waiter, "canceled waiter"); got.err != ErrMiss {
		t.Fatalf("canceled waiter = %v, want ErrMiss", got.err)
	}
	close(release)
	if got := awaitGet(t, owner, "owner"); got.err != nil || got.body != body {
		t.Fatalf("owner = (%d bytes, %v)", len(got.body), got.err)
	}
	if st := s.Stats(); st.Readers != 0 {
		t.Fatalf("verification leaked pins: %+v", st)
	}
}

func TestFirstVerificationCancellationPermitsTakeover(t *testing.T) {
	s, m, body := openUnverifiedTestEntry(t, "verification-takeover")
	defer s.Close()
	ownerArrived, ownerRelease := make(chan struct{}, 1), make(chan struct{})
	defer func() {
		select {
		case <-ownerRelease:
		default:
			close(ownerRelease)
		}
	}()
	ownerCtx, cancelOwner := context.WithCancel(context.Background())
	owner := asyncGet(s, &verificationBarrierContext{Context: ownerCtx, arrived: ownerArrived, release: ownerRelease}, m)
	awaitSignal(t, ownerArrived, "owner verification")
	waiting := make(chan struct{}, 1)
	takeoverArrived, takeoverRelease := make(chan struct{}, 1), make(chan struct{})
	defer func() {
		select {
		case <-takeoverRelease:
		default:
			close(takeoverRelease)
		}
	}()
	takeoverCtx := &verificationBarrierContext{Context: context.Background(), arrived: takeoverArrived, release: takeoverRelease}
	takeover := asyncGet(s, &waiterContext{Context: takeoverCtx, waiting: waiting}, m)
	awaitSignal(t, waiting, "takeover waiter")
	if st := s.Stats(); st.Readers != 1 {
		t.Fatalf("concurrent verification readers = %d, want one owner", st.Readers)
	}
	cancelOwner()
	close(ownerRelease)
	if got := awaitGet(t, owner, "canceled owner"); got.err != ErrMiss {
		t.Fatalf("canceled owner = %v, want ErrMiss", got.err)
	}
	awaitSignal(t, takeoverArrived, "takeover verification")
	close(takeoverRelease)
	if got := awaitGet(t, takeover, "takeover"); got.err != nil || got.body != body {
		t.Fatalf("takeover = (%d bytes, %v)", len(got.body), got.err)
	}
	if st := s.Stats(); st.Readers != 0 {
		t.Fatalf("takeover leaked pins: %+v", st)
	}
}

func TestFirstVerificationCorruptionWakesWaiters(t *testing.T) {
	s, m, _ := openUnverifiedTestEntry(t, "verification-corrupt-waiters")
	defer s.Close()
	arrived, release := make(chan struct{}, 1), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	owner := asyncGet(s, &verificationBarrierContext{Context: context.Background(), arrived: arrived, release: release}, m)
	awaitSignal(t, arrived, "owner verification")
	waiting := make(chan struct{}, 1)
	waiter := asyncGet(s, &waiterContext{Context: context.Background(), waiting: waiting}, m)
	awaitSignal(t, waiting, "corruption waiter")
	if st := s.Stats(); st.Readers != 1 {
		t.Fatalf("concurrent verification readers = %d, want one owner", st.Readers)
	}
	f, err := os.OpenFile(filepath.Join(s.dir, m.Key, "body"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.WriteAt([]byte("corrupt"), 256*1024)
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		t.Fatal(err)
	}
	close(release)
	if got := awaitGet(t, owner, "corrupt owner"); got.err != ErrMiss {
		t.Fatalf("corrupt owner = %v, want ErrMiss", got.err)
	}
	if got := awaitGet(t, waiter, "corruption waiter"); got.err != ErrMiss {
		t.Fatalf("corruption waiter = %v, want ErrMiss", got.err)
	}
	if st := s.Stats(); st.Readers != 0 {
		t.Fatalf("corruption leaked pins: %+v", st)
	}
}

func TestCloseRejectsFirstVerificationWaiters(t *testing.T) {
	s, m, body := openUnverifiedTestEntry(t, "verification-close-waiters")
	arrived, release := make(chan struct{}, 1), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	owner := asyncGet(s, &verificationBarrierContext{Context: context.Background(), arrived: arrived, release: release}, m)
	awaitSignal(t, arrived, "owner verification")
	waiting := make(chan struct{}, 1)
	waiter := asyncGet(s, &waiterContext{Context: context.Background(), waiting: waiting}, m)
	awaitSignal(t, waiting, "shutdown waiter")
	if st := s.Stats(); st.Readers != 1 {
		t.Fatalf("concurrent verification readers = %d, want one owner", st.Readers)
	}
	if err := s.Close(); err == nil {
		t.Fatal("Close with verifier unexpectedly succeeded")
	}
	close(release)
	if got := awaitGet(t, owner, "owner after Close"); got.err != nil || got.body != body {
		t.Fatalf("owner after Close = (%d bytes, %v)", len(got.body), got.err)
	}
	if got := awaitGet(t, waiter, "waiter after Close"); got.err != ErrMiss {
		t.Fatalf("waiter after Close = %v, want ErrMiss", got.err)
	}
	if st := s.Stats(); st.Readers != 0 {
		t.Fatalf("shutdown leaked pins: %+v", st)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("final Close: %v", err)
	}
}
