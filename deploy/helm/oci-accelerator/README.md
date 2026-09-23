# OCI Accelerator Helm chart

Installs one pull-only OCI proxy for Buildkite DinD workloads in an EKS cluster. The default release name is `oci-accelerator`.

```sh
helm upgrade --install oci-accelerator ./deploy/helm/oci-accelerator \
  --namespace oci-accelerator --create-namespace \
  --set-string image.digest="$IMAGE_DIGEST" --wait --wait-for-jobs --timeout 10m
```

Use the Service at `oci-accelerator.oci-accelerator.svc:3128`. Install `oci-accelerator-trust` into DinD clients as a trusted CA. Add exact private ECR registry hosts to `registries`; this chart is for pull traffic and does not support pushes.

## Important values

| Value | Default | Purpose |
| --- | --- | --- |
| `image.repository` | `ghcr.io/buildkite/oci-accelerator` | Image repository |
| `image.digest` / `image.tag` | empty | Required image version; digest takes precedence |
| `buildNamespace` | `buildkite` | Only namespace admitted to proxy port 3128 |
| `registries` | Docker Hub, GHCR, public ECR | Intercepted registries |
| `cache.size` / `cache.storageClassName` | `50Gi` / `gp3` | Cache PVC |
| `cache.existingClaim` | empty | Use an existing RWO cache claim instead |
| `resources` | 250m/256Mi requests, 2 CPU/1Gi limits | Proxy resources |
| `metrics.enabled` | `false` | Expose port 9090 to `metrics.namespace` |
| `metrics.serviceMonitor.enabled` | `false` | Create a ServiceMonitor (requires its CRD and metrics) |

The bootstrap Job creates immutable `oci-accelerator-ca` and `oci-accelerator-trust` resources without Helm ownership. Back up the CA Secret: uninstall intentionally leaves it, its public trust ConfigMap, and the cache PVC behind. Delete retained resources manually only when deliberately discarding the proxy identity and cache.

The ingress policy trusts every pod in `buildNamespace`; it is not an egress firewall and does not restrict proxy destinations. Keep that namespace controlled and configure DinD clients with only the public trust ConfigMap, never the CA Secret.

Set `IMAGE_DIGEST` to the published release's `sha256:…` digest before installing.
The init container requires UID 0 and `CHOWN`, `FOWNER`, `DAC_OVERRIDE` to prepare
private volume paths, including on retries. The serving process is non-root with
all capabilities dropped. Do not add a serving-Pod `fsGroup`: it would broaden
cache permissions on replacement. See the [Kubernetes guide](../../../docs/kubernetes.md).

For OCI chart installation and manual image/chart publication to GHCR, see the
[release guide](../../../docs/releases.md). No image or chart is published merely
by merging the deployment package.
