# Releasing

Buildkite publishes OCI Accelerator's Docker images and GitHub releases. This
guide covers how to cut a release, what publication does, and how to recover
from a failed publication.

## Cut a release

To cut a release, open a PR that increases the root [`VERSION`](VERSION) file, for
example from `v0.1.0` to `v0.1.1`. Use one stable `vMAJOR.MINOR.PATCH` version, with a
lowercase `v` prefix and without leading zeros, prerelease suffix, or build metadata.
CI validates its format and checks numeric increases against the PR's merge base with
`main`. A comparison version must use the same prefixed format. Unprefixed versions
are rejected, including changes that only add the prefix.

After you merge the PR, the non-PR `main` build compares `VERSION` with its first parent.
If the value changed, it publishes a versioned Docker image and GitHub release after
formatting, vet, build, tests, race tests, the image check, and binary packaging pass
and you unblock **Publish to Docker Hub and GitHub?** in Buildkite. This approval is
required for all publication, including Docker `latest` and `build-N` updates.
Adding `VERSION` for the first time also triggers a release. Git tag builds do not publish.

## Publication

Release publication proceeds in this order:

1. Stage the four binary archives and checksum manifest from the same Buildkite build's
   `binaries` step as draft GitHub release assets. Publishing does not rebuild the binaries.
2. Publish the versioned Docker image, such as `buildkite/oci-accelerator:v0.1.1`, for
   `linux/amd64` and `linux/arm64`.
3. Create or verify the Git tag named exactly by `VERSION`, such as `v0.1.1`, pointing to
   the exact release commit, then publish the GitHub release with GitHub-generated notes.

The Git tag is a source reference, distinct from the Docker tag in the image repository.
GitHub publication uses `make_latest=legacy`, so approving a delayed older release does
not replace a newer release as GitHub's latest release.

Successful release publication adds a success annotation to the Buildkite build with
the version, image, platforms, GitHub release, Docker Hub and source links, and a `docker pull` command.
Retries replace the same annotation and say when the version is already published from
the same commit. Ordinary `build-N` and `latest` updates do not add release annotations.
If writing the annotation fails, the job logs a warning but still succeeds because
the release is published. Retry the original publishing job to restore the notice.

The versioned Docker tag identifies the merged commit, not whichever commit is newest when CI
finishes. A release still publishes if `main` has advanced, but that older build does not
update `latest` or `build-N`. Ordinary builds with unchanged versions publish only those
two Docker tags when their checkout is the current `main` tip at the publishing check.
They do not publish Git tags or GitHub releases.

## Retry a failed publication

If publication fails, retry the publishing job for the original release build. An existing
versioned Docker tag with matching source-revision and version metadata is left untouched.
Matching assets and the exact-commit Git tag also remain intact, and missing draft assets
are filled from the same build. Conflicting assets, Docker metadata, or Git tag targets fail
the job, as does an incomplete already-published GitHub release. Authentication and registry
errors also fail. There is no rollback across Docker Hub and GitHub. A failure can leave a
draft release, published image, or Git tag in place for the retry. A retry at the current
`main` tip can still rebuild and publish Docker `latest` and `build-N`.

An interrupted GitHub upload can leave an incomplete asset in the draft. The job fails
rather than replacing it. Inspect and remove that incomplete draft asset before retrying.
Do not remove or replace assets from an already-published release.

## Pipeline requirements

The publish step installs `gh` from GitHub's signed APT repository and installs `jq`
before running the release script. It runs as root on Buildkite's Ubuntu hosted image,
which provides APT, curl, Docker, Git, and Bash. Your publishing agent needs access to
`cli.github.com` and its configured Ubuntu package repositories.

Configure the Buildkite secret `OCI_ACCELERATOR_GITHUB_RELEASE_TOKEN` with a token scoped
to this repository and **Contents: read and write** permission. Retrieve it
only during approved publication, never during checks or binary packaging. Keep the existing
`OCI_ACCELERATOR_DOCKER_HUB_TOKEN` secret for Docker Hub. Retain Buildkite binary artifacts
long enough to cover manual approval delays and retries of the original build.

Publishing jobs share one Buildkite concurrency group. Keep all writers to this image
repository in that group. The check before pushing is not an atomic registry lock and does
not protect against an unrelated writer. Keep branch builds enabled and disable queued-build
skipping, running-build cancellation, and filters that exclude version-only commits. If you
cancel a release build or skip it with a commit-message directive, a later ordinary build
does not recover that release automatically. Run the original release commit again.

## Version reporting

The binary embeds `VERSION`, including in local `go build` and Docker builds. Run
`oci-accelerator --version` or `docker run --rm buildkite/oci-accelerator:v0.1.1 --version`
without a configuration file to print `oci-accelerator v0.1.1`. Between bumps, multiple
`main` builds report the same version. Images built by the pipeline also carry
`org.opencontainers.image.version` and `org.opencontainers.image.revision` as image labels
and multi-platform index annotations so you can identify the exact source commit.
