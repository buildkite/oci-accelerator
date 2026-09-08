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

// assertBeginAdmitsPinnedTombstone deterministically reproduces the concurrent
// corrupt-reader window on an isolated store: one reader keeps a corrupt entry
// pinned while a second Get invalidates it, leaving an invalid+pinned tombstone.
// A Begin for that key during the window must be admitted for re-fill rather than
// refused with AdmissionExisting, which is the thundering-herd wedge this fix
// removes. Kept on a dedicated store so it does not perturb the caller's slot
// and eviction accounting.
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
// the third check, after one hash chunk, so every reader has pinned and started
// hashing before any can finish; ordinary context cancellation interrupts half.
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

func TestConcurrentFirstVerificationCancellation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	body := string(bytes.Repeat([]byte("verified"), 128*1024))
	m := testMeta("verification-cancellation", body)
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
	defer s.Close()
	const clients = 8
	arrived, release := make(chan struct{}, clients), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	results := make(chan error, clients)
	cancels := make([]context.CancelFunc, clients)
	for i := range clients {
		ctx, cancel := context.WithCancel(context.Background())
		cancels[i] = cancel
		defer cancel()
		ctx = &verificationBarrierContext{Context: ctx, arrived: arrived, release: release}
		go func() {
			r, err := s.Get(ctx, m)
			if i%2 == 0 {
				if r != nil {
					r.Close()
				}
				if err != ErrMiss {
					err = errors.New("canceled verification did not miss")
				} else {
					err = nil
				}
			} else if err == nil {
				got, readErr := io.ReadAll(r)
				r.Close()
				err = readErr
				if string(got) != body {
					err = errors.New("verified body changed")
				}
			}
			results <- err
		}()
	}
	for range clients {
		select {
		case <-arrived:
		case <-time.After(3 * time.Second):
			t.Fatal("readers did not enter concurrent first verification")
		}
	}
	for i := 0; i < clients; i += 2 {
		cancels[i]()
	}
	releaseOnce.Do(func() { close(release) })
	for range clients {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	read(t, s, m, body) // Cancellation must not invalidate otherwise good content.
	if err := s.Close(); err != nil {
		t.Fatalf("verification leaked pins: %v", err)
	}
}
