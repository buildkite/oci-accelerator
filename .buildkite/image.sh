#!/usr/bin/env bash
set +x
set -euo pipefail

# Run from the repository root. Check mode never reads publishing credentials.
mode=${1:-check}
image=docker.io/buildkite/oci-accelerator
case "$mode" in
  check) ;;
  publish|release)
    if [[ ${BUILDKITE_PULL_REQUEST:-false} != false ]]; then
      echo 'Publishing is not allowed from pull requests' >&2
      exit 1
    fi
    if [[ "$mode" == publish ]]; then
      if [[ ${BUILDKITE_BRANCH:-} != main || -n ${BUILDKITE_TAG:-} ]]; then
        echo 'Development publishing requires a main branch build, not a tag' >&2
        exit 1
      fi
      build_number=${BUILDKITE_BUILD_NUMBER:-}
      if [[ ! "$build_number" =~ ^[1-9][0-9]*$ ]]; then
        echo 'Publishing requires a positive BUILDKITE_BUILD_NUMBER' >&2
        exit 1
      fi
      git fetch --no-tags --depth=1 origin +refs/heads/main:refs/remotes/origin/main
    else
      if [[ ! ${BUILDKITE_TAG:-} =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
        echo 'Releases require a canonical vMAJOR.MINOR.PATCH tag' >&2
        exit 1
      fi
      version=${BUILDKITE_TAG#v}
      if ((${#version} > 128)); then
        echo 'Release version exceeds the Docker tag length limit' >&2
        exit 1
      fi
      # Ancestry checks need full history, not just the current main tip.
      fetch_args=(--no-tags)
      if [[ $(git rev-parse --is-shallow-repository) == true ]]; then
        fetch_args+=(--unshallow)
      fi
      git fetch "${fetch_args[@]}" origin +refs/heads/main:refs/remotes/origin/main \
        "+refs/tags/$BUILDKITE_TAG:refs/tags/$BUILDKITE_TAG"
    fi
    head=$(git rev-parse HEAD)
    if [[ "$head" != "${BUILDKITE_COMMIT:-}" ]]; then
      echo 'Build commit and checkout must match' >&2
      exit 1
    fi
    if [[ "$mode" == release ]]; then
      if [[ $(git cat-file -t "refs/tags/$BUILDKITE_TAG") != tag ]] ||
         [[ $(git rev-parse "refs/tags/$BUILDKITE_TAG^{commit}") != "$head" ]] ||
         ! git merge-base --is-ancestor "$head" refs/remotes/origin/main; then
        echo 'Release requires an annotated tag on the build commit in main history' >&2
        exit 1
      fi
    else
      main=$(git rev-parse refs/remotes/origin/main)
      if [[ "$head" != "$main" ]]; then
        echo 'Skipping publication: this build is no longer the origin/main tip'
        exit 0
      fi
    fi
    status=$(git status --porcelain --untracked-files=all --ignored)
    if [[ -n "$status" ]]; then
      echo 'Publishing requires a clean checkout with no untracked or ignored files' >&2
      exit 1
    fi
    ;;
  *) echo 'Usage: bash .buildkite/image.sh [check|publish|release]' >&2; exit 1 ;;
esac

temporary=$(mktemp -d)
builder="oci-accelerator-$(basename "$temporary")"
cleanup() {
  docker buildx rm "$builder" >/dev/null 2>&1 || true
  rm -rf "$temporary"
}
trap cleanup EXIT
export DOCKER_CONFIG="$temporary/docker"
mkdir -m 0700 "$DOCKER_CONFIG"

if [[ "$mode" != check ]]; then
  buildkite-agent secret get --format default OCI_ACCELERATOR_DOCKER_HUB_TOKEN \
    | docker login --username buildkite --password-stdin docker.io
fi

if [[ "$mode" == release ]]; then
  bash .buildkite/release.sh "$image" "$version" "$head" "$builder"
  exit 0
fi

# Official BuildKit images bundle QEMU for the Alpine ARM64 runtime stage.
docker buildx create --name "$builder" --driver docker-container --bootstrap
build_args=(--builder "$builder" --platform linux/amd64,linux/arm64)
if [[ "$mode" == publish ]]; then
  build_args+=(--tag "$image:build-$build_number" --tag "$image:edge" --push)
else
  build_args+=(--output "type=oci,dest=$temporary/image.tar")
fi
docker buildx build "${build_args[@]}" .
