# Kubernetes deployment

The Kubernetes target is a single pull-only accelerator in the same EKS cluster
as custom Buildkite agents using Docker-in-Docker. It does not configure the
nodes' container runtime. **Do not opt build-and-push daemons into this deployment:**
pushes to intercepted registries are rejected. Proxy downtime can cause pulls to
fail; there is no automatic direct-to-registry fallback.

## CA bootstrap

Run the image as a separate Kubernetes Job with a mounted service account and
a writable temporary directory:

```sh
oci-accelerator bootstrap-ca --secret oci-accelerator-ca --configmap oci-accelerator-trust
```

The command generates the CA using oci-accelerator and stores it in an immutable
Secret. Only the public certificate goes in the immutable ConfigMap, which can
be distributed to DinD clients. The serving process does not need API access.
See the [bootstrap package](../internal/bootstrap/README.md) for exact permissions,
retry behavior and limits.

Preserve and securely back up the Secret independently of the disposable cache.
Do not rotate it on upgrades or remove it when uninstalling the serving workload.
If the Secret is lost but the public ConfigMap remains, bootstrap fails rather
than changing client trust. If both are lost, restoration requires the original
Secret or explicit new-CA distribution to every client.

Kubernetes Secret projections do not meet the proxy's filesystem ownership and
symlink requirements directly. Copy the two files into a private UID-10001-owned
directory before serving; do not relax the CA loader's checks. Build pods receive
only the public ConfigMap, never the private Secret. Protect the namespace with
RBAC and Secret encryption at rest; a Secret's base64 encoding is not encryption.
