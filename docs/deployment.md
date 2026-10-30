# Deploying and using OCI Accelerator

OCI Accelerator is an HTTP CONNECT proxy. It intercepts configured HTTPS
registries and caches verified SHA-256 blobs and digest manifests on disk, plus
mutable metadata briefly in memory. All other destinations are tunneled
unmodified. See [how it works](how-it-works.md) for the request flow and design.

| Client or platform | Status |
| --- | --- |
| Linux Docker Engine | Validated |
| Native macOS on Apple Silicon | CLI, cache, and proxy tests passed |
| macOS amd64 under Rosetta on Apple Silicon | CLI, cache, and proxy tests passed |
| Native Intel macOS, Linux arm64 binaries | Cross-built only, native runtime not validated |
| Docker Desktop, Windows, containerd, BuildKit-specific integration | Not validated |

## Security model

### No client authentication

Anyone who can reach the listener can open opaque CONNECT tunnels to arbitrary
hosts and ports, which cannot be inspected or restricted to image pulls.

Keep the default loopback bind, or bind a private interface behind network
access controls and a firewall. Never expose the proxy through an Amp portal.

### CA key and certificate

The CA can sign any server identity. Protect `ca.key`, never give it to Docker
clients or bake it into an image, and scope client trust to each intercepted
registry rather than system-wide. [Peering](#peer-discovery) requires securely
provisioning the same CA key to each trusted accelerator instance. Anyone
holding it can impersonate registries and peers
([details](how-it-works.md#trusting-the-accelerators-certificate)).

The CA download is unauthenticated, so verify its fingerprint through a trusted
channel before installing it ([details](#trust-the-ca)).

### Upstream TLS and credentials

Upstream TLS is always verified using the proxy process's system trust store.
There is no insecure mode.

Credentials stay with Docker. Never put credentials in proxy configuration.
Cross-origin redirects never forward credentials or cookies.

### Authorization of cached content

The upstream registry authorizes every cached delivery, either with an upstream
GET or with a recent approval, called a grant, for the same request or for the
same Bearer token and repository. A failed upstream request never falls back
to cached content.

By default, grants last up to 15 minutes for requests without an `Authorization`
header and 30 seconds for requests with one, so revoked access can still be
served from cache until they expire. Set both
[authorization TTLs](#authorization-grants) to `0s` to check every delivery.

A cache hit without a live grant still sends the request upstream for
approval, which may count toward registry rate limits
([details](how-it-works.md#approval-before-every-delivery)).

### Stale mutable metadata

Tag manifests, tag lists, referrer lists, and catalog responses are cached
briefly in memory, even after a push, tag move, or access revocation. Set
`OCI_ACCELERATOR_CACHE_MUTABLE_TTL=0s` to disable this cache
([details](#mutable-response-cache)).

Bearer tokens approved for a repository share its cached metadata. Set
`OCI_ACCELERATOR_CACHE_MUTABLE_SHARED=false` if your registry filters metadata
per token.

### Logs, metrics, and probes

Logs contain repository names, tags, and digests, so restrict access to them.
Metrics and probes are unauthenticated and bind loopback only unless explicitly
opted in.

## Deploy

### Prerequisites

| Deployment | Requires |
| --- | --- |
| Native | A release binary or Go 1.27.1 to build, and a service manager to supervise the process |
| Container | Linux Docker daemon with Buildx, and Compose for the supplied service |

For a native starter layout, `oci-accelerator init` creates the private state
directories and CA before first start. For a custom layout, create the parent
directories yourself (see [state directories](#state-directories)).

### Run the binary

#### Download a release

[GitHub Releases](https://github.com/buildkite/oci-accelerator/releases)
provides four unsigned archives per version: `linux/amd64`, `linux/arm64`,
`darwin/amd64`, and `darwin/arm64`. Each `oci-accelerator-OS-ARCH-vX.Y.Z.tar.gz`
contains only the executable `oci-accelerator`. The checksum manifest is
`oci-accelerator-vX.Y.Z.SHA256SUMS`.

Download and verify the archive for your platform. These examples use
`v0.3.3`. Replace it with the published version you want.

Linux amd64:

```sh
base=https://github.com/buildkite/oci-accelerator/releases/download/v0.3.3
archive=oci-accelerator-linux-amd64-v0.3.3.tar.gz

curl -fL --remote-name-all "$base/$archive" "$base/oci-accelerator-v0.3.3.SHA256SUMS"

grep " $archive\$" oci-accelerator-v0.3.3.SHA256SUMS | sha256sum --check -
```

Linux arm64:

```sh
base=https://github.com/buildkite/oci-accelerator/releases/download/v0.3.3
archive=oci-accelerator-linux-arm64-v0.3.3.tar.gz

curl -fL --remote-name-all "$base/$archive" "$base/oci-accelerator-v0.3.3.SHA256SUMS"

grep " $archive\$" oci-accelerator-v0.3.3.SHA256SUMS | sha256sum --check -
```

macOS on Apple Silicon:

```sh
base=https://github.com/buildkite/oci-accelerator/releases/download/v0.3.3
archive=oci-accelerator-darwin-arm64-v0.3.3.tar.gz

curl -fL --remote-name-all "$base/$archive" "$base/oci-accelerator-v0.3.3.SHA256SUMS"

grep " $archive\$" oci-accelerator-v0.3.3.SHA256SUMS | shasum -a 256 --check -
```

macOS on Intel:

```sh
base=https://github.com/buildkite/oci-accelerator/releases/download/v0.3.3
archive=oci-accelerator-darwin-amd64-v0.3.3.tar.gz

curl -fL --remote-name-all "$base/$archive" "$base/oci-accelerator-v0.3.3.SHA256SUMS"

grep " $archive\$" oci-accelerator-v0.3.3.SHA256SUMS | shasum -a 256 --check -
```

Continue only if verification succeeds. Extract into a new directory and check
the version:

```sh
mkdir bin
tar -xzf "$archive" -C bin

bin/oci-accelerator --version
```

Checksums detect corruption but don't prove who published the files. The
binaries are unsigned, and macOS binaries are not notarized, so Gatekeeper may
block them. Follow your organization's trust policy before allowing them to
run.

#### Build from source

Alternatively, from the repository root:

```sh
go build -o bin/oci-accelerator ./cmd/oci-accelerator
```

#### Configure and start

Initialize a native starter layout with either binary. You do not need a copy of
the repository's example file when using a downloaded binary:

```sh
bin/oci-accelerator init

# Edit .env registries if needed, then validate:
bin/oci-accelerator --env-file .env --check

bin/oci-accelerator --env-file .env
```

`init` creates `.env`, the private directories `data/`, `data/cache/`, and
`data/peers/` with mode 0700, and a root CA in `data/ca/`. It prints the
configuration path, the CA certificate path and SHA-256 fingerprint, and the
startup command. It does not start the proxy or change Docker.

The generated `.env` is the complete `.env.example` with native starter
registries and `./data` paths. Always pass `--env-file .env` when starting the
proxy. See [configuration](#configuration) for how values are loaded and what
`--check` validates.

Then [configure Docker hosts](#configure-docker-hosts).

#### Initialization options

`init` takes no options except `--help`, and it ignores environment variables.

It never modifies existing state. If `.env` exists, it stops before creating
anything. It reuses existing `data/`, `data/cache/`, and `data/peers/`
directories only if they are non-symlink directories that you own with mode
0700. An existing CA is reused unchanged if it meets the
[CA directory requirements](#ca-directory-requirements). Otherwise it fails.

If initialization fails, directories or a generated CA can remain. The
diagnostic asks you to inspect them and retry. `.env` is created only after the
directory layout and CA succeed. Do not delete an established CA to retry
initialization.

For configuration only, generate the same complete starter file without creating
state directories or a CA:

```sh
bin/oci-accelerator init-env

# Or select a different output path whose parent already exists:
bin/oci-accelerator init-env --output proxy.env
```

`init-env` defaults to `.env`, refuses to overwrite an existing output, and does
not create its output's parent directory. Like `init`, it ignores ambient
environment variables.

For a custom layout, use `init-env`, edit the generated registry and path
settings, and create the immediate state parents as the proxy user. You can
provision only the CA with `init-ca --dir DIR` before startup. For example,
after setting `OCI_ACCELERATOR_CA_DIR=./state/ca` and
`OCI_ACCELERATOR_CACHE_DIR=./state/cache` in `proxy.env`:

```sh
mkdir -m 0700 state

# Optional: provision the CA before first start.
bin/oci-accelerator init-ca --dir state/ca

bin/oci-accelerator --env-file proxy.env --check

bin/oci-accelerator --env-file proxy.env
```

Use `init --help`, `init-env --help`, or `init-ca --help` for command-specific
usage.

### Run under a service manager

Run the process under the host's service manager. It stops gracefully on SIGINT
or SIGTERM (see [shutdown](#shutdown)). For Amp orbs, see the
[testing guide](testing.md#run-the-proxy-as-a-service).

### Run in a container

#### Image

The root `Dockerfile` builds a static Go binary into an Alpine-based image.

| Property | Value |
| --- | --- |
| Local tag | `oci-accelerator:local` |
| User and group | `10001:10001` |
| Entrypoint | `/usr/local/bin/oci-accelerator` |
| Default arguments | None |
| Health check | `wget` of `http://127.0.0.1:9090/readyz` |
| State directories | `/var/lib/oci-accelerator` and its `cache/`, UID/GID 10001, mode 0700 (`ca/` is left absent) |

The image sets four environment defaults that match its own layout:

| Variable | Image default | Why |
| --- | --- | --- |
| `OCI_ACCELERATOR_LISTEN` | `0.0.0.0:5128` | A published port must reach the proxy |
| `OCI_ACCELERATOR_METRICS_LISTEN` | `127.0.0.1:9090` | The health check probes it |
| `OCI_ACCELERATOR_CA_DIR` | `/var/lib/oci-accelerator/ca` | The image creates and owns its parent |
| `OCI_ACCELERATOR_CACHE_DIR` | `/var/lib/oci-accelerator/cache` | The image creates and owns this directory |

You must still set `OCI_ACCELERATOR_REGISTRIES`, so the image never chooses
which hosts to intercept for you.

**The image listens on all container interfaces.** Any container on the same
Docker network can connect to the proxy, so keep untrusted workloads off that
network.

Publish the port only on a trusted host address, as the examples below do.
`-p 5128:5128` exposes it on every host interface. With `--network host`, set
`OCI_ACCELERATOR_LISTEN` to a loopback or private address.

How image defaults interact with other settings:

- Docker `-e`, Docker `--env-file`, and Compose `environment` or `env_file`
  values override image defaults.
- Image defaults are process environment variables. They therefore override an
  application `--env-file` passed after the image name. Configure containers
  through Docker or Compose instead.
- You can't unset an image default, only replace it, and an empty value counts
  as set ([empty values](#configuration)).

The health check needs the metrics listener at `127.0.0.1:9090`. Keep it there
or override the health check when changing it. An unhealthy result is diagnostic
and does not itself trigger a restart. Readiness does not check upstream
connectivity or disk capacity.

#### Container environment file

`.env.container-example` holds the container settings:
`OCI_ACCELERATOR_REGISTRIES` and a copy of the four image defaults. Copy it to
`.env.container`, which Git ignores, and edit the copy. Values in the copy
override the image defaults. See the [settings reference](settings.md) for every
other setting and its default.

Docker and Compose read this file with their own dotenv syntax, not the
application's. Keep it to unquoted literal `KEY=value` lines and full-line
comments.

#### Compose

The supplied `compose.yaml` service builds the local image from the repository
root and:

- requires `env_file: .env.container`
- publishes only `127.0.0.1:5128`
- keeps the root filesystem read-only, drops all capabilities, and sets
  `no-new-privileges`
- uses `restart: unless-stopped` and a 40-second stop grace period
  ([shutdown](#shutdown))

| Source | Mount point | Access |
| --- | --- | --- |
| `oci-accelerator-ca` volume | `/var/lib/oci-accelerator` | Writable |
| `oci-accelerator-cache` volume | `/var/lib/oci-accelerator/cache` | Writable |

Volume notes:

- The CA volume mounts the parent so startup can atomically create its `ca/`
  child. Docker copies the image's ownership and permissions into fresh volumes,
  so you don't need to provision host directories or use a root entrypoint. The
  CA volume remains writable at runtime.
- Existing valid CA files are reused, not replaced.
- Run only one proxy against these volumes at a time. The names match the
  [`docker run`](#docker-run) example.
- These examples do not migrate existing host bind-mounted data. If you start
  with new volumes, the proxy creates a new CA, and you must redistribute client
  trust.

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

Compose does not automatically pass exported host variables into the container.
To override settings, edit `.env.container`, or add an `environment` mapping to
the service in a local Compose override file. Compose `environment` values
override `env_file` values:

```yaml
services:
  oci-accelerator:
    environment:
      OCI_ACCELERATOR_CACHE_MAX_BYTES: "68719476736"
      OCI_ACCELERATOR_REGISTRIES: "registry-1.docker.io, ghcr.io"
```

You can put literal values in this mapping, or use Compose interpolation such as
`OCI_ACCELERATOR_CACHE_MAX_BYTES: "${OCI_ACCELERATOR_CACHE_MAX_BYTES}"`. The
latter requires you to set the host variable before running Compose.

First startup generates the CA. Then
[configure Docker hosts](#configure-docker-hosts), installing only the public
certificate.

| Command | Effect on volumes |
| --- | --- |
| `docker compose down` | Preserves both |
| `docker volume rm oci-accelerator-cache` | Deletes only the cache ([reset the cache](#reset-the-cache)) |
| **`docker compose down -v`** | **Deletes both, including the signing CA** |

Preserve the CA volume independently of the replaceable cache.

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
  --stop-timeout 40 -p 127.0.0.1:5128:5128 \
  --mount type=volume,src=oci-accelerator-ca,dst=/var/lib/oci-accelerator \
  --mount type=volume,src=oci-accelerator-cache,dst=/var/lib/oci-accelerator/cache \
  oci-accelerator:local

docker logs oci-accelerator
docker inspect --format '{{.State.Health.Status}}' oci-accelerator
```

Stop it with `docker stop oci-accelerator`. Removing the container preserves
both named volumes.

Docker's `--env-file` appears before the image name, so Docker injects the
values into the container. Add `-e` to override individual values. Docker
applies `-e` after `--env-file`. Because the image supplies the other container
defaults, you can instead pass only the registries:

```sh
docker run -d --name oci-accelerator --restart unless-stopped \
  --read-only --cap-drop ALL --security-opt no-new-privileges \
  --stop-timeout 40 -p 127.0.0.1:5128:5128 \
  --mount type=volume,src=oci-accelerator-ca,dst=/var/lib/oci-accelerator \
  --mount type=volume,src=oci-accelerator-cache,dst=/var/lib/oci-accelerator/cache \
  -e OCI_ACCELERATOR_REGISTRIES=registry-1.docker.io,ghcr.io,public.ecr.aws \
  oci-accelerator:local
```

Docker exposes the proxy only on the host's loopback address. Metrics stay on
container loopback for the image health check and are not published. The UID
10001 state volumes and isolation flags match the Compose example. Exported host
variables are not implicit container overrides. Use `-e`, `--env-file`, or a
Compose `environment` mapping.

#### Multi-architecture images

Buildx can load one platform at a time:

```sh
docker buildx build --platform linux/amd64 --load -t oci-accelerator:amd64 .
docker buildx build --platform linux/arm64 --load -t oci-accelerator:arm64 .
```

- Building or running the arm64 image requires a native arm64 host or
  configured emulation.
- For a multi-platform OCI archive, use a Buildx builder with the
  `docker-container` driver and an OCI output instead of `--load`.
- Prebuilt images are published as `buildkite/oci-accelerator:latest`,
  `:build-N` for each approved `main` build, and a version tag for each
  release. See [releasing](../RELEASING.md).

### Serve remote Docker hosts

The default `127.0.0.1:5128` listener serves Docker on the same host. To serve
remote hosts:

1. Set `OCI_ACCELERATOR_LISTEN` to an explicit private-interface address.
2. Restrict access with network access controls and a firewall (see
   [security model](#security-model)).
3. Use the proxy's private address in each host's
   [Docker proxy configuration](#point-docker-at-the-proxy).

## Configure Docker hosts

These steps are for rootful Linux Docker Engine. Image names, `docker login`,
and Docker-owned credentials don't change.

### Trust the CA

1. Download the public certificate from the proxy listener and print its
   fingerprint. `PROXY_URL` is the same `http://` address you use in Docker's
   proxy configuration:

   ```sh
   curl --noproxy '*' --fail --show-error --output ca.crt "$PROXY_URL/ca.crt"

   openssl x509 -in ca.crt -noout -fingerprint -sha256
   ```

2. Compare the fingerprint with `ca_sha256` from `init`, `init-ca`, or the
   startup logs, obtained through a trusted channel such as SSH or your
   deployment system. OpenSSL prints uppercase hex with colons, and the proxy
   prints lowercase hex without them.

   **Don't install the certificate if the fingerprints differ, or if you can't
   get the expected fingerprint through a trusted channel.** The download is
   unauthenticated. Alternatively, copy only the public `ca.crt` through a
   trusted channel.

3. Install the certificate for each intercepted registry, rather than adding
   system-wide trust:

   ```sh
   for registry in registry-1.docker.io ghcr.io public.ecr.aws; do
     sudo install -D -m 0644 ca.crt "/etc/docker/certs.d/$registry/ca.crt"
   done
   ```

   Name each directory after the exact authority in image names: use
   `registry.example:5000` for a registry on port 5000, and omit `:443` when
   image names omit it. Install only `ca.crt`, never `ca.key`. See Docker's
   [certificate instructions](https://docs.docker.com/engine/security/certificates/)
   for other installation modes and platforms.

### Point Docker at the proxy

Merge this into `/etc/docker/daemon.json` without replacing unrelated settings:

```json
{
  "proxies": {
    "https-proxy": "http://127.0.0.1:5128"
  }
}
```

After arranging for workload disruption, restart Docker and pull as usual:

```sh
sudo systemctl restart docker

docker login ghcr.io
docker pull ghcr.io/your-organization/your-image:tag
```

- Use an `http://` proxy URL. Docker sends CONNECT, then negotiates registry
  TLS inside the tunnel. For [remote hosts](#serve-remote-docker-hosts), use
  the proxy's private address instead of loopback.
- Don't route HTTP registries through this listener.
- Don't list intercepted registries in `no-proxy`, or in a `NO_PROXY` inherited
  from the daemon's service environment. An empty `no-proxy` value in
  `daemon.json` might not clear an inherited one. Shell proxy variables don't
  affect a running daemon. See Docker's
  [daemon proxy configuration](https://docs.docker.com/engine/daemon/proxy/).
- For a private registry, the proxy's system trust store must also trust the
  registry's real CA. Never disable TLS verification or mark the registry
  insecure as a workaround.

### Push and delete

Docker push uses the same proxy and CA configuration as pull.

- The upstream registry enforces write permissions using Docker's credentials.
- Uploads don't warm the cache. The proxy doesn't manage upload sessions or
  follow write redirects.
- Uploads are limited only by `OCI_ACCELERATOR_IDLE_TIMEOUT`, with no total
  time or size limit.
- Writes don't invalidate [authorization grants](#authorization-grants) or
  [mutable responses](#mutable-response-cache), so a moved tag or deleted image
  can still be served until they expire. See
  [authorization of cached content](#authorization-of-cached-content) and
  [stale mutable metadata](#stale-mutable-metadata) to turn off reuse.

## Monitoring

### Logs

JSON logs go to stderr. On successful startup, the `proxy ready` record
includes the compiled `version`, matching `oci-accelerator --version`.

Each completed intercepted request logs a `registry request` record with these
fields:

| Field | Notes |
| --- | --- |
| `registry`, `url` | The intercepted registry and request URL. See below |
| `status`, `outcome` | Response status and a bounded outcome value |
| `duration` | Time to complete the request |
| `bytes` | Body bytes sent to the client. A stream can fail after the status was sent |
| `cache`, `fill`, `wait`, `fallback`, `admission`, `reject` | Cache outcomes. A detached range fill logs `fill=background`. `admission=mutable_capacity` means the mutable cache's capture, entry, or memory limit refused a capture. `reject` gives a cache rejection reason |
| `upstream_body_bytes` | Bytes read from the upstream body, excluding transport read-ahead and detached fills. Not a wire measurement |
| `peer`, `peer_body_bytes` | Peer source selection (`hit` or `unavailable`) and bytes read from the peer body |

The `url` field also appears on `upstream request failed` warnings. It holds
the registry's HTTPS authority and the original escaped path, which expose
repository names, tags, and digests, so
[restrict access to logs](#logs-metrics-and-probes):

```
"url":"https://registry.example/v2/team/image/manifests/latest"
```

- Query strings, userinfo, fragments, redirect URLs, credentials, token bodies,
  and raw upstream errors are never logged. Targets that aren't plain paths, or
  that contain `#`, log only the authority.
- There are no request-start or per-redirect records. Tunnels don't expose
  request URLs to the proxy, so they aren't logged.

### Metrics

Set `OCI_ACCELERATOR_METRICS_LISTEN=127.0.0.1:9090` to enable a separate
listener. There's no metrics route on the proxy listener, no admin API, and no
pprof endpoint.

| Endpoint | Purpose |
| --- | --- |
| `GET /metrics` | Prometheus text |
| `GET /livez` | Process responsiveness |
| `GET /readyz` | Initialized and serving, not shutting down. The container [image](#image) health check uses it |

- Binding a non-loopback address requires
  `OCI_ACCELERATOR_METRICS_ALLOW_REMOTE=true`, and the endpoints are
  unauthenticated ([security model](#logs-metrics-and-probes)). A bind failure
  prevents startup.
- Probes never contact registries or depend on cache capacity, and are
  independent of `OCI_ACCELERATOR_MAX_CONNECTIONS`. In Kubernetes, you can use
  local exec probes with a loopback bind.

See the [metrics reference](metrics.md) for every metric, its labels, notes on
interpreting it, and example cache hit ratio queries.

## Lifecycle

### Shutdown

On SIGINT or SIGTERM, the proxy:

1. Stops admitting connections, disables keepalive on intercepted connections,
   and cancels ranged transfers.
2. Drains until `OCI_ACCELERATOR_SHUTDOWN_TIMEOUT`, then closes remaining
   connections, including tunnels.
3. Waits for ranged transfers, handlers, and relays to release their resources
   before closing the cache, even beyond the drain deadline.

A stuck handler or a stalled filesystem can therefore delay exit. A second
signal uses the operating system's default termination behavior.

With peering enabled, the peer listener then drains for its own
`OCI_ACCELERATOR_SHUTDOWN_TIMEOUT` window. Keep your container's stop grace
period longer than the shutdown timeout plus metrics cleanup, or longer than
twice the timeout with peering. The 40-second period in [Compose](#compose)
covers the 15-second default either way.

### What survives a restart

| State | After restart |
| --- | --- |
| Configuration | Re-read. Changes take effect only at restart |
| Root CA | Re-checked and reused from `OCI_ACCELERATOR_CA_DIR` |
| Interception and peer certificates | Memory only, issued again as needed |
| Authorization grants | Cleared. The next delivery needs fresh upstream approval |
| Mutable responses | Cleared |
| Immutable disk entries | Kept. Each body is rehashed on first use, so the first hit is slower |
| Abandoned fills | Removed at startup |
| Metrics counters | Reset |
| Known peers and node ID | Kept in `OCI_ACCELERATOR_PEER_STATE_DIR` when peering is enabled |
| Peer key hints | Memory only, fetched again from every known peer at startup |
| Peer reachability | Re-checked, not persisted |

## CA management

The CA is `ca.crt` and `ca.key` in `OCI_ACCELERATOR_CA_DIR`: `data/ca/` on the
host in the native example, or `ca/` in the `oci-accelerator-ca` volume for
containers. See
[trusting the accelerator's certificate](how-it-works.md#trusting-the-accelerators-certificate)
and [per-host certificates](how-it-works.md#per-host-certificates) for how the
CA and leaf certificates are used.

### Create the CA

The CA is created on first startup, or ahead of time with `init` or `init-ca`.
Each creates it only when the CA directory is absent, and reuses a valid
existing CA unchanged. Leave the directory absent initially and don't
pre-create it empty.

```sh
bin/oci-accelerator init-ca --dir data/ca
```

- Pass `--dir` with the same path as `OCI_ACCELERATOR_CA_DIR`, so startup
  reuses the CA. A relative path is resolved from the working directory.
- It prints the certificate path (`ca_certificate`) and SHA-256 fingerprint
  (`ca_sha256`), never the private key. Startup logs the same fingerprint. Use
  it to [verify the CA](#trust-the-ca) on clients.
- It creates only the CA. It doesn't read `OCI_ACCELERATOR_*` variables or an
  env file, start the proxy, use the network, install client trust, or rotate
  certificates.

Concurrent initializers are safe: one wins and the others reuse its CA.

### CA directory requirements

Proxy startup, `init`, and `init-ca` apply the same checks.

| Path | Type | Mode |
| --- | --- | --- |
| Immediate parent | Non-symlink directory | No group or other write access |
| CA directory | Non-symlink directory | Exactly 0700 |
| `ca.crt` | Regular file, PEM, up to 64 KiB | 0644 or stricter |
| `ca.key` | Regular file, PEM, up to 64 KiB | 0600 or stricter |

- All four must be owned by the process's effective user: the native process
  user, or UID 10001 in the container. Another user's files are rejected, even
  when running as root. Ancestors above the parent are not checked, so keep
  the containing path trusted.
- The certificate must match the profile the proxy generates: a self-signed
  X.509 v3 CA with an ECDSA P-256 key, an ECDSA-SHA256 signature, and only key
  usage, basic constraints, and subject key identifier extensions. A match
  shows compatibility, not who created the CA.
- Every startup re-checks the files, profile, key pairing, and validity period.
  Any failure stops startup without changing a file. Startup errors name the
  failing check but never include certificate or key contents.

### Back up and replace the CA

Preserve `OCI_ACCELERATOR_CA_DIR` across restarts, back up both CA files
securely, and use one proxy process per CA directory. Rotation is not automatic.
**Never delete an established CA to repair startup.**

To replace a CA:

1. Stop the proxy.
2. Archive the old CA securely.
3. Configure a new, absent CA directory.
4. Start the proxy.
5. Verify the new fingerprint.
6. Redistribute `ca.crt` ([trust the CA](#trust-the-ca)).
7. Remove the old client trust.

### Clean up staging directories

A crash or SIGKILL during CA initialization can leave a temporary `.ca-init-*`
directory in the CA parent. It doesn't block startup. Nothing reuses or
removes it automatically, because it might belong to another initializer that
is still running.

These directories may contain private keys. Keep their permissions, and don't
print or copy their contents.

To clean up:

1. Stop every process that can initialize a CA in that parent, and keep them
   stopped.
2. Inspect only that trusted parent.
3. Remove each orphaned `.ca-init-*` directory by its exact path. Never use a
   wildcard deletion or remove the configured CA directory.

## Cache storage

This section covers cache settings and operations. Digest-addressed blobs and
manifests are cached on disk. See
[disk cache storage](how-it-works.md#disk-cache-storage) for how entries are
stored, filled, and evicted, [entries](how-it-works.md#entries) for how
entries are keyed, and [what gets cached](how-it-works.md#what-gets-cached)
for the exceptions.

### Cache directory requirements

The directory in `OCI_ACCELERATOR_CACHE_DIR` must be mode `0700` and owned by
the proxy's user. Its parent must also be owned by that user and not writable by
group or others. Startup creates the directory if it doesn't exist. A lock file
lets only one proxy process use the cache at a time, so a second process fails
to start.

### Mutable response cache

This separate in-memory cache reuses successful `200` responses for:

- `GET` and `HEAD /v2/<name>/manifests/<tag>`, as separate entries
- `GET /v2/<name>/tags/list`, including distinct pagination queries
- `GET /v2/<name>/referrers/<digest>`, including distinct filter queries
- `GET /v2/_catalog`, including distinct pagination queries

Entries live only in memory and are never shared with peers. They expire after
`OCI_ACCELERATOR_CACHE_MUTABLE_TTL`, 30 seconds by default, so a longer value
means tags can stay stale longer.

See [mutable response caching](how-it-works.md#mutable-response-caching) for
matching, expiry, memory limits, and invalidation.

### Capacity and eviction

- `OCI_ACCELERATOR_CACHE_MAX_BYTES` counts each body plus a fixed
  [metadata charge](#built-in-limits) per entry, including active fills. Leave
  filesystem headroom for blocks, directories, and inodes beyond it.
- When the cache is full, older entries are evicted, and objects that still
  don't fit stream to Docker without being cached. See
  [limits and eviction](how-it-works.md#limits-and-eviction) and
  [filling](how-it-works.md#filling) for how eviction and concurrent fills
  work.

### Startup recovery

- Startup removes abandoned fills, incomplete entries, and entries whose body
  file has the wrong type, mode, owner, or size. It refuses to start on an
  unknown file, or an entry directory or metadata file with the wrong mode or
  owner, rather than deleting it.
- If the cache no longer fits its limits, startup deletes the oldest entries
  until it does. Lowering `OCI_ACCELERATOR_CACHE_MAX_BYTES` or
  `OCI_ACCELERATOR_CACHE_MAX_ENTRIES` therefore deletes cached content at the
  next start.
- Don't edit or delete live cache files. Use [metrics](#metrics) to inspect
  resource use.

### Reset the cache

Resetting permanently deletes cached content but leaves the CA alone. For a
native deployment, stop the proxy and remove the whole
`OCI_ACCELERATOR_CACHE_DIR`. For Compose, remove only the cache volume:

```sh
docker compose down

docker volume rm oci-accelerator-cache

docker compose up -d
```

For `docker run`, remove the container with `docker rm -f oci-accelerator`
before removing the volume, then start it again as in [docker run](#docker-run).

## Peer discovery

Peering lets instances that share a CA download cached content from each
other on a miss, after upstream approval. Registry credentials and
authorization grants are never shared. See
[how it works](how-it-works.md#peer-discovery-and-cache-delivery) for an
overview and the [peer protocol](peer-protocol.md) for wire-level details.

### Set up peering

Provision the same `ca.crt` and `ca.key` into each instance's private CA
directory through your trusted deployment system. Give each instance its own
local state directory or volume, not a shared writable filesystem. Ordinary
Docker clients receive only `ca.crt`.

Add these settings to your existing configuration, substituting reachable
addresses:

```sh
OCI_ACCELERATOR_PEER_LISTEN=0.0.0.0:5130
OCI_ACCELERATOR_PEER_ADVERTISE=accelerator-a.example:5130
OCI_ACCELERATOR_PEER_SEEDS=accelerator-b.example:5130,accelerator-c.example:5130
OCI_ACCELERATOR_PEER_STATE_DIR=./data/peers
```

- `PEER_LISTEN` binds a separate TLS port, distinct from the proxy and metrics
  listeners.
- `PEER_ADVERTISE` is the address other instances dial. Each live node needs
  its own reachable `host:port`, not a wildcard bind, a container-only
  hostname, or a shared load-balanced endpoint.
- Seeds are optional bootstrap addresses, not an allowlist. Ports are always
  explicit, including `:443`, and IPv6 uses `[address]:port`.
  Configuration rejects more than 32 addresses after canonicalization,
  deduplication, and exclusion of self.
- `init` already prepares `data/peers/`. For containers, publish the peer port
  and use a writable volume such as `/var/lib/oci-accelerator/peers`
  ([state directories](#state-directories)).
- Validate with `--check`. To disable peering, clear the listener, advertised
  address, and seeds. The address book stays on disk.

### Peer trust

Peer authentication establishes fleet membership, not trusted individual
identities. Anyone holding the shared CA key can impersonate any peer, so
compromising one instance compromises the whole fleet, and you can't revoke a
single node while it holds that key. See
[trust and membership](how-it-works.md#trust-and-membership) for the reasoning
and [peer certificates](peer-protocol.md#peer-certificates) for the TLS rules.

Peers can make your instance connect to any host and port they name. Mutual TLS
rejects non-peers but can't prevent those connection attempts, so restrict
network access as your deployment requires. Peer connections ignore proxy
environment variables, never follow redirects, and never trust a CA downloaded
from the destination.

### Limits and state

The [peer protocol](peer-protocol.md#limits) lists membership, connection,
transfer, and hint limits.

- Keep a reachable seed or another live peer so separated groups can find each
  other after a partition.
- The state directory holds `node-id` and `peers.json`. Don't edit them while
  the instance runs, and never copy them to another instance, because that
  duplicates its node ID.

To reset membership, for example after startup rejects `peers.json`
([state files](peer-protocol.md#state-files)):

1. Stop the instance.
2. Back up its peer state directory.
3. Remove only `peers.json`. Keep `node-id` to preserve the instance's
   identity.
4. Configure a reachable seed, so verified outbound exchanges can rebuild
   membership.
5. Start the instance.

Listing traffic grows with the square of fleet size, because each node polls
every other node. As an illustrative body estimate, polling one terminal page
of 4,096 keys (274,476 bytes) from each of 32 remote peers per nominal minute
uses about 8.38 MiB per node per minute. This excludes protocol overhead and
full scans and isn't a measured benchmark or a freshness guarantee.

Watch for these log messages:

| Message | Meaning |
| --- | --- |
| `peer identity collision` | Another node claims this instance's node ID or address. Fix the advertised address or duplicated state |
| `peer address book full` | The 32-member limit is reached, so new unrelated members are rejected |
| `peer verification queue full` | Too many unverified candidates. Excess claims are dropped until later gossip resubmits them |
| `peer address book update failed` | Saving `peers.json` failed. The exchange fails, but the proxy keeps serving |
| `peer membership expiry persistence failed` and `…recovered` | Saving expired members failed, then recovered. Expired members stay hidden meanwhile |
| `peer cache keys refresh failed` | Fetching a peer's key listing failed. Earlier hints are kept |

These logs include the peer address but never raw network errors or
credentials. A peer listener failure stops the process rather than leaving
peering silently unavailable.

## Configuration

Configuration comes from three sources, each overriding the one before:
built-in defaults, an optional dotenv file named with `--env-file`, and process
environment variables. There is no automatic file discovery,
`OCI_ACCELERATOR_CONFIG` variable, or hot reload.

`OCI_ACCELERATOR_REGISTRIES`, `OCI_ACCELERATOR_CA_DIR`, and
`OCI_ACCELERATOR_CACHE_DIR` are required. The container [image](#image) supplies
the two directories. See the [settings reference](settings.md) for every
setting, its default, and how timeouts and memory limits behave. The registries
and `./data` paths in [`.env.example`](../.env.example) are examples, not
built-in defaults.

`--check` validates this same combined configuration without inspecting state,
binding listeners, or creating directories. Unknown process environment
variables are ignored.

An unset variable keeps the file value or default. A variable set to an empty
string is an explicit value:

- Empty `OCI_ACCELERATOR_METRICS_LISTEN` disables metrics.
- Empty `OCI_ACCELERATOR_REDIRECT_ALLOWLIST` clears the allowlist.
- Empty `OCI_ACCELERATOR_PEER_LISTEN` disables peering, and the peer advertised
  address and seeds must then also be empty. A nonempty listener enables
  peering. There is no separate enable flag.
- Empty `OCI_ACCELERATOR_PEER_ADVERTISE` and `OCI_ACCELERATOR_PEER_STATE_DIR`
  are valid only while peering is disabled. Empty `OCI_ACCELERATOR_PEER_SEEDS`
  is always valid.
- Any other empty value fails validation, including the required registries
  and paths.

Peer settings are validated even while peering is disabled.

Use exactly `true` or `false` for booleans, decimal integers without a sign or
leading zeros for numbers, and Go duration strings such as `500ms`, `30s`, or
`2m` for durations. Relative paths from all sources are resolved from the
process working directory and converted internally to absolute paths.

For example, process environment values can override a native dotenv file:

```sh
OCI_ACCELERATOR_CACHE_MAX_BYTES=68719476736 bin/oci-accelerator --env-file .env
```

### Dotenv file rules

- Use `KEY=value`, with no space before `=`. Whitespace around the value is
  trimmed. Blank lines and full-line comments are ignored.
- In unquoted values, `#` after a space or tab starts a comment, so `KEY=a #b`
  sets `a`, but `KEY=a#b` sets `a#b`.
- Single or double quotes keep the contents literal, with no escapes or
  interpolation. Only a comment may follow the closing quote.
- A missing file, duplicate or unknown keys, `export`, multiline values, and
  lines of 64 KiB or longer are rejected.
- A syntax error in the file always fails, even if the environment overrides
  that key. An invalid value in the file can be overridden.

### Registry authorities

Each `OCI_ACCELERATOR_REGISTRIES` and `OCI_ACCELERATOR_REDIRECT_ALLOWLIST` entry
is an authority: `host` or `host:port`, with no scheme or path. An omitted port
means 443, and matching includes the port. Both settings take comma-separated
lists. Entries are whitespace-trimmed, empty entries are rejected, and a list
replaces the file value as a whole.

| Entry | Matches |
| --- | --- |
| `ghcr.io` | `ghcr.io:443` |
| `registry.example:5000` | `registry.example:5000` |
| `[2001:db8::1]:5000` | IPv6 with a port (bracketed) |

Entries differing only in case, a trailing dot, or `:443` are duplicates and are
rejected. You can configure private registries explicitly.

### State directories

- The CA and cache directories must be different, and the CA directory must
  not be inside the cache directory, because the cache may remove entries in
  its own directory. The cache directory may be inside the CA directory. These
  checks compare paths as written and don't follow symlinks. Both directories
  are required, so there is no forwarding-only mode.
- A configured peer state directory must not equal, contain, or be contained
  by the CA or cache directories.
- Before first start, create the immediate parent of each directory on
  durable storage as the proxy user, with no group or other write access,
  for example `mkdir -m 0700 data`. If the parent already exists, check its
  ownership and permissions instead. Startup creates the directories
  themselves, but neither startup nor `init-ca` creates ancestors. `init`
  does this for the native `data/` layout.
- Ownership and layout are checked at startup. See the
  [CA](#ca-directory-requirements) and [cache](#cache-directory-requirements)
  directory requirements.

### Built-in limits

| Limit | Value |
| --- | --- |
| CONNECT request and upstream response headers | 64 KiB |
| Upstream connections per destination | 16 |
| Idle upstream connections | 32 globally, 8 per destination |
| Redirects followed | 5 |
| Disk cache metadata charge | 4 KiB per entry |
| Disk eviction removal attempts per admission | 8 |
| Mutable captures and retained response memory | 32 MiB accounted, not process RSS |
| Mutable response body and retained headers | 1 MiB body, 64 KiB headers |
| Mutable concurrent captures and indexed entries | 16 captures, 1,024 entries |
| Root CA validity | 10 years |
| Leaf certificate validity | At most 24 hours |
| Intercepted protocols | TLS 1.2 or newer, HTTP/1.1 |

## Request handling

[How it works](how-it-works.md) describes what is cached and how ranges,
redirects, and authorization work. This section lists additional rules that can
affect your deployment.

### Methods and protocols

| Request | Handling |
| --- | --- |
| GET, HEAD, OPTIONS | Forwarded unless an eligible cache hit applies |
| POST, PUT, PATCH, DELETE | Streamed upstream without caching or path filtering |
| `/v2/`, other HEADs, conditional requests, ineligible ranges | Always passed through |
| Tag manifest GET and HEAD, tag lists, referrer lists, `_catalog` | Eligible for the separate mutable response cache |
| GET, HEAD, or OPTIONS with a body | Denied |
| Upgrades, other methods, authority conflicts, non-origin-form targets | Denied |

### Redirects

The proxy follows up to five redirects for digest-addressed GETs and checks
the destination of each hop. Add private storage hosts, such as an internal
CDN, to `OCI_ACCELERATOR_REDIRECT_ALLOWLIST`. See
[redirects](how-it-works.md#redirects) for the rules.

### Authorization grants

[How it works](how-it-works.md#authorization-grants) explains how grants are
created, matched, and expire. For your deployment:

- The anonymous and credentialed TTLs are independent. Any `Authorization`
  header, even an empty one, makes a request credentialed.
- Bearer grants cover a whole repository, so they assume each token has
  repository-wide pull permission. If your registry uses object-specific pull
  rules, set `OCI_ACCELERATOR_CACHE_AUTHORIZATION_TTL=0s`. Disabling shared
  mutable caching alone isn't enough.
- Grant hits return `Cache-Control: private, no-store`, so downstream caches
  can't extend a grant's lifetime.

See [what revokes it](how-it-works.md#what-revokes-it) for how upstream
responses affect an existing grant.

## Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| `--check` passes but startup fails | `--check` doesn't inspect CA or cache state or bind listeners. Read the startup error and see the rows below |
| Startup fails naming a CA check | The CA files or directories don't meet the [CA directory requirements](#ca-directory-requirements), so fix them. Never delete an established CA. [Replace it](#back-up-and-replace-the-ca) instead |
| Startup fails on a missing directory | Startup and `init-ca` don't create parent directories. Create the parent as the proxy user ([state directories](#state-directories)) |
| Startup refuses unknown cache files | The cache directory contains unrecognized files. Investigate them, or [reset the cache](#reset-the-cache) |
| Second proxy fails to start on the same cache | Each cache directory allows one process. Give each process its own cache and CA directories. Peers can use copies of the same CA files |
| `--check` rejects the metrics listener | The address isn't loopback and `OCI_ACCELERATOR_METRICS_ALLOW_REMOTE` isn't `true`. Use a loopback address, or opt in ([metrics](#metrics)) |
| Startup fails binding metrics | The metrics address is invalid or in use. Fix `OCI_ACCELERATOR_METRICS_LISTEN` or free the address |
| Container health is `unhealthy` | The metrics listener isn't at `127.0.0.1:9090`, or the proxy isn't ready. Restore the listener or override the health check ([image](#image)) |
| Docker reports an unknown certificate authority | The CA isn't installed under the exact `certs.d/<host[:port]>` directory. [Trust the CA](#trust-the-ca) for that authority |
| Private registry fails with a TLS error | The proxy doesn't trust the registry's real CA. Add it to the proxy's system trust store. Never disable verification |
| Pulls work but no request records show the registry | The registry is missing from `OCI_ACCELERATOR_REGISTRIES` (the port must match), listed in `no-proxy`, or Docker wasn't restarted. Fix the registry list or [Docker's proxy configuration](#point-docker-at-the-proxy) |
| Revoked access, a moved tag, or a deleted image still appears briefly | A live [grant](#authorization-grants) or [mutable response](#mutable-response-cache) is still in use. Wait for expiry, or turn off reuse ([authorization of cached content](#authorization-of-cached-content), [stale mutable metadata](#stale-mutable-metadata)) |
| The first pull from cache after a restart is slow | Each body is rehashed on first use. This is expected, and later hits skip the rehash |
| Peers don't form a fleet, or peer warnings appear in logs | Usually an unreachable advertised address, NAT, or a node ID collision. Check `OCI_ACCELERATOR_PEER_ADVERTISE` and [limits and state](#limits-and-state), including its log message table |
| `.ca-init-*` directories in the CA parent | A crash or SIGKILL interrupted CA initialization. [Clean up staging directories](#clean-up-staging-directories) |
| Compose warns about existing volumes | The volumes were created by `docker run`. This is expected. Run only one proxy against them |

## Current limitations

- Redirect checks protect only fetches the proxy makes itself, not tunnel
  egress ([no client authentication](#no-client-authentication)).
- Peering tries one provider per miss, with no shared filesystem, coordinated
  eviction, or prefetch. After a peer stream is accepted, a failure can't
  switch to origin, so the client must retry.
- No Kubernetes manifests are provided.
- See the [testing guide](testing.md#current-limitations) for what the tests
  don't cover.
