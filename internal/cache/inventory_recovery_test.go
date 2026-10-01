package cache

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	s, err := openDefault(dir, 1<<20)
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
					t.Fatalf("child did not reach publication boundary (output:\n%s)", output.String())
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

			s, err := openDefault(dir, 1<<20) // proves the killed process released its flock.
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
