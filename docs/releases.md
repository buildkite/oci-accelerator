# Releases

OCI Accelerator releases are manually selected from commits in `main` and published as
multi-platform images at `docker.io/buildkite/oci-accelerator`. A release commit may be an older
commit in `main`. It does not need to be the current tip.

## Version policy

Release tags are annotated Git tags in canonical `vMAJOR.MINOR.PATCH` form. The image version
omits the leading `v`, for example Git tag `v1.2.3` publishes image tag `1.2.3`.

Before 1.0, increment PATCH for compatible fixes. Increment MINOR for features and for breaking
changes. From 1.0 onward, follow semantic versioning: increment PATCH for compatible fixes,
MINOR for backward-compatible features, and MAJOR for incompatible changes.

Compatibility includes:

- configuration syntax, defaults, and meaning
- command-line flags and commands
- proxy behavior visible to clients, registries, and deployments
- persisted CA and cache state needed across upgrades

Pin `buildkite/oci-accelerator:MAJOR.MINOR.PATCH` or, for immutable deployment identity, pin the
published digest. Do not use `latest` or `edge` where reproducibility is required.

## Publication channels

Successful builds of the current `main` tip publish `build-N`, where `N` is the Buildkite build
number, and update `edge`. A release build publishes `MAJOR.MINOR.PATCH`. It advances `latest`
only when its version is newer than the release currently identified by `latest`. The first
successful release replaces the legacy unversioned `latest` image.

Version tags are immutable release identities. Never move or replace a released Git tag or image
version tag. Protect release tags in GitHub and enable immutable Docker Hub version tags when
those controls are available.

There are no prerelease tags, version aliases, automated GitHub Releases, or downloadable binary
artifacts. After image publication succeeds, create the corresponding GitHub Release manually.

## Buildkite prerequisites

Enable builds for pushed Git tags in the Buildkite pipeline and ensure any pipeline filters admit
release tags. The publish step accepts only canonical `vMAJOR.MINOR.PATCH` tags. These repository
files do not enable tag builds, so verify that setting before releasing. The pipeline is managed
in `buildkite/saas-iac`, so make the configuration change there rather than through the Buildkite UI.

Release jobs use the hosted queue and require Bash, Git, Docker with Buildx, `jq`, and GNU
`sort -V`. Development and release publishers must use the same serialized Buildkite concurrency
group. Serialization coordinates only publishers that use that group. Drain jobs from older
pipelines before the first release so an old publisher cannot overwrite `latest`.

Before the first release, move consumers that expect `latest` to follow `main` onto `edge`.
Existing `latest` remains frozen until the first release publishes successfully.

## Create a release

Choose a commit whose exact contents have passed the required checks. Verify that it belongs to
`main`, then create and push a new annotated tag without moving any existing tag:

```bash
set -euo pipefail
git fetch --no-tags origin +refs/heads/main:refs/remotes/origin/main
commit='<tested-commit-sha>'
version=vMAJOR.MINOR.PATCH
[[ "$version" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]
git rev-parse --verify "$commit^{commit}"
git merge-base --is-ancestor "$commit" refs/remotes/origin/main
remote_tag=$(git ls-remote --tags origin "refs/tags/$version")
[[ -z "$remote_tag" ]] || {
  echo "remote tag already exists: $version" >&2
  exit 1
}
git show-ref --verify --quiet "refs/tags/$version" && {
  echo "local tag already exists: $version" >&2
  exit 1
}
git tag -a "$version" "$commit" -m "Release $version"
git push origin "refs/tags/$version"
```

Do not use force push. Wait for the tag build and all checks and publishing steps to succeed.
Then create a GitHub Release for the same tag manually. Include the changes, upgrade instructions,
and any breaking changes. Only tag commits containing this release pipeline. Older commits
without it cannot publish a release through these steps.

## Retries and failures

A retry inspects the version image first. If it exists, its recorded version and source revision
must match the Git tag and commit. The publisher reuses that exact digest and does not rebuild or
overwrite it. Promotion to `latest` copies the existing index by digest. A same-version `latest`
must already have the same digest, while a newer `latest` is retained.

Only an explicit registry response that the reference is not found is treated as absence and
permits publication. Authentication, authorization, rate-limit, network, malformed manifest, and
other registry errors fail the release. Resolve the underlying error and retry the same tag. Do
not move the tag or replace its version image.

## Verify publishing changes

Run `bash .buildkite/image_test.sh` for disposable Git fixtures and simulated Docker and secret
commands. Run `go test ./...`, `go test -race ./...`, and `go vet ./...` for the repository checks.
Run `bash .buildkite/image.sh check` to build the real amd64 and arm64 image without publishing
or reading registry credentials. These checks do not verify Docker Hub write permissions or
Buildkite's external tag-build setting.
