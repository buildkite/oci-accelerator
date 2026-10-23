# OCI Accelerator

OCI Accelerator is an HTTPS proxy that speeds up Docker image pulls by caching
upstream registry content on local disk. Docker Engine sends its registry
traffic through the proxy, which serves repeat downloads of image layers and
digest-addressed manifests from its cache instead of fetching them from the
upstream registry again.

- **Works as a proxy, not a mirror.** Image names and `docker login` stay the
  same, and any upstream registry implementing the Registry HTTP API V2 works.
- **Caches only content addressed by digest.** Blobs and manifests are cached by
  SHA-256 digest. Tags and pushes go straight to the upstream registry.
- **Requires upstream approval.** Cached content is served only after the registry
  approves the same request, or within a fixed approval window. The default is
  15 minutes without `Authorization` and 30 seconds with it.
- **Keeps credentials at the upstream registry.** Tokens are never read or
  stored.
- **Streams large layers.** Layers are never buffered in memory, and concurrent
  pulls of the same layer share one download.
- **Uses one trusted CA.** Docker hosts trust the root CA once, and certificates for
  each upstream registry are signed on demand.
- **Bounded, self-maintaining storage.** Disk use is capped, least-recently-used
  entries are evicted, and objects that don't fit pass through uncached.
- **Optional peer caching.** Instances sharing a CA can discover and remember
  each other over a separate mutually authenticated TLS port. Peering exchanges
  addresses and persistent node IDs, and each instance lists its cache keys to
  authenticated peers. Background polls maintain memory-only provider hints, with
  a default one-minute delay after each attempt and completion-relative hourly
  full scans, both with ±20% jitter. Fetch duration and capacity queueing add to
  the poll delay, so there is no wall-clock freshness guarantee. On an authorized
  eligible blob or manifest miss, the proxy tries one randomly chosen live
  hinted provider before using the origin body. Blob ranges can use the same full-object stream.
  Registry credentials and authorization grants are never shared.
  See [peer discovery](docs/deployment.md#peer-discovery).

See [how it works](docs/how-it-works.md) for the request flow, authorization,
and cache storage in detail.

## Usage

```
oci-accelerator [--env-file FILE] [--check]
oci-accelerator --version
oci-accelerator init-ca --dir DIR
```

| Command                                 | Description                                                                                                                                  |
| --------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------- |
| `oci-accelerator [--env-file FILE]`     | Start using built-in defaults, the optional dotenv file, and process environment overrides.                                                 |
| `oci-accelerator [--env-file FILE] --check` | Validate the same effective configuration without starting the proxy or touching the CA or cache directories.                          |
| `oci-accelerator init-ca --dir DIR`     | Create a root CA in `DIR`, or validate an existing one, without starting the proxy. It ignores environment configuration and prints the certificate path and its SHA-256 fingerprint. |
| `oci-accelerator --version`            | Print the version compiled from the root `VERSION` file and exit, without configuration or startup.                                          |
| `oci-accelerator --help`                | Print usage. `init-ca --help` describes `init-ca`.                                                                                           |

Configuration uses `OCI_ACCELERATOR_*` environment variables only. You can explicitly
select a dotenv file with `--env-file`. Process environment values override file values,
including empty values. No file is discovered automatically or modified. See the
[configuration guide](docs/deployment.md#configuration) for all variables and rules.
YAML configuration and `--config` are no longer supported. If you are upgrading, migrate
those settings to the corresponding `OCI_ACCELERATOR_*` variables.

To publish a versioned image, open a PR that increases `VERSION`. See the
[release process](docs/deployment.md#releasing-images) for tags and retry behavior.

## Quick start

This builds the proxy and runs it on a Linux Docker Engine host, caching Docker
Hub, GitHub Container Registry, and Amazon ECR Public for that host's Docker.

1. From the repository root, build the proxy and create its private state
   directory:

   ```sh
   go build -o bin/oci-accelerator ./cmd/oci-accelerator
   cp .env.example .env
   # Edit .env if you need different registries or paths.
   mkdir -m 0700 data
   ```

2. Start the proxy. The example listens on `127.0.0.1:3128`, creates its root CA
   and cache under `data` on first start, and logs the CA's fingerprint as
   `ca_sha256`:

   ```sh
   bin/oci-accelerator --env-file .env
   ```

3. In another terminal, download the CA certificate and check that its
   fingerprint matches `ca_sha256`. OpenSSL prints it in uppercase with colons.

   ```sh
   curl --noproxy '*' --fail --output ca.crt http://127.0.0.1:3128/ca.crt
   openssl x509 -in ca.crt -noout -fingerprint -sha256
   ```

4. Trust the CA for each upstream registry:

   ```sh
   for registry in registry-1.docker.io ghcr.io public.ecr.aws; do
     sudo install -D -m 0644 ca.crt "/etc/docker/certs.d/$registry/ca.crt"
   done
   ```

5. Add the proxy to `/etc/docker/daemon.json`, keeping any existing settings,
   then restart Docker:

   ```json
   {
     "proxies": {
       "https-proxy": "http://127.0.0.1:3128"
     }
   }
   ```

   ```sh
   sudo systemctl restart docker
   ```

6. Pull as usual. When you pull the same image again, even after
   `docker image rm`, its layers come from the proxy's cache:

   ```sh
   docker pull alpine
   ```

To cache other upstream registries, add them to `OCI_ACCELERATOR_REGISTRIES` and trust the CA
for each one. The [deployment guide](docs/deployment.md) covers running as a
service or container, remote Docker hosts, and every configuration setting.


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

See the [testing guide](docs/testing.md) for their
prerequisites and the other opt-in suites, and [working rules](AGENTS.md) for
contribution and review requirements.
