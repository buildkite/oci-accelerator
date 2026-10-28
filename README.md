# OCI Accelerator

OCI Accelerator is an HTTPS proxy that speeds up Docker image pulls by caching
upstream registry content on local disk and mutable metadata briefly in memory.
Docker Engine sends its registry traffic through the proxy, which serves repeat
downloads of image layers and digest-addressed manifests from its cache instead
of fetching them from the upstream registry again.

- **Works as a proxy, not a mirror.** Image names and `docker login` stay the
  same, and any upstream registry implementing the Registry HTTP API V2 works.
- **Caches content by digest and metadata briefly.** Blobs and digest manifests
  are verified and cached on disk. Successful tag manifest GETs and HEADs, tag
  lists, referrer lists, and catalog responses have a separate memory cache with
  a default 30-second lifetime. Pushes go straight to the upstream registry.
- **Requires upstream approval.** Cached content is served only after the registry
  approves the request, or within a fixed approval window. A single valid nonempty
  Bearer token reuses approval within the same registry and exact repository for
  immutable content and shared mutable metadata. Each token must obtain its own
  upstream approval before using shared responses. Other immutable grants and
  nonshared mutable responses require an exact-request match, including credentials.
  For digest bodies, the default approval window is 15 minutes
  without `Authorization` and 30 seconds with it.
- **Keeps credentials at the upstream registry.** Tokens remain opaque and are
  never persisted or sent to peers. Authorization identities use process-local HMACs.
- **Streams large layers.** Layers are never buffered in memory, and concurrent
  pulls of the same layer share one download.
- **Uses one trusted CA.** Docker hosts trust the root CA once, and certificates for
  each upstream registry are signed on demand.
- **Bounded, self-maintaining storage.** Disk use is capped, least-recently-used
  entries are evicted, and objects that don't fit pass through uncached. Mutable
  response captures and retained entries share a separate 32 MiB memory budget.
- **Optional peer caching.** Instances sharing a CA can discover and remember
  each other over a separate mutually authenticated TLS port. Peering exchanges
  addresses and persistent node IDs, and each instance lists its cache keys to
  authenticated peers. Background polls maintain memory-only provider hints, with
  a default one-minute delay after each attempt and completion-relative hourly
  full scans, both with ±20% jitter. Fetch duration and capacity queueing add to
  the poll delay, so there is no wall-clock freshness guarantee. On an authorized
  eligible blob or manifest miss, the proxy tries one randomly chosen live
  hinted provider before using the origin body. A live Bearer repository grant skips
  upstream only for verified committed local hits. Every local miss needs its own
  upstream GET and accepted redirects before fetching peer bytes, even with a live grant.
  Blob ranges can use the same full-object stream.
  Registry credentials and authorization grants are never shared.
  See [peer discovery](docs/deployment.md#peer-discovery).

See [how it works](docs/how-it-works.md) for the request flow, authorization,
and cache storage in detail.

Mutable responses expire a fixed interval after upstream headers arrive. With
`OCI_ACCELERATOR_CACHE_MUTABLE_SHARED=true` (the default), valid Bearer requests
for tag GETs and HEADs, tag lists, and referrer lists can share responses across
independently approved tokens. Only `Authorization` is omitted from the shared
response key. Every reader needs both a live token-specific repository grant and
an unexpired response. The response can outlive its producer's grant. Catalog,
anonymous, and other non-Bearer requests retain exact-request caching capped by
the applicable authorization TTL. Setting the shared option to `false` restores
that exact matching and TTL cap for all mutable requests, with no mutable grant
creation and no change to immutable caching. Hits never create or renew grants
or extend response expiry, and writes or unobserved access revocation do not
invalidate them early. In shared mode, an observed mutable denial revokes only
the matching token's repository grant, including its immutable reuse, not other
tokens' grants or shared bytes. Set
`OCI_ACCELERATOR_CACHE_MUTABLE_TTL=0s` to disable this cache. It is process-local,
never persisted, and never shared with peers.

## Usage

```
oci-accelerator [--env-file FILE] [--check]
oci-accelerator --version
oci-accelerator init
oci-accelerator init-env [--output FILE]
oci-accelerator init-ca --dir DIR
```

| Command                                 | Description                                                                                                                                  |
| --------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------- |
| `oci-accelerator [--env-file FILE]`     | Start using built-in defaults, the optional dotenv file, and process environment overrides.                                                 |
| `oci-accelerator [--env-file FILE] --check` | Validate the same effective configuration without starting the proxy or touching the CA or cache directories.                          |
| `oci-accelerator init`                 | Create a native starter `.env`, private `data/` and `data/cache/` directories, and a root CA in `data/ca/`. Prints the configuration path, CA certificate path and fingerprint, startup command, and Docker trust and proxy reminder. |
| `oci-accelerator init-env [--output FILE]` | Write the complete native starter configuration without creating state directories or a CA. Defaults to `.env` and refuses to overwrite an existing file. |
| `oci-accelerator init-ca --dir DIR`     | Create a root CA in `DIR`, or validate an existing one, without starting the proxy. It ignores environment configuration and prints the certificate path and its SHA-256 fingerprint. |
| `oci-accelerator --version`            | Print the version compiled from the root `VERSION` file and exit, without configuration or startup.                                          |
| `oci-accelerator --help`                | Print usage. `init --help`, `init-env --help`, and `init-ca --help` describe each initialization command.                                      |

Configuration uses `OCI_ACCELERATOR_*` environment variables only. You can explicitly
select a dotenv file with `--env-file`. Process environment values override file values,
including empty values. Startup never discovers or modifies a configuration file. See the
[configuration guide](docs/deployment.md#configuration) for all variables and rules.

`init` and `init-env` ignore ambient environment variables and generate the same complete
configuration as `.env.example`. Its registry list and `./data` paths are native starter
examples, not runtime defaults. `init` accepts no options other than help and refuses an
existing `.env` before creating directories. For configuration only or a custom layout,
use [the initialization options](docs/deployment.md#initialization-options).

Download unsigned Linux and macOS binaries for amd64 or arm64 from
[GitHub Releases](https://github.com/buildkite/oci-accelerator/releases). Each
`oci-accelerator-OS-ARCH-vX.Y.Z.tar.gz` archive contains one `oci-accelerator`
executable. Verify it against `oci-accelerator-vX.Y.Z.SHA256SUMS` before
extracting. See [binary installation](docs/deployment.md#download-a-release)
for commands and macOS quarantine limitations.

To publish binaries and a versioned Docker image, open a PR that increases
`VERSION`, then approve **Publish to Docker Hub and GitHub?** in Buildkite after
merging. Git tag builds do not publish. Ordinary `main` builds publish only
Docker `latest` and `build-N` tags after approval. See the
[release process](docs/deployment.md#releasing-images) for Git tags, Docker tags,
and retry behavior.

## Quick start

This builds the proxy and runs it on a Linux Docker Engine host, caching Docker
Hub, GitHub Container Registry, and Amazon ECR Public for that host's Docker.

1. From the repository root, build the proxy and initialize its native layout:

   ```sh
   go build -o bin/oci-accelerator ./cmd/oci-accelerator
   bin/oci-accelerator init
   # Edit .env if you need different registries.
   ```

   Initialization creates `.env`, private `data/` and `data/cache/` directories,
   and the CA in `data/ca/`. It prints the CA certificate path and fingerprint.
   It does not start the proxy or configure Docker.

2. Start the proxy. The example listens on `127.0.0.1:3128`, uses the initialized
   CA and cache under `data`, and logs the CA's fingerprint as `ca_sha256`:

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
