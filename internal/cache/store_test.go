package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

func TestCompletionTracksMatchingFillTerminalState(t *testing.T) {
	s := openTest(t, 1<<20)
	meta := testMeta("completion", "body")
	f, err := s.Begin(meta)
	if err != nil {
		t.Fatal(err)
	}
	done, ok := s.Completion(meta)
	if !ok {
		t.Fatal("active fill not observed")
	}
	mismatch := meta
	mismatch.MediaType = "different"
	if _, ok := s.Completion(mismatch); ok {
		t.Fatal("mismatching metadata observed")
	}
	select {
	case <-done:
		t.Fatal("active fill reported complete")
	default:
	}
	if _, err := f.Write([]byte("body")); err != nil {
		t.Fatal(err)
	}
	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	default:
		t.Fatal("commit did not notify")
	}
	if published, ok := s.Completion(meta); !ok {
		t.Fatal("published completion not observed")
	} else {
		select {
		case <-published:
		default:
			t.Fatal("published completion was not closed")
		}
	}
}

func TestAdmissionReasonsAreDistinctAndBounded(t *testing.T) {
	s, err := OpenWithLimits(filepath.Join(t.TempDir(), "cache"), Limits{MaxBytes: metadataReservation + 1, MaxFills: 1, MaxEntries: 1, FillTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	invalid := testMeta("invalid", "x")
	invalid.Key = "invalid"
	if _, err := s.Begin(invalid); !errors.Is(err, ErrBypass) {
		t.Fatal(err)
	}
	oversized := testMeta("oversized", "xx")
	if _, err := s.Begin(oversized); !errors.Is(err, ErrBypass) {
		t.Fatal(err)
	}
	accepted := testMeta("accepted", "x")
	f, err := s.Begin(accepted)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Begin(accepted); !errors.Is(err, ErrBypass) {
		t.Fatal(err)
	}
	if _, err := s.Begin(testMeta("limited", "x")); !errors.Is(err, ErrBypass) {
		t.Fatal(err)
	}
	if err := f.Abort(); err != nil {
		t.Fatal(err)
	}
	put(t, s, accepted, "x")
	reader, err := s.Get(context.Background(), accepted)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Begin(testMeta("capacity", "x")); !errors.Is(err, ErrBypass) {
		t.Fatalf("capacity admission = %v", err)
	}
	reader.Close()
	moved := s.dir + ".moved"
	if err := os.Rename(s.dir, moved); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Begin(testMeta("io-error", "x")); !errors.Is(err, ErrBypass) {
		t.Fatalf("I/O admission = %v", err)
	}
	if err := os.Rename(moved, s.dir); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Begin(testMeta("closed", "x")); !errors.Is(err, ErrBypass) {
		t.Fatal(err)
	}
	stats := s.Stats()
	for reason, want := range map[AdmissionReason]int64{AdmissionAccepted: 2, AdmissionInvalid: 1, AdmissionOversized: 1, AdmissionExisting: 1, AdmissionFillLimit: 1, AdmissionCapacity: 1, AdmissionClosed: 1, AdmissionIOError: 1} {
		if got := stats.Admissions[reason]; got != want {
			t.Errorf("admission %s = %d, want %d", reason, got, want)
		}
	}
	if len(stats.Admissions) != int(AdmissionReasonCount) {
		t.Fatalf("admission taxonomy length = %d", len(stats.Admissions))
	}
}

func TestCompletionNotifiesAbortAndClose(t *testing.T) {
	s := openTest(t, 1<<20)
	first, _ := s.Begin(testMeta("abort-completion", "a"))
	aborted, _ := s.Completion(first.meta)
	if err := first.Abort(); err != nil {
		t.Fatal(err)
	}
	<-aborted

	second, _ := s.Begin(testMeta("close-completion", "b"))
	closed, _ := s.Completion(second.meta)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	<-closed
}

func TestEntrySlotsAndFillConcurrencyAreDistinctLimits(t *testing.T) {
	t.Run("entry slots", func(t *testing.T) {
		s, err := OpenWithLimits(filepath.Join(t.TempDir(), "cache"), Limits{MaxBytes: 1 << 20, MaxFills: 3, MaxEntries: 1, FillTimeout: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		f, err := s.Begin(testMeta("a", "a"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Begin(testMeta("b", "b")); !errors.Is(err, ErrBypass) {
			t.Fatalf("second slot = %v", err)
		}
		f.Abort()
		a, b := testMeta("stored", "s"), testMeta("next", "n")
		put(t, s, a, "s")
		r, err := s.Get(context.Background(), a)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Begin(b); !errors.Is(err, ErrBypass) || s.Stats().Evictions != 0 {
			t.Fatal("pinned entry slot must refuse admission without eviction")
		}
		r.Close()
		put(t, s, b, "n")
		if st := s.Stats(); st.Entries != 1 || st.Evictions != 1 {
			t.Fatalf("entry slot eviction failed: %+v", st)
		}
	})
	t.Run("active fills", func(t *testing.T) {
		s, err := OpenWithLimits(filepath.Join(t.TempDir(), "cache"), Limits{MaxBytes: 1 << 20, MaxFills: 1, MaxEntries: 3, FillTimeout: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		put(t, s, testMeta("stored", "s"), "s")
		f, err := s.Begin(testMeta("a", "a"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Begin(testMeta("b", "b")); !errors.Is(err, ErrBypass) {
			t.Fatalf("second fill = %v", err)
		}
		read(t, s, testMeta("stored", "s"), "s")
		f.Abort()
	})
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

func TestFillTimeoutAndTerminalPathsCompleteTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, err := OpenWithLimits(filepath.Join(t.TempDir(), "cache"), Limits{MaxBytes: 1 << 20, MaxFills: 4, MaxEntries: 4, FillTimeout: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()

		expired, _ := s.Begin(testMeta("expired", "x"))
		time.Sleep(time.Minute)
		synctest.Wait()
		if stats := s.Stats(); stats.Fills != 0 || stats.Reserved != 0 {
			t.Fatalf("timeout retained reservation: %+v", stats)
		}
		select {
		case <-expired.timerDone:
		default:
			t.Fatal("expiry timer did not complete")
		}

		aborted, _ := s.Begin(testMeta("abort", "x"))
		if err := aborted.Abort(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-aborted.timerDone:
		default:
			t.Fatal("abort retained timer")
		}

		failed, _ := s.Begin(testMeta("failed", "x"))
		if _, err := failed.Write([]byte("xx")); err == nil {
			t.Fatal("oversized write succeeded")
		}
		select {
		case <-failed.timerDone:
		default:
			t.Fatal("failed write retained timer")
		}

		committed, _ := s.Begin(testMeta("commit", "x"))
		committed.Write([]byte("x"))
		if err := committed.Commit(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-committed.timerDone:
		default:
			t.Fatal("commit retained timer")
		}

		shutdown, _ := s.Begin(testMeta("shutdown", "x"))
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-shutdown.timerDone:
		default:
			t.Fatal("shutdown retained timer")
		}
		stats := s.Stats()
		if stats.FillExpiry != 1 || stats.FillAbort != 2 || stats.FillFailure != 1 || stats.FillSuccess != 1 || stats.Fills != 0 || stats.Reserved != 0 {
			t.Fatalf("terminal fill counters = %+v", stats)
		}
	})
}

func TestExpirationRacesNeverPublishOverdueFill(t *testing.T) {
	for _, first := range []string{"contended", "timer", "write", "commit", "abort"} {
		t.Run(first, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const timeout = time.Minute
				s, err := OpenWithLimits(filepath.Join(t.TempDir(), "cache"), Limits{MaxBytes: 1 << 20, MaxFills: 1, MaxEntries: 1, FillTimeout: timeout})
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				m := testMeta("expiry-race", "x")
				f, err := s.Begin(m)
				if err != nil {
					t.Fatal(err)
				}
				if n, err := f.Write([]byte("x")); n != 1 || err != nil {
					t.Fatalf("initial Write = %d, %v", n, err)
				}
				operations := map[string]func() error{
					"write": func() error {
						n, err := f.Write(nil)
						if n != 0 {
							t.Errorf("overdue Write returned %d bytes", n)
						}
						return err
					},
					"commit": f.Commit,
					"abort":  f.Abort,
				}
				results := make(map[string]error)
				if first == "contended" {
					// Hold the fill across its deadline: the timer cannot finish
					// before the competitors are launched. Do not synctest.Wait
					// here: mutex waits are not durably blocked in a bubble.
					f.mu.Lock()
					time.Sleep(timeout)
				} else {
					if first != "timer" {
						// Model a delayed timer callback without changing the fill's
						// deadline; force each operation to win in its own subtest.
						f.timer.Reset(2 * timeout)
					}
					time.Sleep(timeout)
					if first == "timer" {
						synctest.Wait()
					} else {
						results[first] = operations[first]()
						delete(operations, first)
					}
				}
				type result struct {
					operation string
					err       error
				}
				ready, start := make(chan struct{}, len(operations)), make(chan struct{})
				completed := make(chan result, len(operations))
				for name, operation := range operations {
					go func() {
						ready <- struct{}{}
						<-start
						completed <- result{name, operation()}
					}()
				}
				for range operations {
					<-ready
				}
				close(start)
				if first == "contended" {
					if f.done || time.Now().Before(f.deadline) {
						t.Error("expected an overdue, still-active fill before releasing competitors")
					}
					f.mu.Unlock()
				}
				for range operations {
					got := <-completed
					results[got.operation] = got.err
				}
				expiredResults := 0
				for name, err := range results {
					if name == "abort" {
						if err != nil {
							t.Errorf("Abort = %v, want nil", err)
						}
						continue
					}
					want := os.ErrClosed
					if name == first || first == "contended" && errors.Is(err, errFillExpired) {
						want = errFillExpired
					}
					if !errors.Is(err, want) {
						t.Errorf("%s = %v, want %v", name, err, want)
					}
					if errors.Is(err, errFillExpired) {
						expiredResults++
					}
				}
				st := s.Stats()
				if st.Fills != 0 || st.Reserved != 0 || st.Bytes != 0 || st.Entries != 0 || st.FillSuccess != 0 || st.FillFailure != 0 || st.FillExpiry+st.FillAbort != 1 || expiredResults > 1 || int64(expiredResults) > st.FillExpiry {
					t.Fatalf("overdue terminal state: %+v, expiry results=%d", st, expiredResults)
				}
				if first == "abort" && st.FillAbort != 1 || first != "abort" && first != "contended" && st.FillExpiry != 1 {
					t.Fatalf("wrong terminal winner for %s: %+v", first, st)
				}
				select {
				case <-f.timerDone:
				default:
					t.Fatal("terminal operations did not join timer")
				}
				if r, err := s.Get(context.Background(), m); err != ErrMiss {
					if r != nil {
						r.Close()
					}
					t.Fatalf("overdue entry Get = %v, want ErrMiss", err)
				}
			})
		})
	}
}

func TestFillReadRestart(t *testing.T) {
	s := openTest(t, 1<<20)
	m := testMeta("registry/repo/accept", "blob bytes")
	f, err := s.Begin(m)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(f, "blob ")
	if _, err := s.Get(context.Background(), m); err != ErrMiss {
		t.Fatal("partial body exposed")
	}
	if _, err := s.Begin(m); !errors.Is(err, ErrBypass) {
		t.Fatal("competing writer admitted")
	}
	io.WriteString(f, "bytes")
	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}
	read(t, s, m, "blob bytes")
	if stats := s.Stats(); stats.Bytes != m.Size+metadataReservation || stats.Reserved != 0 || stats.Entries != 1 {
		t.Fatalf("stats=%+v", stats)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := openDefault(s.dir, s.max)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if s2.entries[m.Key].verified {
		t.Fatal("restart skipped digest validation")
	}
	read(t, s2, m, "blob bytes")
	if !s2.entries[m.Key].verified {
		t.Fatal("not marked verified")
	}
	for _, field := range []string{"key", "size", "digest", "media"} {
		x := m
		switch field {
		case "key":
			x.Key = strings.Repeat("f", 64)
		case "size":
			x.Size++
		case "digest":
			x.Digest = "sha256:" + strings.Repeat("f", 64)
		case "media":
			x.MediaType = "text/plain"
		}
		if r, err := s2.Get(context.Background(), x); err != ErrMiss {
			if r != nil {
				r.Close()
			}
			t.Errorf("mismatch %s hit", field)
		}
	}
}
func TestFillFailuresNeverPublish(t *testing.T) {
	for _, mode := range []string{"short", "digest", "overlong", "write", "sync", "metadata", "abort", "close", "disk-full"} {
		t.Run(mode, func(t *testing.T) {
			s := openTest(t, 1<<20)
			m := testMeta("key", "abcd")
			f, err := s.Begin(m)
			if err != nil {
				t.Fatal(err)
			}
			completed, ok := s.Completion(m)
			if !ok {
				t.Fatal("active fill has no completion notification")
			}
			var terminalErr error
			wantFailures, wantAborts := int64(1), int64(0)
			switch mode {
			case "short":
				if _, err := io.WriteString(f, "ab"); err != nil {
					t.Fatal(err)
				}
				terminalErr = f.Commit()
				if terminalErr == nil || !strings.Contains(terminalErr.Error(), "cache fill verification failed") {
					t.Fatalf("Commit = %v, want verification failure", terminalErr)
				}
			case "digest":
				if _, err := io.WriteString(f, "xxxx"); err != nil {
					t.Fatal(err)
				}
				terminalErr = f.Commit()
				if terminalErr == nil || !strings.Contains(terminalErr.Error(), "cache fill verification failed") {
					t.Fatalf("Commit = %v, want verification failure", terminalErr)
				}
			case "overlong":
				_, terminalErr = io.WriteString(f, "abcde")
				if terminalErr == nil || !strings.Contains(terminalErr.Error(), "cache fill exceeds declared size") {
					t.Fatalf("Write = %v, want oversize failure", terminalErr)
				}
			case "write":
				if err := f.file.Close(); err != nil {
					t.Fatal(err)
				}
				_, terminalErr = io.WriteString(f, "abcd")
				if !errors.Is(terminalErr, os.ErrClosed) {
					t.Fatalf("Write = %v, want os.ErrClosed", terminalErr)
				}
			case "sync":
				if _, err := io.WriteString(f, "abcd"); err != nil {
					t.Fatal(err)
				}
				if err := f.file.Close(); err != nil {
					t.Fatal(err)
				}
				terminalErr = f.Commit()
				var pathErr *os.PathError
				if !errors.Is(terminalErr, os.ErrClosed) || !errors.As(terminalErr, &pathErr) || pathErr.Op != "sync" {
					t.Fatalf("Commit = %v, want a sync error on the closed file", terminalErr)
				}
			case "metadata":
				if _, err := io.WriteString(f, "abcd"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(f.dir, "meta"), 0o700); err != nil {
					t.Fatal(err)
				}
				terminalErr = f.Commit()
				if !errors.Is(terminalErr, os.ErrExist) {
					t.Fatalf("Commit = %v, want os.ErrExist", terminalErr)
				}
			case "abort":
				if _, err := io.WriteString(f, "abcd"); err != nil {
					t.Fatal(err)
				}
				terminalErr = f.Abort()
				if terminalErr != nil {
					t.Fatalf("Abort = %v", terminalErr)
				}
				wantFailures, wantAborts = 0, 1
			case "close":
				if _, err := io.WriteString(f, "abcd"); err != nil {
					t.Fatal(err)
				}
				terminalErr = s.Close()
				if terminalErr != nil {
					t.Fatalf("Close = %v", terminalErr)
				}
				wantFailures, wantAborts = 0, 1
			case "disk-full":
				// /dev/full always fails writes with ENOSPC, but it is Linux-only.
				if runtime.GOOS != "linux" {
					t.Skip("/dev/full is only available on Linux")
				}
				if err := f.file.Close(); err != nil {
					t.Fatal(err)
				}
				f.file, err = os.OpenFile("/dev/full", os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, terminalErr = io.WriteString(f, "abcd")
				if !errors.Is(terminalErr, syscall.ENOSPC) {
					t.Fatalf("Write = %v, want ENOSPC", terminalErr)
				}
			}
			if err := f.Commit(); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("Commit after terminal operation = %v, want os.ErrClosed", err)
			}
			if err := f.Abort(); err != nil {
				t.Fatalf("Abort after terminal operation = %v", err)
			}
			if !f.done {
				t.Fatal("terminal operation did not mark fill done")
			}
			select {
			case <-completed:
			default:
				t.Fatal("terminal operation did not notify completion")
			}
			select {
			case <-f.timerDone:
			default:
				t.Fatal("terminal failure retained timer")
			}
			if stats := s.Stats(); stats.Entries != 0 || stats.Fills != 0 || stats.Reserved != 0 || stats.Bytes != 0 || stats.FillFailure != wantFailures || stats.FillAbort != wantAborts || stats.FillSuccess != 0 {
				t.Fatalf("leaked %+v", stats)
			}
			names, err := os.ReadDir(s.dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(names) != 1 || names[0].Name() != ".lock" {
				t.Fatal("abandoned temp")
			}
		})
	}
}
func TestCapacityLRUPinningAndConcurrency(t *testing.T) {
	const charge = metadataReservation + 4
	s := openTest(t, 2*charge)
	a, b, c := testMeta("a", "aaaa"), testMeta("b", "bbbb"), testMeta("c", "cccc")
	put(t, s, a, "aaaa")
	put(t, s, b, "bbbb")
	s.setEntryLastLocked(s.entries[a.Key], 1)
	s.setEntryLastLocked(s.entries[b.Key], 2)
	r, err := s.Get(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	put(t, s, c, "cccc")
	if _, err := s.Get(context.Background(), b); err != ErrMiss {
		t.Fatal("did not evict unpinned LRU")
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if string(got) != "aaaa" {
		t.Fatal("evicted reader")
	}
	if s.Stats().Evictions != 1 {
		t.Fatal("missing eviction")
	}
	r1, _ := s.Get(context.Background(), a)
	r2, _ := s.Get(context.Background(), c)
	if _, err := s.Begin(testMeta("d", "dddd")); !errors.Is(err, ErrBypass) {
		t.Fatal("evicted pinned entries")
	}
	r1.Close()
	r2.Close()
	tooLarge := testMeta("big", "")
	tooLarge.Size = math.MaxInt64
	if _, err := s.Begin(tooLarge); !errors.Is(err, ErrBypass) {
		t.Fatal("overflow admitted")
	}
	s2 := openTest(t, charge)
	results := make(chan *Fill, 20)
	for i := 0; i < 20; i++ {
		go func() {
			f, _ := s2.Begin(a)
			results <- f
		}()
	}
	var admitted []*Fill
	for i := 0; i < 20; i++ {
		if f := <-results; f != nil {
			admitted = append(admitted, f)
		}
	}
	if x := s2.Stats(); x.Reserved != charge || x.Bytes != 0 {
		t.Fatalf("reservation %+v", x)
	}
	if len(admitted) != 1 {
		t.Fatalf("admitted %d competing writers", len(admitted))
	}
	admitted[0].Abort()
	if s2.Stats().Reserved != 0 {
		t.Fatal("admission leak")
	}
}
func TestRecoveryAndCorruption(t *testing.T) {
	for _, mode := range []string{"body-missing", "short", "corrupt", "meta-missing", "meta-invalid", "meta-huge", "temporary"} {
		t.Run(mode, func(t *testing.T) {
			s := openTest(t, 1<<20)
			m := testMeta("key", "abcd")
			put(t, s, m, "abcd")
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			d := filepath.Join(s.dir, m.Key)
			switch mode {
			case "body-missing":
				if err := os.Remove(filepath.Join(d, "body")); err != nil {
					t.Fatal(err)
				}
			case "short":
				if err := os.Truncate(filepath.Join(d, "body"), 2); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := os.WriteFile(filepath.Join(d, "body"), []byte("xxxx"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "meta-missing":
				if err := os.Remove(filepath.Join(d, "meta")); err != nil {
					t.Fatal(err)
				}
			case "meta-invalid":
				if err := os.WriteFile(filepath.Join(d, "meta"), []byte("invalid"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "meta-huge":
				if err := os.Truncate(filepath.Join(d, "meta"), 1<<30); err != nil {
					t.Fatal(err)
				}
			case "temporary":
				if err := os.Rename(d, filepath.Join(s.dir, ".fill-abandoned")); err != nil {
					t.Fatal(err)
				}
			}
			s2, err := openDefault(s.dir, s.max)
			if err != nil {
				t.Fatal(err)
			}
			defer s2.Close()
			if r, err := s2.Get(context.Background(), m); err != ErrMiss {
				if r != nil {
					r.Close()
				}
				t.Fatal("bad entry hit")
			}
			put(t, s2, m, "abcd")
			read(t, s2, m, "abcd")
		})
	}
}
func TestMissingLiveEntryRefillsAndCancelledVerification(t *testing.T) {
	s := openTest(t, 1<<20)
	m := testMeta("key", "abcd")
	put(t, s, m, "abcd")
	if err := os.Remove(filepath.Join(s.dir, m.Key, "body")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(context.Background(), m); err != ErrMiss {
		t.Fatal("missing body hit")
	}
	put(t, s, m, "abcd")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := openDefault(s.dir, s.max)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s2.Get(ctx, m); err != ErrMiss {
		t.Fatal("cancelled hit")
	}
	read(t, s2, m, "abcd")
}
func TestPrivateDirectoryLockAndUnknownState(t *testing.T) {
	s := openTest(t, 1<<20)
	if s2, err := openDefault(s.dir, s.max); err == nil {
		s2.Close()
		t.Fatal("second store opened")
	} else if !errors.Is(err, ErrLockConflict) {
		t.Fatalf("second store error = %v, want lock conflict", err)
	}
	m := testMeta("key", "abcd")
	put(t, s, m, "abcd")
	r, _ := s.Get(context.Background(), m)
	if s.Close() == nil {
		t.Fatal("closed with live reader")
	}
	if s2, err := openDefault(s.dir, s.max); err == nil {
		s2.Close()
		t.Fatal("released lock with reader")
	}
	r.Close()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.dir, "operator-file")
	os.WriteFile(path, []byte("keep"), 0o600)
	if s2, err := openDefault(s.dir, s.max); err == nil {
		s2.Close()
		t.Fatal("unknown state accepted")
	} else if !errors.Is(err, ErrUnsafeState) {
		t.Fatalf("unknown state error = %v, want unsafe state", err)
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, []byte("keep")) {
		t.Fatal("deleted unknown data")
	}
	os.Remove(path)
	os.Chmod(s.dir, 0o755)
	if s2, err := openDefault(s.dir, s.max); err == nil {
		s2.Close()
		t.Fatal("insecure dir accepted")
	}
	os.Chmod(s.dir, 0o700)
	link := filepath.Join(filepath.Dir(s.dir), "link")
	os.Symlink(s.dir, link)
	if s2, err := openDefault(link, s.max); err == nil {
		s2.Close()
		t.Fatal("symlink accepted")
	}
}

func TestOpenErrorCategories(t *testing.T) {
	if _, err := OpenWithLimits("", Limits{}); !errors.Is(err, ErrInvalidLimits) {
		t.Fatalf("invalid limits error = %v", err)
	}
	missingParent := filepath.Join(t.TempDir(), "missing-parent", "cache")
	if _, err := openDefault(missingParent, 1<<20); !errors.Is(err, ErrFilesystem) {
		t.Fatalf("filesystem error = %v", err)
	}
}
func TestRestrictiveUmaskAndTinyCapacity(t *testing.T) {
	root := t.TempDir()
	previous := syscall.Umask(0o777)
	defer syscall.Umask(previous)
	s, err := openDefault(filepath.Join(root, "cache"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	m := testMeta("key", "abcd")
	put(t, s, m, "abcd")
	s.Close()
	s, err = openDefault(s.dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Stats().Bytes != 0 {
		t.Fatal("reduced capacity not respected")
	}
	if _, err := s.Begin(m); !errors.Is(err, ErrBypass) {
		t.Fatal("tiny cache admitted blob")
	}
}

func TestRestartKeepsNewestCandidateRegardlessOfInventoryOrder(t *testing.T) {
	s := openTest(t, 1<<20)
	for _, key := range []string{"a", "b", "c"} {
		put(t, s, testMeta(key, "abcd"), "abcd")
	}
	s.Close()
	d, err := os.Open(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	names, err := d.ReadDir(-1)
	d.Close()
	if err != nil {
		t.Fatal(err)
	}
	var newest Metadata
	stamp := int64(100)
	for _, name := range names {
		if !name.IsDir() {
			continue
		}
		path := filepath.Join(s.dir, name.Name(), "meta")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var meta diskMetadata
		if err := json.Unmarshal(raw, &meta); err != nil {
			t.Fatal(err)
		}
		meta.Committed = stamp
		if stamp == 100 {
			newest = meta.Metadata
		}
		stamp--
		raw, _ = json.Marshal(meta)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s2, err := openDefault(s.dir, metadataReservation+4)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	read(t, s2, newest, "abcd")
	if s2.Stats().Entries != 1 {
		t.Fatal("capacity exceeded")
	}
}

func TestRestartReducedMaxEntriesKeepsNewest(t *testing.T) {
	s := openTest(t, 1<<20)
	old, newest := testMeta("old", "abcd"), testMeta("newest", "abcd")
	put(t, s, old, "abcd")
	put(t, s, newest, "abcd")
	s.Close()
	for m, stamp := range map[Metadata]int64{old: 1, newest: 2} {
		path := filepath.Join(s.dir, m.Key, "meta")
		var dm diskMetadata
		raw, _ := os.ReadFile(path)
		if err := json.Unmarshal(raw, &dm); err != nil {
			t.Fatal(err)
		}
		dm.Committed = stamp
		raw, _ = json.Marshal(dm)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s2, err := OpenWithLimits(s.dir, Limits{MaxBytes: 1 << 20, MaxFills: 1, MaxEntries: 1, FillTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	read(t, s2, newest, "abcd")
	if _, err := s2.Get(context.Background(), old); err != ErrMiss {
		t.Fatalf("old entry = %v", err)
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
	if err := s.evictLocked(2*metadataReservation, 0, 2, 0); err != ErrBypass {
		t.Fatalf("eviction without enough eligible space = %v", err)
	}
	read(t, s, a, "a")
	read(t, s, b, body)
	if s.Stats().Evictions != 0 {
		t.Fatal("failed admission evicted useful data")
	}
}

func testMeta(key, body string) Metadata {
	return Metadata{Key: fmt.Sprintf("%x", sha256.Sum256([]byte(key))), Digest: fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(body))), Size: int64(len(body)), MediaType: "application/octet-stream"}
}

func openTest(t *testing.T, limit int64) *Store {
	t.Helper()
	s, err := openDefault(filepath.Join(t.TempDir(), "cache"), limit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

// openDefault opens a Store with a generous set of non-size limits, mirroring
// cachetest.Open for internal tests that cannot import the cachetest package
// without creating an import cycle.
func openDefault(dir string, maxBytes int64) (*Store, error) {
	return OpenWithLimits(dir, Limits{MaxBytes: maxBytes, MaxFills: 16, MaxEntries: 100000, FillTimeout: 10 * time.Minute})
}

func put(t *testing.T, s *Store, m Metadata, body string) {
	t.Helper()
	f, err := s.Begin(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(f, body); err != nil {
		t.Fatal(err)
	}
	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, s *Store, m Metadata, want string) {
	t.Helper()
	r, err := s.Get(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	closeErr := r.Close()
	if err != nil || string(got) != want {
		t.Fatalf("body=%q err=%v", got, err)
	}
	if closeErr != nil {
		t.Fatalf("close reader: %v", closeErr)
	}
}
