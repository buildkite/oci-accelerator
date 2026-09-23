# Kubernetes deployment

The Kubernetes target is a single pull-only accelerator in the same EKS cluster
as custom Buildkite agents using Docker-in-Docker. It does not configure the
nodes' container runtime. **Do not opt build-and-push daemons into this deployment:**
pushes to intercepted registries are rejected. Proxy downtime can cause pulls to
fail; there is no automatic direct-to-registry fallback.

## Install on EKS

Use EC2 worker nodes, the EBS CSI driver, and a NetworkPolicy-enforcing CNI.
The `gp3` StorageClass must exist; an optional encrypted, expandable,
`WaitForFirstConsumer` example is in
[`deploy/kubernetes/examples`](../deploy/kubernetes/examples/gp3-storage-class.yaml).
Review cluster-scoped storage changes before applying them. EBS is zonal: this
deployment is not multi-AZ failover. It does not run on Fargate with this storage.

Choose either the [Helm chart](../deploy/helm/oci-accelerator/README.md) or
[plain manifests](../deploy/kubernetes/README.md), not both for the same instance.
Supply one built/published image digest to both bootstrap and serving containers.
The plain manifests contain an intentional image placeholder; the chart requires
an explicit image tag or digest. Neither assumes an unpublished release exists.

Defaults are one replica, a private ClusterIP Service, a 50 GiB PVC and a 32 GiB
cache. CPU/memory requests are 250m/256Mi with limits of 2 CPU/1Gi; these are
starting points, not tested throughput guarantees. Keep filesystem and metadata
headroom above the cache limit. Never scale this Deployment against one cache
volume. Recreate updates intentionally cause temporary pull failures.

Add every exact private ECR hostname, such as
`123456789012.dkr.ecr.ap-southeast-2.amazonaws.com`, to `registries`; wildcards are
not supported. Docker Hub uses `registry-1.docker.io`, not `docker.io`. GHCR and
`public.ecr.aws` are included. Registry credentials and ECR token refresh remain
the Docker client's responsibility, not the accelerator's.

The serving container runs as UID/GID 10001 with no capabilities, a read-only
root filesystem, and no service-account token. Its init container needs root
with CHOWN, FOWNER and DAC_OVERRIDE to prepare private volumes and retry safely;
strict Pod Security Restricted admission therefore needs a reviewed exception
for this workload. Do not set `fsGroup` on serving pods: kubelet can otherwise
broaden existing cache permissions, which the application correctly rejects.

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

## Probes, metrics and network access

Startup and readiness exec probes call the local `/readyz`; liveness calls
`/livez`. Probe traffic is outside the proxy connection pool and never depends on
registry reachability or cache capacity. Startup permits five minutes of cache
recovery; adjust the manifests if a measured large-cache scan needs longer.
Termination allows 45 seconds around the configured 15-second proxy drain.

The default metrics/probe listener is loopback-only. In Helm, `metrics.enabled`
explicitly enables remote binding, a Service port and an ingress rule for
`metrics.namespace`. `metrics.serviceMonitor.enabled` additionally creates a
ServiceMonitor and requires the operator's CRD. Its labels/selectors may need
to match your Prometheus installation. No collector is installed by this chart.
With plain manifests, change `metrics_listen` to `0.0.0.0:9090`, add
`metrics_allow_remote: true`, and add the corresponding port and a monitoring-only
NetworkPolicy rule together. Never expose metrics publicly.

The proxy ingress rule admits the designated build namespace (`buildkite` by
default), not the entire cluster. Verify enforcement using both an allowed and
a denied namespace before opting in any jobs. ClusterIP alone is not isolation.
The policy does not restrict egress or opaque CONNECT destinations; all admitted
build pods must be trusted to use that network access. An enforcing CNI and its
startup enforcement mode are cluster prerequisites, not installed by this package.

## Upgrades and recovery

Helm runs a revision-named bootstrap Job on install/upgrade; it verifies retained
identity rather than rotating it. Use `--wait --wait-for-jobs` and inspect failed
Jobs before proceeding. The plain bootstrap Job has a fixed name: after editing
its image, delete only that Job and reapply it (or wait for completed-Job TTL
cleanup). Do not delete its Secret or trust ConfigMap.

Helm config changes roll the serving pod via a checksum annotation. For plain
manifests, reapply the ConfigMap and explicitly restart the Deployment; the
application does not reload configuration. Verify readiness and a new DinD pull
after each upgrade. Cached files must survive replacement and retain private
permissions; CA fingerprints must remain unchanged.

Helm uninstall retains the cache PVC and bootstrap-created CA/trust resources.
When reinstalling, `cache.existingClaim` can reuse the retained PVC. For plain
manifests, remove only the serving Deployment, Service and associated policy when
stopping the proxy. **Do not delete the namespace**: that also deletes the CA
Secret. If discarding a broken cache, stop the proxy before replacing its PVC;
preserve the CA Secret and distribute no new trust unless deliberately rotating.

## Deployment checks

`bash .buildkite/check-deployment.sh` installs checksum-pinned Helm, lints the
chart and tests defaults, opt-in metrics, application configuration, workload
security and parity with plain manifests. It needs no Kubernetes credentials.
These checks run in CI. `.agents/setup` also installs pinned Helm, kubectl and
kind for disposable cluster verification. A local kind volume is not an EBS test,
and a cluster without an enforcing CNI cannot verify NetworkPolicy isolation.
