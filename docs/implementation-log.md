# Implementation log

Delivery status for the [approved plan](plans/https-registry-cache.md).
The plan owns scope and design; this log tracks progress and upcoming work.

## Current status

- **Completed implementation slices:** 1 — working explicit proxy and CA lifecycle (1a–1e).
- **In progress:** slice 2; 2c complete, preparing 2d.
- **Next:** 2d Docker acceptance.
- Slice 2 recommendations and autonomous implementation are approved. Stop before
  slice 3. Caching is enabled when a cache directory is configured.

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
| 2. Authorized digest-blob caching | In progress | 2a–2c complete: safe redirects, verified bounded storage, and authorized delivery. Real Docker cache acceptance is next. |
| 3. Digest manifests and generic protocol compatibility | Not started | Representation-aware digest-manifest caching and registry/auth compatibility; tags remain uncached. |
| 4. Concurrency and operational hardening | Not started | Fill coalescing, cancellation, recovery, resource bounds, diagnostics, graceful shutdown, and measured cold/warm behavior. |

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

## Later slices: queued scope

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

2c complete: test/race/vet passed, plus three repeated race runs of warm permission,
cancel/shutdown, and concurrent-client tests. Cold bytes were verified; warm HTTP
fixtures delivered identical cached bytes despite the origin sending only accepted
headers, proving body substitution and prompt cancellation. Denied/revoked clients,
public HEAD/protected GET, storage denial, outages, per-hop no-store, metadata and
protocol bypasses, cross-registry/repository isolation, and failed fills are covered.
Both independent reviews and follow-ups completed; fixed unexpected Serve termination
to shut down the proxy before closing storage. Code-only isolation limitation remains
as recorded above. Commit link follows with 2d.

Slice 2 has an approved concrete breakdown in the plan. Break later slices down
using the implementation then available.

- **Slice 2:** prove cold/warm byte correctness and reduced upstream body
  transfer, with fresh authorization for every delivery. Cover denied/revoked
  clients, public-HEAD/protected-GET behavior, redirected layers, interrupted
  fills, and bounded cache admission. Never serve cache on upstream failure.
- **Slice 3:** cover OCI/Docker media types and multi-platform images, Basic and
  bearer authentication, separate token hosts, and redirect credential stripping.
  Verify fresh tags, registry/repository/representation isolation, and correct
  HEAD/Range/conditional bypasses. Record Docker Hub and GHCR compatibility checks.
- **Slice 4:** exercise concurrent fills, follower cancellation, LRU/read
  pinning, restart/corruption/disk-full recovery, resource limits, logs/metrics,
  and graceful shutdown. Measure cold/warm latency, memory, disk occupancy,
  upstream bytes, and authorization requests; finish operator documentation.

## Keeping this log current

- Update the current status and affected rows as work starts, finishes, or is
  blocked. Keep the next action explicit.
- Mark a subslice complete only after implementation, validation, and independent
  review are finished and feedback is resolved. Record a concise outcome and
  decisive validation evidence, plus its commit link once available.
- Keep implementation decisions and their reasons in the [decision log](decision-log.md);
  do not duplicate that narrative here.
