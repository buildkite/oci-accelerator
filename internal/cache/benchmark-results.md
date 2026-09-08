# Admission contention measurements

Measured in the Linux amd64 orb on 2026-09-08, Intel Xeon 2.60 GHz,
GOMAXPROCS 8. These are local microbenchmarks, not production latency estimates.

## Workload and reproduction

`BenchmarkCacheHitUnderAdmission` seeds real committed entries with four-byte
bodies. One client repeatedly performs Get/read/Close on a pinned hot entry.
One concurrent admission worker either successfully fills new keys or encounters
permission-denied removal of every eligible victim. The hit-only case has no
admission worker. Body bytes, admission outcomes and capacity accounting are
asserted. Setup, warm-up and cleanup are outside hit timing; reported admission
attempt counts include warm-up and the final in-flight attempt.

The orb's root filesystem hit its directory-link limit while seeding 100,000
entries. All reported measurements therefore use tmpfs, with inherited/ambient
DAC-bypass capabilities removed. This isolates index, locking and syscall costs;
it does not model persistent-disk latency or large-layer transfer.

Run as a non-root user on Linux with `setpriv` and sufficient tmpfs space:

```sh
go test -c -o /tmp/cache-benchmark.test ./internal/cache
TMPDIR=/dev/shm setpriv --inh-caps=-all --ambient-caps=-all \
  /tmp/cache-benchmark.test -test.run='^$' \
  -test.bench='^BenchmarkCacheHitUnderAdmission$' -test.benchtime=2s -test.count=3
TMPDIR=/dev/shm setpriv --inh-caps=-all --ambient-caps=-all \
  /tmp/cache-benchmark.test -test.run='^$' \
  -test.bench='^BenchmarkCacheHitUnderAdmission$/^entries=100000$/^removal-failure$' \
  -test.benchtime=10s -test.count=3
rm /tmp/cache-benchmark.test
```

## Results

Ranges below are the minimum and maximum **mean hit latency across three runs**,
not percentiles. Each hit includes opening, reading, and closing its own reader.

| Entries | Hit only | Successful admissions | Failed removals |
| ---: | ---: | ---: | ---: |
| 1,000 | 4.27–5.88 µs | 150–156 µs | 0.965–1.20 ms |
| 10,000 | 4.11–4.60 µs | 156–168 µs | 9.97–15.3 ms |
| 100,000 | 4.03–4.22 µs | 1.27–1.59 ms | 58.1–145 ms |

The 100,000-entry failed-removal column uses the longer 10-second runs: 139, 264,
and 87 hits, with maximum hit latencies of 3.25, 3.10, and 3.22 seconds. Initial
2-second runs yielded only one hit each (2.99–3.13 seconds), motivating the longer
measurement. Scheduling produces bursts of quick hits between long stalls;
neither these means nor observed maxima establish a production distribution.

All cases passed, including actual permission-denied assertions. A separate
1,000-entry benchmark run with the race detector, two repetitions and a 100 ms
target also passed; race-instrumented timings are excluded above.

## Pending decision

The measurements support addressing admission contention, particularly repeated
failed removals. No eviction-attempt budget, LRU structure change, or locking
change is implemented. Those remain a separate user decision after these results.
