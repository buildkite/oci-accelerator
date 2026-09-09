# Source and test review remediation

Address the eight findings in the [source review](https://ampcode.com/threads/T-01a08382-b190-77ae-ad67-9e4c810ecd2f):
response-header write deadlines, discarded startup diagnostics, conflicting state
paths, cache admission contention, permanent large-object admission bypass,
Amp-dependent acceptance execution, missing multiplatform cache-hit assertions,
and unbounded CA file reads.

## Contracts

- Preserve per-request upstream GET authorization, fail-closed behavior, TLS
  verification, redirect credential isolation, and digest verification.
- Preserve the cache disk format, capacity accounting, single-owner lock, pinned
  reader ownership, and prohibition on publishing partial fills.
- No background downloads, background eviction worker, new dependencies, or new
  configuration knobs are proposed.
- Keep `--check` free of filesystem mutation and state-directory inspection.
- Keep all acceptance tests manual and opt-in, including Docker, fault, public
  registry, and load tests. Add no acceptance CI jobs or runner infrastructure.
- Acceptance tests must not become required PR checks or block merging. Preserve
  the existing unit/race/check jobs.

## Eviction policy

### Reclaim space even when this request still bypasses storage

Approved: retain the eight-victim removal budget per admission, but allow a
successful batch to free capacity even if that batch alone cannot admit the
object. A later request can make further progress. Preserve a whole-request
feasibility check: if all currently evictable entries together cannot make the
object fit, evict nothing. No eviction is triggered by denied upstream requests.

This changes the documented preflight policy. It permits useful entries to be
evicted without caching the triggering response, and competing traffic can reuse
the freed capacity. It provides progress across repeated requests in a stable
cache, not guaranteed admission on the first pull or under competing fills.
With many tiny victims, warming a large layer may still require many requests.

## Acceptance execution policy

Approved: acceptance tests remain manual-only and must not gate PR merging.
Removing the Amp dependency and strengthening test assertions remain in scope.
Do not add optional CI jobs, provision runners, or change merge requirements.
Automated acceptance coverage remains absent by choice; document that limitation
rather than claiming the CI coverage gap is fixed.

Provide reproducible commands and prerequisites for manual Docker, crash, and
ENOSPC verification. Preserve private namespaces, owned-process cleanup, and
explicit privilege requirements. No CI runner or CI privilege decision is needed.

## Delivery slices

Each numbered slice is an independent commit. Finish validation and both
independent reviews before committing or starting the next dependent slice.

### 1. Bound response-header writes

In `internal/proxy/forwarding.go`, refresh the downstream write deadline before
forwarding `WriteHeader`, including informational responses. Preserve body-stream
idle deadlines rather than adding an absolute response-duration timeout.

Add a non-reading-client regression test with upstream `103 Early Hints` followed
by a final response. It must fail on current code and prove handler/connection
release within the write-timeout allowance after the fix. Exercise normal
informational/final response forwarding too.

### 2. Preserve safe startup diagnostics

In `cmd/oci-accelerator/main.go`, retain configuration errors explicitly identified
as sanitized `config.ErrInvalid` diagnostics. Keep arbitrary I/O errors redacted.
Give cache startup/cleanup failures fixed actionable categories for lock conflict,
unsafe state, invalid limits, and filesystem failure using the smallest suitable
error contract in `internal/cache`; do not expose raw paths or error strings.

Extend CLI tests to require the safe field/line/category while asserting that
credentials, rejected values, and sensitive paths remain absent. Cover malformed
YAML, invalid timeout, unreadable configuration, and cache lock/permission errors.

### 3. Reject identical state paths

In `internal/config/validate.go`, compare lexically normalized absolute forms of
the resolved CA and cache paths. Reject equality before startup mutation without
following symlinks, requiring paths to exist, or changing returned path semantics.
Do not introduce a blanket ban on nesting.

Test relative/absolute aliases and `..` normalization, distinct sibling paths,
and `--check` rejection with no CA/cache directory created.

### 4. Bound CA PEM reads

In `internal/ca/ca.go`, read each CA PEM file through an explicit 64 KiB limit,
rejecting excess bytes before PEM parsing. Preserve existing ownership/type/mode
validation and never replace an invalid existing installation.

Test oversized certificate and key files independently, bounded reading, normal
reopen behavior, and preservation of rejected on-disk contents.

### 5. Remove admission-time full sorting

Maintain eviction order incrementally in `internal/cache` instead of gathering
and sorting all entries on every pressured admission. Prefer an ordered eligible
index using the standard library. Preserve the existing preflight/refusal policy
in this slice; keep ordering, pinning, invalidation, replacement, and startup
inventory behavior explicit. If exact cursor semantics require a different
structure from a simple LRU list, choose based on those contracts rather than
silently changing eviction policy during the refactor.

Run existing eviction, recovery, pinning, and admission tests. Compare
`BenchmarkCacheHitUnderAdmission` at 1,000, 10,000, and 100,000 entries, including
removal failures, using repeated runs on the same filesystem and toolchain.
Record benchmark conditions; do not treat one timing as a production SLA.

### 6. Move admission filesystem work outside the bookkeeping lock

Use synchronous claim/perform/finalize operations, not a worker pool. Under
`Store.mu`, claim victims or reserve the fill's key, bytes, and slot; perform I/O
outside the mutex; then finalize accounting under the mutex. Claimed victims
cannot be pinned or selected again. Keep their bytes and slot charged until
removal succeeds, transferring failed removals to charged deferred cleanup.

Cover ordinary eviction, deferred cleanup, invalid-entry cleanup, staging
creation/failure, and tombstone supersession. Prevent same-key reuse until the
previous path owner has finished removal. Audit reader-triggered cleanup for the
same lock contention and ownership rules. `Close` must reject new claims and
wait for outstanding filesystem operations before releasing the process lock.
Filesystem syscalls remain non-preemptible; do not promise a hard cleanup deadline.

Use deterministic paused-I/O tests to prove unrelated hits continue while removal
or staging is blocked. Test concurrent admissions, same-key races, pin/replacement
races, removal failures, accounting, and shutdown during each unlocked I/O phase.
Repeat the contention benchmarks. Preserve admission policy until slice 7.

### 7. Allow bounded reclamation progress

Implement the approved eviction policy. Change batch preflight only after
slices 5 and 6 preserve and validate ownership/accounting behavior.

For globally feasible requests, remove up to eight victims even when more space
is needed, then return capacity bypass if still insufficient. If a removal fails
and the request still cannot fit, report the I/O admission category. Count actual
successful removals as evictions even when the request bypasses.

Test a layer needing nine small victims: first attempt frees eight and bypasses;
the next admits after further reclamation. Test that globally impossible requests
evict nothing, and preserve pin, failure, capacity, concurrency, and shutdown
coverage. Update the package README and architecture narrative for the approved
tradeoff, including the lack of an admission guarantee under contention.

### 8. Make acceptance lifecycle independent of Amp

Replace the named global service helpers in `integration` with a focused
test-owned process lifecycle. Support graceful stop, abrupt crash, reaping, and
restart with bounded/file-backed diagnostic output and idempotent cleanup.
Avoid PID discovery through `pgrep` where an owned process handle suffices.

Handle shell, sudo, mount-namespace, and dockerd descendants explicitly; an
unprivileged process-group signal is not sufficient for every descendant. Prove
privileged teardown removes owned daemons, sockets, and namespaces without
touching unrelated services. Launch Orb verification through a supervised test
runner when needed so the agent environment's service-lifetime rules remain met.

Remove Amp from acceptance prerequisites, retain actual Linux/Docker/privilege
requirements, and exercise the deterministic Docker, crash, and ENOSPC cases.
Keep the public/load harnesses compatible with the new lifecycle without changing
their assertions or opt-in status.

Update `.buildkite/README.md` and operating instructions with reproducible manual
commands, prerequisites, cleanup guidance, and the remaining automated-coverage
limitation. Add no CI jobs or infrastructure. Record actual acceptance execution,
not skipped tests, as verification of the harness changes.

### 9. Prove multiplatform warm-cache substitution

In `integration/docker_multiplatform_acceptance_test.go`, assert cache-hit evidence
for eligible digest-addressed objects during warm and restarted-proxy phases.
Prefer a controlled origin phase that accepts authenticated GETs but withholds
the selected representation bodies, making a successful exact-byte pull depend
on cache substitution. Use per-phase cache metrics if Docker negotiation makes
withholding a representation inappropriate; do not infer hits from GET counts.

Retain assertions for fresh authorization, selected platform, image identity,
tag mutation, old digest access, and absence of unselected-platform fetches. Show
that deliberately bypassing cache substitution makes the new assertions fail.
The public-registry test remains a compatibility smoke test, not hit proof.

## Verification and documentation

Every implementation slice requires `gofmt`, `go test ./...`,
`go test -race ./...`, and `go vet ./...`, plus its targeted checks. Run tests
uncached when recording final evidence. A required blocked or failing check keeps
the slice incomplete unless the owner grants an exception.

Use two fresh read-only reviewers per implementation slice against the same code
and test snapshot: one with task/repository context and one with only a filtered
source/test/build/diff bundle. Disclose any inability to isolate the latter's
context. Resolve findings and re-run affected checks before committing.

Update current package READMEs and `docs/architecture.md` / `docs/operations.md`
within the slice that changes the contract, retaining README shape and linked
anchors. Maintain this plan by removing resolved questions and stale proposals,
not by appending an implementation diary.

The existing baseline passes unit/race/vet and `.buildkite/check.sh`; all seven
opt-in acceptance entry points were skipped during the source review. The header
timeout was reproduced in a disposable copy. The 10,000-entry tmpfs benchmark
showed approximately 6.5 microseconds mean hit time without admission versus
345 microseconds with concurrent eviction; rerun under matched conditions before
using these observations to judge the refactor.
