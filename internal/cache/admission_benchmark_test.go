package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const admissionBenchmarkBody = "body"

// Run on a filesystem that supports 100,000 subdirectories (for example tmpfs)
// and without root/DAC override privileges to exercise the removal-failure case.
// Small bodies isolate cache metadata/locking costs, not layer-transfer speed.
func BenchmarkCacheHitUnderAdmission(b *testing.B) {
	for _, entries := range []int{1_000, 10_000, 100_000} {
		b.Run(fmt.Sprintf("entries=%d", entries), func(b *testing.B) {
			// This fixture owns the store and seeds it once. Child benchmarks share
			// the full store so their expensive setup is not calibration-dependent.
			dir := filepath.Join(b.TempDir(), "cache")
			charge := metadataReservation + int64(len(admissionBenchmarkBody))
			s, err := OpenWithLimits(dir, Limits{
				MaxBytes: charge * int64(entries), MaxEntries: entries,
				MaxFills: 1, FillTimeout: time.Minute,
			})
			if err != nil {
				b.Fatal(err)
			}
			defer func() {
				if err := s.Close(); err != nil {
					b.Error(err)
				}
			}()

			var hot Metadata
			for i := range entries {
				meta := testMeta(fmt.Sprintf("seed-%d", i), admissionBenchmarkBody)
				benchmarkPut(b, s, meta)
				if i == entries/2 {
					hot = meta
				}
			}
			if st := s.Stats(); st.Entries != entries || st.Bytes != charge*int64(entries) {
				b.Fatalf("seeded cache is not full: %+v", st)
			}

			// This reader pins the hot entry against churn. Timed operations below
			// still perform the complete Get/read/Close path with their own reader.
			pin, err := s.Get(context.Background(), hot)
			if err != nil {
				b.Fatal(err)
			}
			defer pin.Close()

			b.Run("hit-only", func(b *testing.B) {
				benchmarkHits(b, s, hot, nil)
			})
			b.Run("evict", func(b *testing.B) {
				before := s.Stats()
				attempt := func(i uint64) error {
					// The fixture is shared across -count repetitions too.
					meta := testMeta(fmt.Sprintf("evict-%d-%d", before.FillSuccess, i), admissionBenchmarkBody)
					f, err := s.Begin(meta)
					if err != nil {
						return err
					}
					if _, err = io.WriteString(f, admissionBenchmarkBody); err != nil {
						return err
					}
					return f.Commit()
				}
				attempts := benchmarkHits(b, s, hot, attempt)
				after := s.Stats()
				if attempts == 0 || after.Evictions-before.Evictions < int64(attempts) {
					b.Fatalf("accepted admissions did not evict: attempts=%d before=%+v after=%+v", attempts, before, after)
				}
				assertBenchmarkFull(b, after, entries, charge)
			})
			b.Run("removal-failure", func(b *testing.B) {
				if !benchmarkPermissionDenied(b) {
					b.Skip("chmod does not deny deletion: run the benchmark without root or DAC override capabilities")
				}

				// Make every indexed victim non-removable. Reading files remains
				// permitted. Restore modes before store and TempDir cleanup run.
				var protected []string
				s.mu.Lock()
				for _, e := range s.entries {
					protected = append(protected, e.dir)
				}
				s.mu.Unlock()
				defer func() {
					for _, d := range protected {
						if err := os.Chmod(d, 0o700); err != nil && !errors.Is(err, os.ErrNotExist) {
							b.Error(err)
						}
					}
				}()
				for _, d := range protected {
					if err := os.Chmod(d, 0o500); err != nil {
						b.Fatal(err)
					}
				}

				before := s.Stats()
				attempt := func(i uint64) error {
					f, err := s.Begin(testMeta(fmt.Sprintf("denied-%d", i), admissionBenchmarkBody))
					if f != nil {
						_ = f.Abort()
						return errors.New("admission succeeded without reclaiming capacity")
					}
					var admission *AdmissionError
					if !errors.As(err, &admission) || admission.Reason != AdmissionIOError || !errors.Is(err, os.ErrPermission) {
						return fmt.Errorf("Begin = %v, want AdmissionIOError wrapping os.ErrPermission", err)
					}
					return nil
				}
				attempts := benchmarkHits(b, s, hot, attempt)
				after := s.Stats()
				assertBenchmarkFull(b, after, entries, charge)
				if after.Fills != before.Fills || after.Reserved != before.Reserved || after.Evictions != before.Evictions || after.Admissions[AdmissionAccepted] != before.Admissions[AdmissionAccepted] || after.Admissions[AdmissionIOError]-before.Admissions[AdmissionIOError] != int64(attempts) {
					b.Fatalf("failed removals changed capacity or created fills: attempts=%d before=%+v after=%+v", attempts, before, after)
				}
			})
		})
	}
}

// benchmarkHits runs one hot-hit client and, when attempt is non-nil, one
// sequential admission worker. It waits for that worker's first completed
// attempt before measuring. Attempt count includes warm-up and the final
// in-flight attempt: it describes workload, not a timed throughput rate.
func benchmarkHits(b *testing.B, s *Store, hot Metadata, attempt func(uint64) error) uint64 {
	var completed uint64 // Written only by the worker, read after joining it.
	stop := make(chan struct{})
	done := make(chan struct{})
	workerErr := make(chan error, 1)
	stopped := false
	stopWorker := func() {
		if attempt != nil && !stopped {
			stopped = true
			close(stop)
			<-done
		}
	}
	defer stopWorker()
	if attempt != nil {
		ready := make(chan struct{})
		go func() {
			defer close(done)
			for i := uint64(0); ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				if err := attempt(i); err != nil {
					workerErr <- err
					return
				}
				completed++
				if i == 0 {
					close(ready)
				}
			}
		}()
		select {
		case <-ready:
		case <-done:
			b.Fatalf("first admission attempt: %v", <-workerErr)
		}
	}

	var total, max time.Duration
	var hits uint64
	for b.Loop() {
		start := time.Now()
		r, err := s.Get(context.Background(), hot)
		if err != nil {
			b.Fatal(err)
		}
		got, readErr := io.ReadAll(r)
		closeErr := r.Close()
		elapsed := time.Since(start)
		if readErr != nil || closeErr != nil || string(got) != admissionBenchmarkBody {
			b.Fatalf("hot read = %q, read error %v, close error %v", got, readErr, closeErr)
		}
		total += elapsed
		if elapsed > max {
			max = elapsed
		}
		hits++
		select {
		case err := <-workerErr:
			b.Fatalf("admission worker: %v", err)
		default:
		}
	}
	stopWorker()
	select {
	case err := <-workerErr:
		b.Fatalf("final admission attempt: %v", err)
	default:
	}
	if hits != 0 {
		b.ReportMetric(float64(total.Nanoseconds())/float64(hits), "mean-hit-ns")
	}
	b.ReportMetric(float64(max.Nanoseconds()), "max-hit-ns")
	b.ReportMetric(float64(completed), "admission-attempts")
	return completed
}

func benchmarkPut(b *testing.B, s *Store, meta Metadata) {
	b.Helper()
	f, err := s.Begin(meta)
	if err == nil {
		_, err = io.WriteString(f, admissionBenchmarkBody)
	}
	if err == nil {
		err = f.Commit()
	}
	if err != nil {
		b.Fatal(err)
	}
}

func assertBenchmarkFull(b *testing.B, st Stats, entries int, charge int64) {
	b.Helper()
	if st.Entries != entries || st.Bytes != int64(entries)*charge || st.Fills != 0 || st.Reserved != 0 {
		b.Fatalf("cache is not full and bounded: %+v", st)
	}
}

func benchmarkPermissionDenied(b *testing.B) bool {
	b.Helper()
	dir := b.TempDir()
	path := filepath.Join(dir, "body")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		b.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		b.Fatal(err)
	}
	err := os.Remove(path)
	if restoreErr := os.Chmod(dir, 0o700); restoreErr != nil {
		b.Fatal(restoreErr)
	}
	if err == nil {
		return false
	}
	if !errors.Is(err, os.ErrPermission) {
		b.Fatalf("permission probe removal: %v", err)
	}
	return true
}
