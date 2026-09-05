# Implementation decisions

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
