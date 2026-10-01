# Operating OCI Accelerator

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
- **The CA can sign any server identity.** Protect `ca.key`, never distribute it or bake it
  into an image, and scope client trust to each intercepted registry rather than system-wide.
- **The CA download is unauthenticated.** Verify its fingerprint through a trusted channel
  before installing it ([details](#trust-the-ca)).
- **Upstream TLS is always verified**, using the proxy process's system trust store. There is
  no insecure mode.
- **Credentials stay with Docker.** Never put credentials in proxy YAML. Cross-origin redirects
  never forward credentials or cookies.
- **The registry authorizes deliveries.** Each cached delivery is approved by an upstream GET,
  or by an exact-request grant up to `cache.authorization_ttl` (30 seconds by default), so
  revocation can lag by up to that long. Set `0s` to require upstream approval for every
  delivery. There is no offline or failed-request fallback.
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

Neither CA initialization nor proxy startup creates ancestor directories. Provision the
immediate parent of `ca_dir` and `cache.dir` as the proxy's user before first start (see
[state directories](#state-directories)).

### Run the binary

From the repository root:

```sh
go build -o bin/oci-accelerator ./cmd/oci-accelerator
cp config.example.yaml config.local.yaml
# Edit registries, then validate:
bin/oci-accelerator --config config.local.yaml --check
```

`--check` validates only the configuration. It does not inspect CA or cache state, bind
listeners, or create directories.

Create the state parent (`data` in the example) only if absent. If it exists, check its
ownership and permissions. Optionally provision the CA ahead of time, then start:

```sh
mkdir -m 0700 data
bin/oci-accelerator init-ca --dir data/ca   # optional (startup creates it otherwise)
bin/oci-accelerator --config config.local.yaml
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
| Default arguments | `--config /etc/oci-accelerator/config.yaml`     |
| Health check      | `wget` of `http://127.0.0.1:9090/readyz`        |
| State directories | `/var/lib/oci-accelerator` and its `cache/`, UID/GID 10001, mode 0700 (`ca/` is left absent) |

The health check needs the metrics listener enabled at `127.0.0.1:9090`. Keep it there or
override the health check when changing the YAML. An unhealthy result is diagnostic and does not
itself trigger a restart. Readiness does not check upstream connectivity or disk capacity.

`config.container.yaml` uses `/var/lib/oci-accelerator/ca` for `ca_dir`,
`/var/lib/oci-accelerator/cache` for `cache.dir`, the same registries as `config.example.yaml`,
and a loopback-only metrics listener at `127.0.0.1:9090` that is not published to the host.

#### Compose

The supplied `compose.yaml` service builds the local image from the repository root and:

- publishes only `127.0.0.1:3128`
- keeps the root filesystem read-only, drops all capabilities, and sets `no-new-privileges`
- uses `restart: unless-stopped` and a 40-second stop grace period, allowing for the 15-second
  `shutdown_timeout` and metrics cleanup (increase it if you raise `shutdown_timeout`)

| Source                             | Mount point                       | Access     |
| ---------------------------------- | --------------------------------- | ---------- |
| `./config.container.yaml`          | `/etc/oci-accelerator/config.yaml` | Read-only |
| `oci-accelerator-ca` volume        | `/var/lib/oci-accelerator`        | Writable   |
| `oci-accelerator-cache` volume     | `/var/lib/oci-accelerator/cache`  | Writable   |

Volume notes:

- The CA volume mounts the parent so startup can atomically create its `ca/` child. Docker
  copies the image's ownership and permissions into fresh volumes, so no host directory
  provisioning or root entrypoint is needed. The CA volume remains writable at runtime.
- Existing valid CA files are reused, not replaced.
- Run only one proxy against these volumes at a time. The names match the
  [`docker run`](#docker-run) example. Compose warns when reusing volumes created by
  `docker run` because it does not own them.
- These examples do not migrate existing host bind-mounted data. Starting with new volumes
  creates a new CA that clients must trust.

Build, validate, and start:

```sh
docker compose build
docker compose run --rm --no-deps oci-accelerator \
  --config /etc/oci-accelerator/config.yaml --check
docker compose up -d
```

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
docker run -d --name oci-accelerator --restart unless-stopped \
  --read-only --cap-drop ALL --security-opt no-new-privileges \
  --stop-timeout 40 -p 127.0.0.1:3128:3128 \
  --mount type=bind,src="$PWD/config.container.yaml",dst=/etc/oci-accelerator/config.yaml,readonly \
  --mount type=volume,src=oci-accelerator-ca,dst=/var/lib/oci-accelerator \
  --mount type=volume,src=oci-accelerator-cache,dst=/var/lib/oci-accelerator/cache \
  oci-accelerator:local
docker logs oci-accelerator
docker inspect --format '{{.State.Health.Status}}' oci-accelerator
```

Stop it with `docker stop oci-accelerator`. Removing the container preserves both named volumes.

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
  `buildkite/oci-accelerator:latest` (see the
  [publishing workflow and prerequisites](../.buildkite/README.md#docker-hub-releases)).

### Serve remote Docker hosts

The default `127.0.0.1:3128` listener serves Docker on the same host. To serve remote hosts:

1. Set `listen` to an explicit private-interface address.
2. Restrict access with network access controls and a firewall (see [security model](#security-model)).
3. Use the proxy's private address in each host's [Docker proxy configuration](#point-docker-at-the-proxy).

## Configuration

See [`config.example.yaml`](../config.example.yaml) for a complete configuration. Configuration
is static, and changes take effect after restart.

### File rules

- Exactly one YAML document. Unknown or duplicate keys are rejected.
- Null values retain scalar defaults, but null list elements are invalid.
- String fields reject non-string scalars.
- Numeric limits require decimal YAML integers without leading zeros. Floats, quoted numbers,
  octal or hex forms, and underscores are rejected.
- Malformed durations and invalid zero values are rejected. The only permitted zero durations
  are `cache.fill_wait_timeout: 0s` and `cache.authorization_ttl: 0s`.
- Relative directories are resolved from the configuration file's directory.

### Registry authorities

Each `registries` and `redirect_allowlist` entry is an authority: `host` or `host:port`, with
no scheme or path. An omitted port means 443, and matching includes the port.

| Entry                   | Matches                  |
| ----------------------- | ------------------------ |
| `ghcr.io`               | `ghcr.io:443`            |
| `registry.example:5000` | `registry.example:5000`  |
| `[2001:db8::1]:5000`    | IPv6 with a port (bracketed) |

Entries differing only in case, a trailing dot, or `:443` are duplicates and are rejected.
Explicitly configured private registries are supported.

### State directories

- `ca_dir` and `cache.dir` must both be nonblank and resolve to distinct, lexically normalized
  absolute paths. There is no forwarding-only mode.
- The CA directory must not be inside the cache directory, because the cache owns, and may
  remove, entries in its directory. The reverse nesting is permitted.
- Before first start, create the immediate parent of each directory as the proxy's user,
  without group or other write access (for example `mkdir -m 0700 data`). The directories
  themselves are created at startup, but no ancestors are.
- Configuration validation does not resolve symlinks or inspect state. Filesystem ownership and
  layout checks apply at startup ([CA](#ca-directory-requirements), [cache](#capacity-and-eviction)).

### Settings

**Listeners**

| Setting                | Default          | Notes                                                                                    |
| ---------------------- | ---------------- | ---------------------------------------------------------------------------------------- |
| `listen`               | `127.0.0.1:3128` | host:port with a nonzero numeric port. Empty or wildcard hosts bind all interfaces      |
| `metrics_listen`       | Disabled         | host:port with a nonzero numeric port. Literal loopback IP required unless `metrics_allow_remote` |
| `metrics_allow_remote` | `false`          | Explicit boolean opt-in to non-loopback metrics and probe binding                       |

**Registries and state**

| Setting              | Default  | Notes                                                                     |
| -------------------- | -------- | ------------------------------------------------------------------------- |
| `registries`         | Required | Nonempty list of distinct [authorities](#registry-authorities) to intercept |
| `redirect_allowlist` | Empty    | Private HTTPS storage [authorities](#registry-authorities). Does not enable interception |
| `ca_dir`             | Required | Private persistent CA directory                                           |
| `cache.dir`          | Required | Private persistent cache directory                                        |

**Cache and authorization**

| Setting                           | Default | Notes                                                                                   |
| --------------------------------- | ------- | --------------------------------------------------------------------------------------- |
| `cache.max_bytes`                 | 32 GiB  | Includes body, metadata, and fill reservations                                          |
| `cache.max_fills`                 | 16      | Concurrent storage writers. Excess misses stream uncached                               |
| `cache.max_entries`               | 100000  | Entry slots, including fills and pending cleanup                                        |
| `cache.fill_timeout`              | `10m`   | Bounds ranged fetches from request start. Ends ordinary full-GET cache admission        |
| `cache.fill_wait_timeout`         | `1s`    | `0s` disables waiting for an existing fill                                              |
| `cache.authorization_ttl`         | `30s`   | Maximum fixed lifetime of a process-local approval. `0s` requires an upstream GET for every delivery |
| `cache.max_authorization_entries` | 10000   | Positive bound on in-memory authorization grants                                        |

**Timeouts and connections**

| Setting                   | Default | Notes                                                        |
| ------------------------- | ------- | ------------------------------------------------------------ |
| `dial_timeout`            | `10s`   |                                                              |
| `tls_handshake_timeout`   | `10s`   |                                                              |
| `response_header_timeout` | `30s`   | Also bounds incoming request headers                         |
| `idle_timeout`            | `2m`    | Idle tunnels and I/O                                         |
| `shutdown_timeout`        | `15s`   | Drain deadline                                               |
| `max_connections`         | 256     | Accepted client connections, including incomplete CONNECTs   |
| `max_leaf_certificates`   | 256     | Cached interception certificates                             |

Timeout behavior:

- For eligible blob range requests, `cache.fill_timeout` bounds the upstream fetch, starting
  before the first upstream GET, including when cache-fill admission is refused. Admitted
  background fills remain subject to the same deadline.
- Delivery from an already-open cached reader is not limited by `cache.fill_timeout`, but idle-I/O
  deadlines and shutdown handling still apply.
- Other active transfers have idle-I/O deadlines, not total download deadlines. Uploads have no
  total size limit ([push and delete](#push-and-delete)).
- Kernel filesystem calls cannot be preempted, so fill expiry and shutdown are not hard
  deadlines when storage stalls.

Memory: `cache.max_entries` and `cache.max_authorization_entries` bound their indexes, not total
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

The CA is `ca.crt` and `ca.key` in `ca_dir`: `data/ca/` on the host in the native example, or
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

- `init-ca` provisions only the root CA. It does not load YAML or start the proxy, cache, or any
  network activity.
- `--dir` is required. Relative paths are resolved from the working directory. The parent must
  already exist.
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

Preserve `ca_dir` across restarts, back up both CA files securely, and use one proxy process
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
   a registry on port 5000, and omit `:443` when image names omit it. Never distribute `ca.key`
   or install it as a Docker client key. See Docker's
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
- For a private registry, the proxy process must also trust the registry's real CA in its
  system trust store, as well as Docker. Do not disable TLS verification or configure an
  insecure registry as a workaround.

### Push and delete

Docker push uses the same proxy and CA configuration as pull. 

- The registry enforces write permissions using Docker's credentials.
- Uploads do not warm the cache. The proxy does not manage upload sessions or follow write
  redirects.
- Active uploads are limited by `idle_timeout` for stalled I/O, not by `cache.fill_timeout` or
  a total upload size.
- Successful deletion does not invalidate existing pull [authorization grants](#authorization-grants).
  Previously approved exact GETs may still serve cached content until the original grant
  expiry. Set `cache.authorization_ttl: 0s` when every cache delivery must check current
  upstream access.

## Request handling

[How it works](how-it-works.md) describes what is cached, range requests, redirects, and
authorization approval. This section lists the additional rules operators may need.

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
  the client disconnects, bounded by `cache.fill_timeout` from request start.
- Ranged manifests, `If-Range` and other conditional requests, no-store requests, and a Range
  header named by `Connection` remain passthrough.

### Redirects

The proxy follows up to five redirects, only for eligible query-free SHA-256 blob and digest
manifest GETs.

- Every hop must use verified HTTPS and pass destination checks.
- Userinfo, fragments, loops, and implicit private, link-local, or loopback destinations are
  rejected. Add private storage destinations to `redirect_allowlist`.
- Cross-origin redirects preserve only `Accept`, `Accept-Encoding`, and `User-Agent`, never
  credentials or cookies.
- These restrictions do not apply to tunnel egress (see [limitations](#current-limitations)).

### Authorization grants

See [authorization grants](how-it-works.md#authorization-grants) for grant matching, lifetime,
and the request and response headers that prevent a grant. Additionally:

- Only full immutable digest blob and manifest GETs can reuse authorization.
- Grants are keyed by a process-local HMAC of the original canonical URL and every forwarded
  end-to-end header. Transport-managed fields use their forwarded form, while other exact values and
  value order are preserved. Anonymous requests are distinct.
- A grant starts from an eligible terminal `200`. It is never renewed on hits, persisted, or
  refreshed or coalesced in the background.
- Tokens are opaque, so revocation or expiry can take effect as late as the grant's original
  expiry.
- Every redirect hop must be reusable. Only explicit `max-age` and `s-maxage` ceilings shorten a
  grant below `authorization_ttl`. `Age`, `Date`, and `Expires` are ignored. A grant never
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

- Use one process per cache directory. Its parent must be owner-controlled. Unsafe or unusable
  configured storage fails startup.
- `cache.max_bytes` charges body size plus 4 KiB metadata per entry, including active fills.
  Reserve filesystem block, directory, and inode headroom beyond it.
- Oversized or capacity-rejected objects continue streaming without being cached.
- Failed cleanup retains its capacity charge until removal succeeds.
- Best-effort LRU eviction skips active readers and makes at most eight removal attempts per
  admission, separately from pending-cleanup retries. Objects needing more victims can keep
  bypassing, and competing traffic may consume reclaimed space, so admission is not guaranteed.

### Concurrent fills

- Concurrent clients authorize independently. One writer fills a key, and matching followers may
  wait for it (`cache.fill_wait_timeout`) and then open the verified entry.
- Wait timeout, leader failure, or eviction causes one fresh authorized GET.
- Follower cancellation does not control the leader.
- Ordinary full-GET fills are request-owned: leader cancellation aborts storage.
- An admitted ranged fill may finish after range delivery or client cancellation, bounded by
  `cache.fill_timeout` from request start.
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
stop the proxy and remove the whole `cache.dir`. For Compose, remove only the cache volume:

```sh
docker compose down
docker volume rm oci-accelerator-cache
docker compose up -d
```

## Monitoring

### Logs

JSON logs go to stderr. Request completion records contain:

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

Set `metrics_listen: 127.0.0.1:9090` to enable a separate listener. There is no metrics route
on the CONNECT listener, admin API, or pprof endpoint.

| Endpoint       | Purpose                                                  |
| -------------- | -------------------------------------------------------- |
| `GET /metrics` | Prometheus text                                          |
| `GET /livez`   | Process responsiveness                                   |
| `GET /readyz`  | Initialized and serving, not shutting down               |

- Non-loopback binds are rejected unless `metrics_allow_remote: true`. Remote scraping has no
  authentication: restrict it with private network controls and never expose it publicly or
  through a portal.
- A bind failure prevents startup.
- Probes never contact upstream registries or depend on cache capacity, and are independent of
  `max_connections`. Kubernetes can use local exec probes with a loopback bind when remote
  scraping is disabled.

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
  exact count of requests received by the registry.
- `oci_accelerator_cache_rejections_total` reports `vary` and `digest_mismatch` header
  rejections on eligible responses. Other policy checks may also prevent caching, so it does
  not identify the sole cause of every bypass.

### Cache delivery ratios

`oci_accelerator_served_bytes_total{source="cache|origin"}` and
`oci_accelerator_served_entries_total{source="cache|origin"}` measure how much digest-addressed
content clients received from the cache versus the registry.

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
2. drains until `shutdown_timeout`, then closes remaining connections, including opaque tunnels
3. cancels and joins ranged transfers, and waits for handlers and relay pumps to release their
   resources before the cache is closed, even beyond the drain deadline

A stuck handler can therefore delay exit. A second signal uses the operating system's default
termination behavior. For containers, keep the stop grace period longer than
`shutdown_timeout` (see [Compose](#compose)).

### What survives a restart

| State                    | After restart                                                       |
| ------------------------ | ------------------------------------------------------------------- |
| Configuration            | Re-read. Changes take effect only now                               |
| Root CA                  | Re-checked and reused from `ca_dir`                                 |
| Leaf certificates        | Memory only, re-signed on demand                                    |
| Authorization grants     | Cleared. The next delivery needs fresh upstream approval            |
| Cache entries            | Kept. Each body is rehashed on first reuse                          |
| Abandoned fills          | Removed at startup                                                  |
| Metrics counters         | Reset                                                               |

## Troubleshooting

| Symptom                                                   | Likely cause                                                                                   | Fix                                                                                   |
| --------------------------------------------------------- | ---------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------- |
| `--check` passes but startup fails                        | `--check` does not inspect CA or cache state or bind listeners                                 | Read the startup error. See the rows below                                            |
| Startup fails naming a CA check                           | CA directory or files are empty, incomplete, expired, mismatched, oversized, symlinked, foreign-owned, or wrongly permissioned | Correct the state per [CA directory requirements](#ca-directory-requirements). Never delete an established CA ([replace it](#back-up-and-replace-the-ca) instead) |
| Startup fails on a missing directory                      | Neither `init-ca` nor startup creates ancestors                                                | Create the parent as the proxy user ([state directories](#state-directories))         |
| Startup refuses unknown cache files                       | Unrecognized files in `cache.dir`. Startup never deletes them                                  | Investigate their origin, or [reset the cache](#reset-the-cache)                      |
| Second proxy fails to start on the same cache             | One process per cache directory                                                                | Give each process its own `cache.dir` and `ca_dir`                                    |
| `--check` rejects `metrics_listen`                        | Non-loopback address without `metrics_allow_remote: true`                                      | Use a loopback address, or opt in ([metrics](#metrics))                               |
| Startup fails binding metrics                             | The metrics listener cannot bind. A bind failure prevents startup                              | Fix `metrics_listen` or free the address                                              |
| Container health is `unhealthy`                           | Metrics listener not at `127.0.0.1:9090`, or the proxy is not ready or is shutting down        | Restore the listener or override the health check ([image](#image)). It does not restart the container |
| Docker reports an unknown certificate authority           | CA not installed under the exact `certs.d/<host[:port]>` directory                             | [Trust the CA](#trust-the-ca) for that authority                                      |
| Private registry fails through the proxy with a TLS error | The proxy process does not trust the registry's real CA                                        | Add it to the proxy's system trust store. Never disable verification                  |
| Pulls work but no request records show the registry       | Registry not in `registries` (port must match), listed in `no-proxy`/`NO_PROXY`, or daemon not restarted | Fix `registries` or [Docker's proxy configuration](#point-docker-at-the-proxy)   |
| Revoked access or a deleted image still pulls briefly     | An existing [authorization grant](#authorization-grants)                                       | Wait for `cache.authorization_ttl`, or set it to `0s`                                  |
| `.ca-init-*` directories in the CA parent                 | A crash or SIGKILL during initialization                                                       | [Clean up staging directories](#clean-up-staging-directories)                         |
| Compose warns about existing volumes                      | Volumes were created by `docker run`                                                           | Expected. Run only one proxy against them                                             |

## Current limitations

- Redirect restrictions protect proxy-originated fetches only. They do not constrain the broader
  egress available through opaque CONNECT tunnels, so deployment network controls remain essential.
- No shared or distributed storage, multi-instance coordination, or background prefetch.
- No Kubernetes manifests, and no automated container runtime acceptance in CI.
- Fault tests exercise process termination and private tmpfs exhaustion, not power loss or
  faulty-device certification. See the [testing guide](testing.md).

## Appendix: Amp orbs

### Prepare an orb

Amp runs `.agents/setup` when preparing a fresh orb. It installs checksum-verified Go 1.27.1, C
build tools for race-enabled tests, and Docker Engine, Buildx, and Compose if missing, and
downloads Go module dependencies. Re-running it is safe:

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
- Amp supervises the daemon across CLI updates and orb pause/resume, so no custom resume hook is
  needed.
- Registry credentials belong to the Docker client, never the setup script or a shared orb
  snapshot. Setup does not install a proxy CA into Docker's trust store.

### Run the proxy as an orb service

```sh
amp orb service start registry-proxy \
  --command './bin/oci-accelerator --config config.local.yaml'
amp orb service logs registry-proxy
amp orb service stop registry-proxy
```

Do not expose the proxy through an Amp portal. To run acceptance tests in an orb, see the
[testing guide](testing.md).
