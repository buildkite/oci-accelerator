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

## Before the eviction budget

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

## Eight-attempt admission budget

The approved follow-up limits each Begin's eviction phase to eight victim
directory-removal attempts. The existing cleanup retry batch is separate, and
startup eviction remains uncapped. This bounds attempts, not elapsed time or
the number of filesystem syscalls inside a directory removal.

Before deleting anything, admission checks whether the selected batch could
free enough capacity. If not, it reports AdmissionCapacity without removing
useful entries. A large fill needing more than eight small victims can therefore
continue bypassing until other cache activity creates enough space; retries do
not gradually empty the cache for it. Actual removal failures still preserve the
first filesystem cause as AdmissionIOError, without releasing failed charges.

A shared timestamp/key cursor advances after failed or deferred batches so later
admissions can reach other candidates. It survives fitting admissions and the
cursor entry becoming pinned or disappearing, and resets on successful eviction.
This is deliberately best-effort LRU across batches: after deferral, an unrelated
admission can evict a newer candidate instead of restarting at older victims.
Strict LRU or per-request cursor tracking was not added.

The unchanged benchmark was repeated on the same tmpfs setup, three runs each.
The 100,000-entry failed-removal column again uses the longer 10-second runs.

| Entries | Hit only | Successful admissions | Failed removals |
| ---: | ---: | ---: | ---: |
| 1,000 | 4.05–4.12 µs | 154–163 µs | 167–184 µs |
| 10,000 | 4.02–4.18 µs | 161–170 µs | 192–233 µs |
| 100,000 | 4.03–4.13 µs | 1.44–1.48 ms | 2.00–2.08 ms |

At 100,000 entries, the longer failed-removal runs completed 6,020, 5,790 and
4,966 hits. Observed maximum hit latencies were 68.7, 60.3 and 73.0 ms, versus
3.10–3.25 seconds before the budget. All benchmark assertions passed.

Index scanning and sorting still run under the store mutex. The budget removes
the all-victims deletion stall but does not eliminate admission contention or
establish a latency guarantee. No LRU data-structure or locking change was made.
