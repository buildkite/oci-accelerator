# OCI Accelerator

A standalone Go explicit HTTPS proxy for Docker Engine, with selective registry
TLS interception, persistent CA identity, and authorized SHA-256 blob and
digest-manifest [caching](#caching). Docker Engine on Linux is the validated
client; the proxy uses generic Registry HTTP API V2 behavior rather than
provider-specific routing.

Docker pushes and other registry writes pass through by default, with upstream
permissions enforced by the registry. Uploads stream without populating the cache;
Docker owns authentication, upload sessions, and redirects. Deletion does not
invalidate existing pull grants, which remain valid until their original expiry.

Optional Prometheus metrics expose resource use and fixed request/cache outcomes;
remote binding requires explicit opt-in.

- [Architecture](docs/architecture.md): request flow, security boundaries, cache
  ownership, and design rationale.
- [Operating guide](docs/operations.md): configuration, Docker trust, metrics,
  recovery, and repeatable acceptance checks.
- Package guides map the code and internal contracts:
  [proxy](internal/proxy/README.md), [config](internal/config/README.md), and
  [hostport](internal/hostport/README.md),
  with the command documented under [Usage](#usage) and
  [cache benchmarks](internal/cache/benchmarks.md) for performance investigation.
- [Working rules](AGENTS.md): contribution, review, and verification requirements.

## Build and configure

```sh
go build -o bin/oci-accelerator ./cmd/oci-accelerator
bin/oci-accelerator --config config.example.yaml --check
```

Follow the [operating guide](docs/operations.md) to configure registry authorities,
start the proxy, verify/install its public CA, and configure Docker Engine.
The default listener is loopback-only. Do not expose it publicly: there is no
proxy-user authentication, and unconfigured CONNECT destinations are opaque tunnels.

## Usage

```
oci-accelerator --config FILE [--check]
oci-accelerator init-ca --dir DIR
```

- `--config FILE` — path to the YAML configuration (required for the proxy command).
- `--check` — validate the configuration and exit without starting the proxy.
- `init-ca --dir DIR` — create or validate a root CA directory. `--dir` is
  required; relative paths are resolved from the working directory.

`init-ca` applies the same [CA directory requirements](docs/operations.md#ca-directory-requirements)
as proxy startup. It does not load YAML or start the proxy, cache, listeners,
or network activity. If `DIR` is absent, its existing parent must be safe and
writable; the command creates the directory, `ca.crt`, and `ca.key` using the
P-256, ten-year root CA profile. If a valid pair
already exists, it is validated and reused without changing file contents,
ownership, or permissions. Empty, invalid, expired, incomplete, symlinked, or
insecurely permissioned state fails closed. The command never creates parent
directories.

On success, stdout contains only the public certificate path and its lowercase,
unseparated SHA-256 fingerprint:

```text
ca_certificate="path/to/ca.crt"
ca_sha256=0123456789abcdef...
```

It never prints private-key contents. `oci-accelerator init-ca --help` describes
the command, and top-level help lists `init-ca`.

## Caching

The proxy stores immutable, digest-addressed registry objects on local disk so
repeated pulls avoid downloading the same bytes again. The registry still decides
who may pull: every cache delivery depends on a recent upstream approval of the
same request.

- **What is cached.** Query-free SHA-256 blob GETs (layers and image configs) and
  manifests requested by digest in OCI or Docker schema-2 media types. Tags, HEAD,
  conditional requests, legacy or unknown manifest formats, and responses that are
  encoded, have unknown length, forbid storage, or vary on unsupported headers pass
  through uncached. Entries belong to one registry and repository; there is no
  cross-repository deduplication.
- **Authorization.** A full GET is served straight from cache only when the
  registry approved the same request, with the same credentials and headers, within
  `cache.authorization_ttl` (default `30s`); otherwise it goes upstream first.
  Setting `0s` requires an upstream GET for every delivery. Approvals live only in
  memory, are cleared on restart, and are bounded by `cache.max_authorization_entries`.
  A token revoked after approval can still hit until that approval expires. Upstream
  errors are returned to the client: there is no offline or failed-request fallback.
- **Filling.** On a miss, the body streams to the client while being written to
  disk; layers are never buffered in memory. An entry is published only after its
  full length and SHA-256 digest are verified, so partial downloads are never
  served. Other clients requesting the same object still authorize upstream, then
  wait up to `cache.fill_wait_timeout` (default `1s`) for that fill instead of
  downloading the body again; if it does not complete, each makes one fresh
  upstream request.
- **Range requests.** Blob Range GETs always go upstream as full GETs and never use
  cached approvals. When the body is cached, the proxy serves standard single or
  multipart ranges from it. Otherwise it returns a single range as the full body
  streams past and may keep filling the cache after delivery, bounded by
  `cache.fill_timeout`; multiple ranges on an uncached object get the full `200`.
- **Limits and eviction.** `cache.max_bytes` (default 32 GiB, counting each body
  plus 4 KiB of metadata, including in-progress fills), `cache.max_entries`,
  `cache.max_fills`, and `cache.fill_timeout` bound storage. Leave filesystem
  headroom for block and inode overhead. When full, the proxy evicts approximately
  least-recently-used entries that are not being read. An object that still does
  not fit streams uncached rather than failing.
- **Persistence and integrity.** One process owns the private cache directory.
  Startup removes abandoned partial downloads and refuses to start on unrecognized
  files rather than deleting them. Each stored body is rehashed on its first use
  after startup; a missing or corrupt entry becomes a miss. Do not edit or delete
  live cache files.

See [cache storage and recovery](docs/operations.md#cache-storage-and-recovery) for
operating details and the [architecture guide](docs/architecture.md) for the exact
eligibility, authorization, and capacity rules.

## Certificate minting

Docker trusts the proxy's persistent, self-signed root CA once. When the proxy
intercepts a CONNECT to a configured registry authority, it terminates TLS with a
leaf certificate for that host, signed by the root on demand. Other CONNECT
destinations remain opaque tunnels and never receive CA-issued certificates.

A TLS certificate is only valid for the names in its SAN, so each intercepted
host needs its own leaf chaining to the trusted root. Issuing leaves on demand
keeps the root identity independent of the configured registry list and avoids
generating unused certificates.

- Each leaf covers a single DNS name or IP address and is valid for 24 hours.
- A new leaf is signed when none is cached, when the cached leaf is not currently
  valid (including after a clock rollback), or when it is within one hour of expiry.
- Leaf validity is capped at the root's expiry. A leaf already expiring with the
  root is reused even in its final hour, because renewing cannot extend its
  lifetime. A root that is not currently valid prevents both issuance and reuse.
- Leaves and their private keys exist only in memory. The cache holds at most
  `max_leaf_certificates` entries (default 256) and evicts the least recently
  used; every hit and renewal refreshes an entry's recency.

## Run in a container

From the repository root on a Linux Docker host, review `config.container.yaml`,
then build and start the Alpine image without Compose. Docker creates the named
volumes with the required ownership; the proxy generates its CA on first startup
and reuses it on subsequent starts.

```sh
docker build -t oci-accelerator:local .
docker run -d --name oci-accelerator --restart unless-stopped \
  --read-only --cap-drop ALL --security-opt no-new-privileges \
  --stop-timeout 40 -p 127.0.0.1:3128:3128 \
  --mount type=bind,src="$PWD/config.container.yaml",dst=/etc/oci-accelerator/config.yaml,readonly \
  --mount type=volume,src=oci-accelerator-ca,dst=/var/lib/oci-accelerator \
  --mount type=volume,src=oci-accelerator-cache,dst=/var/lib/oci-accelerator/cache \
  oci-accelerator:local
docker logs oci-accelerator
docker inspect --format '{{.State.Health.Status}}' oci-accelerator
```

Before pulling through the proxy, [verify/install the public CA and configure Docker
Engine](docs/operations.md#configure-ca-trust-and-docker). Never distribute `ca.key`.
Stop with `docker stop oci-accelerator`; removing the container preserves both named
volumes. **Do not use `docker compose down -v` unless you intend to delete both the
CA and cache.** For Compose, architecture-specific builds, and cache recovery,
see [container deployment](docs/operations.md#container-deployment).

## Testing

Run the baseline checks:

```sh
go test ./...
go test -race ./...
go vet ./...
```

Real Docker acceptance is opt-in and uses fresh isolated daemon state with local
fixtures. See the [operating guide](docs/operations.md#repeatable-checks)
for commands, prerequisites, cleanup, tested versions, and limitations.
Container packaging has a separate opt-in integration check documented there.
Amp orb development, including the supervised Docker daemon, is covered in the
[operating guide](docs/operations.md#docker-in-amp-orbs).

## Buildkite CI

The Buildkite pipeline runs Go formatting, vet, build, deterministic tests, and
multi-platform container builds on every change. Successful current `main` builds
publish amd64/arm64 images to `buildkite/oci-accelerator:latest`, replacing that tag.
See the [CI guide](.buildkite/README.md) for publishing prerequisites,
credentials, and local verification.
