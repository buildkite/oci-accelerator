# Implementation decisions

## Buildkite CI (maintenance)

- Use the existing `oci-accelerator` pipeline's repository-upload bootstrap and
  Open Source cluster `hosted` queue, verified through the Buildkite API. Its
  bootstrap is managed in `buildkite/saas-iac`; do not duplicate or modify it here.
- Run formatting/vet/build/example validation, default tests, and race tests as
  three independent bounded jobs. Pin the Docker plugin to v5.14.0 and use
  `golang:1.27.1-bookworm` to match the module/orb toolchain with a C compiler.
  Keep container environment forwarding narrow and prevent auto toolchain upgrades.
  No extra linter, release artifacts, deployment, or shared cache is needed.
- Container verification reproduced Git's dubious-ownership failure during Go's
  automatic VCS stamping. Disable stamping only for the disposable validation
  binary (`-buildvcs=false`); do not add Git safe-directory exceptions or change
  production build instructions. Agent v4's local dry-run also requires a token;
  document a dummy token plus unreachable endpoint so no credential is needed.
- Do not wire opt-in acceptance into a hosted job that cannot run it: the existing
  Docker, public, fault, and load harnesses depend on `amp orb service`, not merely
  a Docker socket. Retain their manual orb workflow and state the coverage gap.
  Porting service supervision is separate test-harness work, not a CI prerequisite
  for the existing default suite. Production/security behavior is unchanged.

## Project rename (maintenance)

- Full clean-break rename to OCI Accelerator: command `oci-accelerator`, module
  `github.com/buildkite/oci-accelerator`, metrics `oci_accelerator_`, and test/report
  environment variables `OCI_ACCELERATOR_`. No compatibility aliases.
- Rename only: preserve Docker Engine terminology, existing protocol/security
  guarantees, configuration/storage formats and paths, and cached data. New CAs
  use `OCI Accelerator CA`; existing certificates/keys and installed trust remain
  untouched, including CAs carrying the old name.
- User will rehome the repository on GitHub. Do not rename the hosted Amp project,
  change the Git remote, or push. Deliver one local implementation commit plus a
  log-only commit linking it. Preserve historical commit URLs and historical
  decision text; update current commands and operating instructions.

## Proxy review follow-up (maintenance)

- Fix findings from a deep `internal/proxy/` review on branch `fix/proxy-findings`
  as independent local commits; do not push. Order lowest-risk to highest-risk.
- A (#14): make production `manifestMediaTypes` the single source of truth and have
  tests reference it. Kept a plain package-private slice rather than a map or a
  copy-returning accessor: four entries, read-only usage, package-internal — a map
  would be over-engineering and a slice reads as the canonical list. Behavior is
  unchanged (exact string match on the `mime.ParseMediaType` base type). Also removed
  a second parallel copy in `TestManifestMetadataAdmission` (the exact drift #14
  targets) surfaced by code-only review.

- B (#11): rename `rawCONNECTValid` to `rawCONNECTAcceptable` with a doc comment
  stating its true contract — it returns false only for a syntactic single-space
  CONNECT request line whose target/Host is unparseable or disagrees, and true
  ("nothing to reject at the raw level") otherwise. Behavior is unchanged; the
  raw sniff is defense-in-depth ahead of net/http, which itself rejects malformed
  request-line spacing before ServeHTTP runs (verified), with validateCONNECT as
  the authoritative target/Host agreement check. Kept the single-space
  `strings.Split` as-is rather than tightening it: deferring irregular spacing is
  safe because net/http rejects it, so changing it would alter behavior for no
  security gain. Added TestRawCONNECTAcceptable covering the deferral cases and
  both false branches (unparseable target in a three-part line, unparseable/missing/
  duplicate Host, target/port mismatch).

- C (#1): restructure `fillingBody.Read` with early returns (fill already ended;
  context canceled) instead of three scattered `b.fill != nil` guards. Kept the
  logic exactly equivalent, including the `err == io.EOF && b.ctx.Err() == nil`
  re-check in the commit branch, so cancellation racing an in-flight Read still
  aborts rather than commits. The cache must never alter the bytes/errors the
  reverse proxy sees, never commit a canceled or truncated body, and abort on
  write failure or non-EOF error; all preserved. Added filling_body_test.go with
  a non-truncating scriptedReader driving commit, pre-cancel abort, non-EOF abort,
  oversize write failure, Close abort, a trailing zero-byte EOF, and a byte-level
  round trip through Store.Get.

- D (#13): give `dialUpstream` four internal refusal reasons (unparseable address,
  resolution failure, forbidden address, connection failure), each wrapping
  errRedirect via `fmt.Errorf("%w: ...")` so `errors.Is(err, errRedirect)` and the
  client-facing 502 are unchanged. A `refuseDial` helper logs the reason at DEBUG
  with only the upstream authority — never resolved IPs, credentials, or client
  data — consistent with the existing request log. The security boundary is
  untouched: validation still precedes any dial and the host is never re-resolved.
  Chose log-only reasons (no client-facing metric) as agreed. Per review, log the
  reason as a structured error value rather than its string, and give each subtest
  a fresh Proxy to remove shared-state/order coupling.

- E (#6): source the `docker_accelerator_heap_alloc_bytes` gauge from
  `runtime/metrics` (`/memory/classes/heap/objects:bytes`) instead of
  `runtime.ReadMemStats(&mem).Alloc`, removing a stop-the-world pause on every
  scrape. Go documents that metric as equivalent to `MemStats.HeapAlloc`/`Alloc`,
  so the reported value is materially the same live-heap-objects figure; the
  metric name and TYPE are unchanged so dashboards keep working. Alias the import
  as `runtimemetrics` to avoid colliding with the package's own `metrics` type,
  and return 0 if the metric is ever unsupported (KindBad) rather than panicking.
  `runtime.NumGoroutine()` (already non-STW) is retained.

- F (#9): the concern that trackedConn.rawOK/checked are read in ServeHTTP without
  synchronizing against the write in trackedConn.Read is not a real race: net/http
  reads a connection's request head (running Read, completing the sniff) and then
  dispatches ServeHTTP on the same connection goroutine, so the two accesses are
  ordered by program order. Confirmed no production change is needed; a reviewer
  empirically verified `-race` does instrument those fields. Added
  TestConcurrentCONNECTSniffingIsRaceFree as a regression guard: many simultaneous
  CONNECTs (valid to a signer-less configured registry -> 503, mismatched Host ->
  400) run under `-race` and would fail if a future change moved the rawOK read or
  write onto another goroutine. It targets the sniff path, not slot contention.

- G (#4/#5): investigate-first. Do not change the caching decision; add a
  measurement counter instead. Refactor `cacheHeaders` into `cacheHeaderRejection`
  (single source of truth returning "", "no_store", "vary", "digest_mismatch"),
  keeping `cacheHeaders` derived and behavior identical. On the bypass path record
  only "vary" or "digest_mismatch" into requestStats.reject for eligible objects,
  and expose `cache_rejections_total{reason=...}` plus a "reject" log field. The
  counter is deliberately an upper bound (headers would reject, regardless of any
  other independent bypass cause) — accurate enough to size the cache-effectiveness
  question before deciding whether to relax the rules in a future, separately
  approved change. Labels stay a fixed two-value enum; no host/digest/path leaks.

- H (#2): a hard-timeout Shutdown could return while a cache-hit request was
  still in flight, leaving its `*cache.Reader` pinned when the CLI's deferred
  `store.Close()` ran, so Close failed with "cache has active readers".
  Investigation showed the originally-planned "drain until p.conns is empty"
  approach is insufficient: p.conns tracks client-socket liveness, and closeAll
  removes a conn on the shutdown goroutine before the intercept goroutine unwinds
  inner.Serve, which is where the reader closes. Switched (with approval) to a
  sync.WaitGroup: intercept does Add(1) under p.mu while closing is false (so it
  is ordered before Shutdown's Wait) and Done after inner.Serve returns, by which
  point the reverse proxy has closed the reader. Shutdown waits on it — unbounded
  on the graceful path (already drained) and bounded by a 5s grace after closeAll
  on the timeout path so a wedged upstream cannot hang shutdown. Added
  shutdown_reader_test.go, a probabilistic reproducer (fails a majority of runs
  without the fix, passes consistently with it) asserting store.Close succeeds
  after a hard-timeout Shutdown with a pinned cache reader.

## Slice 1 authorization

- Implement 1a–1e autonomously, with per-subslice commits and independent review.
  Stop before caching; pause for material scope/security changes or blocked checks.
- Use a ten-year CA and 24-hour automatically renewed leaves. Trusted CA replacement
  remains explicit, avoiding repeated client trust distribution.
- Relay redirects to Docker in slice 1. Add proxy-side following and its destination
  and credential protections together in slice 2.

## 1a — configuration

- Use Go 1.27.1 and `go.yaml.in/yaml/v3` for strict single-document YAML; other
  implementation code uses the standard library. Reject unknown/duplicate fields.
- Require registry authorities and a CA directory. Resolve relative directories
  from the config file, not the process working directory. YAML null retains defaults;
  explicit zero resource limits and durations are invalid.
- Default to loopback port 3128, 10s dial/TLS timeouts, 30s response-header timeout,
  2m idle timeout, 15s shutdown, and 256 connections/leaf certificates. Cache settings
  are accepted for later slices but do not enable caching.
- Canonicalize authorities centrally so registry selection and CONNECT dispatch use
  the same identity. Configuration errors never echo YAML values.

## 1b — CA lifecycle

- Use ECDSA P-256 and publish a fully written, synced staging directory atomically.
  Initialization requires an absent CA directory and an owner-controlled parent;
  a sibling initialization lock prevents competing initializers. Existing incomplete,
  insecure, expired, mismatched, or symlinked material fails closed without replacement.
- Store `ca.key` with mode 0600 and `ca.crt` with mode 0644 inside a 0700 directory.
  The public certificate is separate from the private key for safe trust distribution.
- Keep a bounded FIFO leaf cache, synchronize issuance, and renew within one hour
  of expiry. Cap leaf expiry at CA expiry; reject issuance under an expired CA.
- After an interrupted first initialization, operators must inspect and remove a stale
  `<ca-dir>.lock` or `.ca-init-*` directory only after stopping all initializers.
  Never remove/replace an established CA as an automatic repair.

## 1c — CONNECT and connection ownership

- Accept only CONNECT on the outer HTTP listener; close every refused connection.
  Validate the wire Host as well as the target because Go normalizes CONNECT's Host
  from its request target. Keep the first-header inspection bounded to 64 KiB.
- Track and cap accepted sockets, including incomplete headers and hijacked tunnels.
  Preserve buffered tunnel bytes and TCP half-close; terminate both pumps on errors.
  Activity refreshes idle deadlines, so healthy long transfers are not time-limited.
- Wire CLI signal handling and deadline-bounded shutdown now; `--check` validates
  without opening sockets or creating CA state. Configured registries remain unavailable
  until interception is wired in 1d, never silently tunneled.

## 1d — intercepted forwarding

- Use Go TLS and HTTP/1.1 on intercepted connections. Bind SNI (when present) and
  every HTTP Host to CONNECT. Require origin-form targets to avoid absolute-URI
  Host normalization ambiguity. Allow bodyless GET, HEAD, OPTIONS only; deny upgrades.
- Use the standard reverse proxy with a direct, verified-TLS transport. Preserve
  raw paths/queries and authentication/challenges; strip hop-by-hop/proxy credentials
  and spoofable forwarding headers. Return redirects to the client without following.
- Bound response headers to 64 KiB, upstream sockets to 16 per registry and 32 idle
  globally (8 per registry). Bound idle I/O instead of total layer transfer duration.
  Stop keepalive on intercepted servers during shutdown and drain active responses.
- Log only configured registry, status, duration, bytes, and completion outcome.
  Do not log raw upstream errors, paths, queries, request headers, or token bodies.
  Startup reports the public CA path and SHA-256 fingerprint; `--check` is side-effect-free.

## 1e — Docker acceptance and operator handoff

- Exercise the compiled CLI with a deterministic authenticated TLS registry and a
  tiny valid single-layer image. Use a non-loopback registry address so Docker cannot
  silently bypass the proxy via its loopback exception.
- Use isolated supervised Docker daemons with fresh VFS stores for each phase,
  process-local trust roots, and private fixture-only Docker credentials. Leave
  existing Docker state and global trust untouched. Assert origin traffic and image
  digests, not just successful pull output or log messages.
- Keep privileged acceptance opt-in; default tests validate the image fixture and
  local protocol tests. Report the exact Docker version and distinguish correctness
  evidence from performance or public-registry compatibility claims.
- Document startup, manual trust, unrestricted opaque relay risks, runtime bounds,
  CA recovery/replacement, and no-cache limitations. Ignore example runtime state
  and local config so generated CA keys are not accidentally staged.

## Slice 2 authorization and 2a — redirects

- User approved 2a–2d, per-subslice validation/review/commits/pushes, and isolated
  supervised acceptance services. Stop before slice 3 or on unresolved blockers.
- Use a separate `redirect_allowlist`; it never adds intercepted CONNECT hosts.
  Configured registries also permit private destinations. Validate all DNS answers
  before dialing vetted IPs, retaining hostname verification, and cap chains at
  five redirects. Reject non-HTTPS, userinfo, fragments, and loops.
- Follow only eligible digest-blob GETs; preserve bypass redirect behavior.
  Cross-origin hops retain only Accept, Accept-Encoding, and User-Agent, so custom
  credential headers cannot leak. Never restore credentials after leaving an origin.
- Approved delivery contract: known matching length/media type and verified stored
  SHA-256; check any provided digest header, but do not require it from a CDN.
  Omitted cache directory keeps forwarding-only mode; unusable configured storage
  fails startup, while runtime admission failures do not fail upstream transfers.
- Bring minimum reader/writer/capacity/restart safety into slice 2; leave coalescing
  and broader operational/performance hardening for slice 4.

## 2b — bounded verified storage

- Use private 0700 entry directories containing 0600 body and JSON metadata files.
  Sync both files and their staging directory, then rename the directory atomically
  and sync its parent. A process-held advisory lock enforces one store owner.
- Reserve declared body length plus 4 KiB per entry before writing. The limit is
  logical cache bytes plus conservative sidecar reservations, not filesystem block,
  inode, or directory overhead; operators still need filesystem headroom. Oversized
  or unknown-length objects bypass admission, and failed cleanup retains its charge.
- Keep live LRU order in memory; restart uses commit times as an approximation.
  Read startup inventory in bounded batches and bound sidecar reads. Rehash bodies
  before first reuse after restart, outside the global lock with reader pinning.
- Storage remains unwired until 2c. Same-key concurrent fills bypass admission;
  active readers cannot be evicted. Runtime corruption/missing bodies become misses
  and permit a replacement fill. Unknown operator files are not automatically deleted.

## 2c — authorized delivery

- Retain the standard reverse proxy; a request transport follows safe redirects and
  substitutes only after the current client's terminal GET response passes metadata
  policy. Keys hash canonical registry, repository, blob kind, digest, exact Accept
  value list, and observed media type. No request headers or signed URLs are persisted.
- Honor no-store and digest inconsistencies throughout the redirect chain. Preserve
  original request bypass policy before hop-by-hop stripping. Vary supports Accept
  only; unknown dimensions, trailers, nonidentity encoding, and unknown lengths bypass.
- A response-body wrapper counts upstream bytes consumed and streams into one optional
  fill. Cache write/commit failures never change upstream bytes or stream errors.
  Close/cancellation abort incomplete fills; duplicate cold clients keep independent
  authorized transfers rather than waiting for storage coalescing.
- CLI owns optional store startup/cleanup. Cache initialization failure is sanitized
  and occurs before listen; --check creates no CA or cache files. Regular-file kernel
  stalls cannot be context-preempted; no unowned background writes are introduced.

## 2d — Docker cache acceptance

- Reuse the isolated compiled-binary fixture with deterministic ~8 MiB incompressible
  content. Use a fresh Docker VFS store for cold, warm, and restarted-proxy phases;
  preserve only the proxy's CA/cache state. Keep the tiny default image unit fixture.
- Pace large origin bodies in 32 KiB chunks and wait for handlers to finish before
  snapshots. Assert fresh GETs for layer and config, exact inspected image digests,
  tag resolution, and substantially reduced warm layer bytes. Do not claim zero
  upstream bytes, rate-limit avoidance, or an unmeasured throughput improvement.
- Retain opt-in privileged acceptance and clean up only owned supervised services.
  Security/denial matrices use deterministic local HTTP/TLS tests rather than
  depending on Docker's authentication retries for exact request-count assertions.

## Slices 3–4 authorization and 3a — digest manifests

- User authorized 3a–3d then 4a–4e, per-subslice commits/pushes and paired review,
  anonymous public pulls, and isolated supervised acceptance/fault/load resources
  with cleanup. Required blockers and material scope/security changes require a pause.
- Extend the existing object classifier, metadata and redirect path, preserving blob
  key serialization and disk format. Admit four OCI/Docker manifest/index media types;
  retain exact Accept values and raw Content-Type in keys without rewriting JSON.
- Keep current per-request GET authorization and verified-byte substitution unchanged.
  Missing digest headers are permitted; present ones must match. Tags stay uncached.
- Public Hub/GHCR checks need no account secrets; choose small maintained images,
  record resolved digests, and leave slice 3 incomplete if required external evidence
  is blocked. Local fixtures own credentialed denial/revocation evidence.

## 3b — protocol and authentication fixtures

- Use a distinct localhost token hostname with its own CA trusted only by the client;
  assert the original peer certificate through opaque CONNECT. The proxy only relays
  challenges, never acquires or interprets tokens. Fixture registry grants enforce
  repository, expiry via a deterministic clock, and revocation on every GET.
- Extend manifest-specific bypass, mutable-tag, representation, and redirect tests
  without changing production code. Keep replacement bodies the same length so
  individual policy/key dimensions, not accidental framing differences, are tested.

## 3c — real multi-platform and bearer acceptance

- Reuse compiled proxy and fresh VFS daemons for tag/digest cold, warm and restart
  phases plus tag mutation and old-digest retrieval. OCI indexes use OCI children;
  Docker lists use schema-2 children, with valid amd64/arm64 descriptors.
- Give registry and token service distinct private test hostnames via a bind-mounted
  hosts file in each test-owned service's mount namespace. Global DNS/trust is untouched.
  Docker trusts the token CA, but the proxy does not; successful token exchange must
  remain opaque, not intercepted. Test-only Basic credentials belong to Docker.

## 3d — anonymous external compatibility

- Select Alpine 3.22 on Docker Hub and Flux source-controller v1.7.4 on GHCR.
  Resolve tags on the cold pull, pin subsequent warm/restart pulls, and compare
  inspected root/config/rootfs identities. Use fresh private empty client config.
- Require completed cache entry population to establish proxy use, but keep public
  smoke evidence separate from local authorization-count and byte-substitution proof.
  Omit raw pull failure output because registries may include signed redirect URLs.

## 4a — cache resource bounds

- Extend store admission and startup inventory with independent fill/entry limits;
  reserve slots before creating files, preflight eviction capacity, and count failed
  cleanup conservatively. Preserve existing byte reservations and reader pinning.
- Each fill owns one deadline timer. Terminal writes, commits and aborts stop/join
  it outside the fill lock; expiry aborts storage without touching upstream delivery.
  Recheck deadlines before publication, including after filesystem operations.
- Keep defaults at 16 fills, 100000 entry slots and 10m lifetime. Limits constrain
  caching rather than successful authorized pulls, and do not preempt kernel I/O.

## 4b — authorized follower waiting

- Add store-owned terminal notifications matched against full metadata. Subscribe
  under the store lock; notify only after publish/removal, and acquire ordinary
  verified readers rather than pinning for waiters.
- The proxy closes each follower's independently authorized upstream body before
  waiting. A bounded second transport attempt restarts from the original request,
  repeating redirect and authorization policy without another wait loop.
- Preserve client-owned leader lifecycle. Default wait is 1s; `0s` disables it.
  Cancellation affects only that request; no detached workers or leader handoff.

## 4c — bounded fault evidence

- Kill an owned compiled proxy during an observed nonempty fill, restart the same
  cache/CA, then verify full bytes and headers-only warm substitution. Separate
  store subprocess cases cover actual active/committed API state and explicitly
  staged pre/post-publication disk states. This certifies neither power loss nor
  an interrupted real Commit at every filesystem instruction.
- Exercise actual ENOSPC inside a private 1 MiB tmpfs with 700 KiB ballast outside
  the cache. The valid 600 KiB transfer must survive cache failure; removing only
  test ballast restores admission and a fresh authorized warm hit.
- Corrupt files only while the store is closed. Concurrent first reads must reject
  corrupt bytes, valid readers remain pinned through admission pressure, and
  cancellation/close must release reservations and pins.

## 4d — operational metrics

- Use a separate disabled-by-default literal-loopback listener with only `/metrics`.
  CLI binds before readiness, drains/forces closure and joins the server. Keep
  metrics outside CONNECT routing and never add public/admin/profiling access.
- Emit stdlib Prometheus text with fixed outcome/status/admission enums only.
  Snapshot counters before network writes; count upstream GET attempts at dispatch,
  body bytes and request durations at handler completion, and fill lifecycle at the
  store transition. Counters reset on restart; no credential/path/digest labels.
- Reuse store occupancy and pin accounting. Add Go heap/goroutine gauges to support
  the approved load evidence; RSS and filesystem allocation are measured externally.

## 4e — concurrent acceptance and measurement

- Run the complete 1/8/32-client, 8/64 MiB same/distinct HTTP matrix with independent
  cache state per case. Warm and restart cases explicitly populate serially first,
  reporting those GETs/bytes separately; this isolates warm behavior from admission
  refusal in a concurrent cold fill. No warmup traffic is claimed as savings.
- Measure paced local streaming, not Internet throughput. HTTP uses a 5s wait to
  expose coalescing; a separate 1ms-wait profile exposes fallback. Docker retains
  the default 1s wait with 1/4 fresh VFS daemons for cold/warm/restart.
- Sample metrics and `/proc` every 50ms plus boundary samples; report sampled peaks,
  not guaranteed instantaneous maxima. Assert cache/connection limits, exact bytes,
  per-client authorization and quiescence; retained idle transport connections have
  explicit FD/goroutine allowances. Disk reports distinguish actual file lengths,
  allocated blocks (including directories), and the store's charged reservations.
- Keep latency and RSS as measured evidence, without tuning to an invented SLA.
  Unit/race tests remain the deterministic proof of cap saturation and lifecycle
  interleavings that periodic sampling alone cannot establish.
- Publish JSON only for complete passing matrices, using a same-directory atomic
  rename. Preserve any previous successful report on a failed run; operators use
  a fresh report directory and the test exit status, not file existence, as evidence.
  Do not silently delete previously collected results.

## Maintenance — re-admit invalidated entries and reclaim tombstones

- `Get`, `Completion`, and `Begin` disagreed about an entry marked `invalid` but
  still mapped (removal only happens in `unpin` once `pins==0` and `os.RemoveAll`
  succeeds). `Get`/`Completion` treated it as absent while `Begin`'s bare map
  membership test refused re-admission with `AdmissionExisting`, wedging the
  natural `Get`→`Completion`→`Begin` path used by `internal/proxy/cache.go` and
  causing a thundering herd with no caller able to re-admit. This is a bug fix
  outside the numbered implementation plan; scope is limited to `internal/cache`.
- Change A: `Begin` now treats an invalid tombstone as absent (`ok && !e.invalid`),
  realigning it with `Get`/`Completion`.
- Change B: `unpin` is identity-aware. Both the on-disk `os.RemoveAll(e.dir)` and
  the map delete happen only when the slot still points at this tombstone, because
  a superseding fill shares the same key-directory path and removing it would
  destroy the new entry's files. The charge reclamation (`s.bytes -= e.charge`) is
  made idempotent by an `entry.reclaimed` flag: the tombstone's charge was added
  when it was live and never removed at invalidation, so it must be reclaimed
  exactly once — by whichever of `unpin`, `Begin` (C1), or a superseding `Commit`
  reaches it first. A removal failure leaves the charge in place for a later
  retry, matching prior behavior.
- Change C1: before staging a new fill, `Begin` reclaims a same-key unpinned
  invalid tombstone by retrying `os.RemoveAll` and, on success, dropping the slot
  and reclaiming its charge under the already-held `s.mu`. On persistent failure
  it does not error; it falls through to Change A so the fill overwrites at commit
  and later unpin/eviction reclaims the bytes. This closes the budget-leak window
  on the re-request-after-invalidation path and frees the slot/bytes before the
  subsequent `evictLocked`/admission.
- Commit rename hardening (required by Change A): a still-pinned invalid tombstone
  keeps its directory at the destination key, and `os.Rename` will not replace a
  non-empty directory. An early design cleared the destination but left the
  tombstone mapped, which both independent reviews flagged as a data-loss race: the
  rename and the map install ran in separate `s.mu` critical sections, so a
  concurrent `unpin`/eviction could `os.RemoveAll(dest)` between them and delete the
  just-published content (the identity guard protects the map slot, not the on-disk
  path a rename has repointed). The fix (`supersedeTombstone`) detaches the
  tombstone from the map, reclaims its charge (guarded), and removes its directory
  atomically under `s.mu` before the rename. Once the tombstone leaves the map no
  other path targets `dest`, so the unlocked rename is safe. It fails closed if a
  valid entry unexpectedly occupies the key or if removal fails. Readers holding
  the old body keep their open descriptors after the unlink and still serve the
  original bytes. `TestReadmitCommitReaderCloseRace` stresses this interleaving
  under `-race`.
- Reviewer follow-up (invariant uniformity): `evictLocked` now sets
  `entry.reclaimed = true` when it removes an entry, so "every path that detaches
  an entry from the map reclaims its charge exactly once" holds for eviction too.
  Eviction only selects `pins == 0` victims, so no later unpin can observe the
  entry; the flag is defensive and does not change accounting or eviction choice.
- Finding #5 deferral: surfacing `failedSlots` and leaked-byte totals in `Stats`
  for operator observability is intentionally deferred to a later slice. A code
  comment marks the deferral near the `failedSlots` bookkeeping in `fill.go`.
- Verification note: `TestFillFailuresNeverPublish/disk-full` requires the Linux
  `/dev/full` device, which is absent on darwin; that subtest fails on the
  untouched baseline as well and is unrelated to this change.

## Config validation hardening (post-slice-4 maintenance)

Three independent bug fixes outside the numbered plan. Each tightens validation to
match slice 1a's documented intent — reject malformed configuration — without
changing any security boundary or agreed scope.

### Reject non-integer scalars for numeric limits

- A YAML float silently truncated into the integer limit fields (`max_bytes`,
  `max_fills`, `max_entries`, `max_connections`, `max_leaf_certificates`): the
  scalar was decoded as a plain `int`, so `1.9` loaded as `1`. Only `duration`
  guarded its scalar tag.
- Add strict tagged integer types (`strictInt64`, `strictInt`) mirroring
  `duration.UnmarshalYAML`: a `!!null` node preserves the default, any non-`!!int`
  tag is rejected, and the value is parsed with decimal `strconv.ParseInt`, making
  overflow rejection explicit.
- Restrict accepted `!!int` forms to canonical decimal. The library tags `0xff`,
  `0o17`, `017`, and `1_000` as `!!int` but with a `node.Value` that base-10
  `ParseInt` rejects — except `017`, which base-10 parses as `17`. Silently
  reinterpreting an operator's `017` (octal 15) as decimal 17 is itself a trap, so
  `isDecimalMagnitude` also rejects a leading-zero magnitude. Legitimate config is
  decimal; this favours a clear error over a surprising reinterpretation.
- Keep the existing `> 0` range checks as defense-in-depth. Convert back to
  `int64`/`int` only when building `Config`. The int64→int narrowing in `strictInt`
  is a no-op on the 64-bit build target (int spans the validated int64 range);
  32-bit builds, not a deployment target, are documented as out of scope rather
  than guarded with untestable code.

### Reject null entries in registries and the redirect allowlist

- A `!!null` element in `registries` or `redirect_allowlist` was silently dropped:
  decoding into `[]string` skips nulls before `parseAuthorities` runs, so
  `registries: [example.com, null]` loaded a single registry with no error. A typo
  that quietly narrows the trusted registry or redirect set is exactly the kind of
  misconfiguration validation must reject.
- Decode both lists into `[]*string`, where a YAML null element becomes a nil
  pointer that the decoder preserves. `parseAuthorities` now rejects a nil element
  with `ErrInvalid` and otherwise dereferences and calls `hostport.Parse(_, "443")`
  exactly as before, so an empty string still fails as an invalid authority and the
  canonicalization and duplicate checks are unchanged.
- Errors continue to surface only `ErrInvalid`, never the offending element, so a
  redirect destination or registry name cannot leak through a configuration error.

### Canonicalize listen and metrics bind addresses

- `listen: 'EXAMPLE.com.:3128'` was accepted but `Config.Listen` stored the raw
  string, inconsistent with the canonicalized `Registries`. `validate` also parsed
  the port twice and gated host validation on `host != ""`, so the port was never
  normalized and the two code paths could diverge.
- Extract the host canonicalization already inside `hostport.Parse` into an
  exported `hostport.Canonicalize`, and have `Parse` call it so its observable
  behavior is unchanged (all existing hostport tests pass verbatim). This gives the
  config package the same lowercase, trailing-dot, and IP-literal canonicalization
  the registry list uses, from one source of truth.
- Replace the duplicated listen handling with a single `canonicalBindAddress`:
  split once, range-check the port, re-emit it without leading zeros, and rebuild
  with `net.JoinHostPort` so `127.0.0.1:03128` becomes `127.0.0.1:3128`. The host is
  canonicalized only when it is neither empty nor a wildcard, detected with
  `net.ParseIP(host).IsUnspecified()` (covers `0.0.0.0` and `::`). Empty (`:3128`)
  and wildcard (`0.0.0.0:3128`, `[::]:3128`) binds are preserved verbatim because
  `main.go` passes `Listen`/`MetricsListen` straight to `net.Listen`, so they must
  remain valid tcp bind targets.
- Apply the same routine to `MetricsListen` while keeping its stricter rule: the
  host must be an explicit loopback IP literal with no `%` zone. Canonicalizing the
  loopback literal and normalizing its port keeps metrics consistent with `listen`
  without loosening the loopback-only guarantee.
