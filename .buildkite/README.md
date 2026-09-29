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

After all four checks pass, a non-PR `main` branch build automatically runs
[`image.sh publish`](image.sh), publishing the same multi-platform amd64/arm64
image as both `buildkite/oci-accelerator:latest` and `build-<number>` using the
Buildkite build number (for example, `buildkite/oci-accelerator:build-108`).
`latest` is replaced on each publication. PRs, other branches, and Git-tag builds
never publish; Git-tag builds can remain disabled.

Publishing is serialized with a concurrency group. At the start of the publish
job, the script fetches `origin/main`, verifies the checkout matches
`BUILDKITE_COMMIT`, and skips publication if it is no longer the main tip. Combined
with serialization, this prevents an older queued or retried build from overwriting
an image published by a newer main build. Main can advance while a publish job is
running; the newer build publishes afterward if its checks pass. A clean checkout,
including no untracked or ignored files, is required.

It logs in as `buildkite` using the organization access token from Buildkite secret
`OCI_ACCELERATOR_DOCKER_HUB_TOKEN`, passed directly to `docker login --password-stdin`.
The secret is retrieved only after the Git checks and is not passed into build
containers. Authentication and build/push failures fail the job. Keep `latest`
mutable in Docker Hub. Pin deployments to an image digest when reproducibility
or rollback is needed; pulling `latest` later may return a different image. A retry
of the same build can also replace its `build-<number>` tag. The two tag updates
are not transactional; inspect both tags if publication fails partway through.

Before publishing, configure these external prerequisites:

1. Create the Docker Hub repository and grant the organization token read/write
   access only to `buildkite/oci-accelerator`. Store it as the named Buildkite secret
   in the Open Source cluster. Never put the token in this repository.
2. Restrict secret access outside repository-controlled YAML to this pipeline's
   trusted `main` webhook builds:

   ```yaml
   - pipeline_id: "01a07f28-efda-4ad5-a6cd-383fd12df30c"
     build_source: "webhook"
     build_branch: "main"
   ```

   Protect `main` in GitHub and keep tag builds disabled (a tag named `main` would
   have the same branch claim). Script checks and step conditions are defense in
   depth, not a credential boundary against modified repository code. Do not grant
   the token to untrusted PR/branch builds.

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
