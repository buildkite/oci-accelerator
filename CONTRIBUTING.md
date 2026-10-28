# Contributing

## Develop and test locally

You need Go 1.27.1 and a C compiler for race tests. The default test suite uses
local registry, authentication, and CDN fixtures, needs no network access or
Docker, and runs on Linux and macOS:

```sh
gofmt -l cmd internal integration
go vet ./...
go test ./...
go test -race ./...
```

To run the same checks as CI:

```sh
GOTOOLCHAIN=local CGO_ENABLED=1 bash .buildkite/check.sh
```

Tests against a real Docker Engine are opt-in and run only on Linux. They start
their own Docker daemons with passwordless `sudo` and share the host's network,
so run them only on a disposable host. For example, to test pulls and pushes:

```sh
OCI_ACCELERATOR_E2E=1 go test -v ./integration -count=1 -timeout=10m
```

See the [testing guide](docs/testing.md) for their prerequisites and the other
opt-in suites, and [working rules](AGENTS.md) for contribution and review
requirements.

## Release

See [RELEASING.md](RELEASING.md) to publish a new version.
