#!/usr/bin/env bash
set +x
set -euo pipefail

# Run from the repository root. Check mode never reads publishing credentials.
mode=${1:-check}
image=docker.io/buildkite/oci-accelerator
case "$mode" in
  check) ;;
  publish)
    tag=${BUILDKITE_TAG:-}
    if [[ ! "$tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] ||
       [[ ${BUILDKITE_PULL_REQUEST:-false} != false ]]; then
      echo 'Publishing requires a stable vMAJOR.MINOR.PATCH tag and a non-PR build' >&2
      exit 1
    fi
    version=${tag#v}
    fetch_flags=(--no-tags)
    if [[ $(git rev-parse --is-shallow-repository) == true ]]; then
      fetch_flags+=(--unshallow)
    fi
    git fetch "${fetch_flags[@]}" origin \
      +refs/heads/main:refs/remotes/origin/main "+refs/tags/$tag:refs/tags/$tag"
    head=$(git rev-parse HEAD)
    if [[ "$head" != "${BUILDKITE_COMMIT:-}" ]] ||
       [[ "$head" != "$(git rev-parse "refs/tags/$tag^{commit}")" ]] ||
       ! git merge-base --is-ancestor "$head" refs/remotes/origin/main; then
      echo 'Release tag, build commit, and checkout must match a commit on origin/main' >&2
      exit 1
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

  # Only a definite missing manifest permits publication. Auth/network failures
  # must not masquerade as an unused version. CI serializes this check and push.
  if docker buildx imagetools inspect "$image:$version" >"$temporary/manifest" 2>"$temporary/inspect-error"; then
    echo "Refusing to overwrite $image:$version" >&2
    exit 1
  elif ! grep -Fxq "ERROR: $image:$version: not found" "$temporary/inspect-error"; then
    cat "$temporary/inspect-error" >&2
    exit 1
  fi
fi

# Official BuildKit images bundle QEMU for the Alpine ARM64 runtime stage.
docker buildx create --name "$builder" --driver docker-container --bootstrap
build_args=(--builder "$builder" --platform linux/amd64,linux/arm64)
if [[ "$mode" == publish ]]; then
  build_args+=(--tag "$image:$version" --tag "$image:latest" --push)
else
  build_args+=(--output "type=oci,dest=$temporary/image.tar")
fi
docker buildx build "${build_args[@]}" .
