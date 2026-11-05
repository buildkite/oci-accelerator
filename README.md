# OCI Accelerator

OCI Accelerator is a specialized pull-through caching proxy for Docker and OCI
image registries. It intercepts OCI Distribution API read requests to the
registries you configure and caches their responses to speed up image pulls and
reduce upstream bandwidth.

Use it to:

- Warm up autoscaled or ephemeral hosts that start with an empty Docker cache.
- Speed up CI builds that pull the same base and tool images on every job.
- Pull multi-gigabyte images, such as machine learning images, once per site
  instead of once per host.
- Reduce upstream requests and bandwidth, easing pressure on rate-limited
  registries and metered links.

## Highlights

- **Works with any registry.** It caches any registry that implements the OCI
  Distribution API, not only Docker Hub.
- **No image renaming.** Docker keeps its usual image names and `docker login`,
  so you don't rewrite references or configure registry mirrors.
- **Private images stay private.** Cached content requires a recent upstream
  approval matching either the exact request or the Bearer token's access to
  that registry and repository.
- **No shared credentials.** Each client uses its own credentials. The proxy can
  read them and forwards them to the intended upstream registry, but doesn't
  share them with other clients or cache peers.
- **Plugs in as an HTTPS proxy.** Docker Engine sends registry traffic through
  it using its standard proxy setting, once you trust the proxy's root CA.
- **Optional peer sharing.** Instances can share cached content with each other
  over mutually authenticated TLS, without sharing credentials.
- **Single binary.** It is one Go executable with no runtime dependencies.

See [how it works](docs/how-it-works.md) for the request flow, authorization,
and cache storage in detail.

## Quick start

These steps run OCI Accelerator on Linux and point Docker Engine at it,
caching Docker Hub, GitHub Container Registry, and Amazon ECR Public.

1. Create a new directory, then install OCI Accelerator into it. This needs
   Go 1.21 or later, which downloads the Go version the project requires:
   ```sh
   mkdir oci-accelerator && cd oci-accelerator
   GOBIN=$PWD go install github.com/buildkite/oci-accelerator/cmd/oci-accelerator@v0.5.0
   ```

   Without Go, download a prebuilt binary for your platform from
   [GitHub Releases](https://github.com/buildkite/oci-accelerator/releases)
   and extract it into the directory instead. See
   [download a release](docs/deployment.md#download-a-release) to verify it.

2. Create a starter configuration, cache directory, and root CA:
   ```sh
   ./oci-accelerator init
   ```

3. Start the proxy, which listens on `127.0.0.1:5128`, and leave it running:
   ```sh
   ./oci-accelerator --env-file .env
   ```

4. In another terminal, in the same directory, configure Docker to trust the
   proxy's CA for each registry:

   ```sh
   for registry in registry-1.docker.io ghcr.io public.ecr.aws; do
     sudo install -D -m 0644 data/ca/ca.crt "/etc/docker/certs.d/$registry/ca.crt"
   done
   ```

   Only Docker trusts the CA, and only for these registries' connections. See
   [Trusting a private CA](#trusting-a-private-ca) for why this is safe.

   <details>
   <summary>How to stop trusting the CA</summary>

   ```sh
   for registry in registry-1.docker.io ghcr.io public.ecr.aws; do
     sudo rm "/etc/docker/certs.d/$registry/ca.crt"
     sudo rmdir "/etc/docker/certs.d/$registry"
   done
   ```

   </details><br>

5. Configure Docker to use the proxy in `/etc/docker/daemon.json`, keeping any
   existing settings:

   ```json
   {
     "proxies": {
       "https-proxy": "http://127.0.0.1:5128"
     }
   }
   ```

   Then restart Docker:

   ```sh
   sudo systemctl restart docker
   ```

6. Test the cache by pulling an image, removing it, and pulling it again. The
   second pull comes from the proxy's cache:

   ```sh
   docker pull alpine
   docker image rm alpine
   docker pull alpine
   ```

To cache other registries, add them to `OCI_ACCELERATOR_REGISTRIES` and trust
the CA for each one. See the [deployment guide](docs/deployment.md) to run OCI
Accelerator as a service or container, or for remote Docker hosts.

## Commands reference

```sh
# Start the proxy, reading configuration from an optional dotenv file and the environment
oci-accelerator [--env-file FILE]

# Validate the configuration without starting the proxy
oci-accelerator [--env-file FILE] --check

# Create a starter .env, the data/ directories, and a root CA in data/ca/
oci-accelerator init

# Write only the starter configuration, to .env by default
oci-accelerator init-env [--output FILE]

# Create a root CA in DIR, or validate an existing one
oci-accelerator init-ca --dir DIR

# Print the version and exit
oci-accelerator --version

# Print usage. Each init command also accepts --help
oci-accelerator --help
```

## Configuration

OCI Accelerator reads `OCI_ACCELERATOR_*` environment variables, optionally
loaded from a dotenv file with `--env-file`. Environment variables override
values in the file. These are the settings you're most likely to change:

```sh
# Registries to intercept and cache, as host or host:port (required)
OCI_ACCELERATOR_REGISTRIES=registry-1.docker.io,ghcr.io,public.ecr.aws

# Private, persistent directory for the root CA (required)
OCI_ACCELERATOR_CA_DIR=./data/ca

# Private, persistent directory for cached content, separate from the CA (required)
OCI_ACCELERATOR_CACHE_DIR=./data/cache

# Address Docker connects to as its HTTPS proxy (default 127.0.0.1:5128)
OCI_ACCELERATOR_LISTEN=127.0.0.1:5128

# Maximum cache size in bytes (default 32 GiB)
OCI_ACCELERATOR_CACHE_MAX_BYTES=34359738368
```

See the [settings reference](docs/settings.md) for every setting, including
peering, metrics, and cache and authorization limits.

## Metrics

Metrics are disabled by default. To enable them, set a listen address for a
separate metrics listener:

```sh
OCI_ACCELERATOR_METRICS_LISTEN=127.0.0.1:9090
```

The listener serves three endpoints:

| Endpoint | Purpose |
| --- | --- |
| `GET /metrics` | Prometheus metrics |
| `GET /livez` | Liveness: the process is responsive |
| `GET /readyz` | Readiness: initialized, serving, and not stopping |

The endpoints have no authentication, so the address must be loopback unless you
also set `OCI_ACCELERATOR_METRICS_ALLOW_REMOTE=true`. Only expose them on a
private network.

See the [metrics reference](docs/metrics.md) for every metric and example cache
hit ratio queries, and [metrics](docs/deployment.md#metrics) in the deployment
guide for the listener and its health endpoints.

## Cached endpoints

OCI Accelerator intercepts every request to the registries in
`OCI_ACCELERATOR_REGISTRIES` and caches the OCI Distribution API reads listed
below. Traffic to any other host passes through without being decrypted.

### Immutable endpoints

| Endpoint | Purpose |
| --- | --- |
| `GET /v2/<name>/blobs/sha256:<digest>` | Layer or image config |
| `GET /v2/<name>/manifests/sha256:<digest>` | Manifest or image index, by digest |

These endpoints address content by digest, so the response never changes. They
are cached on disk, verified against their digest, and kept until evicted to
stay within the cache's size limits.

### Mutable endpoints

| Endpoint | Purpose |
| --- | --- |
| `GET`, `HEAD /v2/<name>/manifests/<tag>` | Manifest by tag |
| `GET /v2/<name>/tags/list` | Repository's tags |
| `GET /v2/<name>/referrers/<digest>` | Referrers, including filter variants |
| `GET /v2/_catalog` | Registry catalog |

The responses of these endpoints can change upstream at any time. They are
cached in memory only, for 30 seconds by default. Set
`OCI_ACCELERATOR_CACHE_MUTABLE_TTL` to change this, or `0s` to disable it.

### Uncached endpoints

| Endpoint | Purpose |
| --- | --- |
| `GET /v2/` | API version and authentication check |
| `PUT`, `PATCH`, `POST`, `DELETE` | Pushes and deletions |

These endpoints are forwarded upstream and never cached.

Some requests to cached endpoints, such as digest HEAD requests and ranges, are
handled differently. See [what gets
cached](docs/how-it-works.md#what-gets-cached) for the exceptions.

## Trusting a private CA

To cache a registry's HTTPS traffic, OCI Accelerator decrypts it with a
certificate for the registry's host, signed by its own private root CA, so
every Docker client needs to trust the accelerator's CA to pull through it.

### Is it safe?

Trusting a private CA is usually risky, because whoever holds its key can
impersonate any website to the machines that trust it. OCI Accelerator limits
that risk in two ways:

- **Only Docker trusts the CA, and only for your registries.** You install the
  certificate under `/etc/docker/certs.d/<registry>/`, not in the system trust
  store. Browsers, package managers, and other tools on the host never trust it,
  and Docker trusts it only for the registries you list.
- **The proxy never trusts its own CA upstream.** OCI Accelerator verifies every
  upstream registry against its host's system trust store, never its own CA.
  This protects the proxy's upstream connection. It doesn't protect Docker
  from someone using a stolen CA key to impersonate a registry if Docker
  bypasses the proxy and makes a direct connection.

This relies on the connection between Docker and the proxy being trusted, such
as loopback or a private network, and on the CA not being added to the system
trust store of the proxy's host. Keep `ca.key` private and never copy it to
Docker hosts.

## Peering

When you run several instances, they can share their caches. On a cache miss,
an instance first asks a peer that already has the content, and only downloads
it from the upstream registry if no peer can serve it. The instance still gets
the upstream registry's approval before fetching from a peer, so peering never
bypasses registry authorization.

Peering is disabled by default. See
[peer discovery](docs/deployment.md#peer-discovery) in the deployment guide to
set it up and secure it.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) to build, test, and contribute changes.

## License

This project is licensed under the [MIT license](LICENSE).
