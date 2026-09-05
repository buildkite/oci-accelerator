# Running the proxy with Docker Engine

The proxy caches verified SHA-256 blobs when `cache.dir` is configured. Every
request still goes upstream for current GET authorization before any cache hit.
There is no offline fallback or request-rate-limit avoidance. Local protocol/cache
checks and real Docker cold/warm/restart acceptance pass.
Linux Docker Engine is the validated target; Docker Desktop, Windows, containerd,
and BuildKit-specific integration are not validated.

## Build and start

With Go 1.27.1 installed, from the repository root:

```sh
go build -o bin/docker-accelerator ./cmd/docker-accelerator
cp config.example.yaml config.local.yaml
bin/docker-accelerator --config config.local.yaml --check
bin/docker-accelerator --config config.local.yaml
```

Edit the registry list before starting. Names have no scheme or path; omitted ports
mean 443. IPv4, bracketed IPv6, and explicitly configured private registries are
supported. Matching includes the port. Relative directories resolve from the YAML
file's directory. Unknown/duplicate mapping keys and extra YAML documents are errors.
Null values retain defaults; zero limits/timeouts are invalid. `--check` validates
configuration only: it does not check CA state, bind sockets, or create directories.

In an orb, run the built proxy under supervision instead of the foreground command:

```sh
amp orb service start registry-proxy \
  --command './bin/docker-accelerator --config config.local.yaml'
amp orb service logs registry-proxy
```

Do **not** expose the proxy through an Amp portal. The default `127.0.0.1:3128`
listener is for Docker on the same host. Serving other hosts requires an explicit
private-interface bind address and firewall restrictions. There is no proxy-user
authentication. Any reachable user can CONNECT to arbitrary unconfigured hosts and
ports; opaque tunnels cannot enforce pull-only policy or inspect their application data.

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
| `registries` | Required nonempty list of distinct canonical authorities |
| `redirect_allowlist` | Optional private HTTPS storage authorities; does not enable CONNECT interception |
| `ca_dir` | Required; private persistent state |
| `cache.dir`, `cache.max_bytes` | Optional cache directory; omission disables caching. Default maximum 32 GiB including fill/metadata reservations |
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

Intercepted traffic uses TLS 1.2+ and HTTP/1.1. Only bodyless GET, HEAD, and OPTIONS
are allowed; mutations, upgrades, conflicting TLS/HTTP authorities, and non-origin-form
HTTP targets are denied. Tag, digest, HEAD, Range, and conditional requests all go
upstream. Eligible query-free SHA-256 blob GET redirects are followed by the proxy,
up to five hops. Other redirects are returned unchanged for Docker to handle.
Redirects must use HTTPS with verified certificates; userinfo, fragments, loops,
and implicit private/link-local/loopback destinations are rejected. Configure private
storage authorities in `redirect_allowlist` (or `registries` if they also require
interception). All DNS results are checked before dialing vetted IPs. Cross-origin
hops retain only Accept, Accept-Encoding, and User-Agent, never registry credentials
or cookies. These restrictions do not constrain user-selected opaque CONNECT tunnels.

JSON stderr logs include configured registry, status, duration, downstream body bytes,
completion outcome, `cache` (hit/miss/bypass), `fill` (none/stored/bypass/failed/aborted),
and `upstream_body_bytes` consumed by the forwarding code. The latter excludes
transport/kernel read-ahead and is not a wire-level traffic measurement.
Aborted streaming responses may have already sent status 200;
the client still receives an incomplete-body error. No paths, signed queries, auth
headers, token bodies, or raw upstream errors are logged. A metrics endpoint and
aggregate operational counters remain planned for slice 4.

## Cache behavior and storage

Only full, query-free SHA-256 blob GETs (including image configs) are cached. All
manifests, HEAD, Range, conditional requests, unknown-length bodies, encoded bodies,
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
readers; competing cold clients authorize/transfer independently, with at most one
storage writer per key. Fills are removed on cancellation and only published after
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
DOCKER_ACCELERATOR_E2E=1 go test -v ./integration \
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
behavior. Docker Hub/GHCR, bearer-token flows, multi-platform compatibility, full
load/crash testing, and operational metrics remain for slices 3 and 4.
