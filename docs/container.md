# Container image

The container runs as UID/GID `10001`, includes upstream TLS trust roots, and
accepts the same arguments as the [command](../cmd/oci-accelerator/README.md).
It does not contain configuration or credentials. Pull and push clients use the
same proxy and CA configuration; see the [operating guide](operations.md) and the
proxy package's [write contract](../internal/proxy/README.md#write-passthrough).

Build and smoke-test a local image with Docker Buildx:

```sh
docker buildx build --load -t oci-accelerator:dev .
bash integration/container_smoke.sh oci-accelerator:dev
```

The Dockerfile cross-compiles Linux amd64 and arm64 without requiring emulation.
Both base images are digest-pinned; upstream trust roots come from the pinned
build image. Update these digests regularly for security and trust-store updates.
A multi-platform build is:

```sh
docker buildx build --platform linux/amd64,linux/arm64 \
  --output type=oci,dest=/tmp/oci-accelerator.tar .
```

Running the smoke test on arm64 still requires an arm64 host or QEMU; a successful
cross-build is not runtime verification for that architecture.

See the [release guide](releases.md) for manually publishing the image and OCI
Helm chart to GHCR. Building locally does not publish a release.

Mount a configuration file read-only and pass `--config /path/to/config.yaml`.
Bind `listen` to `0.0.0.0:3128` inside the container. Expose it only to trusted
clients; there is no proxy-user authentication. See the
[operating guide](operations.md) for Docker daemon proxy and CA trust setup.

## Writable state

`/var/lib/oci-accelerator` is prepared for UID `10001`. A mounted volume may hide
that ownership: provision its root as `10001:10001`, mode `0700`, before starting
the process. Configure CA and cache as separate child directories; never put the
CA inside the cache. The process creates those children privately. Persist both
for a standalone container. Losing CA identity breaks existing client trust.

The container supports a read-only root filesystem and dropping all capabilities.
It needs no writable `/tmp` for normal serving. Send SIGTERM and allow longer than
`shutdown_timeout` before forcing termination. One process owns each cache volume;
do not mount one cache into several running replicas.

The smoke test checks UID, upstream trust roots, operation with a read-only root
filesystem, graceful shutdown, and CA persistence across restart. It is not a
registry compatibility or cache-hit test; those are covered by the
[acceptance checks](operations.md#repeatable-checks).
