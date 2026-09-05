# Implementation log

Delivery status for the [approved plan](plans/https-registry-cache.md).
The plan owns scope and design; this log tracks progress and upcoming work.

## Current status

- **Completed implementation slices:** none.
- **In progress:** slice 1; 1a–1d complete.
- **Next:** 1e — Docker acceptance and setup docs.
- Autonomous implementation of 1a–1e is authorized; stop before caching.

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
| 1. Working explicit proxy and CA lifecycle | In progress | Docker Engine pulls through the Go proxy with selective MITM, persistent CA, verified upstream TLS, and pull-only enforcement; no caching. |
| 2. Authorized digest-blob caching | Not started | Per-request upstream GET authorization, safe redirects, verified atomic disk fills, cache substitution, capacity limits, and basic eviction. |
| 3. Digest manifests and generic protocol compatibility | Not started | Representation-aware digest-manifest caching and registry/auth compatibility; tags remain uncached. |
| 4. Concurrency and operational hardening | Not started | Fill coalescing, cancellation, recovery, resource bounds, diagnostics, graceful shutdown, and measured cold/warm behavior. |

## Slice 1: upcoming subslices

Each row is a proposed self-contained commit. The sequence puts configuration
and CA prerequisites before interception. Refine boundaries when implementation
provides evidence, without silently changing the approved scope.

| Subslice | Status | Outcome and validation |
| --- | --- | --- |
| 1a. Go module and configuration | [Complete](https://ampcode.com/@buildkite/docker-accelerator/commit/763cd026ffa760d47929d53a103ec772dfb85a85) | Strict YAML/authority/CLI tests pass, including defaults, malformed/duplicate/unknown values, and redacted errors. |
| 1b. Persistent CA lifecycle | [Complete](https://ampcode.com/@buildkite/docker-accelerator/commit/0b3b6fc5c411f9b6b1340b18c2068876528e019a) | Atomic CA initialization/reuse and bounded leaf issuance; tests for trust, permissions, invalid state, renewal, concurrency, and stable CA identity. |
| 1c. CONNECT routing and opaque tunnels | [Complete](https://ampcode.com/@buildkite/docker-accelerator/commit/268e48b0c6eb7521db85581f31ca251afbe1f159) | Strict authority parsing, configured-host refusal, passthrough, buffered bytes, half-close, idle/cap/shutdown tests. CLI starts the listener and handles signals. |
| 1d. Intercepted registry forwarding | Complete | TLS interception, authority binding, verified upstream TLS, credential/challenge forwarding, write rejection, redirect relay, cancellation and partial-response failure tests. No caching. |
| 1e. Docker Engine acceptance and setup docs | Not started | Document proxy/CA setup and verify a real pull, CA trust failure, and CA persistence after restart. Record evidence and finish the plan's slice-1 checks. |

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

## Later slices: queued scope

Break these into concrete subslices before starting each slice, using the
implementation then available rather than speculating about file boundaries now.

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
