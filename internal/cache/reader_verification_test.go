package cache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type getResult struct {
	body string
	err  error
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

func TestFirstVerificationSharedByWaiters(t *testing.T) {
	s, m, body := openUnverifiedTestEntry(t, "verification-shared")
	defer s.Close()
	arrived, release := make(chan struct{}, 1), make(chan struct{})
	var verifications atomic.Int32
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	owner := asyncGet(s, context.Background(), m, func() {
		verifications.Add(1)
		arrived <- struct{}{}
		<-release
	}, nil)
	awaitSignal(t, arrived, "owner verification")
	const clients = 8
	waiting := make(chan struct{}, clients)
	results := make([]<-chan getResult, clients)
	for i := range clients {
		results[i] = asyncGet(s, context.Background(), m, func() { verifications.Add(1) }, func() { waiting <- struct{}{} })
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
	for _, result := range results {
		if got := awaitGet(t, result, "shared waiter"); got.err != nil || got.body != body {
			t.Fatalf("waiter = (%d bytes, %v)", len(got.body), got.err)
		}
	}
	if got := verifications.Load(); got != 1 {
		t.Fatalf("verifications = %d, want one", got)
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
	owner := asyncGet(s, context.Background(), m, func() {
		arrived <- struct{}{}
		<-release
	}, nil)
	awaitSignal(t, arrived, "owner verification")
	waiting := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	waiter := asyncGet(s, ctx, m, nil, func() { waiting <- struct{}{} })
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
	var verifications atomic.Int32
	owner := asyncGet(s, ownerCtx, m, func() {
		verifications.Add(1)
		ownerArrived <- struct{}{}
		<-ownerRelease
	}, nil)
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
	takeover := asyncGet(s, context.Background(), m, func() {
		verifications.Add(1)
		takeoverArrived <- struct{}{}
		<-takeoverRelease
	}, func() { waiting <- struct{}{} })
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
	if got := verifications.Load(); got != 2 {
		t.Fatalf("verifications = %d, want exactly two", got)
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
	owner := asyncGet(s, context.Background(), m, func() {
		arrived <- struct{}{}
		<-release
	}, nil)
	awaitSignal(t, arrived, "owner verification")
	waiting := make(chan struct{}, 1)
	waiter := asyncGet(s, context.Background(), m, nil, func() { waiting <- struct{}{} })
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
	owner := asyncGet(s, context.Background(), m, func() {
		arrived <- struct{}{}
		<-release
	}, nil)
	awaitSignal(t, arrived, "owner verification")
	waiting := make(chan struct{}, 1)
	waiter := asyncGet(s, context.Background(), m, nil, func() { waiting <- struct{}{} })
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

func openUnverifiedTestEntry(t *testing.T, key string) (*Store, Metadata, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cache")
	body := string(bytes.Repeat([]byte("verified"), 128*1024))
	m := testMeta(key, body)
	s, err := openDefault(dir, 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	put(t, s, m, body)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = openDefault(dir, 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	return s, m, body
}

func asyncGet(s *Store, ctx context.Context, m Metadata, afterVerificationRead, beforeWait func()) <-chan getResult {
	result := make(chan getResult, 1)
	go func() {
		r, err := s.get(ctx, m, afterVerificationRead, beforeWait)
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
