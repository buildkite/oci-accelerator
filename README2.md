# OCI Accelerator

OCI Accelerator is a specialised pull-through caching proxy for Docker and OCI
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
- **Private images stay private.** The upstream registry must approve each
  client before it receives cached content, so the cache never serves an image
  to someone who couldn't pull it directly.
- **No shared credentials.** Each client authenticates with its own credentials,
  and the proxy never stores or forwards them to other hosts.
- **Plugs in as an HTTPS proxy.** Docker Engine sends registry traffic through
  it using its standard proxy setting, once you trust the proxy's root CA.
- **Optional peer sharing.** Instances can share cached content with each other
  over mutually authenticated TLS, without sharing credentials.
- **Single binary.** It is one Go executable with no runtime dependencies.

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

## Quick start

These steps run OCI Accelerator on a Linux laptop and point its Docker Engine at
it, caching Docker Hub, GitHub Container Registry, and Amazon ECR Public.

1. Create a new directory, then download and extract the release into it:
   ```sh
   mkdir oci-accelerator && cd oci-accelerator
   curl -fL https://github.com/buildkite/oci-accelerator/releases/download/v0.3.2/oci-accelerator-linux-amd64-v0.3.2.tar.gz | tar -xz
   ```

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

   Only Docker trusts the CA, and only for these registries. See
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

See [`.env.example`](./.env.example) for every setting, including peering,
metrics, and cache and authorization limits.

## Metrics

Metrics are disabled by default. To enable them, set a listen address for a
separate metrics listener:

```sh
OCI_ACCELERATOR_METRICS_LISTEN=127.0.0.1:9090
```

The listener serves three endpoints:

| Endpoint       | Purpose                                           |
| -------------- | ------------------------------------------------- |
| `GET /metrics` | Prometheus metrics                                |
| `GET /livez`   | Liveness: the process is responsive               |
| `GET /readyz`  | Readiness: initialized, serving, and not stopping |

The endpoints have no authentication, so the address must be loopback unless you
also set `OCI_ACCELERATOR_METRICS_ALLOW_REMOTE=true`. Only expose them on a
private network.

Some of the most useful metrics:

| Metric | Type | Description |
| ------ | ---- | ----------- |
| `oci_accelerator_served_bytes_total{source}` | counter | Image content bytes sent to clients, by `cache`, `origin`, or `peer` |
| `oci_accelerator_cache_total{outcome}` | counter | Cache lookups, by `hit`, `wait_hit`, `miss`, or `bypass` |
| `oci_accelerator_upstream_body_bytes_total` | counter | Response body bytes read from upstream registries |
| `oci_accelerator_cache_bytes` | gauge | Bytes stored in the cache |
| `oci_accelerator_cache_evictions_total` | counter | Entries evicted to stay within limits |
| `oci_accelerator_request_duration_seconds` | histogram | Request duration |

See the [metrics reference](docs/metrics.md) for every metric, and
[metrics](docs/deployment.md#metrics) in the deployment guide for how to
interpret them, including example cache hit ratio queries.

## Cached endpoints

OCI Accelerator intercepts every request to the registries in
`OCI_ACCELERATOR_REGISTRIES` and caches the OCI Distribution API reads listed
below. Traffic to any other host passes through without being decrypted.

### Immutable endpoints

| Endpoint                                   | Purpose                            |
| ------------------------------------------ | ---------------------------------- |
| `GET /v2/<name>/blobs/sha256:<digest>`     | Layer or image config              |
| `GET /v2/<name>/manifests/sha256:<digest>` | Manifest or image index, by digest |

These endpoints address content by digest, so the response never changes. They
are cached on disk, verified against their digest, and kept until evicted to
stay within the cache's size limits.

### Mutable endpoints

| Endpoint                                  | Purpose                              |
| ----------------------------------------- | ------------------------------------ |
| `GET`, `HEAD /v2/<name>/manifests/<tag>`  | Manifest by tag                      |
| `GET /v2/<name>/tags/list`                | Repository's tags                    |
| `GET /v2/<name>/referrers/<digest>`       | Referrers, including filter variants |
| `GET /v2/_catalog`                        | Registry catalog                     |

The responses of these endpoints can change upstream at any time. They are
cached in memory only, for 30 seconds by default. Set
`OCI_ACCELERATOR_CACHE_MUTABLE_TTL` to change this, or `0s` to disable it.

### Uncached endpoints

| Endpoint                         | Purpose                              |
| -------------------------------- | ------------------------------------ |
| `GET /v2/`                       | API version and authentication check |
| `PUT`, `PATCH`, `POST`, `DELETE` | Pushes and deletions                 |

These endpoints are forwarded upstream and never cached.

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
  Even if the CA key leaked, a certificate signed with it could not impersonate
  an upstream registry to the proxy, so Docker hosts that pull through the
  proxy still reach the real registry.

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

Peering is disabled by default. To enable it, give every instance the same
`ca.crt` and `ca.key`, then add these settings to each instance's configuration:

```sh
# Address for the peer listener, separate from the proxy and metrics listeners
OCI_ACCELERATOR_PEER_LISTEN=0.0.0.0:5130

# This instance's own address, as other instances reach it
OCI_ACCELERATOR_PEER_ADVERTISE=accelerator-a.example:5130

# Other instances to contact at startup (optional)
OCI_ACCELERATOR_PEER_SEEDS=accelerator-b.example:5130,accelerator-c.example:5130

# Private, persistent directory for this instance's peer identity and address book
OCI_ACCELERATOR_PEER_STATE_DIR=./data/peers
```

Instances discover each other through their seeds and the peers those seeds
know about, so each one needs only one reachable seed to join.

Peers authenticate each other with mutual TLS using certificates signed by the
shared CA. Any holder of the CA key can join the fleet and read its cached
content, so restrict network access to the peer port and protect `ca.key` on
every instance. Peers never share registry credentials or approvals.

See [peer discovery](docs/deployment.md#peer-discovery) in the deployment guide
for container setups, trust, and retention.
