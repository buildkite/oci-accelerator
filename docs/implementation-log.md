# Implementation log

Delivery status for the [approved plan](plans/https-registry-cache.md).
The plan owns scope and design; this log tracks progress and upcoming work.

## Current status

- **Completed implementation slices:** 1 — working explicit proxy and CA lifecycle;
  2 — authorized digest-blob caching; 3 — digest manifests and compatibility.
- **In progress:** 4b — bounded authorized fill waiting.
- **Next:** 4b–4e. Combined autonomous implementation, checks, reviews,
  commits/pushes, and isolated acceptance/fault/load resources are authorized.

## Completed groundwork

- Requirements and architecture agreed and recorded in the approved plan.
- [Orb setup](../.agents/setup) and [supervised Docker service](../.amp/services.yaml)
  added. Setup passed cold and warm runs; a clean login shell found Go;
  `go test -race encoding/hex` passed; Docker pulled and ran `hello-world`.
  These are environment checks, not proxy tests.
- [Repository working rules](../AGENTS.md) added for incremental commits,
  independent review, decision logging, verification, and security.

## Slice overview

| Slice | Status | Outcome |
| --- | --- | --- |
| 1. Working explicit proxy and CA lifecycle | Complete | Docker Engine pulls through the Go proxy with selective MITM, persistent CA, verified upstream TLS, and pull-only enforcement; no caching. |
| 2. Authorized digest-blob caching | Complete | 2a–2d: safe redirects, verified bounded storage, authorized delivery, and real Docker cold/warm/restart acceptance. |
| 3. Digest manifests and generic protocol compatibility | Complete | Representation-aware caching, local bearer/multi-platform fixtures and anonymous Hub/GHCR acceptance. |
| 4. Concurrency and operational hardening | In progress | 4a resource limits next; coalescing, fault checks, metrics and load evidence follow. |

## Slice 1: completed subslices

Each row is a self-contained implementation commit. Configuration and CA
prerequisites were completed before interception.

| Subslice | Status | Outcome and validation |
| --- | --- | --- |
| 1a. Go module and configuration | [Complete](https://ampcode.com/@buildkite/docker-accelerator/commit/763cd026ffa760d47929d53a103ec772dfb85a85) | Strict YAML/authority/CLI tests pass, including defaults, malformed/duplicate/unknown values, and redacted errors. |
| 1b. Persistent CA lifecycle | [Complete](https://ampcode.com/@buildkite/docker-accelerator/commit/0b3b6fc5c411f9b6b1340b18c2068876528e019a) | Atomic CA initialization/reuse and bounded leaf issuance; tests for trust, permissions, invalid state, renewal, concurrency, and stable CA identity. |
| 1c. CONNECT routing and opaque tunnels | [Complete](https://ampcode.com/@buildkite/docker-accelerator/commit/268e48b0c6eb7521db85581f31ca251afbe1f159) | Strict authority parsing, configured-host refusal, passthrough, buffered bytes, half-close, idle/cap/shutdown tests. CLI starts the listener and handles signals. |
| 1d. Intercepted registry forwarding | [Complete](https://ampcode.com/@buildkite/docker-accelerator/commit/ffb081fa1fd0fa8a671463e1b5e2c27039d9b275) | TLS interception, authority binding, verified upstream TLS, credential/challenge forwarding, write rejection, redirect relay, cancellation and partial-response failure tests. No caching. |
| 1e. Docker Engine acceptance and setup docs | [Complete](https://ampcode.com/@buildkite/docker-accelerator/commit/f068f8d2ee8d13475d612b373501b1bf6f582d35) | Local authenticated registry, compiled binary, fresh Docker daemons, CA rejection/trust/persistence and digest/byte assertions; operator guide and runnable example. |

## Validation and review

1a validation: `go test ./...`, `go test -race ./...`, `go vet ./...` passed.
Two fresh read-only reviewers inspected the same source/test/diff snapshot. The
code-only reviewer received a filtered `/tmp` bundle with no docs or Git history;
ambient tool/project guidance cannot be fully isolated, so this is not a fully
blind review. Its review found no issues. The plan reviewer flagged YAML merge
overrides as duplicate fields; this was rejected because explicit overrides of
merged defaults are defined YAML semantics, not duplicate keys in one mapping.
Actual duplicate mapping keys are rejected. No behavior changed during review.

1b validation: all three baseline checks passed. Both independent reviews and
follow-up reviews completed using matching snapshots and the same code-only
isolation limitation. Fixed restrictive-umask key permissions and added a regression
test. Rejected same-user file/path replacement races as outside the private,
owner-controlled filesystem boundary; cooperative initialization is serialized and
existing keys are never overwritten. Windows compilation was noted but is not a
validated target: this first implementation uses Unix filesystem ownership/permissions
and is tested on Linux Docker Engine. Windows support is not claimed.

1c validation: baseline tests/race/vet passed; CONNECT fuzzing ran 167,653 cases
without a failure. Both independent reviews completed. The code reviewer noted
the raw-header bound is stricter than Go's internal allowance; retained intentionally
as the documented 64 KiB limit, rather than depending on parser implementation slack.
Plan-aware review found no issues. Lifecycle regressions also passed three race runs.

1d validation: tests/race/vet passed, including trusted/untrusted client and upstream
TLS, SNI/HTTP authority rejection, exact bytes/auth/query preservation, method policy,
redirect non-following, protocol statuses, cancellation, stalled/truncated streams,
log redaction, and graceful TLS keepalive shutdown. Both independent reviews completed.
Rejected a claimed truncated-stream completion-log bug: the standard reverse proxy
aborts the HTTP handler on copy failure; an added executable regression confirmed
`outcome=aborted` (not `complete`) and a client read error. Checks rerun successfully.

1e validation: tests/race/vet and example `--check` passed. Real Docker 29.8.0
acceptance passed repeatedly with the compiled binary: missing CA trust reached
zero origin HTTP requests, both authenticated fresh-daemon pulls matched the expected
manifest/config/rootfs digests, each fetched one 118-byte layer, and the proxy CA
fingerprint was unchanged after restart. The expected manifest digest is
`sha256:ea16de605af6cdec08219d0e31bc573cfd28ac2ef8b115e759bfaac7c107edde`.
Both independent reviews and follow-up reviews completed. Fixed the fixture counter
to exclude HEAD from GET body-byte assertions, with a regression test. Acceptance
services/directories were cleaned up; only the repository's normal Docker service
remains. Generated service metadata is kept in ignored `.amp/portals/`, not test source.
See the [operating guide](operations.md) for setup, evidence, and unvalidated flows.

## Slice 2: completed subslices and validation

[2a complete](https://ampcode.com/@buildkite/docker-accelerator/commit/1a3b32c8f947f17235457d2cd2d1e57f69c82d89): test/race/vet passed; path fuzzing ran 139,409 cases and redirect
fuzzing 42,194 without failure. Both independent reviews and focused follow-ups
completed on matching snapshots. Fixed omitted deprecated IPv6 site-local space
with a regression test. Rejected the code-only review's request to allowlist public
CDNs: the approved policy explicitly permits them; the list grants private-destination
exceptions. Code-only input excluded docs/history, with ambient guidance isolation
limitations disclosed.

[2b complete](https://ampcode.com/@buildkite/docker-accelerator/commit/5a435e865b72348ce950ffbf77a705f80dec6745): test/race/vet passed, including atomic fill visibility, digest/length
failures, /dev/full write errors, reservations, competing writers, reader pinning,
restart/corruption cleanup, restrictive umask, and lock ownership. Both independent
reviews and follow-ups completed. Fixed startup eviction ordering and preflighted
reclaimable space before deleting insufficient victims. Retained documented
commit-time approximation after restart rather than per-hit metadata writes.
The low-level store uses explicit Abort for caller-owned fills; request cancellation
wiring is 2c. Rejected adding goroutines to pretend regular-file syscalls are
context-cancellable: Go cannot preempt a stalled kernel filesystem operation, and
abandoning it would leak ownership. Ordinary cancellation/Abort is covered; storage
stall resilience remains an operational limitation, not a verified hard deadline.

[2c complete](https://ampcode.com/@buildkite/docker-accelerator/commit/2b4f3e2c91a7ab71ef797396536a55a6592e8565): test/race/vet passed, plus three repeated race runs of warm permission,
cancel/shutdown, and concurrent-client tests. Cold bytes were verified; warm HTTP
fixtures delivered identical cached bytes despite the origin sending only accepted
headers, proving body substitution and prompt cancellation. Denied/revoked clients,
public HEAD/protected GET, storage denial, outages, per-hop no-store, metadata and
protocol bypasses, cross-registry/repository isolation, and failed fills are covered.
Both independent reviews and follow-ups completed; fixed unexpected Serve termination
to shut down the proxy before closing storage. Code-only isolation limitation remains
as recorded above.

[2d complete](https://ampcode.com/@buildkite/docker-accelerator/commit/19035876bfe7c0697459d84cb076f1b363e04453): test/race/vet and repeated real Docker 29.8.0 acceptance passed,
including a race-instrumented fixture run. The final strengthened test observed
layer GETs/body bytes cold 1/8,390,075, warm 1/32,768, restarted proxy 1/196,608.
Layer and config each received a fresh authenticated GET in both warm phases;
exact inspected manifest/config/rootfs digests matched, tags were re-resolved, and
the CA fingerprint stayed unchanged. Missing CA trust reached zero origin HTTP
requests. Both reviews and follow-ups completed; strengthened per-config GET and
authoritative RepoDigests assertions in response to review. All owned services were
cleaned up. See the operating guide for fixture pacing and benchmark limitations.

## Later slices: queued scope

[3a complete](https://ampcode.com/@buildkite/docker-accelerator/commit/4f1a8c7):
test/race/vet passed; object-path fuzzing executed 225280 cases.
All four manifest types passed cold/warm/store-and-proxy restart substitution,
fresh-GET/revocation, metadata bypass, corrupt-fill, isolation and redirect checks.
Paired snapshot reviews found no issues. The code-only review used a filtered
source/test/diff bundle; ambient guidance cannot be fully isolated.

The existing real Docker acceptance also passed after 3a: Docker 29.8.0, layer
GETs/body bytes cold 1/8390075, warm 1/32768, restart 1/262144, stable CA and
exact inspected digests. No test-owned services remain.

Remaining slices follow the approved plan.

[3b complete](https://ampcode.com/@buildkite/docker-accelerator/commit/d7c8022):
test/race/vet passed, including Basic registry auth, opaque-token-host
certificate identity, client-owned refresh, expiry/revocation/scope denials, mutable
tags, manifest bypasses and redirect credential stripping. Paired reviews and
follow-ups completed. Added Basic/refresh coverage and pinned original-body checks
from review; the reported Accept gap was already covered by 3a's same-URL/media
accept-key test. No production changes were needed. Same filtered-review limitation.

[3c complete](https://ampcode.com/@buildkite/docker-accelerator/commit/07f5122):
test/race/vet and repeated real Docker 29.8.0 acceptance passed.
Eight fresh-daemon phases per OCI/Docker format each acquired a token and fetched
the expected root/child/config/layer; mutated tags selected different config/rootfs,
old digests remained retrievable, no arm64 child was fetched, and CA identity persisted.
Baseline Basic/large-layer acceptance also passed (warm/restart 32768/294912 origin
layer bytes versus 8390075 cold). Paired reviews and follow-ups completed. Fixed
namespace group expansion and asserted socket GID; supplementary root membership
had masked the defect in this orb. All owned services cleaned up.

[3d complete](https://ampcode.com/@buildkite/docker-accelerator/commit/b9adb18): test/race/vet and repeated anonymous Hub/GHCR acceptance passed
(final run 32.46s). Fresh Docker state preserved exact image identities across
cold/warm/restart. Paired reviews and follow-ups completed. Replaced count-only
population assertions with completed-key set comparison to tolerate eviction.
Rejected adding warm-hit proof to public smoke: deterministic local fixtures own
that evidence. The filtered code-only review has the same ambient-guidance limitation.

4a complete: test/race/vet and example `--check` passed. Small-limit tests cover
slot reservations, independent fill limits, pinned refusal/LRU eviction, reduced
startup entry limits, timer cleanup and expiry races. Stream-wrapper checks prove
exact upstream bytes after cap refusal and expiry. Paired snapshot reviews completed.
Rejected code-only findings demanding preemption of kernel I/O and an atomic wall-clock
deadline/publication transaction: the approved deadline is checked under the fill/store
locks, not a real-time scheduling guarantee. Stalled filesystem operations remain an
explicit limitation. Parent review fixed terminal-write timer retention before review.

- **3a:** authorized, representation-aware digest-manifest caching using the
  existing store and redirect stack, preserving blob entries.
- **3b:** deterministic local protocol/authentication coverage, including separate
  bearer token hosts, revocation, negotiation, redirects, and bypasses.
- **3c:** fresh-daemon Docker multi-platform/digest pulls and actual tag mutation.
- **3d:** anonymous public Docker Hub/GHCR smoke flows and operator handoff; agent
  image selection is approved and account credentials are unnecessary. The external
  acceptance gate is approved: blocked external checks leave 3d/slice 3 incomplete
  unless the user grants an exception. Autonomous execution is authorized.
- Slice-3 local and public acceptance use isolated supervised Docker daemons;
  the default Docker socket is not required.
- **4a:** explicit active-fill, entry-count, and fill-lifetime bounds, reusing
  existing reservations, eviction, and reader pinning.
- **4b:** bounded follower waiting after independent authorization; client-owned
  fills, cancellation, and one fresh GET fallback rather than background downloads.
- **4c:** process-crash, real bounded-filesystem exhaustion, concurrent corruption
  recovery, and shutdown fault tests; fix demonstrated ownership failures.
- **4d:** optional loopback metrics and bounded diagnostic outcomes.
- **4e:** concurrent HTTP/Docker acceptance, measured resource/traffic/latency
  evidence, and final operator handoff.
- Slice-4 client-owned fills with bounded follower waiting and one fresh GET
  fallback are approved; background shared downloads and leader handoff are excluded.
  The configurable wait defaults to 1s; zero disables waiting. The configurable
  global active-fill cap of 16 is approved; excess misses stream uncached.
  The configurable 100000-entry cap, including reserved fill slots, is approved;
  evict unused LRU entries or bypass admission when pins prevent eviction.
  The configurable 10m fill lifetime is approved; expiry abandons cache storage,
  not the upstream transfer, subject to the documented kernel-I/O limitation.
  The optional separate loopback-only Prometheus metrics endpoint is approved;
  disabled by default, with no public/admin/profiling endpoint or unbounded labels.
  Isolated process-crash, bounded-filesystem exhaustion, corruption/recovery, and
  active-work shutdown fault tests are approved; unavailable required checks remain
  blockers unless explicitly waived. The 1/8/32-client HTTP and 1/4-daemon Docker
  load matrix is approved, with measured performance rather than a throughput/latency
  SLA. Combined execution is authorized.
  Planning confirmed mount/unshare tools and sudo are
  available, but has not exercised mount privileges or fault workloads.

## Keeping this log current

- Update the current status and affected rows as work starts, finishes, or is
  blocked. Keep the next action explicit.
- Mark a subslice complete only after implementation, validation, and independent
  review are finished and feedback is resolved. Record a concise outcome and
  decisive validation evidence, plus its commit link once available.
- Keep implementation decisions and their reasons in the [decision log](decision-log.md);
  do not duplicate that narrative here.
