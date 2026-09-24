# Image and chart releases

The [manual release workflow](../.github/workflows/release.yml) publishes:

- `ghcr.io/buildkite/oci-accelerator:X.Y.Z`, a Linux amd64/arm64 image index.
- `oci://ghcr.io/buildkite/charts/oci-accelerator`, an OCI Helm chart at `X.Y.Z`.

The chart still requires an explicit image tag or digest; prefer the published
image index digest so amd64 and arm64 nodes select the correct platform. Plain
manifests are maintained in the source repository and use the same image digest.
An example chart version in source is not evidence that a release exists.

## Maintainer prerequisites

Publishing changes shared package state and needs release approval. Merging this
workflow does not publish anything: it has only a manual dispatch trigger, accepts
stable `X.Y.Z` versions, and runs only from this repository's `main` branch. It
checks out the existing `vX.Y.Z` tag; it does not create or move tags, GitHub
releases, or deployments. Restrict creation and movement of release tags.

Before enabling publication:

1. Configure the GitHub `release` environment with required reviewers and restrict
   deployment branches to `main`. Merely naming an environment in YAML does not
   enable approval protection; repository administrators must configure it.
2. Allow the workflow's `GITHUB_TOKEN` to write both GHCR packages. Existing
   packages may need their Actions access granted to this repository. Use the
   short-lived token, not a checked-in or broadly scoped personal token.
3. After the first authorized publication, make **both** GHCR packages public in
   package settings. GHCR packages can initially be private. Confirm anonymous
   image and chart pulls; do not advertise public installation before that check.

GitHub Actions is used only for manually authorized GHCR publication. Routine
change validation remains in [Buildkite](../.buildkite/README.md).

## Validate and publish

Run the Go, deployment-package, container, and
[fresh-DinD Kubernetes checks](kubernetes.md#deployment-checks) against the exact
candidate commit. The publisher repeats Go checks, chart lint/package tests and
the native amd64 container smoke test. Cross-compilation does not establish arm64
runtime correctness: run the container smoke test on a native arm64 host before
claiming that platform validated. Check target EKS storage, CNI policy enforcement,
private ECR credentials, and the custom Buildkite image separately.

To package a chart locally without publishing:

```sh
VERSION=0.1.0 # choose the intended new version
helm lint deploy/helm/oci-accelerator --set-string image.tag="$VERSION"
helm package deploy/helm/oci-accelerator --version "$VERSION" \
  --app-version "$VERSION" --destination /tmp
```

After release approval, create the reviewed `vX.Y.Z` tag through your normal
release process. Choose **Publish image and Helm chart → Run workflow**, select
`main`, enter `X.Y.Z`, and approve the `release` environment. Use a new version:
**do not reuse an already published tag**. The workflow serializes publication but
GHCR tags are mutable; it is not an immutable-version registry policy.

The image publishes before the chart; this is not an atomic operation. If chart
publication fails after the image succeeds, record and preserve the image digest,
inspect which artifacts exist, and finish only the missing chart publication with
the same tagged source and version. Do not blindly rerun the workflow, overwrite
an existing version, or delete artifacts clients may already use.

## Install a published release

Set `VERSION` to an existing chart version and `IMAGE_DIGEST` to the release's
multi-platform image index digest (including the `sha256:` prefix). First create
the target namespace and complete the Kubernetes guide's
[one-time CA provisioning](kubernetes.md#one-time-ca-provisioning) with an explicit context,
namespace, release name, and
`IMAGE=ghcr.io/buildkite/oci-accelerator@$IMAGE_DIGEST`. Then install:

```sh
helm upgrade --install oci-accelerator oci://ghcr.io/buildkite/charts/oci-accelerator \
  --version "$VERSION" --kube-context production-eks --namespace oci-accelerator \
  --set-string image.digest="$IMAGE_DIGEST" --wait --timeout 10m
```

This command writes cluster resources; run it only against the intended cluster
with deployment approval. Follow the [Kubernetes guide](kubernetes.md) for EBS
prerequisites, public trust distribution, private-registry configuration and
DinD opt-in. Never put signing keys or registry credentials in the
published image, chart, or plain manifests.
