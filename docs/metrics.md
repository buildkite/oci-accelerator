# Metrics

OCI Accelerator exposes Prometheus metrics at `GET /metrics` on a separate
metrics listener. Set `OCI_ACCELERATOR_METRICS_LISTEN` to enable it. See
[metrics](deployment.md#metrics) in the deployment guide for the listener, its
health endpoints, and how to interpret the values.

All metrics use the `oci_accelerator_` prefix, shown here without it. Labels are
fixed values and never contain clients, repositories, digests, tokens, or URLs.
Counters reset when the process restarts.

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
| `served_bytes_total{source}` | counter | Image content bytes sent to clients, by `cache`, `origin`, or `peer` |
| `served_entries_total{source}` | counter | Completed image content deliveries, by `cache`, `origin`, or `peer` |
| `downstream_bytes_total` | counter | Response body bytes sent to clients |

## Upstream

| Metric | Type | Description |
| ------ | ---- | ----------- |
| `upstream_get_attempts_total` | counter | Upstream GET connection attempts, including retries |
| `upstream_body_bytes_total` | counter | Response body bytes read from upstream registries |

## Cache storage

| Metric | Type | Description |
| ------ | ---- | ----------- |
| `cache_entries` | gauge | Entries in the cache |
| `cache_bytes` | gauge | Bytes stored in the cache, excluding filesystem overhead |
| `cache_reserved_bytes` | gauge | Bytes reserved for fills in progress |
| `cache_fills` | gauge | Fills in progress |
| `cache_readers` | gauge | Open readers of cached entries |
| `cache_evictions_total` | counter | Entries evicted to stay within limits |
| `cache_admissions_total{reason}` | counter | Cache admission decisions, by `accepted`, `invalid`, `oversized`, `existing`, `fill_limit`, `capacity`, `closed`, or `io_error` |
| `cache_rejections_total{reason}` | counter | Responses not cached because of their headers, by `vary` or `digest_mismatch` |

## Fills and waits

| Metric | Type | Description |
| ------ | ---- | ----------- |
| `fills_total{outcome}` | counter | Cache fills by requests, by `stored`, `failed`, `aborted`, or `bypass` |
| `waits_total{outcome}` | counter | Waits for another request's fill, by `complete`, `timeout`, or `canceled` |
| `active_waits` | gauge | Requests waiting for another request's fill |
| `fallbacks_total{outcome}` | counter | Requests that fetched upstream again after a wait failed |
| `store_fill_success_total` | counter | Fills committed to disk |
| `store_fill_failure_total` | counter | Fills that failed |
| `store_fill_abort_total` | counter | Fills abandoned before completion |
| `store_fill_expiry_total` | counter | Fills that exceeded their time limit |

## Peers

| Metric | Type | Description |
| ------ | ---- | ----------- |
| `peer_fetches_total{outcome}` | counter | Peer source selections, by `hit` or `unavailable` |
| `peer_body_bytes_total` | counter | Response body bytes read from peers |

## Runtime

| Metric | Type | Description |
| ------ | ---- | ----------- |
| `heap_alloc_bytes` | gauge | Go heap allocation, not RSS |
| `goroutines` | gauge | Running goroutines |
