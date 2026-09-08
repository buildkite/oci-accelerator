# OCI Accelerator

A standalone Go explicit HTTPS proxy for Docker Engine, with selective registry
TLS interception, persistent CA identity, and authorized SHA-256 blob and
digest-manifest caching. Every cached delivery still requires a successful upstream
GET. Tags remain uncached; there is no offline fallback or request-rate-limit avoidance.
Docker Engine on Linux is the validated client; the proxy uses generic Registry
HTTP API V2 behavior rather than provider-specific routing.

Concurrent fills have byte, entry, writer and lifetime bounds. Independently
authorized followers can wait briefly for the same fill; timeout uses one fresh
upstream GET, not stale authorization. Optional loopback-only Prometheus metrics
expose resource use and fixed request/cache outcomes.

- [Architecture](docs/architecture.md): request flow, security boundaries, cache
  ownership, and design rationale.
- [Operating guide](docs/operations.md): configuration, Docker trust, metrics,
  recovery, and repeatable acceptance checks.
- [CA package](internal/ca/README.md) and [cache benchmarks](internal/cache/benchmarks.md):
  internal contracts and performance investigation.
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
Amp orb development, including the supervised Docker daemon, is covered in the
[operating guide](docs/operations.md#docker-in-amp-orbs).

## Buildkite CI

The Buildkite pipeline runs Go formatting, vet, build, and the deterministic
test suites on every change. See the [CI guide](.buildkite/README.md) for the
job breakdown, container image, and how to reproduce and dry-run it locally.
