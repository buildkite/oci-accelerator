# Buildkite CI

The [Buildkite pipeline](https://buildkite.com/buildkite/oci-accelerator) uploads
[`pipeline.yml`](pipeline.yml) using a
`buildkite-agent pipeline upload` bootstrap. Bootstrap settings are managed in
`buildkite/saas-iac`; this repository owns the uploaded job definitions.

Four independent checks run on the Open Source cluster's Linux `hosted` queue:

- Check Go formatting (without rewriting files), run `go vet ./...`, build the
  binary, and validate `config.example.yaml` with `--check`. This temporary build
  omits VCS stamping so container/checkout ownership differences do not require
  weakening Git's safe-directory checks.
- Run `go test -count=1 -timeout=10m ./...`.
- Run `go test -race -count=1 -timeout=10m ./...`.
- Build the Dockerfile for `linux/amd64` and `linux/arm64`, exporting a temporary
  OCI archive without registry credentials or a push.

The Go jobs use the Docker plugin and `golang:1.27.1-bookworm`, which supplies Go and
the C compiler required for race tests. `GOTOOLCHAIN=local` prevents an implicit
toolchain upgrade; update the image tags alongside `go.mod` and `.agents/setup`.
Agents need Docker, but no Amp installation, registry credentials, privileged
containers, or mounted Docker socket inside the test container. Job environments
are not broadly forwarded into containers. Test results are not reused from Go's
test cache. Jobs have explicit timeouts and failures are not automatically retried.

The image check runs [`image.sh check`](image.sh) directly on the agent with Docker
Buildx. Its disposable `docker-container` builder uses official BuildKit's bundled
QEMU for the Alpine ARM64 runtime stage; Go itself cross-compiles natively. This
builder needs permission to start BuildKit containers on the hosted agent. It does
not install host emulators or share the agent's Docker credentials. Temporary
builders, archives, and Docker configuration are removed when the script exits.

## Docker Hub releases

After all four checks pass, a non-PR tag build such as `v1.2.3` automatically runs
[`image.sh publish`](image.sh). Only stable `vMAJOR.MINOR.PATCH` tags without leading
zeroes qualify; prereleases and branch builds never publish. The script fetches the
tag and complete `origin/main` history, verifies the tag matches both the checkout
and `BUILDKITE_COMMIT`, and requires that commit to be an ancestor of `origin/main`.

It logs in as `buildkite` using the organization access token from Buildkite secret
`OCI_ACCELERATOR_DOCKER_HUB_TOKEN`, passed directly to `docker login --password-stdin`.
The secret is retrieved only after the Git checks and is not passed into build
containers. The step builds and pushes one multi-platform image to
`buildkite/oci-accelerator:1.2.3` and `buildkite/oci-accelerator:latest`.

Publishing is serialized with a concurrency group. An existing version tag is
rejected; authentication and registry lookup failures also stop publication.
Configure Docker Hub immutable version tags as an additional guard against other
writers. `latest` remains mutable and follows the most recently published release,
not necessarily the highest semantic version. Registry updates to the version and
`latest` tags are not transactional: a partial push requires operator inspection,
and a retry refuses to overwrite a version already published.

Before enabling releases, configure these external prerequisites:

1. In `buildkite/saas-iac`, add `build_tags = true` to
   `module "pipeline_oci_accelerator"` in `public-tf/pipelines.tf`, and apply it
   through that repository's infrastructure workflow. Repository YAML cannot
   enable GitHub tag webhooks; do not override the Terraform-managed bootstrap.
2. Create the Docker Hub repository and grant the organization token read/write
   access only to `buildkite/oci-accelerator`. Store it as the named Buildkite secret
   in the Open Source cluster. Never put the token in this repository.
3. Restrict secret access outside repository-controlled YAML. For this pipeline,
   an access policy can require the pipeline ID, webhook source, and release ref:

   ```yaml
   - pipeline_id: "01a07f28-efda-4ad5-a6cd-383fd12df30c"
     build_source: "webhook"
     build_branch: "v*"
   ```

   GitHub tag webhooks set both `BUILDKITE_BRANCH` and `BUILDKITE_TAG` to the tag
   name. This policy does not distinguish a tag from a similarly named branch or
   prove main ancestry. Protect creation and modification of **both** `v*` tags
   and `v*` branches in GitHub, permitting only trusted release maintainers.
   Untrusted PR/branch code must not be able to satisfy the secret policy by
   choosing a release-like branch name. Script checks and step conditions are
   defense in depth, not a credential boundary against modified repository code.

## Local verification

Docker, public-registry, fault, and load acceptance are manual opt-in checks:
their test-owned process harness runs on Linux with the documented Docker and
namespace privileges, without Amp. CI runs the default local-fixture suite,
**not** those acceptance matrices. Acceptance jobs and merge gates are deliberately
absent; manual execution remains necessary for that coverage.
See the [operating guide](../docs/operations.md#repeatable-checks)
for that additional coverage.

To reproduce CI locally with Go 1.27.1 and a C compiler, run from the repo root:

```sh
GOTOOLCHAIN=local CGO_ENABLED=1 bash .buildkite/check.sh
GOTOOLCHAIN=local CGO_ENABLED=1 go test -count=1 -timeout=10m ./...
GOTOOLCHAIN=local CGO_ENABLED=1 go test -race -count=1 -timeout=10m ./...
```

The default Go suite exercises the publishing guards against local Git histories
and fake Docker/secret commands; it never retrieves credentials or publishes images.
To run the real multi-platform build without publishing (Docker and Buildx required):

```sh
bash .buildkite/image.sh check
```

With `buildkite-agent` installed, parse the pipeline without uploading any jobs.
Agent v4 requires a token even for a local dry run; use this dummy value and an
unreachable local endpoint, not a real credential:

```sh
BUILDKITE_AGENT_ACCESS_TOKEN=local-dry-run-only \
BUILDKITE_AGENT_ENDPOINT=http://127.0.0.1:1 \
buildkite-agent pipeline upload --dry-run --reject-parse-warnings .buildkite/pipeline.yml
```
