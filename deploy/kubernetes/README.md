# Kubernetes manifests

These plain manifests deploy a single pull-only OCI Accelerator for same-cluster
Buildkite agents on EKS/EC2. See the authoritative [Kubernetes guide](../../docs/kubernetes.md)
for trust and agent configuration.

Replace every `ghcr.io/buildkite/oci-accelerator:REPLACE_ME` image reference in
the Job and Deployment with the same published release digest, then install in order:

```sh
kubectl apply -f deploy/kubernetes/00-namespace.yaml
kubectl apply -f deploy/kubernetes/10-config.yaml
kubectl apply -f deploy/kubernetes/20-bootstrap-rbac.yaml
kubectl apply -f deploy/kubernetes/21-bootstrap-job.yaml
kubectl wait -n oci-accelerator --for=condition=complete job/oci-accelerator-bootstrap --timeout=5m
kubectl apply -f deploy/kubernetes/30-cache-pvc.yaml
kubectl apply -f deploy/kubernetes/42-network-policy.yaml
kubectl apply -f deploy/kubernetes/40-deployment.yaml
kubectl apply -f deploy/kubernetes/41-service.yaml
```

Add exact private ECR hosts to `registries` before applying the ConfigMap. The
PVC expects an EBS CSI `gp3` StorageClass; `examples/gp3-storage-class.yaml` is
an optional cluster-scoped example and is not part of the commands above.

The bootstrap Job creates the immutable `oci-accelerator-ca` Secret and
`oci-accelerator-trust` ConfigMap. To rerun it after TTL cleanup, apply the Job
manifest again. Preserve the CA Secret and cache PVC when replacing or removing
the serving workload. Loss of both CA resources creates a new trust identity on
the next bootstrap.

The init container intentionally runs as root with `CHOWN`, `FOWNER`, and
`DAC_OVERRIDE` to prepare mounted paths, including after an init retry; the proxy
runs as UID/GID 10001. Do not add `fsGroup` to the serving Pod: recursive volume
permission changes would violate the cache's private-file checks. The ingress
policy is effective only with a NetworkPolicy-enforcing CNI and admits port 3128
only from the `buildkite` namespace. It does not restrict egress. Metrics remain
loopback-only and are not exposed by the Service.
