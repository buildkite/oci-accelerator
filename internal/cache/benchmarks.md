# Cache admission benchmarks

`BenchmarkCacheHitUnderAdmission` measures the Get/read/Close path for a pinned
hot entry while another client attempts cache admission. It seeds real committed
four-byte bodies at 1,000, 10,000, and 100,000 entries and exercises:

- hits without a concurrent admission worker
- successful admissions that evict unused entries
- failed admissions when filesystem permissions prevent victim removal

The benchmark asserts body bytes, admission outcomes, and capacity accounting.
Setup, warm-up, and cleanup are outside hit timing. Reported admission attempts
include warm-up and the final in-flight attempt, so they are not a timed throughput
rate. Reported mean and maximum hit durations are not latency percentiles.

## Running

Use a filesystem supporting at least 100,000 subdirectories and sufficient
space/inodes. A tmpfs such as `/dev/shm` isolates metadata, locking, and syscall
costs from persistent-disk latency. Run as a non-root user without DAC override
privileges. The removal-failure case skips if chmod does not actually deny deletion.

From the repository root on Linux with `setpriv` installed:

```sh
benchmark_dir=$(mktemp -d)
go test -c -o "$benchmark_dir/cache-benchmark.test" ./internal/cache
TMPDIR=/dev/shm setpriv --inh-caps=-all --ambient-caps=-all \
  "$benchmark_dir/cache-benchmark.test" -test.run='^$' \
  -test.bench='^BenchmarkCacheHitUnderAdmission$' -test.benchtime=2s -test.count=3
TMPDIR=/dev/shm setpriv --inh-caps=-all --ambient-caps=-all \
  "$benchmark_dir/cache-benchmark.test" -test.run='^$' \
  -test.bench='^BenchmarkCacheHitUnderAdmission$/^entries=100000$/^removal-failure$' \
  -test.benchtime=10s -test.count=3
rm "$benchmark_dir/cache-benchmark.test"
rmdir "$benchmark_dir"
```

Check the exit status and any skips. Record code revision, Go version, CPU,
GOMAXPROCS, filesystem, workload, and repetitions alongside results when making
comparisons. Do not compare race-instrumented timings with normal runs.

## Interpretation

Admission's eviction phase is limited to eight directory-removal attempts, while pending
cleanup retries have a separate budget. These limits bound attempts, not elapsed
time or the syscalls within a removal. Startup bulk-sorts inventory while the
live eviction order is maintained incrementally, avoiding admission-time sorting,
and removal runs outside the bookkeeping mutex under serialized admission/path
ownership. The
[operating guide](../../docs/operations.md#capacity-and-eviction) and
comments in `eviction.go` describe best-effort LRU, admission limits, and
cleanup ownership.

Tiny-body microbenchmarks do not measure large-layer transfer, Internet bandwidth,
or production latency. Use the [integration load checks](../../docs/testing.md#bounded-load)
for compiled-proxy HTTP and Docker workloads.
