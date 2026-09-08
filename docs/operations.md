# Running the proxy with Docker Engine

The proxy caches verified SHA-256 blobs and digest manifests when `cache.dir` is configured. Every
request still goes upstream for current GET authorization before any cache hit.
There is no offline fallback or request-rate-limit avoidance. Local protocol/cache
checks and real Docker cold/warm/restart acceptance pass.
Linux Docker Engine is the validated target; Docker Desktop, Windows, containerd,
and BuildKit-specific integration are not validated.

## Build and start

With Go 1.27.1 installed, from the repository root:

```sh
go build -o bin/oci-accelerator ./cmd/oci-accelerator
cp config.example.yaml config.local.yaml
bin/oci-accelerator --config config.local.yaml --check
bin/oci-accelerator --config config.local.yaml
```

Edit the registry list before starting. Names have no scheme or path; omitted ports
mean 443. IPv4, bracketed IPv6, and explicitly configured private registries are
supported. Matching includes the port. Relative directories resolve from the YAML
file's directory. Unknown/duplicate mapping keys and extra YAML documents are errors.
Null values retain defaults; zero limits/timeouts are invalid except `fill_wait_timeout: 0s`. `--check` validates
configuration only: it does not check CA state, bind sockets, or create directories.

In an orb, run the built proxy under supervision instead of the foreground command:

```sh
amp orb service start registry-proxy \
  --command './bin/oci-accelerator --config config.local.yaml'
amp orb service logs registry-proxy
```

Do **not** expose the proxy through an Amp portal. The default `127.0.0.1:3128`
listener is for Docker on the same host. Serving other hosts requires an explicit
private-interface bind address and firewall restrictions. There is no proxy-user
authentication. Any reachable user can CONNECT to arbitrary unconfigured hosts and
ports; opaque tunnels cannot enforce pull-only policy or inspect their application data.

## Upgrading from Docker Accelerator

This rename is a clean break: rebuild `oci-accelerator` and update service commands
and scripts that invoke `docker-accelerator`. The Go module/import prefix changes
from `ampcode.com/buildkite/docker-accelerator` to
`github.com/buildkite/oci-accelerator`. The old command directory is removed.

Update Prometheus queries, dashboards, and alerts from `docker_accelerator_` to
`oci_accelerator_`. Rename test/report environment variable prefixes from
`DOCKER_ACCELERATOR_` to `OCI_ACCELERATOR_`, retaining the suffix (including
`DOCKER_LOAD_REPORT`, which measures Docker Engine). Old metric names are not
emitted and old environment variables are not read.

Keep the existing configuration, CA directory, and cache directory. Their format,
paths, and contents do not change. New installations generate an **OCI Accelerator
CA**; existing certificates, including **Docker Accelerator CA**, are loaded
unchanged. Do not regenerate the CA or reinstall trust solely for this rename.
Docker Engine remains the validated client; this adds no client support or
changes to upstream authorization and TLS policy.

## CA trust and Docker configuration

First startup creates `data/ca/ca.crt` and `data/ca/ca.key` with the example config.
Leave the CA directory absent initially; do not pre-create an empty directory.
The direct parent must be owned by the proxy user and not group/world writable.
Startup logs the public certificate path and its SHA-256 fingerprint.

Transfer **only `ca.crt`** over a trusted channel to the Docker host, and compare its
fingerprint with startup output before trusting it:

```sh
openssl x509 -in data/ca/ca.crt -noout -fingerprint -sha256
```

OpenSSL adds colons and uses uppercase; startup uses lowercase hex without colons.
Never download a CA over an unauthenticated connection and trust it without verification.
The CA can sign arbitrary server identities, so protect its key and limit trust scope.

On native rootful Linux Docker Engine, install the verified public certificate for
each intercepted registry. For the example registry list:

```sh
sudo install -D -m 0644 data/ca/ca.crt /etc/docker/certs.d/registry-1.docker.io/ca.crt
sudo install -D -m 0644 data/ca/ca.crt /etc/docker/certs.d/ghcr.io/ca.crt
```

For a registry addressed as `registry.example:5000`, use a directory with that exact
host and port. Omit `:443` when image names omit it. Do not distribute `ca.key` or
install it as a Docker client key. See Docker's [certificate instructions](https://docs.docker.com/engine/security/certificates/)
for rootless or other platform-specific certificate locations.

Merge this fragment into the Docker daemon's existing `/etc/docker/daemon.json`;
do not overwrite unrelated settings:

```json
{
  "proxies": {
    "https-proxy": "http://127.0.0.1:3128",
    "no-proxy": ""
  }
}
```

The proxy URL is `http://`: Docker sends CONNECT, then negotiates registry TLS
inside the tunnel. Replace loopback with the proxy's private address when remote.
Do not configure HTTP registries through this proxy: its outer listener accepts
CONNECT only. Do not list intended registries in `no-proxy`; Docker may bypass
proxies for loopback registry addresses regardless. Client-shell proxy variables
alone do not configure an already-running daemon.

After arranging any workload disruption, restart Docker to apply changes:

```sh
sudo systemctl restart docker
docker login ghcr.io
docker pull ghcr.io/your-organization/your-image:tag
```

Image names and Docker-owned login credentials remain unchanged. Do not place
credentials in proxy YAML. Follow Docker's [daemon proxy instructions](https://docs.docker.com/engine/daemon/proxy/)
if the host uses service environment variables instead of daemon JSON.

The proxy independently verifies upstream certificates using its process's system
trust store. A private registry's real CA must also be trusted by the proxy process;
trusting the interception CA in Docker does not establish upstream trust. Never
use an insecure-registry setting or disable TLS verification to work around a failure.

## Configuration and runtime limits

See [the complete example](../config.example.yaml). Configuration is static until restart.

| Setting | Default / requirement |
| --- | --- |
| `listen` | `127.0.0.1:3128`; explicit host:port, valid nonzero numeric port |
| `metrics_listen` | Disabled; optional literal loopback IP and explicit nonzero port (for example `127.0.0.1:9090`) |
| `registries` | Required nonempty list of distinct canonical authorities |
| `redirect_allowlist` | Optional private HTTPS storage authorities; does not enable CONNECT interception |
| `ca_dir` | Required; private persistent state |
| `cache.dir`, `cache.max_bytes` | Optional cache directory; omission disables caching. Default maximum 32 GiB including fill/metadata reservations |
| `cache.max_fills` | `16` active writers globally; excess misses stream uncached |
| `cache.max_entries` | `100000` completed entries plus reserved fill slots; evict unused LRU entries or bypass |
| `cache.fill_timeout` | `10m` per fill; expiry discards cache admission, not the upstream transfer |
| `cache.fill_wait_timeout` | `1s`; an authorized follower waits for an existing fill. `0s` disables waiting |
| `dial_timeout`, `tls_handshake_timeout` | `10s` each |
| `response_header_timeout` | `30s`; also bounds incoming request headers |
| `idle_timeout` | `2m`; idle tunnels, upstream I/O, downstream writes, and keepalive |
| `shutdown_timeout` | `15s` drain deadline, then close remaining sockets |
| `max_connections` | `256` accepted client connections, including incomplete CONNECTs |
| `max_leaf_certificates` | `256` cached certificates |

CONNECT headers and upstream response headers are bounded to 64 KiB. Upstream
connections are capped at 16 per registry, with 32 idle globally and 8 idle per
registry. Layers stream rather than accumulate in memory. Active transfers have
idle-I/O bounds, not a fixed total download timeout.

All cache limits are positive. Fill expiry releases cache reservations even when
the upstream pauses; timer work is joined on terminal fill operations. Kernel file
operations cannot be preempted, so expiry and shutdown are not hard deadlines for
a stalled filesystem. Byte and entry caps both apply after restart. The entry cap
bounds index growth, not RSS: 100000 entries can use hundreds of MiB; tune to capacity.

Intercepted traffic uses TLS 1.2+ and HTTP/1.1. Only bodyless GET, HEAD, and OPTIONS
are allowed; mutations, upgrades, conflicting TLS/HTTP authorities, and non-origin-form
HTTP targets are denied. Tag, digest, HEAD, Range, and conditional requests all go
upstream. Eligible query-free SHA-256 blob/digest-manifest GET redirects are followed by the proxy,
up to five hops. Other redirects are returned unchanged for Docker to handle.
Redirects must use HTTPS with verified certificates; userinfo, fragments, loops,
and implicit private/link-local/loopback destinations are rejected. Configure private
storage authorities in `redirect_allowlist` (or `registries` if they also require
interception). All DNS results are checked before dialing vetted IPs. Cross-origin
hops retain only Accept, Accept-Encoding, and User-Agent, never registry credentials
or cookies. These restrictions do not constrain user-selected opaque CONNECT tunnels.

JSON stderr logs include configured registry, status, duration, downstream body bytes,
completion outcome, `cache` (hit/wait_hit/miss/bypass), `fill` (none/stored/bypass/failed/aborted),
`wait` (complete/canceled/timeout or empty), `fallback` (retry or empty),
`admission` (a bounded refusal reason or empty),
and `upstream_body_bytes` consumed by the forwarding code. The latter excludes
transport/kernel read-ahead and is not a wire-level traffic measurement.
Aborted streaming responses may have already sent status 200;
the client still receives an incomplete-body error. No paths, signed queries, auth
headers, token bodies, or raw upstream errors are logged.

### Metrics

Set `metrics_listen: 127.0.0.1:9090` to expose Prometheus text at `/metrics` on a
separate local listener. Hostnames, wildcard/public addresses, zoned IPv6 and port
zero are rejected. There is no metrics route on the CONNECT listener, public bind,
admin API, or pprof endpoint. Bind failure fails startup. Protect local access and
do not expose this endpoint through a portal or public reverse proxy.

Metric names start with `oci_accelerator_`:

- `requests_total`, `request_status_total`, and `request_duration_seconds` histogram:
  completed/aborted outcomes, bounded status classes, and elapsed handler duration.
- `cache_total`, `fills_total`, `waits_total`, `fallbacks_total`: request outcomes.
  `upstream_get_attempts_total` includes redirects and both fallback attempts;
  `upstream_body_bytes_total` counts consumed bytes, not wire read-ahead.
  Body byte counters are recorded on request completion, not continuously mid-stream.
- `cache_rejections_total{reason=...}`: diagnostic counter for eligible immutable
  objects the upstream response headers keep out of the cache: `vary` (a `Vary`
  header naming anything other than `Accept`) or `digest_mismatch` (a
  `Docker-Content-Digest` disagreeing with the requested digest). It is an upper
  bound — the response may also be uncacheable for another reason — and is
  intended to measure cache-effectiveness loss from CDN header conventions. It
  does not affect the caching decision.
- `cache_admissions_total{reason=...}`: accepted, invalid, oversized, existing,
  fill_limit, capacity, closed, or io_error. `store_fill_*_total` records actual
  success, failure, abort and expiry transitions, including idle-fill expiry.
- Active requests/waits/connections; cache entries/fills/readers, charged bytes,
  reserved bytes and evictions; Go heap allocation and goroutine gauges.

All labels are fixed enums; no client, repository, path, digest, token or URL labels.
Counters reset at process restart. Scrapes copy bounded state before writing;
slow/failed scrapes do not hold request/cache locks during network output. Logical
cache bytes exclude filesystem block/inode overhead, and heap allocation is not RSS.

## Cache behavior and storage

Full, query-free SHA-256 blob GETs (including image configs) and digest manifests
are cached. Manifest admission supports OCI image manifests/indexes and Docker
schema-2 manifests/lists; legacy and unknown types are forwarded uncached. Keys
preserve object kind, exact Accept values, and observed Content-Type; bodies are
hashed without JSON conversion. Tag manifests, HEAD, Range, conditionals, unknown-length bodies, encoded bodies,
no-store responses/requests, and unsupported Vary dimensions bypass caching.
Each delivery needs a terminal upstream 200 with matching length/media type and
consistent digest headers when present. CDN digest headers are optional. Denials,
rate limits, upstream outages, TLS failures, and metadata mismatches never trigger
an offline/stale fallback. Responses use current upstream headers, not saved cookies
or authentication metadata. Warm GETs can transfer bytes before cancellation.

Use one process per private cache directory, with an owner-controlled parent.
An unsafe/unusable configured directory fails startup; omitted `cache.dir` leaves
forwarding-only mode. Runtime capacity and storage-write failures bypass admission
without making the upstream transfer depend on a cache write.

The limit charges declared body bytes plus 4 KiB per entry for metadata, including
in-progress fills. Leave filesystem headroom for blocks, directories, and inodes.
Entries larger than the budget are forwarded uncached. LRU eviction skips active
readers; competing cold clients authorize independently, with at most one
storage writer per key. Matching authorized followers close their origin body and
wait up to `fill_wait_timeout` for that fill, then use a normal verified pinned read.
Timeout, leader failure, or eviction causes one fresh original GET through the full
authorization/redirect policy, with no second wait. New denials are never replaced
by the earlier authorization. Zero wait streams immediately. Leader disconnect
aborts storage and wakes followers; follower cancellation does not keep or stop
the leader's download. There are no detached downloads or leader handoffs.
Fills are removed on cancellation and only published after
complete length and digest verification. Startup removes recognized abandoned fills
and invalid pairs and rehashes persisted bodies before first use. Live hit recency
is in memory; restart approximates order using commit times. Do not modify live
cache files. Unknown files cause startup refusal, not arbitrary deletion.

Network cancellation and shutdown close client-owned transfers and abort incomplete
fills. Regular-file kernel I/O cannot be preempted by Go context cancellation;
a stalled filesystem can delay cleanup. This is not a hard storage-stall deadline.

## Shutdown, recovery, and CA replacement

SIGINT/SIGTERM stops admission, disables intercepted keepalive, and drains until
the configured deadline. Opaque tunnels may require deadline closure. In an orb:

```sh
amp orb service stop registry-proxy
```

Preserve the CA directory across restarts. The CA lasts ten years; leaves last at
most 24 hours and renew under the same CA within an hour of expiry. Existing empty,
incomplete, expired, mismatched, symlinked, or incorrectly permissioned CA state
fails startup without replacement. Expected modes: directory 0700, key 0600,
certificate 0644. Back up both CA files securely; keep state and its parent under
one trusted operator's control. Use one proxy instance per state directory.

After a failed first initialization, stop all initializers before inspecting a sibling
`<ca-dir>.lock` or `.ca-init-*` staging directory. Remove only confirmed abandoned
initialization artifacts. Do not delete an established CA to "repair" startup.
For intentional replacement, stop the proxy, securely archive the old CA, configure
a new absent CA directory, start it, verify the new fingerprint, then redistribute
the public certificate and remove old trust on clients. There is no automatic rotation.

## Acceptance evidence and repeatable checks

The local fixture verifies Basic authentication, a Docker schema-2 image, CA trust
failure, and cold/warm/restarted-proxy caching using the compiled binary. Each pull
uses a fresh isolated daemon, avoiding Docker's local layer cache. Origin request
and byte counters plus exact inspected manifest/config/rootfs digests are asserted.
Both layer and config blobs receive one fresh authenticated GET per warm phase;
tag resolution also reaches upstream on every pull.

Run in an orb with Go, Docker Engine, Amp service management, passwordless sudo,
and a non-loopback IPv4 interface:

```sh
OCI_ACCELERATOR_E2E=1 go test -v ./integration \
  -run TestDockerPullThroughCompiledProxy -count=1 -timeout=5m
```

This creates temporary authenticated TLS fixtures, private client config, and named
supervised proxy/daemon services. It cleans up only its own state and leaves the
existing Docker daemon, global trust, and images untouched. The test uses process-local
CA trust overrides, not insecure TLS. Normal `go test ./...` skips this opt-in test
but validates the deterministic image fixture.

Validated with Go 1.27.1 and Docker Engine/CLI 29.8.0 on Linux amd64: no-trust pull
reached zero origin HTTP requests, image digests matched in all three successful
phases, and CA identity survived restart. A representative final run recorded:

| Phase (fresh Docker store each time) | Authorized layer GETs | Origin layer body bytes |
| --- | ---: | ---: |
| Cold | 1 | 8,390,075 |
| Warm, same proxy | 1 | 32,768 |
| Warm, restarted proxy | 1 | 196,608 |

The deterministic manifest digest is
`sha256:f0c62108e748e004823f36ecaa075912d0ed5a00912730e96fef75e66ea02c75`.
The fixture streams an incompressible ~8 MiB layer in 32 KiB chunks with 1 ms
pacing; cancellation may still write some upstream bytes. Assertions require less
than half the layer on warm requests, not these exact timing-dependent byte counts.
Counters settle after handlers finish before each phase snapshot. This proves
correctness and body-transfer savings, not production latency or throughput.

Default tests cover denied/revoked clients, public HEAD/protected GET, CDN denials,
TLS and upstream failures, bypass semantics, and interrupted/corrupt/full-cache
behavior, including client-owned bearer refresh and a separately trusted token host.

Multi-platform acceptance is also opt-in:

```sh
OCI_ACCELERATOR_E2E=1 go test -v ./integration \
  -run TestDockerMultiPlatformThroughCompiledProxy -count=1 -timeout=10m
```

It uses OCI indexes/manifests and Docker lists/schema-2 manifests, each with valid
amd64 and arm64 children. Fresh daemons pull the selected amd64 image by tag and
digest cold/warm/after proxy restart, then observe an actual tag change and retrieve
the old immutable digest. Assertions cover root/config/rootfs identity, selected
child GETs, independent bearer acquisition, and absence of unselected-platform GETs.
Test hostnames are mapped only inside private mount namespaces (`sudo`, `unshare`,
and `mount` required); token-service trust is present only in Docker, not the proxy.
Images are never executed. Small manifest bandwidth savings are not promised.
Concurrent load and crash checks are described below; metrics are optional.

### Public registry compatibility

Anonymous smoke checks passed on 2026-09-05 with Go 1.27.1, Docker Engine/CLI
29.8.0, and Linux amd64. Each uses fresh Docker state for a cold tag pull, warm
pinned-digest pull, and pinned-digest pull after proxy restart. Root/config/rootfs
identity and CA persistence match; completed cache entries prove the proxy was used.
These are compatibility checks, not measurements of upstream authorization counts
or cache-hit savings. Deterministic local fixtures remain authoritative for those.

| Public image | Resolved root digest |
| --- | --- |
| `docker.io/library/alpine:3.22` | `sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce` |
| `ghcr.io/fluxcd/source-controller:v1.7.4` | `sha256:16f21ac1795528df80ddef51ccbb14a57b78ea26e66dc8551636ef9a3cec71b3` |

```sh
OCI_ACCELERATOR_PUBLIC_E2E=1 go test -v ./integration \
  -run TestDockerPublicRegistriesThroughCompiledProxy -count=1 -timeout=10m
```

No account credentials are used; anonymous bearer exchange stays client-owned.
The test creates private empty client configuration, uses normal system TLS trust
plus its private proxy CA, never executes images, and cleans up its services/state.
Tags can change; each run records the resolved digest and pins subsequent pulls.
Network/rate-limit failures fail an explicitly enabled test rather than becoming skips.

### Crash and filesystem-exhaustion checks

```sh
OCI_ACCELERATOR_FAULT_E2E=1 go test -v ./integration \
  -run '^TestCache(ENOSPC|Crash)Acceptance$' -count=1 -timeout=10m
```

These Linux checks use owned supervised proxy processes and private state. ENOSPC
requires passwordless sudo, mount namespaces and tmpfs; no shared/root filesystem
is exhausted. A 1 MiB tmpfs with 700 KiB ballast refuses caching of a 600 KiB body,
but every upstream byte reaches the client. After reclaiming ballast, cold/warm
128 KiB transfers match, with two fresh GETs but only one origin body.

The crash check kills a compiled proxy during a nonempty fill, then restarts the
same cache and CA. Abandoned files disappear, the lock is reacquired, and exact
cold/warm bytes are recovered with a stable CA. Unit subprocess cases also exercise
actual active/committed API state and staged incomplete/renamed publication state.
These are process-crash and restart checks, not power-loss or faulty-device claims.
Concurrent corruption/reader pin tests and active-fill/waiter shutdown tests run in
the normal race suite. Do not modify cache files while a proxy is running.

### Concurrent load acceptance

```sh
reports=$(mktemp -d)
OCI_ACCELERATOR_LOAD_E2E=1 \
OCI_ACCELERATOR_LOAD_REPORT="$reports/http.json" \
OCI_ACCELERATOR_DOCKER_LOAD_REPORT="$reports/docker.json" \
  go test -v ./integration \
  -run '^(TestBoundedHTTPLoadAcceptance|TestDockerLoadAcceptance)$' \
  -count=1 -timeout=20m
```

Use the prerequisites above, several GiB of disposable disk and enough resources
for four simultaneous Docker daemons. The HTTP matrix streams tens of GiB; it does
not buffer large objects in the proxy or fixture. Services and cache/client state
are private and cleaned up. Reports publish atomically only for complete passing
matrices (39 HTTP cases or six Docker phases). A failed/partial run does not replace
an existing report; use a fresh report directory as above and check the test exit
status rather than treating an older file as new evidence.

The HTTP matrix covers 1/8/32 clients × 8/64 MiB × same/distinct keys ×
cold/warm/restarted proxy, plus wait-hit, timeout/fallback and 4 KiB entry churn.
Every case has its own cache. Warm/restart cases populate serially first; reports
separate those GETs/body bytes from the measured phase. Normal HTTP pacing is 1 ms
per streaming chunk with a 5s fill wait (not the production default). Limits are
2 GiB + 1 MiB, 16 fills, 96 entry slots and 64 connections; churn reduces slots to
eight. Deterministic digest generation and hashing contribute fixture CPU/latency.

Representative results on 2026-09-05, Go 1.27.1, Docker 29.8.0, Linux amd64:

| HTTP, 32 clients × 64 MiB | p50 / p95 milliseconds | Origin GETs | Origin body bytes |
| --- | ---: | ---: | ---: |
| Same key, cold | 4097 / 4150 | 32 | 67,460,202 |
| Same key, warm | 1149 / 1391 | 32 | 1,123,996 |
| Same key, restart | 1320 / 1463 | 32 | 13,364,594 |
| Distinct keys, cold | 8078 / 8297 | 32 | 2,147,483,648 |
| Distinct keys, warm | 1225 / 1425 | 32 | 776,594 |
| Distinct keys, restart | 1592 / 1734 | 32 | 31,621,120 |

The distinct warm cases first transferred 2,147,483,648 bytes in 32 warmup GETs;
same-key warmup transferred 67,108,864 bytes in one GET. Those costs are not savings.
The separate 32-client 8 MiB wait-hit profile (5ms pacing, 5s wait) recorded 31 wait
hits, 32 GETs, 8,883,989 origin bytes and p95 1628ms. The timeout profile (10ms pacing,
1ms wait) recorded 31 fallbacks, 63 GETs, 135,110,941 bytes and p95 2991ms. Different
pacing deliberately exercises both outcomes; this is not a controlled latency
comparison. Fallback repeats authorization and can transfer duplicate data.

Docker uses one/four fresh VFS daemons per cold/warm/restart phase (15 total), a
paced ~8 MiB image, the default 1s wait and a 32 MiB cache. Every daemon performs its
own authenticated layer/config GETs and tag resolution; image identities and CA
persistence match. Docker-local cache cannot explain these savings.

| Docker daemons / phase | p50 / p95 milliseconds | Layer + config GETs | Layer + config origin bytes |
| --- | ---: | ---: | ---: |
| 1 / cold | 470 / 470 | 2 | 8,390,238 |
| 1 / warm | 152 / 152 | 2 | 32,931 |
| 1 / restart | 188 / 188 | 2 | 262,307 |
| 4 / cold | 494 / 494 | 8 | 8,489,031 |
| 4 / warm | 164 / 165 | 8 | 217,476 |
| 4 / restart | 185 / 187 | 8 | 885,388 |

Metrics and `/proc` are sampled every 50ms plus boundary samples. HTTP sampled
maxima were RSS 21,327,872 bytes, Go heap 6,582,704 bytes, 73 FDs, 137 goroutines,
16 fills and 32 connections; Docker maxima were RSS 16,736,256 bytes, heap 3,220,576
bytes, 19 FDs and 29 goroutines. These are proxy-only sampled observations, not
instantaneous maxima, deployment sizing promises or throughput/latency SLAs.
The eight-entry churn proves cap/eviction behavior, not memory usage at the default
100000-entry cap; that larger index still needs capacity planning.

Tests assert exact bytes, independent authorization, warm savings, sampled
byte/entry/fill/connection bounds, and zero fills/readers/reservations/active
requests/waits/connections after quiescence. FD/goroutine return allows retained
idle transport state (+16/+32 from baseline); deterministic unit/race tests own
cap-saturation and lifecycle invariants that sampling cannot prove. Reports include
baseline/end resources and separate file lengths, allocated filesystem blocks
(including directories), charged cache bytes and in-flight reservations. For the
32-distinct warm case, file lengths totalled 2,147,491,456 bytes and allocation
2,147,618,816 bytes; filesystem allocation is not identical to logical accounting.
No incomplete cache entries or owned supervised services remained after the run.
