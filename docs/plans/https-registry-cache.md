# Go HTTPS registry pull-through cache

## Goal

Build a standalone Go binary that accelerates Docker Engine image pulls through an explicit HTTPS-intercepting forward proxy. Image names remain unchanged. Docker Engine hosts configure the proxy and trust its CA.

Slice 1 is implemented and validated. See the [operating guide](../operations.md)
and [implementation log](../implementation-log.md). Caching slices remain queued.

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

Add the exact-request upstream GET authorization path, safe redirects, filesystem entries, verified atomic fills, and cache substitution. Include hard capacity handling and basic eviction rather than temporarily allowing an unbounded cache.

Verify cold and warm pulls, same bytes/digest, reduced upstream body bytes on warm large objects, and one authorization request per client request. Test separate clients with allowed/denied/revoked access to the same warm entry. Include a fake upstream where HEAD is public but GET is protected: it must not leak cached content. Keep tag manifests uncached.

### 3. Digest manifests and generic protocol compatibility

Add digest-manifest caching with representation-aware keys. Test OCI and Docker manifest media types, image indexes/multi-platform images, Basic and bearer auth, separate token-service hosts, relative/absolute redirect chains, and cross-origin credential stripping. Keep HEAD/Range/conditional forwarding as explicit bypasses.

Verify that changing a tag is observed on the next pull, identical paths on two registry hosts cannot collide, negotiated representations cannot collide, and warm private content cannot bypass current registry permissions. Exercise Docker Hub and GHCR using credentials provided to the Docker client only; record the exact versions and tested flows.

### 4. Concurrency and operational hardening

Finish fill coalescing, follower cancellation, LRU/read pinning, crash recovery, disk-full/corruption behavior, bounds, structured diagnostics, metrics, graceful shutdown, and operator documentation. Validate restart/recovery under load before calling the implementation ready.

Measure concurrent large-image cold/warm pulls: peak memory, upstream bytes, latency, disk occupancy, and authorization request count. Tune from results; do not invent an unrequested throughput SLA.

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

The GET-authorization strategy is approved. Autonomous implementation of subslices 1a–1e is authorized; stop before caching and pause for material scope/security decisions or unresolved verification blockers.
