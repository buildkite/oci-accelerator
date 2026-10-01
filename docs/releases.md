# Releases

OCI Accelerator releases are driven by the `VERSION` file in the repository root. To release,
you merge a pull request that changes it. Buildkite then publishes a multi-platform image to
`docker.io/buildkite/oci-accelerator`.

## Version policy

`VERSION` contains one canonical `MAJOR.MINOR.PATCH` version with no leading `v`, for example
`1.2.3`. Each release must be greater than the previous one. There are no prerelease versions or
releases of older version lines.

Before 1.0, increment PATCH for compatible fixes. Increment MINOR for features and for breaking
changes. From 1.0 onward, follow semantic versioning: increment PATCH for compatible fixes,
MINOR for backward-compatible features, and MAJOR for incompatible changes.

Compatibility includes:

- configuration syntax, defaults, and meaning
- command-line flags and commands
- proxy behavior visible to clients, registries, and deployments
- persisted CA and cache state needed across upgrades

## Publication channels

| Image tag             | Published by                                         | Moves |
| --------------------- | ---------------------------------------------------- | ----- |
| `build-N`             | Every successful build of the current `main` tip     | No    |
| `edge`                | Every successful build of the current `main` tip     | Yes   |
| `MAJOR.MINOR.PATCH`   | The `main` commit that changes `VERSION`             | No    |
| `latest`              | A release newer than the one `latest` identifies     | Yes   |

`oci-accelerator --version` reports `MAJOR.MINOR.PATCH` in release images and
`MAJOR.MINOR.PATCH-dev+<12-character commit>` in `build-N` and `edge` images.

Never replace a published version image. If a release is broken, release the next PATCH version.

## Create a release

1. Open a pull request that changes `VERSION` to the new version. It may include other changes.
   The `VERSION check` step fails if the new version is not greater than the one on `main`.
2. Merge it. The `main` build of that commit runs the checks, then the release step builds and
   pushes the version image and advances `latest` if the version is newer.
3. Optionally, create a GitHub Release that describes the changes, upgrade instructions, and any
   breaking changes. Buildkite does not create Git tags or GitHub Releases.

Only the commit that changes `VERSION` publishes it, even if newer commits have landed on `main`
by the time its build runs. Later `main` builds check that the current version was published and
do nothing else.

## Retries and failures

If the release step fails, fix the cause and retry that build in Buildkite. A retry reuses an
existing version image only if it was built from the same commit, and never rebuilds or overwrites
it. Registry errors other than an explicit "not found" fail the step rather than publishing.

If Buildkite skips the commit that changed `VERSION`, for example because several commits were
pushed together, or that build fails without a retry, later `main` builds add a warning annotation
that names the commit. Start a new Buildkite build of that commit on `main` to publish the version.
If the version cannot be published from that commit, release the next PATCH version instead.
