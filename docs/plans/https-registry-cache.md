# Go HTTPS registry pull-through cache

## Goal

Build a standalone Go binary that accelerates Docker Engine image pulls through an explicit HTTPS-intercepting forward proxy. Image names remain unchanged. Docker Engine hosts configure the proxy and trust its CA.

Slices 1 and 2 are implemented and validated. See the [operating guide](../operations.md)
and [implementation log](../implementation-log.md). Digest manifests and broader
operational hardening remain queued for slices 3 and 4.

## Agreed scope

- Standalone Go binary; no container packaging initially.
- Docker Engine is the first validated client.
- Generic Docker Registry HTTP API V2 support, not provider-specific routing. Docker Hub and GHCR are initial external compatibility checks, not the only permitted registries.
- Intercept only explicitly configured registry hosts. Tunnel other CONNECT destinations unchanged, without TLS inspection or caching.
- Generate and persist a CA on first startup; reuse it across restarts.
- No configured upstream credentials. Forward client authentication; never persist or log passwords, Authorization headers, bearer tokens, or signed redirect URLs.
- Require current upstream authorization for each cache hit, including hits on public content. Never share an earlier client's authorization decision.
- Authorize cache hits with a normal upstream GET, not HEAD; after an accepted matching response, close the upstream body and serve cached bytes. Upstream request latency, rate limits, and some transfer before cancellation remain.
- One proxy instance with local persistent disk storage; configurable maximum size, default 32 GiB, and least-recently-used eviction.
- Cache only digest-addressed blobs and manifests. Always resolve tag manifests upstream; no stale-tag or offline fallback.
- Pull-only on intercepted registry traffic. Reject registry mutations, including POST, PUT, PATCH, and DELETE.
- One YAML configuration file selected with `--config`; no rpardini environment-variable compatibility layer.

Opaque passthrough is an explicit exception to pull-only enforcement: the proxy cannot detect or block image pushes or other application traffic inside uninspected tunnels.

## Reference-derived requirements and deliberate differences

Source baseline: [rpardini/docker-registry-proxy at 7515d698](https://github.com/rpardini/docker-registry-proxy/tree/7515d698a10157e34acc3e4e4cd6957fab76729d). Its nginx and shell implementation is evidence of behavior, not a dependency or configuration contract for this project.

| Reference capability | First-version treatment |
| --- | --- |
| CONNECT proxy and selective HTTPS MITM | Preserve, implemented in Go. |
| Persistent CA and client trust setup | Preserve; generate per-host leaf certificates rather than a startup certificate covering every host. |
| Upstream TLS verification | Required; no insecure verification bypass. |
| Arbitrary configured registries and redirected layers | Preserve generically; constrain redirect schemes, destinations, hops, and credential forwarding. |
| Client credentials and optional central credentials | Client credentials only; Docker Engine owns login and token acquisition. |
| Shared private-image cache | Share stored bytes, but require fresh upstream permission before each delivery. |
| URI-only cache keys | Replace with registry/repository/object/representation-aware entries. |
| Digest blobs and digest manifests | Preserve, with verified complete bodies and atomic publication. |
| Optional tag TTL tiers and stale-on-error | Exclude: tags always go upstream and authorization failures never fall back to cache. |
| HEAD and Range handling | Preserve protocol behavior. Forward HEAD, Range, and conditional requests upstream initially; do not cache their responses. |
| Concurrent cache fills | Coalesce storage fills without sharing authorization decisions between clients. |
| Persistent size-bounded cache | Preserve; add explicit crash recovery, corruption handling, and disk-full tests. |
| Optional push forwarding | Exclude; intercepted traffic is pull-only. |
| General CONNECT passthrough | Preserve for unconfigured destinations. |
| Access logs and cache diagnostics | Structured redacted logs and bounded-cardinality cache/request counters. |
| nginx tuning, generated shell setup, debug MITM chaining | Do not reproduce. Provide manual Docker Engine setup documentation instead. |

Reference evidence: [proxy/auth/cache/redirect configuration](https://github.com/rpardini/docker-registry-proxy/blob/7515d698a10157e34acc3e4e4cd6957fab76729d/nginx.conf), [shared private-cache warning](https://github.com/rpardini/docker-registry-proxy/blob/7515d698a10157e34acc3e4e4cd6957fab76729d/README.md#gotchas), [defaults](https://github.com/rpardini/docker-registry-proxy/blob/7515d698a10157e34acc3e4e4cd6957fab76729d/Dockerfile), and [CA generation](https://github.com/rpardini/docker-registry-proxy/blob/7515d698a10157e34acc3e4e4cd6957fab76729d/create_ca_cert.sh).

## Request path and authorization

1. Accept CONNECT and parse its authority strictly. For an unconfigured destination, establish an ordinary byte tunnel. For a configured registry, terminate client TLS using a leaf signed by the persisted CA.
2. Bind the intercepted connection to its approved CONNECT authority. Reject conflicting TLS names or HTTP authorities rather than allowing a request to change its upstream destination.
3. Reject intercepted write methods. Forward read requests using verified upstream TLS. Strip hop-by-hop/proxy-only headers, preserve client authentication and representation negotiation, and relay authentication challenges.
4. Forward `/v2/`, tag manifests, authentication endpoints, and other non-cacheable requests without caching. A token service on an unconfigured hostname works through an opaque tunnel; it does not need automatic interception. Never fetch an arbitrary challenge realm on the client's behalf.
5. For a potentially cacheable GET, issue the normal upstream GET using that client's headers, even if a local entry exists. A `/v2/` success, parsed bearer scope, or another client's successful request is not authorization for the requested repository object.
6. Follow registry redirects with a bounded chain. Preserve GET semantics, verify TLS, and strip Authorization, cookies, and other origin-sensitive headers on origin changes. Do not leak registry credentials to storage/CDN hosts.
7. Only substitute cached content after a terminal upstream `200` and matching object/representation metadata. Use the current upstream response's relevant headers, not cached authentication or cookie headers. Close the unused upstream body promptly and serve the verified local body.
8. If no usable entry exists, stream the upstream response while filling a temporary cache object. Publish only a complete, length-checked, digest-verified result. A digest mismatch or interrupted download must never create a hit.

### Why GET rather than HEAD for cache-hit authorization

The [OCI distribution spec](https://github.com/opencontainers/distribution-spec/blob/main/spec.md) specifies object HEAD endpoints, but leaves authentication out of scope. It does not guarantee identical HEAD and GET authorization policies. The [Distribution token scope rules](https://github.com/distribution/distribution/blob/main/docs/content/spec/auth/scope.md) require repository/action-specific access checks.

Using the same GET that an uncached pull would issue delegates the permission decision to the upstream resource server without trusting an undocumented HEAD policy. A successful redirected GET also exercises the storage destination's checks before substitution.

This saves most response-body transfer on large warm objects, not upstream requests or authorization latency. Upstream servers may start sending data before cancellation; bytes transferred before cancellation and server-side work are not eliminated. It does not avoid registry request-based rate limits. Measure savings rather than assuming them.

Do not add authorization-result caching or a HEAD optimization in the first version. Authorization remains valid at the time the upstream accepts the request, just as for an ordinary download; this is not continuous revocation enforcement during delivery.

### Failure and bypass rules

- Relay `401` and its `WWW-Authenticate` challenges so Docker Engine can acquire a token and retry. Preserve meaningful upstream denial/not-found responses, including `403` and `404`.
- On upstream errors, rate limits, timeouts, TLS failures, or unreachable auth/storage services, never return cached bytes in place of the failure. Return the upstream response or an appropriate proxy error.
- If required cache-substitution metadata is absent or inconsistent, forward the upstream body normally; do not guess permission or representation from the cached entry.
- HEAD responses never create body entries. Range/conditional requests and non-identity content encodings are forwarded uncached initially. Preserve `206`, `304`, and `416` semantics without allowing partial or bodyless responses to poison full-object entries.
- Unsupported digest algorithms bypass caching. SHA-256 is the initial cacheable digest algorithm; generic registry support does not require accepting unverifiable content into the cache.
- Respect `no-store`; never cache authentication responses or error responses. Unknown `Vary` dimensions also bypass caching rather than being ignored.

## Cache design

Keep the first implementation filesystem-based, with no distributed storage or cross-registry deduplication.

- Key entries by canonical registry authority (including non-default port), repository, object kind, requested digest, and representation dimensions. Manifest negotiation includes the exact Accept value and observed media type. Query-bearing object requests bypass caching initially rather than silently dropping query parameters.
- Store immutable completed bodies with sidecar metadata: digest, length, media type, representation/key information, and last-use information. Do not persist credentials or transient redirect URLs.
- Use same-filesystem temporary files and atomic publication. Startup reconciliation removes abandoned fills and incomplete metadata/body pairs. Complete bytes without valid metadata are not usable entries.
- Detect missing/truncated/corrupt entries and treat them as misses. Revalidate persisted body digests before first reuse after restart; subsequent reuse assumes a private, process-owned cache directory. Check size on access.
- Track LRU usage in memory with bounded metadata updates. Evict completed unused entries to keep within the byte limit, accounting for metadata and in-progress reservations. Never evict a body pinned by an active reader.
- When there is insufficient cache capacity, or an object cannot fit, bypass admission and continue the authorized upstream transfer. Disk full must not turn an otherwise successful pull into a cache dependency failure.
- One active writer per cache key. Each concurrent client still obtains its own upstream authorization. Once authorized, a follower can wait for a completed fill; if that fill fails or waiting is not worthwhile, it performs a fresh upstream transfer rather than serving partial bytes.
- Bound fill time, concurrent fills, open connections, and memory. Do not buffer layers in memory. Cancel abandoned work when there are no interested clients; do not reproduce rpardini's unconditional continuation after disconnect.

HEAD/Range cache acceleration, content-addressed cross-repository deduplication, and more elaborate eviction policies can follow measured need. Correct forwarding comes first.

## Configuration, TLS, and operating boundary

The YAML schema covers listen address, registry authorities, cache directory/size, CA directory, and timeouts/resource bounds. Validate it at startup and reject unknown fields. Configuration is static until restart; hot reload and provider plugins are out of scope.

- Default the listener to loopback on port 3128. Serving other machines requires an explicit bind address and firewall/network restriction. There is no proxy-user authentication in this version; upstream image authorization does not secure the generic CONNECT relay against abuse.
- Intercept only configured authorities. Keep system DNS and IPv4/IPv6 support. Allow explicitly configured private registries; generic support must not silently exclude private address space.
- Allow ordinary HTTPS redirects to public CDN/storage destinations. Reject non-HTTPS redirects, loops, excessive chains, and implicit redirects into private/link-local/loopback destinations. Private redirect destinations must be explicitly configured. Apply address checks at connection time, including all selected DNS results, to resist rebinding; do not block only by hostname spelling.
- These redirect restrictions apply to proxy-originated fetches. User-selected unrestricted opaque tunneling is a broader egress capability and must be protected by deployment network controls.
- Persist the CA certificate and private key in a private directory with restrictive permissions. Initialize atomically; refuse to silently replace missing/inconsistent CA state on an existing installation. Never overwrite an existing key or silently rotate a trusted CA.
- Generate short-lived per-host leaf certificates using Go's TLS/X.509 support and a bounded in-memory certificate cache. Use modern TLS defaults and normal upstream certificate/hostname verification.
- Use a ten-year CA and 24-hour leaf certificates, renewing leaves under the same CA. CA replacement remains explicit.
- Startup reports the CA certificate path/fingerprint and fails clearly for unusable/expired CA material. Document manual trust installation and explicit CA replacement. Do not expose private key material or encourage downloading a CA over an unauthenticated network and trusting it blindly.
- Use structured logs with request outcome, registry, cache result, duration, and byte counts. Exclude credentials, signed query strings, raw configuration dumps, and response token bodies. Metrics should distinguish authorized hits, misses, bypasses, failures, fills, evictions, disk use, and actual upstream bytes; avoid per-image/token labels.
- Support graceful shutdown: stop accepting work, drain with a deadline, and remove incomplete fills. Run without root on the default port. Document single-writer ownership of the cache directory.

## Delivery slices

### 1. Working explicit proxy and CA lifecycle

**Complete:** subslices 1a–1e passed baseline checks, independent review, and real
Docker Engine acceptance using an authenticated local TLS registry. See the
[recorded evidence](../operations.md#acceptance-evidence-and-repeatable-checks).

Create the Go module, binary entry point, validated YAML configuration, CONNECT dispatcher, TLS interception, and transparent upstream forwarding. Enforce intercepted pull-only methods. Persist the CA and document Docker Engine setup. Keep ownership boundaries simple: configuration, proxy/TLS, registry policy, and disk cache; avoid provider adapters and generic plugin interfaces.

Verify with local TLS origin servers and a real Docker Engine pull: trusted CA succeeds, missing trust fails, invalid upstream TLS fails, configured hosts are intercepted, unconfigured hosts tunnel, mismatched authorities are rejected, mutation methods are denied, and restarting preserves the CA fingerprint. This slice performs no caching.

In slice 1, relay redirects unchanged to the client. Proxy-originated redirect following and its destination/credential protections arrive together in slice 2.

### 2. Authorized digest-blob caching

**Complete:** 2a–2d passed required checks, paired independent review, and real
Docker cold/warm/restart acceptance. Stored SHA-256 blobs reduce origin body
transfer while each request still obtains current GET authorization. See the
[measured evidence](../operations.md#acceptance-evidence-and-repeatable-checks).

Add the exact-request upstream GET authorization path, safe redirects, filesystem entries, verified atomic fills, and cache substitution. Include hard capacity handling and basic eviction rather than temporarily allowing an unbounded cache.

Verify cold and warm pulls, same bytes/digest, reduced upstream body bytes on warm large objects, and one authorization request per client request. Test separate clients with allowed/denied/revoked access to the same warm entry. Include a fake upstream where HEAD is public but GET is protected: it must not leak cached content. Keep tag manifests uncached.

#### Approved implementation sequence

Keep the existing CONNECT/TLS boundary and standard reverse proxy. Add registry
request/response policy under `internal/proxy` and filesystem ownership under
`internal/cache`; wire cache startup before opening the listener in
`cmd/docker-accelerator/main.go`. Do not introduce provider adapters or replace
the working forwarding stack. Each row is a separate implementation commit,
validated and reviewed before beginning the next row.

| Subslice | Outcome | Decisive validation |
| --- | --- | --- |
| 2a. Blob policy and safe redirects | Classify exact, query-free SHA-256 blob GETs, including image config blobs. Exclude manifests, HEAD, Range, conditionals, unsupported digests, and ambiguous paths. Follow eligible blob GET redirects with a fixed five-hop limit, verified TLS, loop rejection, connection-time IP checks, and origin-sensitive header stripping. Other request redirects remain client-handled. Add the approved private-destination configuration. | Table tests and fuzzing for paths/redirects; local TLS registry/CDN tests for relative and absolute locations, signed queries, same-origin and cross-origin headers, HTTPS-only policy, private addresses, mixed DNS results, rebinding, hop limits, and terminal denials. No disk caching yet. |
| 2b. Bounded verified disk store | Add private single-instance cache storage, immutable bodies and bounded sidecars, same-filesystem atomic publication, SHA-256/length verification, capacity reservations including metadata, basic LRU eviction, and reader pinning. Keep it unwired from request delivery until 2c. | Exact-byte store tests; capacity and concurrent admission assertions; duplicate-writer exclusion; cancellation, truncation, digest mismatch, write failure, pinned-reader eviction, startup cleanup, and first-reuse digest verification after restart. |
| 2c. Authorized cache delivery | Wire the store and response policy into forwarding. Always obtain the client's upstream GET and terminal response before substitution. Serve a pinned verified body only after matching metadata; otherwise stream upstream while optionally filling. Use fresh response headers; never persist credentials, cookies, or redirect URLs. Add redacted cache outcome and upstream-body-byte diagnostics. | Cold/warm exact-byte and origin-request assertions; separate allowed/denied/revoked users; public HEAD/protected GET; outage, TLS failure, 401/403/404/429/5xx; metadata mismatch, no-store, Vary, encoding, partial/conditional bypasses; registry/repository isolation; interrupted fills, disk failure, and shutdown cleanup. |
| 2d. Docker acceptance and operator handoff | Extend the compiled-binary fixture with a large deterministic incompressible layer and fresh Docker daemons for cold, warm, and restarted-proxy pulls. Update README, configuration example, and operating guide to describe actual cache behavior and limits. | Matching manifest/config/rootfs digests; fresh upstream blob GET on each delivery; materially fewer origin body bytes on warm pulls, including after proxy restart; unchanged CA identity and fresh tag resolution. Keep local HTTP security tests authoritative for exact denial and request-count cases Docker may retry. |

For every subslice, run `go test ./...`, `go test -race ./...`, and `go vet ./...`,
plus focused checks above. Use two fresh read-only reviewers on the same snapshot:
one with plan context, one with a code/test/diff-only bundle. Disclose the existing
limitation that ambient agent guidance cannot be fully isolated. Resolve findings
before committing; maintain the decision and implementation logs and push each
completed commit according to the repository rules.

**Minimum safety now, optimization later.** Slice 2 must already exclude competing
writers, reserve space before writes, pin readers, clean abandoned temporary state,
validate persisted entries before reuse, and abandon failed fills without breaking
an otherwise valid upstream transfer. These cannot wait until slice 4 once caching
is enabled. Concurrent requests for an already-filling key can transfer independently
without writing another cache entry; waiting/fill coalescing remains slice 4. Broader
crash/load testing, detailed metrics, and performance tuning also remain slice 4.
Use existing connection and idle-I/O bounds for client-owned fills; release storage
and writer ownership on cancellation or the shutdown deadline.

#### Approved slice-2 decisions

1. **Private redirect destinations.** Use a separate optional
   `redirect_allowlist` of canonical HTTPS authorities (including port), in addition
   to already configured registry authorities. This permits a private storage host
   without adding it to the TLS-interception registry list. It grants destination
   permission only: TLS verification and cross-origin credential stripping still
   apply. Public HTTPS storage needs no entry. Reject a DNS answer containing any disallowed destination address
   and dial only vetted addresses, keeping the original hostname for TLS checks.
2. **Warm-hit metadata contract.** Require terminal `200`, known
   matching Content-Length and media type, identity encoding, and no unsupported
   Vary dimensions or no-store directive. Key blobs by registry, repository, digest,
   exact Accept value, and observed media type. Verify every present
   Docker-Content-Digest against the requested SHA-256, but do not require that
   header at the final CDN: the original digest-addressed GET plus its accepted
   redirect chain identifies the object, and stored bytes were independently hashed.
   Missing required length/media type or inconsistent digest metadata means normal
   forwarding, not substitution.
3. **Cache enablement and startup failure.** Preserve forwarding-only
   mode when `cache.dir` is omitted, activating caching when it is supplied. An
   explicitly configured directory that cannot be safely opened should fail startup
   rather than silently disable caching; runtime capacity/write failures should only
   bypass admission. Keep `--check` side-effect-free. This preserves existing configs
   while making the example's existing cache directory effective.

The user approved these defaults and minimum-safety sequencing and authorized
2a–2d, per-subslice commits/pushes, and supervised local acceptance services. Stop
after slice 2, or earlier for a material scope/security change or unresolved required
check.

Acceptance runs use their own supervised isolated Docker 29.8.0 daemons and a
deterministic incompressible ~8 MiB layer. Per-path GETs and cancellation-aware
streamed origin bytes are asserted; warm authorization can transfer some bytes
before cancellation and need not transfer zero. Services are removed afterward.

### 3. Digest manifests and generic protocol compatibility

Add digest-manifest caching with representation-aware keys. Test OCI and Docker manifest media types, image indexes/multi-platform images, Basic and bearer auth, separate token-service hosts, relative/absolute redirect chains, and cross-origin credential stripping. Keep HEAD/Range/conditional forwarding as explicit bypasses.

Verify that changing a tag is observed on the next pull, identical paths on two registry hosts cannot collide, negotiated representations cannot collide, and warm private content cannot bypass current registry permissions. Exercise Docker Hub and GHCR with anonymous public pulls and client-owned token exchange; record the exact versions and tested flows. Credentialed authentication and private-access enforcement use deterministic local fixtures.

#### Approved implementation sequence

Autonomous implementation of 3a–3d followed by 4a–4e is authorized. Reuse the existing
reverse proxy, redirect protections, and verified disk store. Request classification
currently lives in `internal/proxy/blob.go`; eligibility is also consumed by
`forwarding.go` and `redirect.go`, and metadata/key construction lives in `cache.go`.
Generalize these together rather than adding a second cache or redirect stack.
Keep the existing blob key serialization and disk metadata format unchanged so
completed slice-2 entries survive the upgrade. No new configuration is proposed.

| Subslice | Outcome | Decisive validation |
| --- | --- | --- |
| 3a. Authorized digest-manifest caching | Generalize immutable-object classification and metadata to distinguish blobs from digest manifests. Admit the four media types below, reuse verified streaming fills and current-GET substitution, and extend safe redirect following to eligible digest-manifest GETs. Tags and all existing bypasses stay upstream-only. | Classification tables/fuzzing; cold/warm/restart exact bytes and fresh GET counts for all four types; deterministic headers-only warm origin proves substitution; digest mismatch/truncation never publishes; denied/revoked requests and missing/inconsistent metadata never hit; object-kind/registry/repository/Accept/media-type isolation; manifest redirect credential stripping; fixed legacy blob-key compatibility regression and existing blob suite. |
| 3b. Local protocol and authentication matrix | Add deterministic TLS registry/token/CDN fixtures covering Basic and bearer authentication, separate token hosts, negotiation, tag mutation, and redirected manifests. Keep token acquisition and refresh entirely client-owned. Add only production fixes demonstrated necessary by these tests. | Registry-enforced repository scopes, expired/revoked tokens, allowed/denied clients sharing warm content, and public HEAD/protected GET. Assert exact statuses/challenges/bodies and per-client origin GETs. Verify relative/absolute redirect chains, terminal denial, no-store on intermediate hops, no credential restoration after cross-origin hops, and HEAD/Range/conditional/query/encoding/Vary bypasses. An unconfigured TLS token hostname is reached through CONNECT, receives client-owned token requests, and causes no proxy-side realm fetch. |
| 3c. Docker multi-platform and mutable-tag acceptance | Extend the compiled-binary local fixture with OCI indexes and Docker manifest lists, valid platform-specific children, and distinct images behind a mutable tag. Retain Basic acceptance and exercise bearer acquisition through a separate token host with real Docker. | Fresh isolated daemons for cold/warm/restarted-proxy digest and tag pulls; exact selected child/config/rootfs digests and fresh upstream digest GETs. Change tag A to B with a different selected child/config/rootfs, prove the next pull observes B, and prove A remains retrievable by digest. Verify index/list platform selection without executing images or requiring emulation. Retain large-blob savings and CA-persistence checks. |
| 3d. Public registry compatibility and handoff | Run opt-in anonymous Docker Hub and GHCR public-image pull smoke tests using agent-selected references and client-owned token exchange; update README, configuration comments if needed, and operating instructions to match implemented behavior. | Record Docker/Go versions, date, registry, tag/resolved digest, platform, authentication mode, and cold/warm/restart outcomes without secrets. Local fixtures remain authoritative for permission, origin-count, and cache-substitution claims; public successful pulls alone do not prove cache hits. Record unavailable/rate-limited flows as blockers, not passes. |

Each row is one self-contained implementation commit. Format Go and run
`go test ./...`, `go test -race ./...`, and `go vet ./...` for each row, together
with its focused checks. Required Docker checks run opt-in with fresh daemon state;
the existing command is documented in the operating guide, and new test names and
timeouts will be recorded with their implementation. Go and Docker executables are
present in this orb, but the default Docker socket was unavailable during planning;
start and verify supervised services before promising acceptance results.

Use two fresh read-only reviewers on the same source/test/diff snapshot before each
implementation commit: one plan-aware and one given a code/test/build/diff-only
bundle. Ambient agent guidance cannot be fully isolated; disclose that limitation.
Resolve findings and rerun affected checks before advancing. Maintain the decision
and implementation logs, link completed commits, and push each completed commit
once execution/commit/push authorization is granted. The user has requested planning
slice 4 for a combined autonomous run and has now approved that run. Advance
past slice 3 only after its completion gate; pause
for a material scope/security decision or unresolved required check.

#### Decisions requested before autonomous implementation

1. **Manifest formats — approved; representation implementation as proposed.** Cache
   `application/vnd.oci.image.manifest.v1+json`,
   `application/vnd.oci.image.index.v1+json`,
   `application/vnd.docker.distribution.manifest.v2+json`, and
   `application/vnd.docker.distribution.manifest.list.v2+json` only. Forward legacy
   schema-1 and unknown media types normally without caching. Admit by the parsed
   media-type base; preserve the exact Content-Type (including valid parameters)
   and exact Accept value list as key dimensions. Do not rewrite JSON, convert
   formats, fetch child descriptors proactively, or add a second Accept-negotiation
   engine: the registry selects the representation and Docker validates it. Hash
   the actual complete body against the requested SHA-256; a transformed response
   with a different digest cannot be published. This bounds compatibility work
   without restricting ordinary forwarding.
2. **Manifest hit and redirect contract — approved.** Extend the
   approved blob contract unchanged: fresh client GET and terminal 200, matching
   known length/media type, identity encoding, acceptable Vary/no-store policy,
   and agreement of every present Docker-Content-Digest with the requested digest.
   Do not require a digest response header when absent, including at a final CDN;
   stored bytes are independently verified. Apply the same five-hop HTTPS,
   destination-checking, and cross-origin credential-stripping policy to eligible
   manifest GETs. Metadata gaps/mismatches bypass substitution; errors and denials
   never fall back. No HEAD optimization or authorization-result caching.
3. **External acceptance — anonymous public pulls and completion gate approved.**
   Anonymous public-image pulls are sufficient for Docker Hub and GHCR, including
   client-owned bearer-token exchange where required. Account login and private-image
   checks are not required. Deterministic local fixtures remain responsible for
   credentialed authentication, private-access denial, and revocation coverage. The user authorized
   agent selection of small, maintained public multi-platform images, recording exact
   references and resolved digests. Pull only; do not execute or publish images.
   Use isolated Docker-client state without account credentials; do not repurpose
   unrelated orb credentials. Approved gate: complete authorized local
   work first, but leave 3d/slice 3 incomplete and ask if image availability,
   network access, or rate limits block external evidence. Deferral needs an explicit
   exception rather than silently dropping the agreed public-registry checks.

Small manifests can transfer in full before upstream cancellation, so do not make
manifest bandwidth savings a Docker acceptance gate. Prove substitution with a
controlled HTTP fixture, and use Docker to verify protocol behavior and image
identity. Local bearer fixtures use a distinct unconfigured TLS token hostname,
private test-only name/trust configuration, and registry-side authorization checks;
the proxy neither parses tokens nor follows challenge realms. Concurrency waiting,
metrics, broader load/crash hardening, and performance tuning remain slice 4.

### 4. Concurrency and operational hardening

Finish fill coalescing, follower cancellation, LRU/read pinning, crash recovery, disk-full/corruption behavior, bounds, structured diagnostics, metrics, graceful shutdown, and operator documentation. Validate restart/recovery under load before calling the implementation ready.

Measure concurrent large-image cold/warm pulls: peak memory, upstream bytes, latency, disk occupancy, and authorization request count. Tune from results; do not invent an unrequested throughput SLA.

#### Existing foundation and approved sequence

Do not rebuild slice-2 safety mechanisms. `internal/cache/{store,fill,reader}.go`
already owns reservations, single writers, atomic publication, LRU/reader pins,
startup cleanup, and first-reuse digest verification. `internal/proxy/cache.go`
owns client-driven streaming and fill abort; `server.go` and the CLI already drain
connections on shutdown. `Store.Stats` exposes occupancy/reservations/evictions,
and `forwarding.go` emits redacted per-request logs. Extend these owners directly.

Missing work is bounded concurrent-fill waiting, independent fill/index bounds,
aggregate metrics, and recovery/resource evidence under concurrency. Current tests
simulate corrupt/incomplete state and use `/dev/full` for a store write failure;
they do not establish process-crash or real filesystem-exhaustion behavior through
the compiled proxy. Preserve Linux, private single-owner storage, and the existing
security contract. No distributed coordination, background prefetch, cache-serving
without current upstream authorization, or new storage backend is proposed.

| Subslice | Outcome | Decisive validation |
| --- | --- | --- |
| 4a. Explicit cache resource limits | Extend cache configuration/admission with the proposed active-fill, entry-count, and fill-lifetime limits below. Entry slots are reserved for fills, and both byte and entry constraints apply during startup and eviction. Timeout abandons only cache storage, not the valid upstream transfer. | Small-limit deterministic tests for concurrent admission, pinned/full cache, startup with reduced limits, expiry racing Write/Commit/Abort, and recovered capacity. Exact upstream bytes survive admission refusal/expiry; no completed partial entry. Side-effect-free --check and configuration validation/default tests. |
| 4b. Bounded authorized fill waiting | Add store-owned completion notification and proxy-owned bounded waiting using the model below. Keep the initiating request as fill owner; no detached download workers. Retry only the follower's own request when a wait cannot yield a verified entry. | Barrier-driven success, leader failure/disconnect, follower cancellation, wait timeout/disablement, commit/eviction races, shutdown, and mixed allowed/denied clients for blobs/manifests. Every client authorizes independently; fallback has exactly one additional full GET attempt and no repeated wait loop. Assert bytes, origin GETs, writer count, released reservations/connections, and no partial delivery. Run focused race repetitions. |
| 4c. Recovery and shutdown fault coverage | Exercise existing storage and connection ownership with abrupt process exit, interrupted publication, real bounded-filesystem exhaustion, corruption between restarts, and shutdown during fills/waits/readers. Fix demonstrated lifecycle failures in their owning modules. Keep fault injection test-local rather than adding a configurable filesystem layer. | Subprocess crashes at controlled fill/publication boundaries; restart exposes only complete verified objects and removes recognized abandoned state. Real ENOSPC in a private bounded test filesystem preserves otherwise valid upstream transfers and later admission recovers after space is reclaimed. Concurrent first-reuse verification/cancellation, eviction/read pinning, lock release, and shutdown leave no owned workers/fills/reservations after quiescence. Never alter unrelated files/services or exhaust the orb root filesystem. |
| 4d. Bounded operational metrics | Add the optional separate loopback metrics listener below; retain existing logs and add bounded wait/fallback/admission reasons. Instrument actual transitions, reuse Store.Stats, and keep CLI ownership of listener startup/shutdown. | Exact counters/gauges after cold/warm/bypass/denial/fallback/fill failure/eviction scenarios, including aborted requests and both GET attempts on fallback. Scrape while requests and shutdown run under race detection. Disabled mode opens no listener; invalid address/bind failure is clear; output/logs contain no credentials, paths, queries, digests, or unbounded labels. |
| 4e. Concurrent acceptance, measurements, and handoff | Run the approved bounded local workload matrix below with compiled binaries and fresh Docker client state. Measure cold/warm/restart behavior and recovery under load; update README, configuration example, operations, decision log, and implementation log with commands and evidence. | Exact image/object digests, independent authorization, warm body savings, enforced byte/entry/fill/connection limits, and resource return after quiescence. Record proxy peak RSS/heap, file descriptors/goroutines, latency distribution, logical reservations and actual filesystem allocation, origin GET/body counters, versions, and workload sizes. Repeat real Docker cold/warm/restart acceptance, now including concurrent clients. Required failures remain blockers rather than being hidden by skips. |

Run 4a–4e sequentially after completed slice 3. Each row requires gofmt,
`go test ./...`, `go test -race ./...`, `go vet ./...`, its focused checks, and the
same two-review snapshot gate described for slice 3 before its own implementation
commit. Resolve material findings with follow-up review, update docs when behavior
changes rather than waiting for 4e, and push each completed commit after execution
approval. A baseline test skip is not evidence that an opt-in fault/Docker check
passed. Unavailable privileges or required checks block advancement unless the user
grants an explicit exception. Combined authorization includes owned local fault-test
processes/filesystems and supervised acceptance services, with cleanup.

#### Slice-4 decisions to settle one at a time

1. **Fill ownership and waiting — approved.** Retain client-owned fills. A follower
   first obtains its own accepted terminal upstream GET and validates complete
   matching metadata. If that same object/representation has an active fill, close
   the follower's upstream body before waiting, freeing the origin connection.
   On completion, acquire a normal pinned verified reader and serve it with the
   follower's own accepted response headers. Never share authentication results or
   serve partially filled files. On timeout, failure, or eviction before acquisition,
   start one fresh GET from the follower's original request/headers through the
   entire redirect/authorization/metadata path, with no further wait loop. Relay
   any new denial/failure; do not reuse earlier permission as fallback authorization.
   A fallback may fill if admission allows, but cannot displace another writer.
   Leader disconnect aborts its fill and wakes followers; follower disconnect only
   cancels its wait. Followers do not keep a disconnected leader's download alive.
   This trades possible duplicate transfer and a second GET for bounded delay and
   simpler ownership. Background shared downloads and leader handoff are excluded
   from the approved model. The configurable `cache.fill_wait_timeout` defaults to
   1s; zero disables waiting. This bounds only the cache wait, not upstream GET or
   total pull duration.
2. **Resource defaults — fill/entry caps and lifetime approved.**
   The configurable `cache.max_fills` defaults to 16 globally. At the limit,
   additional misses stream uncached; hits and waiting on existing fills remain
   available. This limits cache writers, not total pulls; same-key waiters do not
   consume additional fill slots. The configurable `cache.max_entries` defaults to
   100000, in addition to the byte limit. Evict unused LRU entries at the cap;
   if pinned readers prevent admission, continue the new transfer uncached.
   The configurable `cache.fill_timeout` defaults to 10m per fill.
   These limits must be positive. Active fills
   reserve entry slots as well as body/metadata bytes; no admission queue is added.
   Expiry aborts storage and wakes waiters without killing the upstream transfer or
   starting another fill on that same response. Stop/join owned timer activity on
   completion/abort/shutdown. Regular-file kernel operations remain non-preemptible:
   neither fill expiry nor shutdown promises a hard deadline through a stuck
   filesystem. Existing network idle/connection bounds still apply. Keep the 32 GiB
   logical byte default and in-memory LRU with commit-time restart approximation;
   no per-hit metadata writes or automatic filesystem free-space reserve. The entry
   cap bounds small-object index growth but is not a fixed RSS guarantee; at 100k
   entries it can consume hundreds of MiB. Measure and document that tradeoff.
3. **Metrics interface — approved.** Expose standard Prometheus text at `/metrics`
   on an optional separate `metrics_listen` address, disabled when omitted. Require
   a literal loopback address and explicit nonzero port; example `127.0.0.1:9090`.
   No metrics on the CONNECT listener, public bind, pprof, admin API, or new auth/TLS
   subsystem. Use the standard library for this small fixed metric set. Include
   request outcomes/durations, authorized cache hits/misses/bypasses, upstream GET
   attempts and consumed response-body bytes, downstream bytes, fill/wait/fallback
   outcomes, evictions, active work, entries, charged bytes and reservations.
   Labels are fixed enums and, where useful, configured registry authorities only;
   never repository/digest/client/token/redirect-URL labels. Counters reset at process
   restart. Consumed bytes exclude network read-ahead and are not wire-byte savings.
   Listener startup failure fails startup; scrape failures do not affect pulls.
4. **Failure and performance acceptance — approved.** Use private disposable local
   fixtures, subprocess termination, and a size-limited filesystem in a private mount
   namespace for real ENOSPC; never fill the host/root disk or stop unrelated services.
   Validate capabilities before running, clean up all owned resources, and report
   process-crash tests as such—not power-loss or faulty-device certification. Exercise
   restart recovery, corruption, and shutdown with active fills, waiters, and readers.
   Missing required privileges or failed checks leave the subslice incomplete until
   resolved or explicitly waived. Approved performance acceptance: measure
   deterministic streamed HTTP workloads at 1/8/32 clients with 8 MiB and 64 MiB
   objects, using same-key and distinct-key cold/warm/restart phases, plus tiny-object
   churn for the entry limit. Use 1 and 4 fresh isolated Docker daemons for concurrent
   image acceptance. Retain the existing paced large-body savings gate; do not set
   a manifest savings requirement or a throughput/latency SLA. Report both wait-hit
   and timeout/fallback workloads so the bandwidth/latency tradeoff is visible.
   Bounded peak work and post-quiescence cleanup are correctness gates; measured
   latency/RSS are evidence, not an invented performance promise. Required failures
   or environment limitations leave the subslice incomplete until fixed or waived.

Implementation detail: observe a fill and subscribe to its terminal notification
atomically under the store lock, matching full metadata. Publish terminal state
before wake-up, never wait while holding a store/fill lock, and preserve existing
fill-to-store lock ordering. Eviction before a follower pins is an allowed miss,
not a reason to keep entries pinned for disconnected waiters. Reuse the normal
verified-read path. Avoid speculative LRU redesign or restart-hash coalescing;
change them only if tests/measurements demonstrate a concrete correctness or
resource problem. Reconcile these extension points with the actual completed
slice-3 code before starting 4a; ask if that requires a material plan change.

## Verification and completion criteria

Once implementation exists, the baseline checks are `go test ./...`, `go test -race ./...`, and `go vet ./...`. Use deterministic local registry/auth/CDN fixtures for failure and security tests; public registries are compatibility smoke tests, not dependencies of the unit suite. Fuzz CONNECT authorities, registry-path classification, and redirect handling.

Docker Engine end-to-end tests must use fresh client state between cold/warm pulls so Docker's local layer store cannot create a false proxy-cache success. Assert origin request/body counters, cache metrics, and image digests rather than checking logs for a HIT label alone. Confirm Docker daemon availability in the test environment before promising end-to-end execution.

The first version is complete when:

1. A Docker Engine client can pull through the binary using its own credentials and the installed CA.
2. Warm digest content demonstrably reduces upstream body transfer, with a fresh upstream authorization decision before every delivery.
3. Unauthorized, revoked, cross-repository, and cross-registry requests cannot obtain cached bytes; upstream outage fails closed.
4. Tag updates are immediately observable; representation negotiation and partial requests remain correct.
5. Cache entries survive restarts, partial/corrupt entries never become hits, and disk/memory usage remains bounded under concurrency and failure.
6. Documentation explains configuration, CA trust, generic tunneling risks, pull-only scope, absence of offline/rate-limit guarantees, and tested registry/client compatibility.

## Deferred

Container packaging, containerd/BuildKit-specific setup and validation, S3 or shared storage, multi-instance coordination, push passthrough, tag caching, stale/offline mode, proxy-held credentials, authorization-decision caching, environment-variable migration compatibility, automatic CA distribution, and automatic CA rotation.

The GET-authorization strategy and slice-2 decisions above are approved and slices
1–2 are complete. Slice-3 format, security, and external-acceptance decisions are
approved. The user requested planning slice 4 for subsequent autonomous execution
of both slices. Slice-4 ownership, resource defaults, metrics, and fault/load
acceptance decisions are approved. The user authorized combined autonomous execution,
per-subslice commits/pushes, paired reviews, anonymous public pulls, and isolated
supervised acceptance/fault/load resources with cleanup. The run ends after 4e and pauses
for material scope/security decisions or unresolved required verification blockers.
