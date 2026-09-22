# Architecture and security

OCI Accelerator is a standalone explicit HTTPS proxy with a local disk cache for
immutable registry content. Docker Engine keeps its image names, credentials, and
token acquisition. The proxy can briefly reuse a narrowly keyed upstream approval
for the same request; it never interprets credentials or shares approval between
different request fingerprints.
See the [operating guide](operations.md) for configuration and deployment.

## Code ownership

| Package | Responsibility |
| --- | --- |
| `cmd/oci-accelerator` | CLI, startup, listeners, signals, and shutdown ownership |
| `internal/config` | Strict single-document YAML, defaults, validation, and relative paths |
| `internal/hostport` | Canonical host/port identity for configuration and requests |
| `internal/ca` | Persistent root CA and bounded per-host leaf certificate cache |
| `internal/proxy` | CONNECT, TLS interception, forwarding, authorization/redirect/cache policy, and metrics |
| `internal/cache` | Private single-owner storage, verified fills/readers, capacity, eviction, and recovery |
| `integration` | Compiled-binary Docker, fault, public-registry, and load checks |

The forwarding path uses Go's standard HTTP reverse proxy and TLS transport.
YAML decoding is the external runtime dependency; there are no provider adapters
or separate cache backends.

## Connection and request flow

1. The outer HTTP listener accepts CONNECT only. It validates the target and wire
   Host, tracks accepted sockets, and enforces the connection limit.
2. An unconfigured authority gets an opaque byte tunnel. The proxy does not
   inspect its TLS, cache its content, or enforce pull-only methods inside it.
3. A configured registry gets a leaf certificate signed by the persisted CA.
   Client TLS SNI, when present, and every intercepted HTTP authority must agree
   with CONNECT. The connection cannot switch upstream authorities.
4. Intercepted requests use HTTP/1.1 and origin-form targets. Only bodyless GET,
   HEAD, and OPTIONS are allowed; mutations and upgrades are rejected.
5. The proxy forwards through verified upstream TLS, stripping hop-by-hop,
   proxy-only, and spoofable forwarding headers. Client authentication and
   upstream challenges remain part of the ordinary registry exchange.
6. Eligible immutable-object GETs pass through redirect and cache policy below.
   Other traffic, including tags, `/v2/`, HEAD, Range, and conditional requests,
   goes upstream without cache substitution; its redirects remain client-handled.

Bearer tokens are opaque to the proxy. It does not parse their scopes, acquire or
refresh tokens, fetch challenge realms, or hold central registry credentials.
An unconfigured token-service authority is reached through an opaque tunnel.

## Authorization before cache use

Only full, immutable, digest-addressed blob and manifest GETs can use an
authorization grant. Before the first reuse, the client's ordinary GET follows the
complete redirect chain and must end in an eligible `200`; a `/v2/` request, HEAD,
partial request, tag request, or another fingerprint's approval grants nothing.
Registries need not authorize HEAD and GET alike, so approval always originates
from the same GET that would otherwise supply the body.

The process computes an HMAC fingerprint over the original canonical URL and all
forwarded end-to-end request headers. Header names are canonicalized, while exact
values and their order are retained. Anonymous requests are explicitly distinct
from authenticated requests. The HMAC key is process-local and never persisted;
restart therefore clears every grant without retaining credentials, raw header
values, signed redirect URLs, or reversible cache keys.

`cache.authorization_ttl` defaults to `30s`; `0s` disables grant reuse and retains
the previous guarantee that every cache delivery performs an upstream GET.
`cache.max_authorization_entries` defaults to 10000 and must be positive. Grants
are memory-only and bounded by that limit. Their TTL starts at upstream approval
and is never renewed by hits. There is no background refresh or authorization
request coalescing. Tokens remain opaque, so a token revoked or expired after
approval may continue to authorize an exact-fingerprint hit until the grant's
original expiry.

Approval is conservative across every redirect hop. Any `no-cache`, `no-store`,
`private`, `min-fresh`, `Pragma`, or otherwise nonreusable HTTP freshness restriction
prevents a grant. Only the explicit `max-age` and `s-maxage` ceilings can shorten the
configured TTL; restrictions at any hop apply. `Age`, `Date`, and `Expires` are not
consulted, so a grant never outlives the configured TTL but is not further reduced by
upstream age: cached objects are immutable and digest-verified, and the short TTL bounds
any over-trust. Any hop containing
`Set-Cookie`, `Authentication-Info`, `Proxy-Authentication-Info`, or `WWW-Authenticate`
also prevents reuse.

A fast hit requires both a live exact grant and a present, verified body. It returns
that body with only reconstructed Content-Type, Content-Length,
Docker-Content-Digest, a current Date, and `Cache-Control: private, no-store` so
downstream caches cannot extend the authorization lifetime. If the body is absent or
cannot be verified, the request goes upstream; a grant is never a body or failure
fallback. Existing body-cache admission and request-bypass rules are unchanged.

An upstream `401`, `403`, `404`, or `410`, or another authoritative response that
cannot establish reuse, invalidates the exact grant. Timeouts, DNS or TLS errors,
`429`, and `5xx` responses preserve an existing grant only until its original
expiry, but the failed request itself is returned: it never falls back to the
cached body. TLS verification is never disabled.

## Redirect boundary

Eligible SHA-256 blob and digest-manifest GETs follow at most five redirects,
including when the response cannot be stored. Locations must resolve to HTTPS
without userinfo or fragments; loops and invalid locations fail the request.
TLS certificate and hostname verification are never disabled.

Public HTTPS storage destinations require no allowlist entry. Private and other
non-public destinations require a configured registry authority or an entry in
`redirect_allowlist`, including the port. The allowlist permits a destination;
it does not enable interception or waive TLS verification. Before dialing, the
proxy checks the entire DNS answer and connects only to vetted IPs without
resolving again, while retaining the hostname for TLS verification.

On an origin change, only Accept, Accept-Encoding, and User-Agent survive. An
allowlist of retained headers prevents custom credential headers from leaking,
not just standard Authorization or Cookie headers. Credentials are not restored
if a later hop returns to the original origin. Referer is removed on redirects.
Cache and grant restrictions on any intermediate response apply to the complete
chain; terminal success cannot erase them.

These restrictions protect proxy-originated fetches. They do not constrain the
broader egress available through opaque CONNECT tunnels: deployment network
controls remain essential.

## Cache eligibility and representation

Only exact, unescaped, query-free SHA-256 blob and digest-manifest GET paths are
eligible. Tags, unsupported digest algorithms, Range, and `If-*` requests bypass
caching. The original request's bypass policy survives hop-by-hop header stripping.

Response admission and substitution require:

- terminal `200`, known Content-Length, no Content-Range or trailers;
- absent or identity Content-Encoding, without transport decompression;
- a single parseable Content-Type of at most 1024 bytes;
- no no-store directive and no Vary dimension other than Accept;
- every present Docker-Content-Digest value exactly matching the requested digest.

A digest response header is not required: the original digest-addressed GET and
accepted redirect chain identify the object, and stored bytes are independently
hashed. A present but inconsistent header prevents caching.

Digest manifests additionally require one of these parsed media types:

- `application/vnd.oci.image.manifest.v1+json`
- `application/vnd.oci.image.index.v1+json`
- `application/vnd.docker.distribution.manifest.v2+json`
- `application/vnd.docker.distribution.manifest.list.v2+json`

Legacy schema-1 and unknown manifest formats are forwarded without caching. The
proxy does not rewrite JSON, convert representations, prefetch children, or choose
platforms; the registry and Docker perform negotiation and image selection.

Keys hash canonical registry authority, repository, object kind, requested digest,
the exact Accept value list, and the exact response Content-Type, including valid
parameters. Stored metadata contains only the key, digest, length, media type, and
commit time. There is no cross-registry or cross-repository deduplication.

## Fill and reader ownership

A miss streams the upstream body to the client while optionally mirroring it to
one temporary fill per key. Layers are not buffered in memory. Publication requires
clean EOF on a live request, exact declared length, and a matching SHA-256. Cache
write, capacity, expiry, and publication failures abandon storage without replacing
the valid upstream stream's bytes or errors.

The initiating request owns the fill. Matching followers that are not fast grant
hits first obtain their own accepted GET and metadata, close their upstream bodies,
and wait up to `cache.fill_wait_timeout`. A completed fill supplies an ordinary
pinned, verified reader. Waiters do not see partial files.

Timeout, failed fill, or eviction before reader acquisition triggers one fresh
GET from the follower's original request through the full authorization and
redirect path, with no second wait loop. New denials are relayed. Leader
disconnection aborts its fill and wakes followers; follower cancellation only
ends its own request. There are no detached downloads or leader handoffs. This
keeps ownership bounded at the cost of possible duplicate transfer and a second GET.

## Storage, capacity, and recovery

The cache uses private directories, regular body/metadata files, and a
process-held advisory lock. One process owns the directory. Temporary fills and
completed entries share a filesystem; body, metadata, and directory synchronization
precede atomic rename, followed by parent-directory synchronization.

Admission reserves declared body bytes plus 4 KiB per entry and a slot before
writing. Limits cover logical bytes, entry slots, active writers, and fill lifetime,
not filesystem block/inode overhead or a fixed process RSS. Failed cleanup keeps
its byte/slot charge until removal succeeds. Pinned readers prevent normal eviction;
invalidated readers retain their byte charge until they close, even if a replacement
is published. Old readers cannot remove a newer entry at the same key.

Eviction is best-effort LRU with in-memory recency. Each admission makes at most
eight eviction removal attempts, separately from up to eight pending-cleanup
retries. If all currently eligible victims together cannot make the request fit,
it evicts nothing. Otherwise it removes a bounded batch even when that batch
cannot admit the fill. Successful removals count as evictions; the response
bypasses storage if capacity remains insufficient. Repeated requests can reclaim
more space, but competing traffic may consume it: admission is not guaranteed.
A shared cursor gives later candidates a turn after partial or failed batches,
so eviction is not strict LRU. Startup eviction is uncapped.
Startup sorts its inventoried entries when pressure requires eviction and once
more to bulk-build the live order. That incrementally maintained order avoids
sorting on each later admission, but candidate scanning and directory removal
are synchronous and admission-serialized while directory I/O runs outside the
bookkeeping mutex. Claimed victims remain charged and cannot be pinned or selected
again. Unrelated hits and statistics therefore continue during blocked filesystem
calls; attempt bounds are not elapsed-time guarantees. See
[cache benchmarks](../internal/cache/benchmarks.md).

Startup inventories entries in bounded batches, removes recognized abandoned
fills and discardable incomplete entries, and rejects unknown or unsafe state
rather than blindly deleting it. LRU order is reconstructed approximately from
commit times; hits do not write metadata to disk.

Persisted bodies are rehashed on first reuse after startup, with one verifier per
entry and cancellable followers. Later reads check file type and size but rely on
the private process-owned directory for integrity. Missing, truncated, or detected
corrupt entries become misses and may be replaced. Do not modify a live cache.

## CA and deployment boundary

The proxy creates a ten-year ECDSA P-256 CA only when its configured directory is
absent. It stages both files and publishes them together; an existing incomplete,
unsafe, incompatible, or expired CA is an error, not a reason to rotate trust.
Concurrent initializers adopt a valid published winner without a persistent CA lock.
Operators provision the trusted parent directory and distribute only the public
certificate through a trusted channel, checking the startup fingerprint.

Per-host leaves last at most 24 hours, are capped by CA expiry, and renew within
one hour of expiry. The bounded leaf cache uses LRU. Detailed filesystem,
cryptographic profile, and API contracts live in the [CA package guide](../internal/ca/README.md).

The listener defaults to loopback and has no proxy-user authentication. Upstream
image authorization does not protect an open CONNECT relay against abuse. Use
private network access controls for remote clients. The optional metrics listener
is separate and literal-loopback-only; counters use fixed labels without sensitive
request identifiers. See [logs and metrics](operations.md#logs-and-metrics).

## Limits of support and verification

The CLI requires a nonblank cache directory; there is no configuration switch for
forwarding-only operation. Configuration is static until restart. Linux Docker
Engine is the validated client; filesystem ownership depends on Unix semantics
and numeric limits target 64-bit builds. Docker Desktop, Windows, containerd, and
BuildKit-specific integration are not validated.

There is no shared/distributed storage, multi-instance coordination, background
prefetch or grant refresh, tag caching, stale/offline mode, proxy-held credentials,
automatic CA distribution/rotation, or intercepted push support.
The repository provides a Go binary, not container packaging.

Deterministic local registry/auth/CDN tests assert bytes, denials, and upstream
request counts. Docker acceptance uses fresh daemon stores to distinguish proxy
hits from Docker's own cache. Public Docker Hub/GHCR pulls are compatibility
checks, not proof of private access enforcement or warm-hit substitution.
Fault tests exercise process termination and private tmpfs exhaustion, not
power-loss or faulty-device certification. Load checks measure local paced
workloads without establishing a throughput or latency SLA. Regular-file kernel
operations cannot be preempted by fill expiry or shutdown deadlines.

See [repeatable checks](operations.md#repeatable-checks) for commands and prerequisites.
