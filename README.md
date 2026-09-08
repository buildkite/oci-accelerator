# OCI Accelerator

A standalone Go explicit HTTPS proxy for Docker Engine, with selective registry
TLS interception, persistent CA identity, and authorized SHA-256 blob and
digest-manifest caching. Every cached delivery still requires a successful upstream
GET. Tags remain uncached; there is no offline fallback or request-rate-limit avoidance.
See the [implementation plan](docs/plans/https-registry-cache.md).
Track completed and upcoming slices in the [implementation log](docs/implementation-log.md).

Concurrent fills have byte, entry, writer and lifetime bounds. Independently
authorized followers can wait briefly for the same fill; timeout uses one fresh
upstream GET, not stale authorization. Optional loopback-only Prometheus metrics
expose resource use and fixed request/cache outcomes. See the
[load evidence](docs/operations.md#concurrent-load-acceptance) for measured
cold/warm/restart behavior, coalescing tradeoffs, and repeatable checks.

## Build and configure

```sh
go build -o bin/oci-accelerator ./cmd/oci-accelerator
bin/oci-accelerator --config config.example.yaml --check
```

Follow the [operating guide](docs/operations.md) to configure registry authorities,
start the proxy, verify/install its public CA, and configure Docker Engine.
The default listener is loopback-only. Do not expose it publicly: there is no
proxy-user authentication, and unconfigured CONNECT destinations are opaque tunnels.

## Development in Amp orbs

Amp runs `.agents/setup` when preparing a fresh orb. It installs checksum-verified
Go 1.27.1, C build tools for race-enabled tests, and Docker Engine if missing.
It downloads module dependencies once `go.mod` exists. Re-running it is safe:

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

Run the baseline checks:

```sh
go test ./...
go test -race ./...
go vet ./...
```

Registry credentials belong to the Docker client, never the setup script or a
shared orb snapshot. Setup does not install a proxy CA into Docker's trust store.

Real Docker acceptance is opt-in and uses fresh isolated daemon state:

```sh
OCI_ACCELERATOR_E2E=1 go test -v ./integration \
  -run TestDockerPullThroughCompiledProxy -count=1 -timeout=5m
```

It validates CA rejection/trust, authenticated cold/warm/restart pulls, exact image
digests, fresh upstream GETs, reduced warm layer bytes, and persistent CA identity
using local fixtures—not public registries.
See the [guide](docs/operations.md#acceptance-evidence-and-repeatable-checks) for
prerequisites, cleanup, tested versions, and limitations.

## Buildkite CI

The [Buildkite pipeline](https://buildkite.com/buildkite/oci-accelerator) uploads
[`.buildkite/pipeline.yml`](.buildkite/pipeline.yml) using its existing
`buildkite-agent pipeline upload` bootstrap. Bootstrap settings are managed in
`buildkite/saas-iac`; no Buildkite UI changes are needed for this file.

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

Docker, public-registry, fault, and load acceptance remain manual opt-in checks:
their current harness uses `amp orb service` and cannot run on ordinary hosted
agents. CI runs the default local-fixture suite, **not** those acceptance matrices.
Use the orb commands above and in the operating guide for that additional coverage.

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

## Project rename

Formerly Docker Accelerator. The Go module is now
`github.com/buildkite/oci-accelerator`; the binary, metrics, and test environment
variables use the OCI Accelerator name without compatibility aliases. See the
[migration notes](docs/operations.md#upgrading-from-docker-accelerator).
This is a name change, not expanded client support: Docker Engine remains the
validated client.
