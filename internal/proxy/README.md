# `internal/proxy`

The explicit HTTP `CONNECT` forward proxy. It terminates TLS for explicitly
configured registry authorities, forwards their requests upstream with bounded
redirect following and SSRF-hardened dialing, serves an authorized blob and
manifest cache, and tunnels every other destination opaquely. This document
maps the package for developers. For the request flow, security boundaries, and
design rationale, see the
[architecture guide](../../docs/architecture.md#connection-and-request-flow).

## Files

| File | Responsibility |
| --- | --- |
| `server.go` | `Proxy` type, `New`, `Serve`/`Shutdown`, and `ServeHTTP` CONNECT dispatch/validation |
| `connections.go` | Connection tracking (`trackingListener`, `trackedConn`), connection limits, raw-CONNECT header inspection, and connection context |
| `relay.go` | The opaque bidirectional TCP relay, with an explicit idle timeout and ownership of both connections until its workers finish |
| `intercept.go` | MITM interception of configured registries: hijack, terminate TLS with a CA-signed leaf, and serve the inner HTTP server over a single-connection listener |
| `forwarding.go` | The inner reverse proxy to upstream registries: request validation, header stripping, and the logging/metrics response writer |
| `registry_transport.go` | Upstream `RoundTrip`, bounded redirect following, cross-origin header stripping, and fill-wait retry |
| `dial.go` | SSRF-hardened DNS resolution/dialing (`dialUpstream`, `publicIP`) and upstream idle deadlines |
| `cache_policy.go` | Immutable OCI object recognition, request/response cache eligibility, media types, and metadata/key derivation |
| `cache.go` | Hit/miss/fill orchestration and `waitForFill` coalescing after the caller's accepted upstream GET |
| `cache_body.go` | Streaming upstream-byte accounting and cache-fill commit/abort ownership |
| `metrics.go` | Per-request statistics and Prometheus text-format metrics: recording and rendering |
| `errors.go` | Upstream error sentinels, dial-refusal reasons, and fixed, non-leaking log labels |

## Public interface

### `New(cfg config.Config, signer *ca.Authority, logger *slog.Logger, store *cache.Store) *Proxy`

Constructs a proxy from resolved configuration, a CA for interception leaves, a
logger, and an optional cache store (a nil store disables caching). It does not
bind or serve.

### `(*Proxy) Serve(listener net.Listener) error`

Accepts and handles `CONNECT` connections on `listener` until `Shutdown`.

### `(*Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request)`

The `CONNECT` dispatch entry point (registry interception versus opaque relay).
Exported to satisfy `http.Handler`; normally driven by `Serve`.

### `(*Proxy) Shutdown(ctx context.Context) error`

Stops accepting, disables intercepted keep-alives, drains in-flight work until
`ctx` expires, then closes remaining sockets.

### `(*Proxy) MetricsHandler() http.Handler`

Returns the Prometheus text-format handler for the optional loopback metrics
listener.

## Request flow

A client issues `CONNECT host:port`. `ServeHTTP` (`server.go`) validates the
request, then dispatches on the target:

```
CONNECT host:port
  configured registry?
    yes -> intercept: hijack, TLS-terminate with a CA leaf, serve an inner
           HTTP server whose handler is the reverse proxy (forwarding.go)
             -> registryTransport.RoundTrip (registry_transport.go): dial
                with SSRF guard (dial.go), follow redirects, then consult
                the cache (cache.go)
    no  -> opaque TCP relay (relay.go): bidirectional copy, no inspection
```

Only requests to configured registries are intercepted. Everything else is a
byte-for-byte tunnel: the proxy never inspects, caches, or enforces methods
inside it.

Downstream response writes refresh the configured idle write deadline, including
informational headers such as `103 Early Hints`, final headers, body writes, and
flushes. A client that stops reading cannot hold a header write indefinitely;
the deadline is not an absolute limit on the duration of a streaming response.

## Caching

The cache only stores immutable, digest-addressed objects. `cache_policy.go` recognizes
`GET /v2/<repo>/{blobs,manifests}/sha256:<digest>` requests with no range or
conditional headers, derives a metadata key, and checks response eligibility
(media type, encoding, and cache-control/vary/digest headers). `cache.go` either
serves a hit, coalesces onto an in-progress fill (`waitForFill`), or starts a new
fill. `cache_body.go` mirrors the upstream stream into `internal/cache` and
commits or aborts the fill as the response body is consumed or closed.

Every cache delivery still requires the client's own authorized upstream GET to
succeed first; a hit substitutes bytes, never authorization. See the
[architecture guide](../../docs/architecture.md#authorization-before-cache-use).

## Security boundaries

- **SSRF hardening** (`dial.go`): the entire DNS answer is validated before
  any connection, non-public address ranges are refused for hosts that are not
  configured destinations (the registries plus the redirect allowlist), and the
  hostname is never re-resolved after validation (no DNS rebinding).
- **Redirect containment**: redirects are bounded and de-duplicated; a
  cross-origin hop drops all headers except a safe allowlist so custom registry
  authentication never reaches a different origin.
- **Read-only interception**: intercepted requests must be origin-form, match
  the expected authority, carry no body, request no protocol upgrade, and use
  GET/HEAD/OPTIONS.
- **No leaks in logs or errors** (`errors.go`): upstream errors collapse to
  fixed labels; URLs, signed queries, certificate names, and response data are
  never logged.

## Concurrency and shutdown

`Proxy.mu` guards the listener and the sets of tracked connections, tunnels, and
inner servers. An `inflight` `WaitGroup` covers CONNECT handlers, dials, relays,
and interception. `Shutdown` stops accepting, disables intercepted keep-alives,
drains until its context expires, then closes remaining sockets; it waits for
handlers and relay pumps to release their resources — including cache bodies —
before returning, so a stuck handler can delay exit beyond the deadline.

## Dependencies

`proxy` depends on `internal/ca` (leaf certificates for interception),
`internal/cache` (the object store), `internal/config` (its configuration), and
`internal/hostport` (authority parsing). The caller owns the cache store and
closes it after `Shutdown`; a nil store disables caching.
