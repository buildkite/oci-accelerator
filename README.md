# Docker accelerator

A planned standalone Go HTTPS pull-through cache for Docker Engine images.
Implementation is in progress. See the [approved plan](docs/plans/https-registry-cache.md).
Track completed and upcoming slices in the [implementation log](docs/implementation-log.md).

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
shared orb snapshot. Future proxy/CA configuration will be documented alongside
the implementation; setup does not install a proxy CA into Docker's trust store.
