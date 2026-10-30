# Contributing

## Develop and test locally

You need Go 1.27.3 and a C compiler for race tests. The standard checks need no
network access or Docker:

```sh
gofmt -l version.go starter.go cmd internal integration
go vet ./...

go test ./...
go test -race ./...
```

See the [testing guide](docs/testing.md) for what these cover, the CI checks,
and the opt-in acceptance suites that use real Docker Engine. See
[working rules](AGENTS.md) for contribution and review requirements.

## Release

See [RELEASING.md](RELEASING.md) to publish a new version.
