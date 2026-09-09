# Operating OCI Accelerator with Docker Engine

OCI Accelerator is an HTTP CONNECT proxy that intercepts configured HTTPS registries and
caches verified SHA-256 blobs and digest manifests. Every cached delivery still requires an
upstream GET for current authorization; there is no offline or stale fallback.

Linux Docker Engine is the validated client. Docker Desktop, Windows, containerd, and
BuildKit-specific integration are not validated.

## Build and start

Install Go 1.27.1, then run from the repository root:

```sh
go build -o bin/oci-accelerator ./cmd/oci-accelerator
cp config.example.yaml config.local.yaml
bin/oci-accelerator --config config.local.yaml --check
```

Edit `registries` before starting. Registry names have no scheme or path; omitted ports mean
443. Matching includes the port. IPv4, bracketed IPv6, and explicitly configured private
registries are supported. Relative directories are resolved from the configuration file.
Unknown or duplicate keys, extra YAML documents, invalid zero values, and malformed durations
are rejected. `cache.fill_wait_timeout: 0s` is the one permitted zero duration. Null values
retain scalar defaults, but null list elements are invalid. String fields reject non-string
scalars. Numeric limits require decimal YAML integers without leading zeros: floats, quoted
numbers, octal/hex forms, and underscores are rejected. `ca_dir` and `cache.dir` must both be
nonblank and must resolve to distinct, lexically normalized absolute paths; there is no
forwarding-only configuration mode. This validation does not resolve symlinks or inspect state;
the filesystem ownership and layout checks still apply at startup.

`--check` validates only the configuration. It does not inspect CA/cache state, bind listeners,
or create directories. Before starting, provision the immediate parent of each state directory
as the proxy's user. For the example, create `data` only if absent, and check its ownership and
permissions if it exists:

```sh
mkdir -m 0700 data
bin/oci-accelerator --config config.local.yaml
```

Keep the containing path trusted and on durable storage. The proxy does not create ancestors.

Run the process under the host's service manager. In an Amp orb, for example:

```sh
amp orb service start registry-proxy \
  --command './bin/oci-accelerator --config config.local.yaml'
amp orb service logs registry-proxy
```

Do not expose the proxy through an Amp portal. The default `127.0.0.1:3128` listener is for
Docker on the same host. Serving remote hosts requires an explicit private-interface bind,
network access controls, and a firewall. The proxy has no client authentication: any user who
can reach it can open an opaque CONNECT tunnel to arbitrary hosts and ports. Such tunnels
cannot be inspected or restricted to image pulls.

## Configure CA trust and Docker

With the example configuration, first startup creates `data/ca/ca.crt` and
`data/ca/ca.key`. Leave the CA directory absent initially; do not pre-create an empty
directory. Its parent must be owned by the proxy user and must not be group- or
world-writable. Startup logs the certificate path and its SHA-256 fingerprint.

Transfer only `ca.crt` to the Docker host over a trusted channel and verify its fingerprint:

```sh
openssl x509 -in data/ca/ca.crt -noout -fingerprint -sha256
```

OpenSSL displays uppercase colon-separated hex, while startup logs lowercase hex without
colons. The CA can sign arbitrary server identities, so protect `ca.key` and narrowly scope
trust.

On rootful Linux Docker Engine, install the public certificate for each intercepted registry:

```sh
sudo install -D -m 0644 data/ca/ca.crt /etc/docker/certs.d/registry-1.docker.io/ca.crt
sudo install -D -m 0644 data/ca/ca.crt /etc/docker/certs.d/ghcr.io/ca.crt
```

For `registry.example:5000`, use that exact host and port as the directory name. Omit `:443`
when image names omit it. Never distribute `ca.key` or install it as a Docker client key. See
Docker's [certificate instructions](https://docs.docker.com/engine/security/certificates/)
for other installation modes and platforms.

Merge this into the existing `/etc/docker/daemon.json` without replacing unrelated settings:

```json
{
  "proxies": {
    "https-proxy": "http://127.0.0.1:3128",
    "no-proxy": ""
  }
}
```

The proxy URL is `http://`: Docker sends CONNECT and then negotiates registry TLS inside the
tunnel. Use the proxy's private address instead of loopback for a remote deployment. Do not
route HTTP registries through this listener, and do not put intercepted registries in
`no-proxy`. Shell proxy variables do not configure an already-running daemon. After arranging
for workload disruption, apply the change:

```sh
sudo systemctl restart docker
docker login ghcr.io
docker pull ghcr.io/your-organization/your-image:tag
```

Image names and Docker-owned credentials do not change. Never put credentials in proxy YAML.
Docker also documents [daemon proxy configuration](https://docs.docker.com/engine/daemon/proxy/)
for service-environment setups.

The proxy independently verifies upstream certificates with its process's system trust store.
A private registry's real CA must therefore be trusted by the proxy process as well as Docker.
Do not disable TLS verification or configure an insecure registry as a workaround.

## Configuration and limits

See [`config.example.yaml`](../config.example.yaml) for a complete configuration. Configuration
is static and changes take effect after restart.

| Setting | Default or requirement |
| --- | --- |
| `listen` | `127.0.0.1:3128`; host:port with a nonzero numeric port; empty/wildcard hosts bind all interfaces |
| `metrics_listen` | Disabled; optional literal loopback IP and nonzero port |
| `registries` | Required, nonempty list of distinct canonical authorities |
| `redirect_allowlist` | Optional private HTTPS storage authorities; does not enable interception |
| `ca_dir` | Required private persistent state directory |
| `cache.dir` | Required private persistent cache directory |
| `cache.max_bytes` | 32 GiB, including body, metadata, and fill reservations |
| `cache.max_fills` | 16 concurrent storage writers; excess misses stream uncached |
| `cache.max_entries` | 100000 entry slots, including fills and pending cleanup |
| `cache.fill_timeout` | `10m`; expiry ends cache admission, not upstream forwarding |
| `cache.fill_wait_timeout` | `1s`; `0s` disables waiting for an existing fill |
| `dial_timeout`, `tls_handshake_timeout` | `10s` each |
| `response_header_timeout` | `30s`; also bounds incoming request headers |
| `idle_timeout` | `2m` for idle tunnels and I/O |
| `shutdown_timeout` | `15s` drain deadline |
| `max_connections` | 256 accepted client connections, including incomplete CONNECTs |
| `max_leaf_certificates` | 256 cached interception certificates |

CONNECT and upstream response headers are limited to 64 KiB. Upstream connections are capped
at 16 per destination, with 32 idle globally and 8 idle per destination. Bodies stream rather than
accumulate in memory. Active transfers have idle-I/O deadlines, not total download deadlines.
Kernel filesystem calls cannot be preempted, so fill expiry and shutdown are not hard deadlines
when storage stalls. The entry limit bounds index size, not process RSS; tune it to available
memory.

Intercepted traffic uses TLS 1.2 or newer and HTTP/1.1. Only bodyless GET, HEAD, and OPTIONS are
allowed. Mutations, upgrades, authority conflicts, and non-origin-form requests are denied. Tag,
digest, HEAD, Range, and conditional requests always go upstream.

The proxy follows up to five redirects only for eligible query-free SHA-256 blob and digest
manifest GETs. Redirects must use verified HTTPS and pass destination checks; userinfo,
fragments, loops, and implicit private, link-local, or loopback destinations are rejected.
Private storage destinations belong in `redirect_allowlist`. Cross-origin redirects preserve
only Accept, Accept-Encoding, and User-Agent, never credentials or cookies. See
[`architecture.md`](architecture.md) for the protocol and security rationale.

## Logs and metrics

JSON logs go to stderr. Request completion records contain bounded status and outcome fields,
cache/fill/wait/fallback/admission outcomes, duration, downstream body bytes, and bytes consumed
from the upstream body. The upstream counter excludes transport read-ahead and is not a wire
traffic measurement. Streaming failures can occur after status 200 has been sent. Logs omit
paths, signed queries, authorization headers, token bodies, and raw upstream errors.

Set `metrics_listen: 127.0.0.1:9090` to serve Prometheus text at `GET /metrics` on a separate
listener. Hostnames, wildcard/public addresses, zoned IPv6, and port zero are rejected. A bind
failure prevents startup. There is no metrics route on the CONNECT listener, admin API, or pprof
endpoint. Keep metrics local and do not expose them through a portal or public reverse proxy.

Metrics use the `oci_accelerator_` prefix and include:

- request counts, bounded status classes, duration histograms, and active requests/connections;
- cache, fill, wait, fallback, and upstream GET/body counters;
- cache admission and rejection reasons, entries, readers, fills, bytes, reservations, and
  evictions (the fills gauge includes reservations whose staging is still in progress);
- Go heap allocation and goroutine gauges.

Labels are fixed enums and never contain clients, repositories, paths, digests, tokens, or URLs.
Counters reset on restart. Body counters are recorded when requests complete. Logical cache
bytes exclude filesystem overhead, and Go heap allocation is not RSS.

`oci_accelerator_upstream_get_attempts_total` counts connection-acquisition attempts, including
transport retries and DNS/dial/TLS failures before a GET is sent. It is not an exact count of
requests received by the registry. `oci_accelerator_cache_rejections_total` reports `vary` and
`digest_mismatch` header rejections on eligible responses; another policy check may also prevent
caching, so these counts do not identify the sole cause of every bypass.

## Cache storage and recovery

The cache admits full, query-free SHA-256 blob GETs (including image configuration blobs) and
digest manifests in supported OCI and Docker schema-2 media types. Cache keys preserve registry,
repository, digest, object kind, exact Accept values, and observed Content-Type; bodies are
hashed as received. Tags, HEAD, Range, conditional and non-identity encoded responses,
unknown-length bodies, no-store traffic, and unsupported Vary dimensions bypass storage.

Every cache delivery requires a current terminal upstream 200 and matching length, media type, and
digest metadata when present. Authorization denials, rate limits, outages, TLS failures, and
metadata mismatches never fall back to stale data. Current upstream headers are returned rather
than saved authentication metadata.

Use one process per cache directory. Its parent must be owner-controlled; unsafe or unusable
configured storage fails startup. The limit charges body size plus 4 KiB metadata per entry,
including active fills, so reserve filesystem block, directory, and inode headroom. Oversized or
capacity-rejected objects continue streaming without being cached. Failed cleanup retains its
capacity charge until removal succeeds. Best-effort LRU eviction skips active readers and makes
at most eight eviction removal attempts per admission, separately from pending-cleanup retries.
Objects requiring more victims can continue bypassing; see the
[capacity contract](architecture.md#storage-capacity-and-recovery).

Concurrent clients authorize independently. One writer fills a key; matching followers may
wait for it and then open a verified cache entry. Timeout, leader failure, or eviction causes one
fresh authorized GET. Leader cancellation aborts storage; follower cancellation does not control
the leader. Entries are published only after complete length and digest verification.

Startup removes recognized abandoned fills and discardable incomplete entries, and refuses
unknown files rather than deleting them. Persisted bodies are rehashed on first reuse, not at
startup; subsequent access checks size and relies on private directory ownership for integrity.
Restart reconstructs approximate LRU order from commit times. Do not edit or delete live cache
files; use metrics to inspect resource use.

## Shutdown, recovery, and CA replacement

SIGINT or SIGTERM stops admission, disables intercepted keepalive, and drains until
`shutdown_timeout`; remaining connections, including opaque tunnels, are then closed. Shutdown
waits for handlers and relay pumps to release their resources before closing the cache, even
beyond the drain deadline. A stuck handler can therefore delay exit. A second signal uses the
operating system's default termination behavior. In an orb:

```sh
amp orb service stop registry-proxy
```

Preserve `ca_dir` across restarts. The CA is valid for ten years; generated leaf certificates
last at most 24 hours and renew under the same CA within an hour of expiry. Expected modes are
0700 for the directory, 0600 for the key, and 0644 for the certificate. Empty, incomplete,
expired, mismatched, symlinked, or incorrectly permissioned state fails startup without
replacement. Back up both CA files securely and use one proxy process per CA directory.

CA initialization uses no persistent lock. After interrupted first initialization, stop all
initializers before examining `.ca-init-*` staging directories in the CA parent. Remove only
exact paths confirmed abandoned, following the [CA cleanup guidance](../internal/ca/README.md#orphaned-initialization-directories);
never delete an established CA to repair startup. To replace a CA, stop the proxy, archive the
old CA securely, configure a new absent directory, start the proxy, verify the new fingerprint,
redistribute `ca.crt`, and remove old client trust. Rotation is not automatic.

## Docker in Amp orbs

Amp runs `.agents/setup` when preparing a fresh orb. It installs checksum-verified
Go 1.27.1, C build tools for race-enabled tests, and Docker Engine if missing, and
downloads Go module dependencies. Re-running it is safe:

```sh
.agents/setup
go version
```

Start Docker as a supervised service, not from setup or a background shell:

```sh
amp orb services ensure
sudo docker info
```

The daemon uses only a local Unix socket; do not expose it through a portal.
Use `sudo docker` for daemon operations. If it is not ready, inspect
`amp orb service logs docker-daemon`. Amp supervises the daemon across CLI
updates and orb pause/resume; no custom resume hook is needed.

Registry credentials belong to the Docker client, never the setup script or a
shared orb snapshot. Setup does not install a proxy CA into Docker's trust store.

## Repeatable checks

Run the standard deterministic checks first:

```sh
go test ./...
go test -race ./...
go vet ./...
```

The following integration checks are opt-in because they need additional host capabilities.
The Docker checks build the current binary and use isolated Docker state, processes, and CA
trust; local checks use private registry fixtures. They never execute pulled images. Test
cleanup terminates and reaps owned processes before removing their private directories,
without altering host trust or unrelated Docker state. Privileged fixtures run inside private
PID and mount namespaces; termination also removes daemonized descendants. Diagnostics use
private log files capped at the first 64 KiB, discarding later output. Amp is not a prerequisite. Acceptance
tests remain manual-only, with no CI jobs or merge gates. A skipped opt-in check is not a pass.

Real Docker cold, warm, restart, authentication, and trust behavior requires Linux, Go,
Docker Engine, passwordless sudo, `mount`, a non-loopback IPv4 interface, and
permission to create private PID/mount namespaces and mount procfs:

The multiplatform fixture withholds selected configuration and layer
bodies during warm and restarted-proxy phases while still authorizing every GET
and returning valid metadata. Each phase uses fresh Docker state, so successful
verified pulls prove proxy-cache delivery rather than Docker's local cache.
Manifest bodies remain live: Docker reorders `Accept` values across requests,
which intentionally produces distinct representation keys. Manifest delivery is
therefore a compatibility check, not cache-hit proof. Tag mutation and access to
the old digest remain covered.

```sh
OCI_ACCELERATOR_E2E=1 go test -v ./integration \
  -run '^(TestDockerPullThroughCompiledProxy|TestDockerMultiPlatformThroughCompiledProxy)$' \
  -count=1 -timeout=10m
```

Anonymous Docker Hub/GHCR compatibility uses the same Docker prerequisites and fresh,
credential-free client state. It requires network access and is subject to upstream tag,
availability, and rate-limit changes. Successful public pulls do not prove cache substitution
or private-access enforcement; deterministic local tests assert those properties:

```sh
OCI_ACCELERATOR_PUBLIC_E2E=1 go test -v ./integration \
  -run TestDockerPublicRegistriesThroughCompiledProxy -count=1 -timeout=10m
```

Linux crash recovery needs Go and a non-loopback IPv4 interface. Isolated tmpfs ENOSPC
verification additionally needs passwordless sudo, `nsenter`, `mount`, `dd`,
`findmnt`, and private PID/mount namespace, procfs, and tmpfs support. The fixture fills
only its disposable filesystem, not the host root disk. The lifecycle check also verifies
privileged descendant teardown:

```sh
OCI_ACCELERATOR_FAULT_E2E=1 go test -v ./integration \
  -run '^(TestCache(ENOSPC|Crash)Acceptance|TestPrivilegedProcessLifecycle)$' -count=1 -timeout=10m
```

In an Amp orb, run the entire test runner as a supervised service so its child
processes survive CLI updates. The wrapper stays alive after the one-shot test
to prevent the service supervisor from automatically rerunning it:

```sh
runner=$(mktemp)
cat > "$runner" <<'SH'
OCI_ACCELERATOR_E2E=1 OCI_ACCELERATOR_FAULT_E2E=1 go test -v ./integration \
  -run '^(TestDockerPullThroughCompiledProxy|TestDockerMultiPlatformThroughCompiledProxy|TestCache(ENOSPC|Crash)Acceptance|TestPrivilegedProcessLifecycle)$' -count=1 -timeout=10m
printf 'Acceptance exit status: %s\n' "$?"
exec sleep infinity
SH
amp orb service start acceptance --command "sh $runner"
amp orb service logs acceptance
# After the exit status is printed, stop the wrapper and remove it:
amp orb service stop acceptance
rm "$runner"
```

Allow normal test cleanup to finish. If the runner is forcibly killed, confirm its
owned processes have exited before removing only the exact temporary directories
reported by that run; never delete unrelated Docker state or host trust files.

Bounded HTTP and Docker concurrency matrices require several GiB of disposable disk, Linux
`/proc`, and enough resources for four simultaneous Docker daemons, plus the Docker prerequisites
above. HTTP covers 1/8/32 clients, 8/64 MiB same/distinct-key objects, cold/warm/restart phases,
wait-hit and fallback profiles, and tiny-object churn. Docker covers 1/4 fresh daemons across
cold/warm/restart phases. Reports are optional and are atomically replaced only after a complete
passing matrix:

```sh
reports=$(mktemp -d)
OCI_ACCELERATOR_LOAD_E2E=1 \
OCI_ACCELERATOR_LOAD_REPORT="$reports/http.json" \
OCI_ACCELERATOR_DOCKER_LOAD_REPORT="$reports/docker.json" \
  go test -v ./integration \
  -run '^(TestBoundedHTTPLoadAcceptance|TestDockerLoadAcceptance)$' \
  -count=1 -timeout=20m
```

Use a fresh report directory and rely on the test exit status; a failed run leaves any existing
report untouched. These checks validate bounded behavior and correctness, not deployment sizing,
throughput, or latency guarantees. Resource peaks are periodic samples, not guaranteed maxima.
See [cache benchmarks](../internal/cache/benchmarks.md) for isolated admission contention checks.
