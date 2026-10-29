# Settings

This reference lists every OCI Accelerator environment variable and its
default, grouped like [`.env.example`](../.env.example). See
[configuration](deployment.md#configuration) in the deployment guide for how
values are loaded, dotenv syntax, value formats, and state directory rules.
The container [image](deployment.md#image) overrides some of these defaults.

## Required

| Environment variable | Default | Notes |
| --- | --- | --- |
| `OCI_ACCELERATOR_REGISTRIES` | Required | Nonempty list of distinct [authorities](deployment.md#registry-authorities) to intercept |
| `OCI_ACCELERATOR_CA_DIR` | Required | Private persistent CA directory |
| `OCI_ACCELERATOR_CACHE_DIR` | Required | Private persistent cache directory |

## Proxy

| Environment variable | Default | Notes |
| --- | --- | --- |
| `OCI_ACCELERATOR_LISTEN` | `127.0.0.1:5128` | host:port with a nonzero numeric port. Empty or wildcard hosts bind all interfaces |

## Peering

| Environment variable | Default | Notes |
| --- | --- | --- |
| `OCI_ACCELERATOR_PEER_LISTEN` | Disabled | Separate TLS listener, host:port with a nonzero numeric port |
| `OCI_ACCELERATOR_PEER_ADVERTISE` | Empty | Required reachable host:port when peering is enabled |
| `OCI_ACCELERATOR_PEER_SEEDS` | Empty | Optional comma-separated host:port bootstrap addresses |
| `OCI_ACCELERATOR_PEER_STATE_DIR` | Empty | Required private persistent directory when peering is enabled. May remain configured while disabled. Native starter example: `./data/peers` |

## Peering hints

| Environment variable | Default | Notes |
| --- | --- | --- |
| `OCI_ACCELERATOR_PEER_HINT_REFRESH_TIMEOUT` | `1m` | Positive overall duration per cache-key fetch attempt, excluding scheduler queueing. No unlimited value |
| `OCI_ACCELERATOR_PEER_HINT_REFRESH_INTERVAL` | `1m` | At least `1s`. Poll delay after each attempt completes, with ±20% jitter |
| `OCI_ACCELERATOR_PEER_HINT_FULL_REFRESH_INTERVAL` | `1h` | At least `1s`. Full-scan interval from successful full-scan completion, with ±20% jitter |

## Metrics

| Environment variable | Default | Notes |
| --- | --- | --- |
| `OCI_ACCELERATOR_METRICS_LISTEN` | Disabled | host:port with a nonzero numeric port. Literal loopback IP required unless `OCI_ACCELERATOR_METRICS_ALLOW_REMOTE` |
| `OCI_ACCELERATOR_METRICS_ALLOW_REMOTE` | `false` | Explicit boolean opt-in to non-loopback metrics and probe binding |

## Registry redirects

| Environment variable | Default | Notes |
| --- | --- | --- |
| `OCI_ACCELERATOR_REDIRECT_ALLOWLIST` | Empty | Private HTTPS storage [authorities](deployment.md#registry-authorities). Does not enable interception |

## Cache storage

| Environment variable | Default | Notes |
| --- | --- | --- |
| `OCI_ACCELERATOR_CACHE_MAX_BYTES` | 32 GiB | Includes body, metadata, and fill reservations |
| `OCI_ACCELERATOR_CACHE_MAX_ENTRIES` | 100000 | Entry slots, including fills and pending cleanup |
| `OCI_ACCELERATOR_CACHE_MAX_FILLS` | 16 | Concurrent storage writers. Excess misses stream uncached |
| `OCI_ACCELERATOR_CACHE_FILL_TIMEOUT` | `10m` | Bounds ranged fetches from request start and peer transfers. Ends ordinary full-GET cache admission |
| `OCI_ACCELERATOR_CACHE_FILL_WAIT_TIMEOUT` | `1s` | Wait for an existing immutable fill or mutable capture. `0s` disables waiting, not caching |

## Mutable cache

| Environment variable | Default | Notes |
| --- | --- | --- |
| `OCI_ACCELERATOR_CACHE_MUTABLE_TTL` | `30s` | Fixed mutable response lifetime from upstream headers. Shared expiry is independent of grants. Exact mode is capped by the applicable authorization TTL. `0s` disables |
| `OCI_ACCELERATOR_CACHE_MUTABLE_SHARED` | `true` | Share repository mutable responses across independently approved Bearer tokens. `false` uses exact mutable matching and authorization TTL caps, with no mutable grant creation. Immutable caching is unchanged |

## Cache authorization

| Environment variable | Default | Notes |
| --- | --- | --- |
| `OCI_ACCELERATOR_CACHE_AUTHORIZATION_TTL` | `30s` | Fixed approval lifetime for requests with Authorization, including empty. `0s` disables that class |
| `OCI_ACCELERATOR_CACHE_ANONYMOUS_AUTHORIZATION_TTL` | `15m` | Fixed approval lifetime for requests without Authorization. `0s` disables that class |
| `OCI_ACCELERATOR_CACHE_MAX_AUTHORIZATION_ENTRIES` | 10000 | Positive shared bound on both classes of in-memory authorization grants |

## Connections and limits

| Environment variable | Default | Notes |
| --- | --- | --- |
| `OCI_ACCELERATOR_DIAL_TIMEOUT` | `10s` | |
| `OCI_ACCELERATOR_TLS_HANDSHAKE_TIMEOUT` | `10s` | |
| `OCI_ACCELERATOR_RESPONSE_HEADER_TIMEOUT` | `30s` | Also bounds incoming request headers |
| `OCI_ACCELERATOR_IDLE_TIMEOUT` | `2m` | Idle tunnels and I/O |
| `OCI_ACCELERATOR_SHUTDOWN_TIMEOUT` | `15s` | Drain deadline |
| `OCI_ACCELERATOR_MAX_CONNECTIONS` | 256 | Accepted client connections, including incomplete CONNECTs |
| `OCI_ACCELERATOR_MAX_LEAF_CERTIFICATES` | 256 | Cached interception certificates |

## Timeout behavior

- `OCI_ACCELERATOR_CACHE_FILL_TIMEOUT` is a total deadline that doesn't reset
  on progress. It bounds blob range requests from before the first upstream
  GET, including their background fills, and peer downloads, including headers
  and body. It applies whether or not the body is cached. A serving peer
  applies its own fill timeout to its side of the transfer.
- All other transfers, including cached deliveries and uploads, have only
  idle-I/O deadlines (`OCI_ACCELERATOR_IDLE_TIMEOUT`), with no total time
  limit.
- Filesystem calls can't be interrupted, so a stalled disk can delay fill
  expiry and shutdown.

## Memory

Total process memory isn't capped. `OCI_ACCELERATOR_CACHE_MAX_ENTRIES` and
`OCI_ACCELERATOR_CACHE_MAX_AUTHORIZATION_ENTRIES` bound only their in-memory
indexes, so size them to the memory you have.

Immutable bodies stream through without being buffered in memory. The mutable
response cache has [fixed internal limits](deployment.md#built-in-limits) that
you can't configure.
