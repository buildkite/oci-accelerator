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

### Copy public trust to the build namespace

ConfigMaps are namespace-scoped. Create the `buildkite` namespace, then copy only
the named public ConfigMap's `ca.crt` field. This transformation drops source
identity and server-managed metadata (`uid`, `resourceVersion`, timestamps,
managed fields and owner references) while preserving `immutable: true`:

```sh
kubectl create namespace buildkite --dry-run=client -o yaml | kubectl apply -f -
kubectl get configmap oci-accelerator-trust -n oci-accelerator -o json |
  jq '{apiVersion:"v1",kind:"ConfigMap",metadata:{name:"oci-accelerator-trust",namespace:"buildkite"},data:{"ca.crt":.data["ca.crt"]}} + (if has("immutable") then {immutable:.immutable} else {} end)' |
  kubectl apply -f -
```

Grant the operator only the namespace/read access needed for that public
ConfigMap. Never copy, mount, or grant build workloads access to the
`oci-accelerator-ca` Secret; it contains the CA signing key.

## Docker-in-Docker pull clients

[`dind-pull.yaml`](../deploy/kubernetes/examples/dind-pull.yaml) is a minimal,
standalone smoke Pod for the `buildkite` namespace. Apply it after copying trust:

```sh
kubectl apply -f deploy/kubernetes/examples/dind-pull.yaml
kubectl wait -n buildkite --for=condition=ready pod/dind-pull-smoke --timeout=2m
kubectl exec -n buildkite dind-pull-smoke -- docker pull alpine:3.22
```

The example has no API token, trusts only the public CA, and starts
`docker:28.5.1-dind` privileged with bounded resource starting points and
ephemeral `/var/lib/docker` and `/var/run`. It copies the projected certificate
to a regular `.crt` file (Alpine's trust updater does not reliably include
ConfigMap symlinks), then updates system trust before
executing `dockerd-entrypoint.sh dockerd` and exposes only the shared Unix socket
at `/var/run/docker.sock`; it neither disables TLS on an insecure port such as
2375 nor exposes a Docker TCP listener. Its daemon-only `HTTP_PROXY` and
`HTTPS_PROXY` point at the in-cluster accelerator. `NO_PROXY` contains only
loopback and cluster service suffixes--never add a registry hostname, because
that bypasses acceleration and its authorization path.

For custom Buildkite agents on EKS/EC2, use a production
custom image pinned by digest rather than the smoke image. Adapt the agent pod
template so the privileged daemon container owns the two ephemeral volumes and
the agent CLI container mounts the same `docker-run` volume, uses
`DOCKER_HOST=unix:///var/run/docker.sock`, and waits for `docker info` before
accepting work. Set the proxy variables on the daemon container, not globally on
the Buildkite agent container. Ensure the CLI user's group or socket ownership
permits access without making the socket universally writable. Preserve any
custom image entrypoint setup (agent hooks, credentials, certificates and user
initialization): append the CA update and daemon exec to that setup rather than
blindly replacing it. The readiness probe in the example is the minimum startup
gate; production scheduling must likewise prevent commands racing daemon startup.

This deployment is pull-only. Do not opt in a daemon that builds and pushes
images. Registry credentials remain the Docker client's responsibility. For a
private ECR registry, authenticate the client to its exact configured hostname:

```sh
aws ecr get-login-password --region us-east-1 |
  docker login --username AWS --password-stdin 123456789012.dkr.ecr.us-east-1.amazonaws.com
```

The default `cache.authorization_ttl` is `30s`: an exact successful immutable
GET may reuse its short-lived process-local authorization grant. Set it to `0s`
when every delivery must perform an upstream authorization GET. Neither setting
creates credentials or permits offline fallback.

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

Before opting in agents, test with fresh smoke Pods, not only repeated pulls in
one daemon:

1. Record the public CA fingerprint from the source and copied ConfigMaps and
   require an exact match:

   ```sh
   for namespace in oci-accelerator buildkite; do
     kubectl get configmap oci-accelerator-trust -n "$namespace" -o jsonpath='{.data.ca\.crt}' |
       openssl x509 -noout -fingerprint -sha256
   done
   ```

2. Create a fresh Pod and pull an image by immutable digest (cold). Delete the
   Pod, create its replacement, and repeat (warm). A second pull in the same Pod
   can come from Docker's ephemeral local `/var/lib/docker` and does not prove an
   accelerator hit.
3. Compare accelerator `oci_accelerator_cache_total` outcomes,
   `oci_accelerator_upstream_get_attempts_total`, and fill counters across the
   cold and replacement pulls. Enable the private metrics Service as described
   above; do not expose it publicly.
4. Attempt a private pull without login (or with deliberately invalid client
   credentials) and require denial even when the body is warm. Then log in with
   valid client credentials and require success. Do not use production
   credentials for the negative check.

These are operator acceptance procedures, not claims that this repository has
run the custom Buildkite image on EKS, verified a particular CNI's NetworkPolicy
enforcement, or runtime-tested public or private registries in your cluster.

`bash .buildkite/check-deployment.sh` installs checksum-pinned Helm, lints the
chart and tests defaults, opt-in metrics, application configuration, workload
security and parity with plain manifests. It needs no Kubernetes credentials.
These checks run in CI. `.agents/setup` also installs pinned Helm, kubectl and
kind for disposable cluster verification. A local kind volume is not an EBS test,
and a cluster without an enforcing CNI cannot verify NetworkPolicy isolation.

The opt-in Kubernetes acceptance test uses fresh privileged DinD pods, the real
Helm package, and the deterministic authenticated TLS registry fixture. Run it
only against a disposable kind cluster on a Linux amd64 host with Docker,
Buildx, kind, kubectl, Helm and Go installed:

```sh
kind create cluster --name oci-accelerator-test
docker buildx build --load -t oci-accelerator:dev .
kind load docker-image oci-accelerator:dev --name oci-accelerator-test
OCI_ACCELERATOR_KUBERNETES_E2E=1 \
OCI_ACCELERATOR_KUBE_CONTEXT=kind-oci-accelerator-test \
OCI_ACCELERATOR_KUBE_IMAGE=oci-accelerator:dev \
  go test ./integration -run '^TestKubernetesPullThroughProxy$' -count=1 -v -timeout=15m
kind delete cluster --name oci-accelerator-test
```

The test creates and removes uniquely named namespaces. It requires the host's
non-loopback IP to be reachable from pods and registry access to pull the DinD
image. It verifies TLS rejection without client trust, exact image digests,
upstream request and byte counts on cold/warm/replacement pulls, retained CA and
PVC cache state, and a fresh upstream 401 for an unauthenticated warmed blob.
Authorization TTL is explicitly zero in this test. DinD uses `vfs` without
container networking for portability; it verifies pulls, not container execution
or custom Buildkite startup. Public-registry compatibility is a separate opt-in
[Docker acceptance check](operations.md#repeatable-checks).
