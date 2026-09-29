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
  [cache](internal/cache/README.md), [config](internal/config/README.md),
  [hostport](internal/hostport/README.md), and the
  [command](cmd/oci-accelerator/README.md), with
  [cache benchmarks](internal/cache/benchmarks.md) for performance investigation.
- [Working rules](AGENTS.md): contribution, review, and verification requirements.

## Build and configure

```sh
go build -o bin/oci-accelerator ./cmd/oci-accelerator
bin/oci-accelerator --config config.example.yaml --check
```

The repository also provides an Alpine-based container image and a hardened
Compose deployment. Build and provision it with:

```sh
docker compose build
sudo install -d -o 10001 -g 10001 -m 0700 data/identity
docker run --rm --network none --read-only --cap-drop ALL \
  --security-opt no-new-privileges \
  --mount type=bind,src="$PWD/data/identity",dst=/var/lib/oci-accelerator \
  oci-accelerator:local init-ca --dir /var/lib/oci-accelerator/ca
docker compose run --rm --no-deps oci-accelerator \
  --config /etc/oci-accelerator/config.yaml --check
docker compose up -d
```

The CA child directory must be absent before provisioning. See
[container deployment](docs/operations.md#container-deployment) for storage,
trust, health-check, multi-platform build, and recovery details.

Follow the [operating guide](docs/operations.md) to configure registry authorities,
start the proxy, verify/install its public CA, and configure Docker Engine.
The default listener is loopback-only. Do not expose it publicly: there is no
proxy-user authentication, and unconfigured CONNECT destinations are opaque tunnels.

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

The Buildkite pipeline runs Go formatting, vet, build, and the deterministic
test suites on every change. See the [CI guide](.buildkite/README.md) for the
job breakdown, container image, and how to reproduce and dry-run it locally.
