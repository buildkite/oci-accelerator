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

The [Buildkite pipeline](https://buildkite.com/buildkite/oci-accelerator) uploads
[`.buildkite/pipeline.yml`](.buildkite/pipeline.yml) using a
`buildkite-agent pipeline upload` bootstrap. Bootstrap settings are managed in
`buildkite/saas-iac`; this repository owns the uploaded job definitions.

Three independent jobs run on the Open Source cluster's Linux `hosted` queue:

- Check Go formatting (without rewriting files), run `go vet ./...`, build the
  binary, and validate `config.example.yaml` with `--check`. This temporary build
  omits VCS stamping so container/checkout ownership differences do not require
  weakening Git's safe-directory checks.
- Run `go test -count=1 -timeout=10m ./...`.
- Run `go test -race -count=1 -timeout=10m ./...`.

Each job uses the Docker plugin and `golang:1.27.1-bookworm`, which supplies Go and
the C compiler required for race tests. `GOTOOLCHAIN=local` prevents an implicit
toolchain upgrade; update the image tags alongside `go.mod` and `.agents/setup`.
Agents need Docker, but no Amp installation, registry credentials, privileged
containers, or mounted Docker socket inside the test container. Job environments
are not broadly forwarded into containers. Test results are not reused from Go's
test cache. Jobs have explicit timeouts and failures are not automatically retried.

Docker, public-registry, fault, and load acceptance are manual opt-in checks:
their harness uses `amp orb service` and cannot run on ordinary hosted
agents. CI runs the default local-fixture suite, **not** those acceptance matrices.
Use the orb commands above and the [operating guide](docs/operations.md#repeatable-checks)
for that additional coverage.

To reproduce CI locally with Go 1.27.1 and a C compiler, run from the repo root:

```sh
GOTOOLCHAIN=local CGO_ENABLED=1 bash .buildkite/check.sh
GOTOOLCHAIN=local CGO_ENABLED=1 go test -count=1 -timeout=10m ./...
GOTOOLCHAIN=local CGO_ENABLED=1 go test -race -count=1 -timeout=10m ./...
```

With `buildkite-agent` installed, parse the pipeline without uploading any jobs.
Agent v4 requires a token even for a local dry run; use this dummy value and an
unreachable local endpoint, not a real credential:

```sh
BUILDKITE_AGENT_ACCESS_TOKEN=local-dry-run-only \
BUILDKITE_AGENT_ENDPOINT=http://127.0.0.1:1 \
buildkite-agent pipeline upload --dry-run --reject-parse-warnings .buildkite/pipeline.yml
```
