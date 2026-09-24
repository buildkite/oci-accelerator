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
Provision the CA once before installing either package, using the same
built/published image digest as the serving container. The plain manifests
contain an intentional image placeholder; the chart requires an explicit image
tag or digest. Neither assumes an unpublished release exists.

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

## One-time CA provisioning

The operator, not Kubernetes or Helm, owns CA installation. Set every value
explicitly, use a persistent local parent directory outside the source tree, and
run this before installing the serving workload. For Helm, resource names are
`<release>-ca` and `<release>-trust`; the plain manifests use
`oci-accelerator-ca` and `oci-accelerator-trust` (so set `RELEASE` to
`oci-accelerator`). `IMAGE` must be the exact image digest that will serve:

```bash
set -euo pipefail
CONTEXT=production-eks
NAMESPACE=oci-accelerator
RELEASE=oci-accelerator
IMAGE=ghcr.io/buildkite/oci-accelerator@sha256:REPLACE_ME
CA_PARENT="$HOME/.local/share/oci-accelerator-production"

kubectl --context "$CONTEXT" get namespace "$NAMESPACE" >/dev/null
secret="$(kubectl --context "$CONTEXT" -n "$NAMESPACE" get secret \
  "$RELEASE-ca" --ignore-not-found -o name)"
trust="$(kubectl --context "$CONTEXT" -n "$NAMESPACE" get configmap \
  "$RELEASE-trust" --ignore-not-found -o name)"
if [[ -n "$secret" || -n "$trust" ]]; then
  printf 'CA resources already exist; reuse them or follow recovery below.\n' >&2
  exit 1
fi

install -d -m 0700 "$CA_PARENT"
docker run --rm --user "$(id -u):$(id -g)" \
  --mount "type=bind,src=$CA_PARENT,dst=/state" \
  "$IMAGE" init-ca --dir /state/ca

kubectl --context "$CONTEXT" -n "$NAMESPACE" create secret generic "$RELEASE-ca" \
  --from-file=ca.crt="$CA_PARENT/ca/ca.crt" \
  --from-file=ca.key="$CA_PARENT/ca/ca.key" --dry-run=client -o json |
  jq '.immutable=true' |
  kubectl --context "$CONTEXT" -n "$NAMESPACE" create -f -
kubectl --context "$CONTEXT" -n "$NAMESPACE" create configmap "$RELEASE-trust" \
  --from-file=ca.crt="$CA_PARENT/ca/ca.crt" --dry-run=client -o json |
  jq '.immutable=true' |
  kubectl --context "$CONTEXT" -n "$NAMESPACE" create -f -
```

For a new identity, leave the `ca` child directory absent: `init-ca` creates it
with private permissions. For recovery, restore the original pair there with
its private ownership and permissions; `init-ca` validates and reuses it rather
than generating a replacement. The initial lookups distinguish API failures from absent
objects and stop if either identity resource exists. The create-only pipelines
fail closed on any stage and deliberately do not apply or overwrite identity.
Never pass CA files through Helm values or commit them to YAML.

If Secret creation succeeds but ConfigMap creation fails, retain the original
files. Do not generate another CA or delete resources to bypass `AlreadyExists`.
Verify that the local public certificate matches the Secret, then run only the
ConfigMap create pipeline above:

```bash
openssl x509 -in "$CA_PARENT/ca/ca.crt" -outform DER | openssl dgst -sha256
kubectl --context "$CONTEXT" -n "$NAMESPACE" get secret "$RELEASE-ca" \
  -o jsonpath='{.data.ca\.crt}' | base64 -d |
  openssl x509 -outform DER | openssl dgst -sha256
```

The two hashes must match exactly. This compares only public certificates and
does not expose the signing key. If the original files are unavailable, stop
and restore them from backup rather than manufacturing mismatched trust.

If the Secret is lost but the trust ConfigMap survives, restore and validate the
original pair with `init-ca`, compare its public certificate with the ConfigMap,
and run only the Secret create pipeline. If both resources are lost, the full
procedure can reuse the restored pair. Never delete surviving trust to bypass
the preflight check or generate a new identity during recovery.

Back up the original `ca.crt` and `ca.key` securely and test restoration.
Immutability prevents in-place changes, not deletion. Helm and the plain
manifests do not own, reconcile, rotate, or remove these resources during
install, upgrade, or uninstall.

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

```bash
set -euo pipefail
kubectl --context "$CONTEXT" create namespace buildkite --dry-run=client -o yaml |
  kubectl --context "$CONTEXT" apply -f -
kubectl --context "$CONTEXT" get configmap "$RELEASE-trust" -n "$NAMESPACE" -o json |
  jq '{apiVersion:"v1",kind:"ConfigMap",metadata:{name:"oci-accelerator-trust",namespace:"buildkite"},data:{"ca.crt":.data["ca.crt"]}} + (if has("immutable") then {immutable:.immutable} else {} end)' |
  kubectl --context "$CONTEXT" apply -f -
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

Existing deployments retain their valid CA identity. There is no automatic
consistency reconciliation: before distributing trust, the operator must verify
that the Secret's public certificate matches the trust ConfigMap (using the
explicit `CONTEXT`, `NAMESPACE`, and `RELEASE` values from provisioning):

```bash
kubectl --context "$CONTEXT" -n "$NAMESPACE" get secret "$RELEASE-ca" \
  -o jsonpath='{.data.ca\.crt}' | base64 -d |
  openssl x509 -outform DER | openssl dgst -sha256
kubectl --context "$CONTEXT" -n "$NAMESPACE" get configmap "$RELEASE-trust" \
  -o jsonpath='{.data.ca\.crt}' |
  openssl x509 -outform DER | openssl dgst -sha256
```

Require equal hashes. A missing Secret blocks the serving Pod's volume mount;
an invalid certificate/key pair fails the CA loader rather than silently
replacing identity.

Helm config changes roll the serving pod via a checksum annotation. For plain
manifests, reapply the ConfigMap and explicitly restart the Deployment; the
application does not reload configuration. Verify readiness and a new DinD pull
after each upgrade. Cached files must survive replacement and retain private
permissions; CA fingerprints must remain unchanged.

Helm uninstall retains the cache PVC and operator-created CA/trust resources.
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

The test runs `init-ca` locally and provisions immutable CA resources before
installing the chart; no in-cluster provisioner is used. It creates and removes
uniquely named namespaces. It requires the host's
non-loopback IP to be reachable from pods and registry access to pull the DinD
image. It verifies TLS rejection without client trust, exact image digests,
upstream request and byte counts on cold/warm/replacement pulls, retained CA and
PVC cache state, and a fresh upstream 401 for an unauthenticated warmed blob.
Authorization TTL is explicitly zero in this test. DinD uses `vfs` without
container networking for portability; it verifies pulls, not container execution
or custom Buildkite startup. Public-registry compatibility is a separate opt-in
[Docker acceptance check](operations.md#repeatable-checks).
