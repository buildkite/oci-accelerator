#!/usr/bin/env bash
set +x
set -euo pipefail

# Run from the repository root. Check mode never reads publishing credentials.
mode=${1:-check}
image=docker.io/buildkite/oci-accelerator
case "$mode" in
  check) ;;
  publish)
    if [[ ${BUILDKITE_BRANCH:-} != main ]] || [[ -n ${BUILDKITE_TAG:-} ]] ||
       [[ ${BUILDKITE_PULL_REQUEST:-false} != false ]]; then
      echo 'Publishing requires a non-PR main branch build, not a tag' >&2
      exit 1
    fi
    git fetch --no-tags --depth=1 origin +refs/heads/main:refs/remotes/origin/main
    head=$(git rev-parse HEAD)
    if [[ "$head" != "${BUILDKITE_COMMIT:-}" ]]; then
      echo 'Build commit and checkout must match' >&2
      exit 1
    fi
    main=$(git rev-parse refs/remotes/origin/main)
    if [[ "$head" != "$main" ]]; then
      echo 'Skipping publication: this build is no longer the origin/main tip'
      exit 0
    fi
    status=$(git status --porcelain --untracked-files=all --ignored)
    if [[ -n "$status" ]]; then
      echo 'Publishing requires a clean checkout with no untracked or ignored files' >&2
      exit 1
    fi
    ;;
  *) echo 'Usage: bash .buildkite/image.sh [check|publish]' >&2; exit 1 ;;
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

if [[ "$mode" == publish ]]; then
  buildkite-agent secret get --format default OCI_ACCELERATOR_DOCKER_HUB_TOKEN \
    | docker login --username buildkite --password-stdin docker.io
fi

# Official BuildKit images bundle QEMU for the Alpine ARM64 runtime stage.
docker buildx create --name "$builder" --driver docker-container --bootstrap
build_args=(--builder "$builder" --platform linux/amd64,linux/arm64)
if [[ "$mode" == publish ]]; then
  build_args+=(--tag "$image:latest" --push)
else
  build_args+=(--output "type=oci,dest=$temporary/image.tar")
fi
docker buildx build "${build_args[@]}" .
