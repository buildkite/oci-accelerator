# Deploying and using OCI Accelerator

OCI Accelerator is an HTTP CONNECT proxy. It intercepts configured HTTPS registries and caches
verified SHA-256 blobs and digest manifests. All other destinations are tunneled unmodified.
See [how it works](how-it-works.md) for the request flow and design.

| Client or platform                                                        | Status        |
| ------------------------------------------------------------------------- | ------------- |
| Linux Docker Engine                                                       | Validated     |
| Docker Desktop, Windows, containerd, BuildKit-specific integration        | Not validated |

## Security model

- **No client authentication.** Anyone who can reach the listener can open opaque CONNECT
  tunnels to arbitrary hosts and ports, which cannot be inspected or restricted to image
  pulls. Keep the default loopback bind, or bind a private interface behind network access
  controls and a firewall. Never expose the proxy through an Amp portal.
- **The CA can sign any server identity.** Protect `ca.key`, never give it to Docker clients
  or bake it into an image, and scope client trust to each intercepted registry rather than
  system-wide. [Peering](#peer-discovery) requires securely provisioning the same CA key to
  each trusted accelerator instance.
- **The CA download is unauthenticated.** Verify its fingerprint through a trusted channel
  before installing it ([details](#trust-the-ca)).
- **Upstream TLS is always verified**, using the proxy process's system trust store. There is
  no insecure mode.
- **Credentials stay with Docker.** Never put credentials in proxy configuration. Cross-origin redirects
  never forward credentials or cookies.
- **The upstream registry authorizes deliveries.** Each cached delivery is approved by an
  upstream GET or an exact-request grant. The default window is 15 minutes without
  `Authorization` and 30 seconds with it, including an empty value. Public-to-private
  revocation can therefore lag by 15 minutes by default. Set both authorization TTLs to
  `0s` to check every delivery. There is no offline or failed-request fallback.
- **Logs contain repository names, tags, and digests.** Restrict access to them.
- **Metrics and probes are unauthenticated.** They bind loopback only unless explicitly opted in.
- **Redirect checks do not constrain tunnels.** Destination checks protect only fetches the
  proxy makes itself, so deployment network controls remain essential.

## Deploy

### Prerequisites

| Deployment | Requires                                                              |
| ---------- | --------------------------------------------------------------------- |
| Native     | Go 1.27.1 to build, and a service manager to supervise the process    |
| Container  | Linux Docker daemon with Buildx, and Compose for the supplied service |

Neither CA initialization nor proxy startup creates ancestor directories. Before first start, you must
provision the immediate parent of `OCI_ACCELERATOR_CA_DIR` and
`OCI_ACCELERATOR_CACHE_DIR` as the proxy's user (see
[state directories](#state-directories)).

### Run the binary

From the repository root:

```sh
go build -o bin/oci-accelerator ./cmd/oci-accelerator
cp .env.example .env
# Edit registries if needed, then validate:
bin/oci-accelerator --env-file .env --check
```

`--check` validates only the configuration. It does not inspect CA or cache state, bind
listeners, or create directories.

Create the state parent (`data` in the example) only if absent. If it exists, check its
ownership and permissions. Optionally provision the CA ahead of time, then start:

```sh
mkdir -m 0700 data
bin/oci-accelerator init-ca --dir data/ca   # optional (startup creates it otherwise)
bin/oci-accelerator --env-file .env
```

Then [configure Docker hosts](#configure-docker-hosts).

### Run under a service manager

Run the process under the host's service manager. It stops gracefully on SIGINT or SIGTERM
(see [shutdown](#shutdown)). For Amp orbs, see the [appendix](#appendix-amp-orbs).

### Run in a container

#### Image

The root `Dockerfile` builds a static Go binary into an Alpine-based image.

| Property          | Value                                           |
| ----------------- | ----------------------------------------------- |
| Local tag         | `oci-accelerator:local`                         |
| User and group    | `10001:10001`                                   |
| Entrypoint        | `/usr/local/bin/oci-accelerator`                |
| Default arguments | None                                            |
| Health check      | `wget` of `http://127.0.0.1:9090/readyz`        |
| State directories | `/var/lib/oci-accelerator` and its `cache/`, UID/GID 10001, mode 0700 (`ca/` is left absent) |

The image sets four environment defaults that match its own layout:

| Variable                         | Image default                    | Why                                              |
| -------------------------------- | -------------------------------- | ------------------------------------------------ |
| `OCI_ACCELERATOR_LISTEN`         | `0.0.0.0:3128`                   | A published port must reach the proxy             |
| `OCI_ACCELERATOR_METRICS_LISTEN` | `127.0.0.1:9090`                 | The health check probes it                        |
| `OCI_ACCELERATOR_CA_DIR`         | `/var/lib/oci-accelerator/ca`    | The image creates and owns its parent             |
| `OCI_ACCELERATOR_CACHE_DIR`      | `/var/lib/oci-accelerator/cache` | The image creates and owns this directory         |

You must still set `OCI_ACCELERATOR_REGISTRIES`, so the image never chooses which hosts to
intercept for you.

**The image listens on all container interfaces.** Publishing the port only on a trusted host
address, as in the examples below, limits how the host exposes it. Any container on the same
Docker network can still connect to the proxy directly, so keep untrusted workloads off that
network. With `--network host`, or with a port published on all host addresses, such as
`-p 3128:3128`, the proxy accepts connections on every host interface. In those setups, set
`OCI_ACCELERATOR_LISTEN` to a loopback or private address.

How image defaults interact with other settings:

- Docker `-e`, Docker `--env-file`, and Compose `environment` or `env_file` values override
  image defaults.
- Image defaults are process environment variables. They therefore override an application
  `--env-file` passed after the image name. Configure containers through Docker or Compose
  instead.
- You cannot unset an image default, only replace it. An empty value counts as set: an empty
  `OCI_ACCELERATOR_METRICS_LISTEN` disables metrics, and an empty `OCI_ACCELERATOR_LISTEN`,
  `OCI_ACCELERATOR_CA_DIR`, or `OCI_ACCELERATOR_CACHE_DIR` fails validation.

The health check needs the metrics listener at `127.0.0.1:9090`. Keep it there or override
the health check when changing it. An unhealthy result is diagnostic and does not itself
trigger a restart. Readiness does not check upstream connectivity or disk capacity.

#### Container environment file

`.env.container-example` holds the container settings: `OCI_ACCELERATOR_REGISTRIES` and a
copy of the four image defaults. Copy it to `.env.container`, which Git ignores, and edit
the copy. Values in the copy override the image defaults. See `.env.example` for every other
setting and its default.

Docker and Compose read this file with their own dotenv syntax, not the application's. Keep it
to unquoted literal `KEY=value` lines and full-line comments.

#### Compose

The supplied `compose.yaml` service builds the local image from the repository root and:

- requires `env_file: .env.container`
- publishes only `127.0.0.1:3128`
- keeps the root filesystem read-only, drops all capabilities, and sets `no-new-privileges`
- uses `restart: unless-stopped` and a 40-second stop grace period, allowing for the 15-second
  `OCI_ACCELERATOR_SHUTDOWN_TIMEOUT` and metrics cleanup (increase it if you raise
  `OCI_ACCELERATOR_SHUTDOWN_TIMEOUT`)

| Source                             | Mount point                      | Access   |
| ---------------------------------- | -------------------------------- | -------- |
| `oci-accelerator-ca` volume        | `/var/lib/oci-accelerator`       | Writable |
| `oci-accelerator-cache` volume     | `/var/lib/oci-accelerator/cache` | Writable |

Volume notes:

- The CA volume mounts the parent so startup can atomically create its `ca/` child. Docker
  copies the image's ownership and permissions into fresh volumes, so you don't need to
  provision host directories or use a root entrypoint. The CA volume remains writable at runtime.
- Existing valid CA files are reused, not replaced.
- Run only one proxy against these volumes at a time. The names match the
  [`docker run`](#docker-run) example. Compose warns when reusing volumes created by
  `docker run` because it does not own them.
- These examples do not migrate existing host bind-mounted data. If you start with new volumes,
  the proxy creates a new CA, and you must redistribute client trust.

Build, validate, and start:

```sh
cp .env.container-example .env.container
# Edit .env.container if you need different registries.
docker compose build
docker compose run --rm --no-deps oci-accelerator --check
# Optional: provision the CA before first start. init-ca needs the image's CA path explicitly.
docker compose run --rm --no-deps oci-accelerator init-ca --dir /var/lib/oci-accelerator/ca
docker compose up -d
```

Compose does not automatically pass exported host variables into the container. To override
settings, edit `.env.container`, or add an `environment` mapping to the service in a local
Compose override file. Compose `environment` values override `env_file` values:

```yaml
services:
  oci-accelerator:
    environment:
      OCI_ACCELERATOR_CACHE_MAX_BYTES: "68719476736"
      OCI_ACCELERATOR_REGISTRIES: "registry-1.docker.io, ghcr.io"
```

You can put literal values in this mapping, or use Compose interpolation such as
`OCI_ACCELERATOR_CACHE_MAX_BYTES: "${OCI_ACCELERATOR_CACHE_MAX_BYTES}"`. The latter requires
you to set the host variable before running Compose.

`--check` does not inspect the mounted CA or cache state. First startup generates the CA. Then
[configure Docker hosts](#configure-docker-hosts), installing only the public certificate.

| Command                                   | Effect on volumes                                      |
| ----------------------------------------- | ------------------------------------------------------ |
| `docker compose down`                     | Preserves both                                         |
| `docker volume rm oci-accelerator-cache`  | Deletes only the cache ([reset the cache](#reset-the-cache)) |
| **`docker compose down -v`**              | **Deletes both, including the signing CA**             |

Preserve the CA volume independently of the replaceable cache. Deleting it makes the next
startup generate a new identity and requires redistributing client trust.

#### docker run

The equivalent without Compose, from the repository root:

```sh
docker build -t oci-accelerator:local .
cp .env.container-example .env.container
# Edit .env.container if you need different registries.
# Optional: provision the CA before first start.
docker run --rm --read-only \
  --mount type=volume,src=oci-accelerator-ca,dst=/var/lib/oci-accelerator \
  oci-accelerator:local init-ca --dir /var/lib/oci-accelerator/ca
docker run -d --name oci-accelerator --restart unless-stopped \
  --env-file .env.container \
  --read-only --cap-drop ALL --security-opt no-new-privileges \
  --stop-timeout 40 -p 127.0.0.1:3128:3128 \
  --mount type=volume,src=oci-accelerator-ca,dst=/var/lib/oci-accelerator \
  --mount type=volume,src=oci-accelerator-cache,dst=/var/lib/oci-accelerator/cache \
  oci-accelerator:local
docker logs oci-accelerator
docker inspect --format '{{.State.Health.Status}}' oci-accelerator
```

Stop it with `docker stop oci-accelerator`. Removing the container preserves both named volumes.

Docker's `--env-file` appears before the image name, so Docker injects the values into the
container. Add `-e` to override individual values. Docker applies `-e` after `--env-file`.
Because the image supplies the other container defaults, you can instead pass only the
registries:

```sh
docker run -d --name oci-accelerator --restart unless-stopped \
  --read-only --cap-drop ALL --security-opt no-new-privileges \
  --stop-timeout 40 -p 127.0.0.1:3128:3128 \
  --mount type=volume,src=oci-accelerator-ca,dst=/var/lib/oci-accelerator \
  --mount type=volume,src=oci-accelerator-cache,dst=/var/lib/oci-accelerator/cache \
  -e OCI_ACCELERATOR_REGISTRIES=registry-1.docker.io,ghcr.io,public.ecr.aws \
  oci-accelerator:local
```

Docker exposes the proxy only on the host's loopback address. Metrics stay on container
loopback for the image health check and are not published. The UID 10001 state volumes and
isolation flags match the Compose example. Exported host variables are not implicit container
overrides. Use `-e`, `--env-file`, or a Compose `environment` mapping.

#### Multi-architecture images

Buildx can load one platform at a time:

```sh
docker buildx build --platform linux/amd64 --load -t oci-accelerator:amd64 .
docker buildx build --platform linux/arm64 --load -t oci-accelerator:arm64 .
```

- Building or running the arm64 image requires a native arm64 host or configured emulation: Go
  compilation runs on the builder's architecture, but Alpine's runtime setup executes on the
  target architecture.
- For a multi-platform OCI archive, use a Buildx builder with the `docker-container` driver and
  an OCI output instead of `--load`.
- Buildkite validates both architectures and publishes successful current `main` builds to
  `buildkite/oci-accelerator:latest` and `buildkite/oci-accelerator:build-N`, where `N` is
  the Buildkite build number. Version bumps also publish a version tag.

#### Releasing images

To cut a release, open a PR that increases the root [`VERSION`](../VERSION) file, for
example from `v0.1.0` to `v0.1.1`. Use one stable `vMAJOR.MINOR.PATCH` version, with a
lowercase `v` prefix and without leading zeros, prerelease suffix, or build metadata.
CI validates its format and checks numeric increases against the PR's merge base with
`main`. Historical unprefixed versions are accepted for comparison. Adding the prefix
alone, such as `0.1.0` to `v0.1.0`, publishes the prefixed tag without changing the old tag.

After you merge the PR, the non-PR `main` build compares `VERSION` with its first parent.
If the value changed, it publishes `buildkite/oci-accelerator:v0.1.1` for both `linux/amd64`
and `linux/arm64`, after formatting, vet, build, tests, race tests, and the image check pass.
Adding `VERSION` for the first time also triggers a release. Git tag builds do not publish.

Successful release publication adds a success annotation to the Buildkite build with
the version, image, platforms, Docker Hub and source links, and a `docker pull` command.
Retries replace the same annotation and say when the version is already published from
the same commit. Ordinary `build-N` and `latest` updates do not add release annotations.
If writing the annotation fails, the job logs a warning but still succeeds because
the release is published. Retry the original publishing job to restore the notice.

The version tag identifies the merged commit, not whichever commit is newest when CI
finishes. A release still publishes if `main` has advanced, but that older build does not
update `latest` or `build-N`. Ordinary builds with unchanged versions publish only those
two moving tags when their checkout is the current `main` tip at the publishing check.

If publication fails, retry the publishing job for the original release build. An existing
version tag with matching source-revision and version metadata is left untouched. A tag
with different or missing metadata fails the job, as do authentication and registry errors.
You can retry a partially published build without replacing its version tag. A retry at
the current `main` tip can still rebuild and publish `latest` and `build-N`.

Publishing jobs share one Buildkite concurrency group. Keep all writers to this image
repository in that group. The check before pushing is not an atomic registry lock and does
not protect against an unrelated writer. Keep branch builds enabled and disable queued-build
skipping, running-build cancellation, and filters that exclude version-only commits. If you
cancel a release build or skip it with a commit-message directive, a later ordinary build
does not recover that release automatically. Run the original release commit again.

The binary embeds `VERSION`, including in local `go build` and Docker builds. Run
`oci-accelerator --version` or `docker run --rm buildkite/oci-accelerator:v0.1.1 --version`
without a configuration file to print `oci-accelerator v0.1.1`. Between bumps, multiple
`main` builds report the same version. Images built by the pipeline also carry
`org.opencontainers.image.version` and `org.opencontainers.image.revision` as image labels
and multi-platform index annotations so you can identify the exact source commit.

### Serve remote Docker hosts

The default `127.0.0.1:3128` listener serves Docker on the same host. To serve remote hosts:

1. Set `OCI_ACCELERATOR_LISTEN` to an explicit private-interface address.
2. Restrict access with network access controls and a firewall (see [security model](#security-model)).
3. Use the proxy's private address in each host's [Docker proxy configuration](#point-docker-at-the-proxy).

## Configuration

See [`.env.example`](../.env.example) for all settings and their actual defaults.
`OCI_ACCELERATOR_REGISTRIES`, `OCI_ACCELERATOR_CA_DIR`, and
`OCI_ACCELERATOR_CACHE_DIR` are required. The container image supplies the two directories and
other container defaults (see [image](#image)). The registries and `./data` paths in that file
are examples, not built-in defaults. Configuration applies built-in defaults, an optional explicitly
selected dotenv file, then process environment overrides. There is no file discovery,
environment mutation, `OCI_ACCELERATOR_CONFIG` variable, or hot reload.

YAML configuration and `--config` are not supported. When migrating an older deployment,
replace YAML setting names with the `OCI_ACCELERATOR_*` variables below.

`--check` validates this same combined configuration without inspecting state, binding listeners,
or creating directories. Unknown process environment variables are ignored. Unknown names in a
dotenv file are rejected.

An unset variable retains the file value or default. A present empty variable is an explicit
value. It disables `OCI_ACCELERATOR_METRICS_LISTEN` and clears
`OCI_ACCELERATOR_REDIRECT_ALLOWLIST`. Empty `OCI_ACCELERATOR_PEER_LISTEN` disables peering,
provided `OCI_ACCELERATOR_PEER_ADVERTISE`, `OCI_ACCELERATOR_PEER_SEEDS`, and
`OCI_ACCELERATOR_PEER_STATE_DIR` are also empty. Peer hint timings still accept overrides
and use their defaults when peering is disabled. An empty `OCI_ACCELERATOR_PEER_SEEDS` is
valid even when peering is enabled. Other empty values fail validation, including required
registries and paths. `OCI_ACCELERATOR_REGISTRIES` and `OCI_ACCELERATOR_REDIRECT_ALLOWLIST`
are comma-separated authority lists. Entries are whitespace-trimmed, the
whole list replaces the file value, and empty members are rejected.

Use exactly `true` or `false` for booleans, decimal integers without a sign or leading zeros
for numbers, and Go duration strings such as `500ms`, `30s`, or `2m` for durations. Relative
paths from all sources are resolved from the process working directory and converted
internally to absolute paths.

For example, process environment values can override a native dotenv file:

```sh
OCI_ACCELERATOR_CACHE_MAX_BYTES=68719476736 bin/oci-accelerator --env-file .env --check
OCI_ACCELERATOR_CACHE_MAX_BYTES=68719476736 bin/oci-accelerator --env-file .env
```

### Dotenv file rules

- A file is read only with `--env-file FILE`. A missing named file is an error.
- Use `KEY=value`, with no surrounding whitespace in the key. Outer whitespace in values is
  trimmed. Blank lines and full-line comments are accepted.
- In an unquoted value, `#` starts an inline comment at the beginning of the value or after a
  space or tab. Otherwise it is literal.
- Optional matching single or double quotes group the value. The contents are literal, with no
  escapes or interpolation. `export` and multiline values are not supported.
- Each line must be shorter than 64 KiB. Duplicate and unknown keys are rejected.
- The complete file syntax is checked before effective typed values are validated. A valid
  process value can override an invalid typed file value, but cannot hide a syntax error.

### Registry authorities

Each `OCI_ACCELERATOR_REGISTRIES` and `OCI_ACCELERATOR_REDIRECT_ALLOWLIST` entry is an
authority: `host` or `host:port`, with
no scheme or path. An omitted port means 443, and matching includes the port.

| Entry                   | Matches                  |
| ----------------------- | ------------------------ |
| `ghcr.io`               | `ghcr.io:443`            |
| `registry.example:5000` | `registry.example:5000`  |
| `[2001:db8::1]:5000`    | IPv6 with a port (bracketed) |

Entries differing only in case, a trailing dot, or `:443` are duplicates and are rejected.
You can configure private registries explicitly.

### State directories

- `OCI_ACCELERATOR_CA_DIR` and `OCI_ACCELERATOR_CACHE_DIR` must both be nonblank and resolve
  to distinct, lexically normalized
  absolute paths. There is no forwarding-only mode.
- The CA directory must not be inside the cache directory, because the cache owns, and may
  remove, entries in its directory. You can place the cache directory inside the CA directory.
- Before first start, create the immediate parent of each directory as the proxy's user,
  without group or other write access (for example `mkdir -m 0700 data`). The directories
  themselves are created at startup, but no ancestors are.
- Configuration validation does not resolve symlinks or inspect state. Filesystem ownership and
  layout checks apply at startup ([CA](#ca-directory-requirements), [cache](#capacity-and-eviction)).

### Settings

**Listeners**

| Environment variable   | Default          | Notes                                                                                    |
| ---------------------- | ---------------- | ---------------------------------------------------------------------------------------- |
| `OCI_ACCELERATOR_LISTEN` | `127.0.0.1:3128` | host:port with a nonzero numeric port. Empty or wildcard hosts bind all interfaces      |
| `OCI_ACCELERATOR_METRICS_LISTEN` | Disabled | host:port with a nonzero numeric port. Literal loopback IP required unless `OCI_ACCELERATOR_METRICS_ALLOW_REMOTE` |
| `OCI_ACCELERATOR_METRICS_ALLOW_REMOTE` | `false` | Explicit boolean opt-in to non-loopback metrics and probe binding                       |

**Peer discovery**

| Environment variable | Default | Notes |
| --- | --- | --- |
| `OCI_ACCELERATOR_PEER_LISTEN` | Disabled | Separate TLS listener, host:port with a nonzero numeric port |
| `OCI_ACCELERATOR_PEER_ADVERTISE` | Empty | Required reachable host:port when peering is enabled |
| `OCI_ACCELERATOR_PEER_SEEDS` | Empty | Optional comma-separated host:port bootstrap addresses |
| `OCI_ACCELERATOR_PEER_STATE_DIR` | Empty | Required private persistent directory when peering is enabled |
| `OCI_ACCELERATOR_PEER_HINT_REFRESH_TIMEOUT` | `1m` | Positive overall duration per cache-key fetch attempt, excluding scheduler queueing. No unlimited value |
| `OCI_ACCELERATOR_PEER_HINT_REFRESH_INTERVAL` | `1m` | Positive poll delay after each attempt completes, with ±20% jitter |
| `OCI_ACCELERATOR_PEER_HINT_FULL_REFRESH_INTERVAL` | `1h` | Positive full-scan interval from successful full-scan completion, with ±20% jitter |

**Registries and state**

| Environment variable | Default  | Notes                                                                     |
| -------------------- | -------- | ------------------------------------------------------------------------- |
| `OCI_ACCELERATOR_REGISTRIES` | Required | Nonempty list of distinct [authorities](#registry-authorities) to intercept |
| `OCI_ACCELERATOR_REDIRECT_ALLOWLIST` | Empty | Private HTTPS storage [authorities](#registry-authorities). Does not enable interception |
| `OCI_ACCELERATOR_CA_DIR` | Required | Private persistent CA directory                                           |
| `OCI_ACCELERATOR_CACHE_DIR` | Required | Private persistent cache directory                                        |

**Cache and authorization**

| Environment variable              | Default | Notes                                                                                   |
| --------------------------------- | ------- | --------------------------------------------------------------------------------------- |
| `OCI_ACCELERATOR_CACHE_MAX_BYTES` | 32 GiB  | Includes body, metadata, and fill reservations                                          |
| `OCI_ACCELERATOR_CACHE_MAX_FILLS` | 16      | Concurrent storage writers. Excess misses stream uncached                               |
| `OCI_ACCELERATOR_CACHE_MAX_ENTRIES` | 100000 | Entry slots, including fills and pending cleanup                                        |
| `OCI_ACCELERATOR_CACHE_FILL_TIMEOUT` | `10m` | Bounds ranged fetches from request start. Ends ordinary full-GET cache admission        |
| `OCI_ACCELERATOR_CACHE_FILL_WAIT_TIMEOUT` | `1s` | `0s` disables waiting for an existing fill                                           |
| `OCI_ACCELERATOR_CACHE_AUTHORIZATION_TTL` | `30s` | Fixed approval lifetime for requests with Authorization, including empty. `0s` disables that class |
| `OCI_ACCELERATOR_CACHE_ANONYMOUS_AUTHORIZATION_TTL` | `15m` | Fixed approval lifetime for requests without Authorization. `0s` disables that class |
| `OCI_ACCELERATOR_CACHE_MAX_AUTHORIZATION_ENTRIES` | 10000 | Positive shared bound on both classes of in-memory authorization grants               |

**Timeouts and connections**

| Environment variable      | Default | Notes                                                        |
| ------------------------- | ------- | ------------------------------------------------------------ |
| `OCI_ACCELERATOR_DIAL_TIMEOUT` | `10s` |                                                              |
| `OCI_ACCELERATOR_TLS_HANDSHAKE_TIMEOUT` | `10s` |                                                     |
| `OCI_ACCELERATOR_RESPONSE_HEADER_TIMEOUT` | `30s` | Also bounds incoming request headers                |
| `OCI_ACCELERATOR_IDLE_TIMEOUT` | `2m` | Idle tunnels and I/O                                         |
| `OCI_ACCELERATOR_SHUTDOWN_TIMEOUT` | `15s` | Drain deadline                                           |
| `OCI_ACCELERATOR_MAX_CONNECTIONS` | 256 | Accepted client connections, including incomplete CONNECTs   |
| `OCI_ACCELERATOR_MAX_LEAF_CERTIFICATES` | 256 | Cached interception certificates                         |

Timeout behavior:

- For eligible blob range requests, `OCI_ACCELERATOR_CACHE_FILL_TIMEOUT` bounds the upstream
  fetch, starting
  before the first upstream GET, including when cache-fill admission is refused. Admitted
  background fills remain subject to the same deadline.
- Delivery from an already-open cached reader is not limited by
  `OCI_ACCELERATOR_CACHE_FILL_TIMEOUT`, but idle-I/O deadlines and shutdown handling still apply.
- Other active transfers have idle-I/O deadlines, not total download deadlines. Uploads have no
  total size limit ([push and delete](#push-and-delete)).
- Kernel filesystem calls cannot be preempted, so fill expiry and shutdown are not hard
  deadlines when storage stalls.

Memory: `OCI_ACCELERATOR_CACHE_MAX_ENTRIES` and
`OCI_ACCELERATOR_CACHE_MAX_AUTHORIZATION_ENTRIES` bound their indexes, not total
process RSS, so tune both to available memory. Bodies stream rather than accumulate in memory.

### Built-in limits

| Limit                                        | Value                              |
| -------------------------------------------- | ---------------------------------- |
| CONNECT request and upstream response headers | 64 KiB                            |
| Upstream connections per destination         | 16                                 |
| Idle upstream connections                    | 32 globally, 8 per destination     |
| Redirects followed                           | 5                                  |
| Cache metadata charge                        | 4 KiB per entry                    |
| Eviction removal attempts per admission      | 8                                  |
| CA PEM file size                             | 64 KiB each                        |
| Root CA validity                             | 10 years                           |
| Leaf certificate validity                    | At most 24 hours                   |
| Intercepted protocols                        | TLS 1.2 or newer, HTTP/1.1         |

## CA management

The CA is `ca.crt` and `ca.key` in `OCI_ACCELERATOR_CA_DIR`: `data/ca/` on the host in the native example, or
`ca/` in the `oci-accelerator-ca` volume for containers. See
[trusting the accelerator's certificate](how-it-works.md#trusting-the-accelerators-certificate)
and [per-registry certificates](how-it-works.md#per-registry-certificates) for how the CA and
leaf certificates are used.

### Create the CA

The CA is created on first startup, or ahead of time with `init-ca`. Both only create it when
the CA directory is absent. Leave it absent initially and do not pre-create an empty directory.

```sh
bin/oci-accelerator init-ca --dir data/ca
```

- `init-ca` provisions only the root CA. It does not read `OCI_ACCELERATOR_*` variables or an
  environment file, and it does not start the proxy, cache, or any network activity.
- You must pass `--dir`. Use the same path as `OCI_ACCELERATOR_CA_DIR`, so that startup reuses
  the CA you provisioned. Relative paths are resolved from the working directory, and the parent
  must already exist.
- On success it prints `ca_certificate="..."` (the public certificate path) and `ca_sha256=...`
  (the lowercase SHA-256 fingerprint). It never prints the private key. Startup logs the same
  `ca_sha256` for trusted distribution.
- A valid existing pair is validated and reused unchanged, preserving its ownership and
  permissions.
- New CAs use a P-256, ten-year root CA profile.
- Provisioning does not install client trust or enable automatic rotation.
- Keep the containing path trusted and on durable storage.

Initialization stages the certificate and key in a private `.ca-init-*` directory under the CA
parent, then atomically renames it into place. Concurrent initializers adopt the published
winner. There is no persistent lock.

### CA directory requirements

Proxy startup and `init-ca` apply the same checks. Provision the parent and ensure its
durability first, so permissions do not depend on the umask and a crash cannot lose newly
created ancestors.

| Path              | Type                    | Owner               | Mode                            |
| ----------------- | ----------------------- | ------------------- | ------------------------------- |
| Immediate parent  | Non-symlink directory   | Process's effective user | No group or other write access |
| CA directory      | Non-symlink directory   | Process's effective user | Exactly 0700                |
| `ca.crt`          | Regular file, ≤ 64 KiB PEM | Process's effective user | At most 0644 (new: 0644)  |
| `ca.key`          | Regular file, ≤ 64 KiB PEM | Process's effective user | At most 0600 (new: 0600)  |

- The owner is the native process user, or UID 10001 in the container. Another user's
  installation is rejected, even when running as root.
- Ancestors above the parent are not checked, so keep the containing path trusted.
- Stricter file modes are accepted.
- The certificate must match the generated profile: a self-signed X.509 v3 CA with an ECDSA
  P-256 key, ECDSA-SHA256 signature, and only key usage, basic constraints, and subject key
  identifier extensions. Matching the profile establishes compatibility, not provenance.
- Every startup re-checks the files, profile, key pairing, and validity period.
- Empty, incomplete, expired, mismatched, oversized, symlinked, foreign-owned, or incorrectly
  permissioned state fails startup without replacing or modifying any file. Errors name the
  failing check but never include certificate or private-key contents.

### Back up and replace the CA

Preserve `OCI_ACCELERATOR_CA_DIR` across restarts, back up both CA files securely, and use one proxy process
per CA directory. Rotation is not automatic. **Never delete an established CA to repair
startup.**

To replace a CA:

1. Stop the proxy.
2. Archive the old CA securely.
3. Configure a new, absent CA directory.
4. Start the proxy.
5. Verify the new fingerprint.
6. Redistribute `ca.crt` ([trust the CA](#trust-the-ca)).
7. Remove the old client trust.

### Clean up staging directories

A crash or SIGKILL during initialization can leave a `.ca-init-*` directory in the CA parent.
It does not prevent later startup, and it is never adopted or removed automatically, because it
may belong to another active initializer. Staging directories may contain private keys: keep
their restrictive permissions and do not print or copy their contents.

To clean up:

1. Stop every process that can initialize a CA in that parent, and keep them stopped.
2. Inspect only that trusted parent.
3. Remove each orphaned `.ca-init-*` directory by its exact path. Never use a wildcard deletion
   or remove the configured CA directory.

## Configure Docker hosts

These steps are for rootful Linux Docker Engine. Image names, `docker login`, and
Docker-owned credentials do not change.

### Trust the CA

1. Download the public certificate from the proxy's HTTP listener. `PROXY_URL` is the same
   `http://` address used in Docker's proxy configuration:

   ```sh
   curl --noproxy '*' --fail --show-error --output ca.crt "$PROXY_URL/ca.crt"
   openssl x509 -in ca.crt -noout -fingerprint -sha256
   ```

2. Compare the fingerprint with `ca_sha256` from `init-ca` or startup logs, obtained through a
   trusted channel such as SSH or your deployment system. OpenSSL prints uppercase
   colon-separated hex, while the proxy logs lowercase hex without colons.
   **Do not install the certificate if they differ, or if you cannot obtain a trusted expected
   fingerprint.** The download is unauthenticated: an attacker could replace the certificate,
   and a fingerprint fetched over the same connection would not establish authenticity.
   Alternatively, transfer only the public `ca.crt` through an already trusted channel.

3. Install the certificate for each intercepted registry, rather than adding system-wide trust:

   ```sh
   for registry in registry-1.docker.io ghcr.io public.ecr.aws; do
     sudo install -D -m 0644 ca.crt "/etc/docker/certs.d/$registry/ca.crt"
   done
   ```

   The directory name is the exact authority from image names: use `registry.example:5000` for
   a registry on port 5000, and omit `:443` when image names omit it. Never give `ca.key`
   to Docker clients or install it as a Docker client key. See Docker's
   [certificate instructions](https://docs.docker.com/engine/security/certificates/) for other
   installation modes and platforms.

### The `/ca.crt` endpoint

- `GET /ca.crt` and `HEAD /ca.crt` are always available on the proxy listener to anyone who can
  reach it, with no registry credentials or additional configuration.
- It serves the root CA loaded by the running process, never the private key. It does not
  install trust or rotate certificates.
- Only the exact `/ca.crt` path matches. Other methods return `405`, and every response closes
  its connection.

### Point Docker at the proxy

Merge this into `/etc/docker/daemon.json` without replacing unrelated settings:

```json
{
  "proxies": {
    "https-proxy": "http://127.0.0.1:3128"
  }
}
```

After arranging for workload disruption, restart Docker and pull as usual:

```sh
sudo systemctl restart docker
docker login ghcr.io
docker pull ghcr.io/your-organization/your-image:tag
```

- The proxy URL is `http://`: Docker sends CONNECT, then negotiates registry TLS inside the
  tunnel.
- For a remote deployment, use the proxy's private address instead of loopback.
- Do not route HTTP registries through this listener.
- Do not list intercepted registries in `no-proxy`, or in a `NO_PROXY` inherited from the
  daemon's service environment. An empty `no-proxy` value in `daemon.json` does not clear it.
- Shell proxy variables do not configure an already-running daemon. Docker also documents
  [daemon proxy configuration](https://docs.docker.com/engine/daemon/proxy/) for
  service-environment setups.
- For a private registry, the proxy process must also trust the upstream registry's real CA in its
  system trust store, as well as Docker. Do not disable TLS verification or configure an
  insecure registry as a workaround.

### Push and delete

Docker push uses the same proxy and CA configuration as pull. 

- The upstream registry enforces write permissions using Docker's credentials.
- Uploads do not warm the cache. The proxy does not manage upload sessions or follow write
  redirects.
- Active uploads are limited by `OCI_ACCELERATOR_IDLE_TIMEOUT` for stalled I/O, not by
  `OCI_ACCELERATOR_CACHE_FILL_TIMEOUT` or
  a total upload size.
- Successful deletion does not invalidate existing pull [authorization grants](#authorization-grants).
  Previously approved exact GETs may still serve cached content until the original grant
  expiry. Set both authorization TTLs to `0s` when every cache delivery must check current
  upstream access.

## Request handling

[How it works](how-it-works.md) describes what is cached, range requests, redirects, and
authorization approval. This section lists additional rules that may affect your deployment.

### Methods and protocols

| Request                                                        | Handling                                            |
| -------------------------------------------------------------- | --------------------------------------------------- |
| GET, HEAD, OPTIONS                                             | Forwarded. Requests with a body are denied           |
| POST, PUT, PATCH, DELETE                                       | Streamed upstream without caching or path filtering |
| Tag, `/v2/`, HEAD, conditional requests, ineligible ranges     | Always passed through                               |
| Upgrades, other methods, authority conflicts, non-origin-form targets | Denied                                       |

### Range requests

Eligible range requests are unconditional SHA-256 blob Range GETs.

- They go upstream on every request as full GETs, and never look up, create, invalidate, or
  otherwise update authorization grants.
- A validated cache hit is served with standard single, suffix, or multipart range semantics
  from its pinned reader.
- Until the full body is cached, a single range is selected from the full upstream stream and
  returned as `206`, while comma-separated ranges fall back to the full `200`.
- An admitted fill continues reading and verifying the same GET after the selected range ends or
  the client disconnects, bounded by `OCI_ACCELERATOR_CACHE_FILL_TIMEOUT` from request start.
- Ranged manifests, `If-Range` and other conditional requests, no-store requests, and a Range
  header named by `Connection` remain passthrough.

### Redirects

The proxy follows up to five redirects, only for eligible query-free SHA-256 blob and digest
manifest GETs.

- Every hop must use verified HTTPS and pass destination checks.
- Userinfo, fragments, loops, and implicit private, link-local, or loopback destinations are
  rejected. Add private storage destinations to `OCI_ACCELERATOR_REDIRECT_ALLOWLIST`.
- Hosts that could be read as a different endpoint are rejected: IPv6 addresses without
  brackets and non-ASCII hostnames. Use brackets for IPv6 and punycode (`xn--`) for
  internationalized names.
- The redirect target's host and port are sent exactly as written, so presigned URLs that
  sign the `Host` header stay valid.
- Cross-origin redirects preserve only `Accept`, `Accept-Encoding`, and `User-Agent`, never
  credentials or cookies.
- These restrictions do not apply to tunnel egress (see [limitations](#current-limitations)).

### Authorization grants

See [authorization grants](how-it-works.md#authorization-grants) for grant matching, lifetime,
and the request and response headers that prevent a grant. Additionally:

- The anonymous and credentialed TTLs are independent. A request with any `Authorization`
  header, including an empty one, uses the credentialed TTL. If `Connection` strips that
  header before forwarding, the request cannot reuse or create an anonymous approval.
- Only full immutable digest blob and manifest GETs can reuse authorization.
- Grants are keyed by a process-local HMAC of the original canonical URL and every forwarded
  end-to-end header. Transport-managed fields use their forwarded form, while other exact values and
  value order are preserved. Anonymous requests are distinct. Both classes share the
  `OCI_ACCELERATOR_CACHE_MAX_AUTHORIZATION_ENTRIES` bound and can refer to the same verified disk body.
- A grant starts from an eligible terminal `200`. It is never renewed on hits, persisted, or
  refreshed or coalesced in the background.
- Tokens are opaque, so revocation or expiry can take effect as late as the grant's original
  expiry.
- Every redirect hop must be reusable. Only explicit `max-age` and `s-maxage` ceilings shorten a
  grant below its configured TTL. `Age`, `Date`, and `Expires` are ignored. A grant never
  outlives the TTL, but may be trusted for the full residual TTL regardless of upstream age.
- A grant hit requires a present verified body. Otherwise the request goes upstream. The
  response reconstructs only content type, length, digest, a current `Date`, and
  `Cache-Control: private, no-store`. Downstream caches must not extend the grant's lifetime.

Upstream outcomes affect an existing grant as follows. The failed request is always returned to
the client: there is no failed-request fallback.

| Upstream outcome                                                          | Grant                              |
| ------------------------------------------------------------------------- | ---------------------------------- |
| `401`, `403`, `404`, `410`, or an authoritative nonreusable response      | Invalidated                        |
| Any response with `Set-Cookie`, `Authentication-Info`, `Proxy-Authentication-Info`, or `WWW-Authenticate` | Invalidated |
| Timeout, DNS or TLS failure, `408`, `429`, `5xx`                          | Kept until its original expiry     |

## Cache storage

See [cache storage and eviction](how-it-works.md#cache-storage-and-eviction) for entry layout,
filling, and eviction.

### What is cached

- Admitted: query-free SHA-256 blob GETs (including image configuration blobs) and digest
  manifests in supported OCI and Docker schema-2 media types. Eligible blob ranges can populate
  the same full-body entry.
- Cache keys preserve registry, repository, digest, object kind, exact `Accept` values, and
  observed `Content-Type`. Bodies are hashed as received.
- Bypassed: tags, HEAD, ranged manifests, conditional and non-identity-encoded responses,
  unknown-length bodies, no-store traffic, and unsupported `Vary` dimensions.

### Capacity and eviction

- Use one process per cache directory, and make sure its parent is owner-controlled. Unsafe or
  unusable configured storage fails startup.
- `OCI_ACCELERATOR_CACHE_MAX_BYTES` charges body size plus 4 KiB metadata per entry, including active fills.
  Reserve filesystem block, directory, and inode headroom beyond it.
- Oversized or capacity-rejected objects continue streaming without being cached.
- Failed cleanup retains its capacity charge until removal succeeds.
- Best-effort LRU eviction skips active readers and makes at most eight removal attempts per
  admission, separately from pending-cleanup retries. Objects needing more victims can keep
  bypassing, and competing traffic may consume reclaimed space, so admission is not guaranteed.
- Hits and publications advance a store-local logical recency sequence, so equal or backwards
  wall-clock times cannot move them behind earlier accesses. This access history is not persisted.

### Concurrent fills

- Concurrent clients authorize independently. One writer fills a key, and matching followers may
  wait for it (`OCI_ACCELERATOR_CACHE_FILL_WAIT_TIMEOUT`) and then open the verified entry.
- Wait timeout, leader failure, or eviction causes one fresh authorized GET.
- Follower cancellation does not control the leader.
- Ordinary full-GET fills are request-owned: leader cancellation aborts storage.
- An admitted ranged fill may finish after range delivery or client cancellation, bounded by
  `OCI_ACCELERATOR_CACHE_FILL_TIMEOUT` from request start.
- Entries are published only after complete length and digest verification.

### Startup recovery

- Startup removes recognized abandoned fills and discardable incomplete entries, and refuses to
  start on unknown files rather than deleting them.
- Persisted bodies are rehashed on first reuse, not at startup. Later access checks size and
  relies on private directory ownership for integrity.
- Approximate LRU order is rebuilt from commit times.
- Do not edit or delete live cache files. Use [metrics](#metrics) to inspect resource use.

### Reset the cache

Resetting permanently deletes cached content. The CA is unaffected. For a native deployment,
stop the proxy and remove the whole `OCI_ACCELERATOR_CACHE_DIR`. For Compose, remove only the cache volume:

```sh
docker compose down
docker volume rm oci-accelerator-cache
docker compose up -d
```

## Peer discovery

Peering exchanges addresses and node IDs, and each instance lists its cache keys to authenticated
peers. It does not share cached content, registry credentials, or authorization grants. Image pulls
and readiness do not depend on reachable peers.

Provision the same `ca.crt` and `ca.key` into each instance's private CA directory through your
trusted deployment system. Use separate local state directories or volumes for each instance,
not a shared writable filesystem. Ordinary Docker clients receive only `ca.crt`.

Add these settings to your existing configuration, substituting reachable addresses:

```sh
OCI_ACCELERATOR_PEER_LISTEN=0.0.0.0:7443
OCI_ACCELERATOR_PEER_ADVERTISE=accelerator-a.example:7443
OCI_ACCELERATOR_PEER_SEEDS=accelerator-b.example:7443,accelerator-c.example:7443
OCI_ACCELERATOR_PEER_STATE_DIR=./data/peers
OCI_ACCELERATOR_PEER_HINT_REFRESH_TIMEOUT=1m
OCI_ACCELERATOR_PEER_HINT_REFRESH_INTERVAL=1m
OCI_ACCELERATOR_PEER_HINT_FULL_REFRESH_INTERVAL=1h
```

- `PEER_LISTEN` binds a separate TLS port. Keep the proxy and metrics listeners separate.
- `PEER_ADVERTISE` is the address other instances dial. With containers, use a reachable host
  address and published peer port, not the wildcard bind or a container-only hostname.
  Each live node needs its own reachable `host:port`, not a shared load-balanced endpoint.
- Seeds are optional, not an allowlist. An instance with no seeds can wait for an incoming
  exchange. Ports are always explicit, including `:443`, and IPv6 uses `[address]:port`.
  Seed entries are whitespace-trimmed, canonicalized, deduplicated, and exclude self.
- Create the immediate parent of the state directory as the process user without group or
  other write access. Startup creates the state directory with mode 0700. It must not equal,
  contain, or be contained by the CA or cache directories. Relative paths use the working
  directory. For the container layout, `/var/lib/oci-accelerator/peers` is a suitable sibling.
- For Docker or Compose, explicitly publish the chosen peer port and persist peer state on a
  writable volume. The default container configuration leaves peering disabled.
- Run `oci-accelerator --env-file FILE --check` to validate without opening listeners or state.
  To disable peering, clear the listener, advertised address, seeds, and state directory settings.
  You can leave the positive hint timings configured. The existing address book remains on disk.

### Peer trust

Both sides require TLS 1.3 and dedicated, short-lived peer certificates signed by the locally
configured CA. Each instance generates a separate peer leaf key and renews its certificate
before expiry. Registry interception certificates are not accepted as peer certificates, and
the public CA download does not grant peer access. The CA private key is never sent by the
peer protocol. Certificates cannot outlive the CA.

This establishes fleet membership, not independently trusted node identities. Every holder of
the shared CA key can impersonate any peer. Compromising one instance compromises the fleet's
trust domain, so individual-node revocation is not possible while it retains that key.

Learned destinations are unrestricted. An authenticated peer can cause connection attempts to
any hostname/IP and port reachable from your instance. Mutual TLS rejects non-peers but cannot
prevent those initial connection attempts. Restrict network access as your deployment requires.
Peer connections ignore proxy environment variables, never follow redirects, and never trust a
CA downloaded from the destination. None of this changes registry authorization or proxy egress.

### Discovery and retention

The HTTPS `POST /v1/peers/exchange` request and response have the same schema:

```json
{
  "address": "node-a.internal:7443",
  "nodeId": "fzyaLmuBTQWo",
  "peers": {
    "node-b.internal:7443": "K8r2_vN5xQpL",
    "node-c.internal:7443": "mT4bY9-wH6sR"
  }
}
```

The receiver trusts the sender's own `address` and `nodeId`. It atomically replaces the
mapping at that address and removes any other address with that ID. Third-party mappings
in `peers` are added when neither their address nor their ID conflicts with a stored mapping.
Otherwise, the receiver leaves the stored mapping unchanged and queues an exchange to the
claimed address. It uses the responder's actual ID and advertised address, even if they differ
from the claim or contacted alias. A failed verification leaves the book unchanged. Late
responses that could undo intervening updates are discarded. Each reply contains the receiver's
own identity and all its known mappings. An empty book sends `"peers": {}`.

Exchanges start immediately, normally repeat every 30 seconds with ±20% jitter, and use
exponential failure backoff capped at five minutes before jitter. A successfully merged
inbound exchange defers the next outbound request to the sender's advertised address by the
same jittered interval, without shortening an existing delay or clearing failure backoff.
Third-party addresses learned from requests or responses remain eligible for immediate direct
contact unless they already have a cooldown or backoff.
Incoming requests are still answered during the cooldown, and requests already in flight can
cross. Invalid messages and failed merges do not defer requests. Cooldowns are held only in
memory and reset on restart.

Inbound exchanges do not prove that the sender's advertised listener is reachable. Regular
inbound exchanges can keep postponing outbound probes and their outage warnings. Node IDs
are self-asserted within the existing fleet-wide trust model, not independently authenticated.

Requests have a five-second deadline. Regular discovery and verification share four outbound
exchange slots, with at most two used for verification-only addresses so regular discovery
cannot be starved by conflicting gossip. Verification is asynchronous and does not delay inbound
responses. Its queue deduplicates addresses and retains up to 1,024 pending or cooling-down probes. Excess claims
are dropped with a `peer verification queue full` warning and can be submitted by later gossip.
After an attempt, a verification-only address expires after its cooldown or backoff unless
new gossip requests another attempt. Addresses already in the book keep their normal retries.
Each instance permits 32 accepted peer connections, including incomplete TLS handshakes. Incoming
headers, bodies, writes, and idle connections also have five-second deadlines. JSON messages
are limited to 512 KiB and 1,024 peer mappings. Unknown fields, duplicate JSON keys, invalid
IDs, and contradictory spellings of the same canonical address are rejected.

Each instance retains up to 1,024 canonical addresses, excluding its configured self-address.
Unreachable entries do not expire merely because contact fails. At capacity, new unrelated
addresses are rejected with a `peer address book full` warning, but identity replacements
still work. Learning a node at a new address removes its previous address. Contacted aliases
are replaced by the responder's advertised address. Losing a node's identity state gives it
a new ID and does not automatically remove an unreachable old address.

`peers.json` contains a versioned address-to-ID object with sorted keys and is atomically
replaced after changes. Empty-string IDs represent unresolved local discovery candidates.
You may read it to inspect known addresses, but do not edit live state. Reachability and retry
timing are local observations, not persisted or accepted from another instance. Startup also
merges configured seeds and retries retained addresses. Seeds are not advertised until their
IDs are learned. Removing a seed from configuration does not remove it from the book.
Resetting one book does not forget an address fleet-wide, because
other peers can advertise it again.

On first peering-enabled startup, each instance generates a 12-character, case-sensitive
base64url node ID from 72 cryptographically random bits and persists it in `node-id` in the
peer state directory. Restarts and advertised-address changes preserve that ID. Give each
instance its own persistent peer state volume. Do not clone this state into another instance,
because doing so duplicates its identity. The ID is not a secret or authentication credential.
Direct claims that conflict with this instance's own ID or address fail the exchange and log
`peer identity collision`. Fix the advertised address or duplicated state rather than expecting
automatic ID rotation. Two other nodes sharing one ID can repeatedly replace each other's records.

Upgrade all peers together. The identity-aware schema is incompatible with address-only
exchanges, with no fallback. Startup migrates version-1 `peers.json` lists to version-2 local
candidates and learns their IDs through discovery. Keep a stopped-state backup if you need
rollback, because older binaries cannot read the new state directory.

State files must be regular, process-owned, mode 0600 files. Startup takes an exclusive process
lock, removes recognized abandoned staging files, and rejects corrupt state, unsafe files, or
unknown files without replacing the book. Bind or state startup failures prevent startup.
Runtime persistence failures fail the exchange and log `peer address book update failed` while
the proxy continues serving. If replacement succeeded but directory sync failed, the book retains
the replaced state and retries persistence before acknowledging another exchange.
A peer-listener failure stops the process rather than leaving an
enabled service silently unavailable. Outage and recovery logs include the peer address but no
raw network errors or credentials.

### Cache key listing

The peer listener also serves the instance's committed cache keys at HTTPS `GET /v1/cache/keys`.
A page holds up to 4,096 keys, least recently used first and most recently used last:

```json
{
  "nodeId": "K8r2_vN5xQpL",
  "keys": [
    "0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a",
    "0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b"
  ],
  "next": ""
}
```

When more keys follow, the page is full and `next` is an opaque, versioned, URL-safe cursor
identifying its final key's logical recency position and the open cache's random incarnation.
Request the following page with `GET /v1/cache/keys?after=<next>`. An empty `next` ends the listing,
including when the final page is full. A cursor from before a cache restart is rejected with
`400`. An explicit invalid-cursor response ends the attempt and clears the client's checkpoint
so its next poll starts a fresh scan.

The listing excludes in-progress, failed, and invalidated fills and entries claimed for removal.
It grants no access, and a subsequent pull can still miss. Pages walk the cache's existing access
order without copying or sorting the whole index. An unchanged page boundary resumes directly
after its entry. If that entry moved or disappeared, the next request scans past its old logical
position. No server-side listing session is retained.

Pages are live, not a snapshot. Hits and newly committed fills move to the most recent end.
A key already listed can appear again on a later page after a hit, but keys must be unique
within each page. Later pages can include new publications or omit evicted entries. Logical
positions never move backwards within an open cache, even if the wall clock does.
The request must have no body or other query parameters. Invalid cursors fail with `400`.
An inventory with invalid or duplicate keys fails with `500`. Responses are capped at 512 KiB.

Cache keys are SHA-256 hashes of each entry's registry, repository, digest, object kind, `Accept`
values, and `Content-Type`. An authenticated peer that can guess those inputs can confirm whether
your instance has cached that object. This matches the fleet trust model in
[peer trust](#peer-trust).

Each instance fetches peers' listings to keep advisory hints about which peer holds which key:

- At startup, it fetches the listing of every peer its address book identifies, because hints
  and scan progress live only in memory. These full scans are eligible immediately.
- While running, a peer becomes eligible for an immediate full scan when discovery adds that
  node ID to the book.
  This includes a seed whose ID resolves and a new ID at a reused address, but not a known ID
  that moves to another address. An ID that leaves the book and returns is fetched again.
- Fetches run in the background, with four concurrent fetches total and at most one per peer.
  They do not block startup, readiness, or discovery on network requests. Shutdown cancels
  and joins the workers.
  A failure logs `peer cache keys refresh failed` with the peer's `node_id` and keeps
  earlier hints, successfully applied pages, and validated progress for the next poll.
- After each attempt completes, whether successful or not, the next poll waits
  `OCI_ACCELERATOR_PEER_HINT_REFRESH_INTERVAL` (default `1m`) with ±20% jitter.

Each peer retains its last validated nonterminal cursor in memory. Incremental polls resume
from that cursor, replaying the terminal tail so recent hits and publications can be found.
The terminal page does not advance the checkpoint. A one-page inventory has no checkpoint
and is fetched from the start on every poll. Partial failures retain validated progress,
but an explicit invalid-cursor response clears it and schedules a fresh scan on the next poll.

When `OCI_ACCELERATOR_PEER_HINT_FULL_REFRESH_INTERVAL` (default `1h`) has elapsed since a
successful full scan completed, with ±20% jitter, the next attempt starts from the beginning instead of doing
incremental work. An interrupted full scan resumes from its validated progress on later polls
until it completes. Its interval is measured from successful completion, not from scan start
or from a failed attempt.

A fetch checks that every page comes from the node ID the address book maps to that address
and requires full pages while more follow. Each validated page immediately adds that peer
to each listed key's providers, without clearing earlier claims. There is no key-count fetch
cutoff. `OCI_ACCELERATOR_PEER_HINT_REFRESH_TIMEOUT` (default `1m`) bounds each fetch attempt,
starting when its worker begins. The existing five-second per-request timeout also applies.
Expiry reports an incomplete refresh and retains applied pages and validated progress. Full
scans remain additive, not reconciliation. Neither a full scan nor an empty listing removes
hints for keys that have disappeared. All peers share a FIFO of 200,000 distinct keys.
Repeated claims, additional providers, and lookups never move a retained key.
Eviction removes a whole key and all of its providers, so one large peer can displace others.
Provider counts do not consume key slots, so this is not a byte-memory bound. Lookups exclude
IDs absent from your current address book, but their retained claims become visible if they
rejoin. Hints can still be stale despite periodic refresh, grant no access, and are not used
to serve pulls.

## Monitoring

### Logs

JSON logs go to stderr. On successful startup, the `proxy ready` record includes the
compiled `version`, matching `oci-accelerator --version`.

Request completion records contain:

| Field or group                        | Notes                                                                        |
| ------------------------------------- | ---------------------------------------------------------------------------- |
| Status and outcome                    | Bounded values                                                               |
| Cache, fill, wait, fallback, admission | Outcomes. An admitted detached range fill logs `fill=background`            |
| Duration                              |                                                                              |
| Downstream body bytes                 | Streaming failures can occur after the response status was sent             |
| `upstream_body_bytes`                 | Bytes consumed from the upstream body, excluding transport read-ahead and a detached fill pump's bytes. Not a wire measurement |
| `url`                                 | See below                                                                    |

The `url` field appears on completion records and upstream-failure warnings, including cache
hits and rejected requests. It contains the intercepted registry's HTTPS authority and the
original escaped request path:

```
"url":"https://registry.example/v2/team/image/manifests/latest"
```

- Non-origin-form targets and targets containing a literal `#` log only the authority.
- Paths expose repository names, tags, and digests, so restrict access to logs accordingly.
- Never logged: query strings, userinfo, fragments, redirect URLs, authorization headers, token
  bodies, and raw upstream errors.
- No request-start or per-redirect records are emitted. Opaque CONNECT tunnels do not expose
  HTTP request URLs to the proxy.

### Metrics

Set `OCI_ACCELERATOR_METRICS_LISTEN=127.0.0.1:9090` to enable a separate listener. There is no metrics route
on the CONNECT listener, admin API, or pprof endpoint.

| Endpoint       | Purpose                                                  |
| -------------- | -------------------------------------------------------- |
| `GET /metrics` | Prometheus text                                          |
| `GET /livez`   | Process responsiveness                                   |
| `GET /readyz`  | Initialized and serving, not shutting down               |

- To bind a non-loopback address, you must set `OCI_ACCELERATOR_METRICS_ALLOW_REMOTE=true`. Remote scraping has no
  authentication: restrict it with private network controls and never expose it publicly or
  through a portal.
- A bind failure prevents startup.
- Probes never contact upstream registries or depend on cache capacity, and are independent of
  `OCI_ACCELERATOR_MAX_CONNECTIONS`. In Kubernetes, you can use local exec probes with a loopback bind when
  remote scraping is disabled.

Metrics use the `oci_accelerator_` prefix and include:

- request counts, bounded status classes, duration histograms, and active requests and
  connections
- cache, fill, wait, fallback, and upstream GET and body counters
- cache admission and rejection reasons, entries, readers, fills, bytes, reservations, and
  evictions (the fills gauge includes reservations whose staging is still in progress)
- Go heap allocation and goroutine gauges

Interpreting them:

- Labels are fixed enums and never contain clients, repositories, paths, digests, tokens, or URLs.
- Counters reset on restart. Request-owned body counters are recorded when requests complete.
- For a detached range fill, `oci_accelerator_upstream_body_bytes_total` and
  `oci_accelerator_fills_total` update when its pump completes, while store fill statistics remain
  available independently.
- Logical cache bytes exclude filesystem overhead. Go heap allocation is not RSS.
- `oci_accelerator_upstream_get_attempts_total` counts connection-acquisition attempts,
  including transport retries and DNS, dial, or TLS failures before a GET is sent. It is not an
  exact count of requests received by the upstream registry.
- `oci_accelerator_cache_rejections_total` reports `vary` and `digest_mismatch` header
  rejections on eligible responses. Other policy checks may also prevent caching, so it does
  not identify the sole cause of every bypass.

### Cache delivery ratios

`oci_accelerator_served_bytes_total{source="cache|origin"}` and
`oci_accelerator_served_entries_total{source="cache|origin"}` measure how much digest-addressed
content clients received from the cache versus the upstream registry.

- **Scope.** Only `200` responses to query-free `GET`s for a full, unescaped SHA-256 blob or
  manifest digest, without `Range` or `If-*` headers. Errors, `HEAD`, tags, ranges, conditional
  requests, queries, and opaque tunnels are excluded.
- **Bytes** count what the client actually accepted, including partial delivery when a transfer
  aborts. **Entries** increment only when delivery completes. They count deliveries, not unique
  digests, successful cache fills, or integrity checks.
- **Source** is where the response body came from. `cache` covers ordinary hits,
  authorization-grant hits, and responses served after waiting for another request's fill.
  `origin` covers cold fills and responses that bypassed storage because of `no-store`,
  capacity, or unsupported metadata. Upstream authorization GETs do not affect attribution.
- Both sources are exported as zero before traffic, update when each request ends, and reset on
  restart. They measure application deliveries, not wire bandwidth or bandwidth savings.

Five-minute cache byte percentage across all instances. Apply `rate` before aggregation, and
drop a zero-traffic denominator so dashboards display N/A rather than coercing no traffic to
zero:

```promql
(
  100 * sum(rate(oci_accelerator_served_bytes_total{source="cache"}[5m]))
    / sum(rate(oci_accelerator_served_bytes_total[5m]))
)
and on () (sum(rate(oci_accelerator_served_bytes_total[5m])) > 0)
```

Delivery percentage:

```promql
(
  100 * sum(rate(oci_accelerator_served_entries_total{source="cache"}[5m]))
    / sum(rate(oci_accelerator_served_entries_total[5m]))
)
and on () (sum(rate(oci_accelerator_served_entries_total[5m])) > 0)
```

For origin percentage, replace `source="cache"` with `source="origin"`, or subtract the cache
percentage from 100 when traffic is present.

## Lifecycle

### Shutdown

On SIGINT or SIGTERM the proxy:

1. stops admitting connections and disables intercepted keepalive
2. drains until `OCI_ACCELERATOR_SHUTDOWN_TIMEOUT`, then closes remaining connections, including opaque tunnels
3. cancels and joins ranged transfers, and waits for handlers and relay pumps to release their
   resources before the cache is closed, even beyond the drain deadline

A stuck handler can therefore delay exit. A second signal uses the operating system's default
termination behavior. For containers, keep the stop grace period longer than
`OCI_ACCELERATOR_SHUTDOWN_TIMEOUT` (see [Compose](#compose)).

When peering is enabled, outbound exchanges are cancelled on shutdown. Peer cleanup drains
inbound exchanges with its own `OCI_ACCELERATOR_SHUTDOWN_TIMEOUT` window and joins remaining
handlers before releasing the state lock. Allow time for both proxy and peer drain windows,
plus metrics cleanup, in your container stop grace period. Filesystem stalls can delay exit.

### What survives a restart

| State                    | After restart                                                       |
| ------------------------ | ------------------------------------------------------------------- |
| Configuration            | Re-read. Changes take effect only now                               |
| Root CA                  | Re-checked and reused from `OCI_ACCELERATOR_CA_DIR`                 |
| Leaf certificates        | Memory only, re-signed on demand                                    |
| Authorization grants     | Cleared. The next delivery needs fresh upstream approval            |
| Cache entries            | Kept. Each body is rehashed on first reuse                          |
| Abandoned fills          | Removed at startup                                                  |
| Metrics counters         | Reset                                                               |
| Known peers              | Retained in `OCI_ACCELERATOR_PEER_STATE_DIR` when peering is enabled |
| Peer cache key hints     | Memory only, fetched again from every known peer at startup         |
| Peer reachability and leaf keys | Re-established, not persisted                                |

## Troubleshooting

| Symptom                                                      | Likely cause                                                                                   | Fix                                                                                   |
| ------------------------------------------------------------ | ---------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------- |
| `--check` passes but startup fails                           | `--check` does not inspect CA or cache state or bind listeners                                 | Read the startup error. See the rows below                                            |
| Startup fails naming a CA check                              | CA directory or files are empty, incomplete, expired, mismatched, oversized, symlinked, foreign-owned, or wrongly permissioned | Correct the state per [CA directory requirements](#ca-directory-requirements). Never delete an established CA ([replace it](#back-up-and-replace-the-ca) instead) |
| Startup fails on a missing directory                         | Neither `init-ca` nor startup creates ancestors                                                | Create the parent as the proxy user ([state directories](#state-directories))         |
| Startup refuses unknown cache files                          | Unrecognized files in `OCI_ACCELERATOR_CACHE_DIR`. Startup never deletes them                  | Investigate their origin, or [reset the cache](#reset-the-cache)                      |
| Second proxy fails to start on the same cache                | One process per cache directory                                                                | Give each process its own `OCI_ACCELERATOR_CACHE_DIR` and `OCI_ACCELERATOR_CA_DIR`     |
| `--check` rejects `OCI_ACCELERATOR_METRICS_LISTEN`           | Non-loopback address without `OCI_ACCELERATOR_METRICS_ALLOW_REMOTE=true`                        | Use a loopback address, or opt in ([metrics](#metrics))                               |
| Startup fails binding metrics                                | The metrics listener cannot bind. A bind failure prevents startup                              | Fix `OCI_ACCELERATOR_METRICS_LISTEN` or free the address                              |
| Container health is `unhealthy`                              | Metrics listener not at `127.0.0.1:9090`, or the proxy is not ready or is shutting down        | Restore the listener or override the health check ([image](#image)). It does not restart the container |
| Docker reports an unknown certificate authority              | CA not installed under the exact `certs.d/<host[:port]>` directory                             | [Trust the CA](#trust-the-ca) for that authority                                      |
| Private registry fails through the proxy with a TLS error    | The proxy process does not trust the upstream registry's real CA                               | Add it to the proxy's system trust store. Never disable verification                  |
| Pulls work but no request records show the upstream registry | Registry not in `OCI_ACCELERATOR_REGISTRIES` (port must match), listed in `no-proxy`/`NO_PROXY`, or daemon not restarted | Fix `OCI_ACCELERATOR_REGISTRIES` or [Docker's proxy configuration](#point-docker-at-the-proxy) |
| Revoked access or a deleted image still pulls briefly        | An existing [authorization grant](#authorization-grants)                                       | Wait for the applicable authorization TTL, or set both TTLs to `0s`                    |
| `.ca-init-*` directories in the CA parent                    | A crash or SIGKILL during initialization                                                       | [Clean up staging directories](#clean-up-staging-directories)                         |
| Compose warns about existing volumes                         | Volumes were created by `docker run`                                                           | Expected. Run only one proxy against them                                             |

## Current limitations

- Redirect restrictions protect proxy-originated fetches only. They do not constrain the broader
  egress available through opaque CONNECT tunnels, so deployment network controls remain essential.
- No shared or distributed cache storage, peer-to-peer content delivery, or background prefetch.
  Periodically refreshed peer cache key hints are advisory and are not used to serve pulls.
- No Kubernetes manifests, and no automated container runtime acceptance in CI.
- Fault tests exercise process termination and private tmpfs exhaustion, not power loss or
  faulty-device certification. See the [testing guide](testing.md).

## Appendix: Amp orbs

### Prepare an orb

Amp runs `.agents/setup` when preparing a fresh orb. It installs checksum-verified Go 1.27.1, C
build tools for race-enabled tests, and Docker Engine, Buildx, and Compose if missing, and
downloads Go module dependencies. You can safely re-run it:

```sh
.agents/setup
go version
```

Start Docker as a supervised service, not from setup or a background shell:

```sh
amp orb services ensure
sudo docker info
```

- Use `sudo docker` for daemon operations. If the daemon is not ready, inspect
  `amp orb service logs docker-daemon`.
- The daemon uses only a local Unix socket. Do not expose it through a portal.
- Amp supervises the daemon across CLI updates and orb pause/resume, so you don't need a custom
  resume hook.
- Registry credentials belong to the Docker client, never the setup script or a shared orb
  snapshot. Setup does not install a proxy CA into Docker's trust store.

### Run the proxy as an orb service

```sh
amp orb service start registry-proxy \
  --command './bin/oci-accelerator --env-file .env'
amp orb service logs registry-proxy
amp orb service stop registry-proxy
```

Do not expose the proxy through an Amp portal. To run acceptance tests in an orb, see the
[testing guide](testing.md).
