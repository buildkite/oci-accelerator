# Kubernetes manifests

These plain manifests deploy a single OCI pull cache with push passthrough for same-cluster
Buildkite agents on EKS/EC2. See the authoritative [Kubernetes guide](../../docs/kubernetes.md)
for trust and agent configuration.

First set an explicit kubectl context and create the namespace:

```sh
CONTEXT=production-eks
kubectl --context "$CONTEXT" apply -f deploy/kubernetes/00-namespace.yaml
```

Complete the Kubernetes guide's
[one-time CA provisioning](../../docs/kubernetes.md#one-time-ca-provisioning) using that
context and the plain resource names `oci-accelerator-ca` and
`oci-accelerator-trust`. Replace the
`ghcr.io/buildkite/oci-accelerator:REPLACE_ME` Deployment image with the same
published release digest, then install the serving resources:

```sh
kubectl --context "$CONTEXT" apply -f deploy/kubernetes/10-config.yaml
kubectl --context "$CONTEXT" apply -f deploy/kubernetes/30-cache-pvc.yaml
kubectl --context "$CONTEXT" apply -f deploy/kubernetes/42-network-policy.yaml
kubectl --context "$CONTEXT" apply -f deploy/kubernetes/40-deployment.yaml
kubectl --context "$CONTEXT" apply -f deploy/kubernetes/41-service.yaml
```

Add exact private ECR hosts to `registries` before applying the ConfigMap. The
PVC expects an EBS CSI `gp3` StorageClass; `examples/gp3-storage-class.yaml` is
an optional cluster-scoped example and is not part of the commands above.

The CA Secret and public trust ConfigMap are operator-owned and are not changed
by install, upgrade, or uninstall. Preserve them and the original signing-key
backup when replacing or removing the serving workload; never generate a new CA
merely because either cluster resource is missing.

The init container intentionally runs as root with `CHOWN`, `FOWNER`, and
`DAC_OVERRIDE` to prepare mounted paths, including after an init retry; the proxy
runs as UID/GID 10001. Do not add `fsGroup` to the serving Pod: recursive volume
permission changes would violate the cache's private-file checks. The ingress
policy is effective only with a NetworkPolicy-enforcing CNI and admits port 3128
only from the `buildkite` namespace. It does not restrict egress. Metrics remain
loopback-only and are not exposed by the Service.
