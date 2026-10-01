#!/usr/bin/env bash
set +x
set -euo pipefail

# Run from the repository root. Check mode never reads publishing credentials.
mode=${1:-check}
image=docker.io/buildkite/oci-accelerator

# Validate the build context before any credentials are read.
case "$mode" in
  check) ;;
  publish|release)

    # Refuse to publish from pull request builds.
    if [[ ${BUILDKITE_PULL_REQUEST:-false} != false ]]; then
      echo 'Publishing is not allowed from pull requests' >&2
      exit 1
    fi

    # Development images and releases come only from untagged main branch builds.
    if [[ ${BUILDKITE_BRANCH:-} != main || -n ${BUILDKITE_TAG:-} ]]; then
      echo 'Publishing requires a main branch build, not a tag' >&2
      exit 1
    fi

    if [[ "$mode" == publish ]]; then

      # Require a positive build number for the build-N image tag.
      build_number=${BUILDKITE_BUILD_NUMBER:-}
      if [[ ! "$build_number" =~ ^[1-9][0-9]*$ ]]; then
        echo 'Publishing requires a positive BUILDKITE_BUILD_NUMBER' >&2
        exit 1
      fi

      # Fetch the current main tip to confirm this build is still the latest.
      git fetch --no-tags --depth=1 origin +refs/heads/main:refs/remotes/origin/main
    else

      # Ancestry and previous VERSION checks need full history, not just the main tip.
      fetch_args=(--no-tags)
      if [[ $(git rev-parse --is-shallow-repository) == true ]]; then
        fetch_args+=(--unshallow)
      fi
      git fetch "${fetch_args[@]}" origin +refs/heads/main:refs/remotes/origin/main
    fi

    # Ensure the checked-out commit is the commit Buildkite is building.
    head=$(git rev-parse HEAD)
    if [[ "$head" != "${BUILDKITE_COMMIT:-}" ]]; then
      echo 'Build commit and checkout must match' >&2
      exit 1
    fi
    if [[ "$mode" == release ]]; then

      # Releases may come from an older main commit, but never from outside main history.
      if ! git merge-base --is-ancestor "$head" refs/remotes/origin/main; then
        echo 'Release requires a build commit in main history' >&2
        exit 1
      fi

    else

      # Skip publishing quietly if a newer commit has landed on main.
      main=$(git rev-parse refs/remotes/origin/main)
      if [[ "$head" != "$main" ]]; then
        echo 'Skipping publication: this build is no longer the origin/main tip'
        exit 0
      fi

    fi

    # Refuse to publish from a checkout with local modifications or stray files.
    status=$(git status --porcelain --untracked-files=all --ignored)
    if [[ -n "$status" ]]; then
      echo 'Publishing requires a clean checkout with no untracked or ignored files' >&2
      exit 1
    fi
    ;;
  *) echo 'Usage: bash .buildkite/image.sh [check|publish|release]' >&2; exit 1 ;;

esac

# Read VERSION before any credentials are read. Development builds report it
# with the commit appended, never as a bare release version.
# shellcheck source=SCRIPTDIR/version.sh
. "$(dirname "$0")/version.sh"
file_version=$(read_version VERSION)
commit=$(git rev-parse HEAD)
build_version="$file_version-dev+${commit:0:12}"

# Only the commit that changes VERSION publishes it. Other main builds verify
# that the current version was published and warn if it was missed.
if [[ "$mode" == release ]]; then
  # Only a root commit or a parent without VERSION counts as having no previous version.
  previous=
  if parent=$(git rev-parse --verify --quiet "$head^1"); then
    entry=$(git ls-tree --name-only "$parent" -- VERSION)
    if [[ -n "$entry" ]]; then
      previous=$(git show "$parent:VERSION")
    fi
  fi
  if [[ "$file_version" == "$previous" ]]; then
    release_action=verify
    bump_commit=$(git log -1 --first-parent --format=%H "$head" -- VERSION)
  else
    release_action=publish
    if [[ -n "$previous" ]]; then
      previous=$(parse_version "$previous")
      if ! version_greater "$file_version" "$previous"; then
        echo "VERSION $file_version must be greater than the previous $previous" >&2
        exit 1
      fi
    fi
  fi
fi

# Create a temporary workspace and a uniquely named builder, both removed on exit.
temporary=$(mktemp -d)
builder="oci-accelerator-$(basename "$temporary")"
cleanup() {
  docker buildx rm "$builder" >/dev/null 2>&1 || true
  rm -rf "$temporary"
}
trap cleanup EXIT

# Keep Docker credentials in a private, temporary config directory.
export DOCKER_CONFIG="$temporary/docker"
mkdir -m 0700 "$DOCKER_CONFIG"

# Log in to Docker Hub with the Buildkite secret when publishing.
if [[ "$mode" != check ]]; then
  buildkite-agent secret get --format default OCI_ACCELERATOR_DOCKER_HUB_TOKEN \
    | docker login --username buildkite --password-stdin docker.io
fi

# Hand releases off to the release script.
if [[ "$mode" == release ]]; then
  if [[ "$release_action" == publish ]]; then
    bash .buildkite/release.sh publish "$image" "$file_version" "$head" "$builder"
  else
    bash .buildkite/release.sh verify "$image" "$file_version" "$bump_commit"
  fi
  exit 0
fi

# Official BuildKit images bundle QEMU for the Alpine ARM64 runtime stage.
docker buildx create --name "$builder" --driver docker-container --bootstrap

# Build for both architectures, pushing edge and build-N tags or exporting a local OCI archive.
build_args=(--builder "$builder" --build-arg "VERSION=$build_version" --platform "linux/amd64,linux/arm64")
if [[ "$mode" == publish ]]; then
  build_args+=(--tag "$image:build-$build_number" --tag "$image:edge" --push)
else
  build_args+=(--output "type=oci,dest=$temporary/image.tar")
fi
docker buildx build "${build_args[@]}" .
