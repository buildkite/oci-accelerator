package cache

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

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
						// deadline. Force each operation to win in its own subtest.
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
