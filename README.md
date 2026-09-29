# OCI Accelerator

A standalone Go explicit HTTPS proxy for Docker Engine, with selective registry
TLS interception, persistent CA identity, and authorized SHA-256 blob and
digest-manifest caching. By default, recently approved requests can reuse a verified
body without another upstream GET for up to 30 seconds; setting
`cache.authorization_ttl: 0s` restores authorization on every delivery. Tags remain
uncached, and there is no offline or failed-request fallback.
Eligible blob Range GETs always perform a full upstream GET without using authorization
grants. Warm entries serve standard single and multipart ranges; a cold single range is
selected while the full body streams and may continue filling the cache after delivery,
while a cold multipart request falls back to the full `200` response.
Docker Engine on Linux is the validated client; the proxy uses generic Registry
HTTP API V2 behavior rather than provider-specific routing.

Docker pushes and other registry writes pass through by default, with upstream
permissions enforced by the registry. Uploads stream without populating the cache;
Docker owns authentication, upload sessions, and redirects. Deletion does not
invalidate existing pull grants, which remain valid until their original expiry.

Concurrent fills have byte, entry, writer and lifetime bounds. Authorization grants
are process-local, fixed-lived, conservatively constrained by HTTP freshness, and
bounded by `cache.max_authorization_entries`. Optional Prometheus metrics expose
resource use and fixed request/cache outcomes; remote binding requires explicit opt-in.

- [Architecture](docs/architecture.md): request flow, security boundaries, cache
  ownership, and design rationale.
- [Operating guide](docs/operations.md): configuration, Docker trust, metrics,
  recovery, and repeatable acceptance checks.
- Package guides map the code and internal contracts:
  [proxy](internal/proxy/README.md), [ca](internal/ca/README.md),
  [cache](internal/cache/README.md), [config](internal/config/README.md), and
  [hostport](internal/hostport/README.md), with the command documented under
  [Usage](#usage) and [cache benchmarks](internal/cache/benchmarks.md) for performance investigation.
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

`init-ca` uses the same [root CA lifecycle](internal/ca/README.md#public-interface)
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
