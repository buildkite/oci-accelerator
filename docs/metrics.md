# Metrics

OCI Accelerator exposes Prometheus metrics at `GET /metrics` on a separate
metrics listener. Set `OCI_ACCELERATOR_METRICS_LISTEN` to enable it. See
[metrics](deployment.md#metrics) in the deployment guide for the listener and
its health endpoints.

All metrics use the `oci_accelerator_` prefix, shown here without it. Labels are
fixed values and never contain clients, repositories, paths, digests, tokens,
or URLs. Counters reset when the process restarts. Request body counters update
when each request completes.

## Requests

| Metric | Type | Description |
| ------ | ---- | ----------- |
| `requests_total{outcome}` | counter | Completed requests, by `complete` or `aborted` |
| `request_status_total{class}` | counter | Responses by status class: `1xx` to `5xx`, or `other` |
| `request_duration_seconds` | histogram | Request duration |
| `active_requests` | gauge | Requests in progress |
| `active_connections` | gauge | Open client connections |

## Cache delivery

| Metric | Type | Description |
| ------ | ---- | ----------- |
| `cache_total{outcome}` | counter | Cache lookups, by `hit`, `wait_hit`, `miss`, or `bypass` |
| `served_bytes_total{source}` | counter | Image content bytes sent to clients, by `cache`, `origin`, or `peer`. See [cache delivery ratios](#cache-delivery-ratios) |
| `served_entries_total{source}` | counter | Completed image content deliveries, by `cache`, `origin`, or `peer` |
| `downstream_bytes_total` | counter | Response body bytes sent to clients |

### Cache delivery ratios

`served_bytes_total` and `served_entries_total` measure how much
digest-addressed content clients received from the local cache, the upstream
registry, or a peer.

- **Scope.** Only `200` responses to query-free `GET`s for a full, unescaped
  SHA-256 blob or manifest digest, without `Range` or `If-*` headers. Errors,
  `HEAD`, tags, ranges, conditional requests, queries, and tunnels are
  excluded.
- **Bytes** count what the client actually accepted, including partial
  deliveries. **Entries** increment only when a delivery completes. They count
  deliveries, not unique digests, cache fills, or integrity checks.
- **Source** is where the body came from. `cache` covers ordinary hits, grant
  hits, and responses served after waiting for another request's fill.
  `origin` covers cold fills and responses that bypassed storage because of
  `no-store`, capacity, or unsupported metadata. `peer` covers accepted peer
  streams, even when storage admission fails. Upstream authorization GETs don't
  affect attribution.
- The `cache` and `origin` series start at zero, and `peer` appears on first
  delivery. They measure application deliveries, not wire bandwidth or
  bandwidth savings.

This query shows the cache's share of delivered bytes over five minutes, across
all instances. Apply `rate` before aggregating, and drop a zero-traffic
denominator so dashboards show N/A instead of zero:

```promql
(
  100 * sum(rate(oci_accelerator_served_bytes_total{source="cache"}[5m]))
    / sum(rate(oci_accelerator_served_bytes_total[5m]))
)
and on () (sum(rate(oci_accelerator_served_bytes_total[5m])) > 0)
```

This query shows the cache's share of completed deliveries:

```promql
(
  100 * sum(rate(oci_accelerator_served_entries_total{source="cache"}[5m]))
    / sum(rate(oci_accelerator_served_entries_total[5m]))
)
and on () (sum(rate(oci_accelerator_served_entries_total[5m])) > 0)
```

For the origin percentage, replace `source="cache"` with `source="origin"`, and
use `source="peer"` for the peer percentage. Subtracting the cache percentage
from 100 combines origin and peer delivery.

## Upstream

| Metric | Type | Description |
| ------ | ---- | ----------- |
| `upstream_get_attempts_total` | counter | Upstream GET connection attempts, including retries and DNS, dial, or TLS failures. Not an exact count of requests the registry received |
| `upstream_body_bytes_total` | counter | Response body bytes read from upstream registries. For a detached range fill, it updates when the fill finishes |

## Cache storage

| Metric | Type | Description |
| ------ | ---- | ----------- |
| `cache_entries` | gauge | Entries in the cache |
| `cache_bytes` | gauge | Bytes stored in the cache, excluding filesystem overhead |
| `cache_reserved_bytes` | gauge | Bytes reserved for fills in progress |
| `cache_fills` | gauge | Fills in progress, including reservations still staging |
| `cache_readers` | gauge | Open readers of cached entries |
| `cache_evictions_total` | counter | Entries evicted to stay within limits |
| `cache_admissions_total{reason}` | counter | Cache admission decisions, by `accepted`, `invalid`, `oversized`, `existing`, `fill_limit`, `capacity`, `closed`, or `io_error` |
| `cache_rejections_total{reason}` | counter | Responses not cached because of their headers, by `vary` or `digest_mismatch`. Other policy checks can also prevent caching |

## Fills and waits

| Metric | Type | Description |
| ------ | ---- | ----------- |
| `fills_total{outcome}` | counter | Cache fills by requests, by `stored`, `failed`, `aborted`, or `bypass`. For a detached range fill, it updates when the fill finishes |
| `waits_total{outcome}` | counter | Waits for another request's fill, by `complete`, `timeout`, or `canceled` |
| `active_waits` | gauge | Requests waiting for another request's fill or mutable capture |
| `fallbacks_total{outcome}` | counter | Requests that fetched upstream again after a wait failed |
| `store_fill_success_total` | counter | Fills committed to disk |
| `store_fill_failure_total` | counter | Fills that failed |
| `store_fill_abort_total` | counter | Fills abandoned before completion |
| `store_fill_expiry_total` | counter | Fills that exceeded their time limit |

## Peers

| Metric | Type | Description |
| ------ | ---- | ----------- |
| `peer_fetches_total{outcome}` | counter | Peer source selections, by `hit` or `unavailable`. `unavailable` covers no live hint, full outbound capacity, and failed requests. `hit` means headers were accepted, not that the stream completed or verified |
| `peer_body_bytes_total` | counter | Response body bytes read from peers. For a detached range fill, it updates when the fill finishes |

## Runtime

| Metric | Type | Description |
| ------ | ---- | ----------- |
| `heap_alloc_bytes` | gauge | Go heap allocation, not RSS |
| `goroutines` | gauge | Running goroutines |
