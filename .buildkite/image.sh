#!/usr/bin/env bash
set +x
set -euo pipefail

# Run from the repository root. Check mode never reads publishing credentials.
mode=${1:-check}
image=docker.io/buildkite/oci-accelerator
head=$(git rev-parse HEAD)
case "$mode" in
  check) ;;
  publish)
    if [[ ${BUILDKITE_BRANCH:-} != main ]] || [[ -n ${BUILDKITE_TAG:-} ]] ||
       [[ ${BUILDKITE_PULL_REQUEST:-false} != false ]]; then
      echo 'Publishing requires a non-PR main branch build, not a tag' >&2
      exit 1
    fi
    build_number=${BUILDKITE_BUILD_NUMBER:-}
    if [[ ! "$build_number" =~ ^[1-9][0-9]*$ ]]; then
      echo 'Publishing requires a positive BUILDKITE_BUILD_NUMBER' >&2
      exit 1
    fi
    if [[ "$head" != "${BUILDKITE_COMMIT:-}" ]]; then
      echo 'Build commit and checkout must match' >&2
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

# Stable SemVer only, bounded by Docker's tag length. Do not use shell integer
# arithmetic for version components, which can exceed the integer range.
version=$(cat VERSION)
version_pattern='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'
if [[ ! "$version" =~ $version_pattern ]] || ((${#version} > 128)) ||
   { ! cmp -s VERSION <(printf '%s\n' "$version") && ! cmp -s VERSION <(printf '%s' "$version"); }; then
  echo 'VERSION must contain one stable MAJOR.MINOR.PATCH version without leading zeros or a v prefix' >&2
  exit 1
fi

# Fetch both histories so PR merge bases and older main releases work even with
# a depth-one checkout. Never infer a first release from missing shallow history.
fetch_args=(--no-tags)
if [[ $(git rev-parse --is-shallow-repository) == true ]]; then
  fetch_args+=(--unshallow)
fi
git fetch "${fetch_args[@]}" origin +refs/heads/main:refs/remotes/origin/main "$head"
main=$(git rev-parse refs/remotes/origin/main)
if [[ "$mode" == publish ]]; then
  if ! git rev-list --first-parent "$main" | grep -Fx "$head" >/dev/null; then
    echo 'Publishing requires a commit on the first-parent history of origin/main' >&2
    exit 1
  fi
fi
if [[ ${BUILDKITE_BRANCH:-} == main && ${BUILDKITE_PULL_REQUEST:-false} == false && -z ${BUILDKITE_TAG:-} ]]; then
  base=$(git rev-parse "$head^1")
else
  base=$(git merge-base "$head" "$main")
fi
previous=
if [[ -n $(git ls-tree "$base" -- VERSION) ]]; then
  previous=$(git show "$base:VERSION")
fi
release=false
if [[ "$version" != "$previous" ]]; then
  if [[ -n "$previous" ]]; then
    if [[ ! "$previous" =~ $version_pattern ]] ||
       [[ $(printf '%s\n' "$previous" "$version" | LC_ALL=C sort -V | head -n 1) != "$previous" ]]; then
      echo 'VERSION must increase from the comparison commit' >&2
      exit 1
    fi
  fi
  release=true
fi
if [[ "$mode" == publish && "$head" != "$main" && "$release" == false ]]; then
  echo 'Skipping publication: this is neither a release nor the origin/main tip'
  exit 0
fi

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

tags=()
if [[ "$mode" == publish && "$release" == true ]]; then
  # Only the exact not-found diagnostic permits creating a tag. Authentication,
  # transport, and unexpected CLI errors must not be mistaken for an absent image.
  if published=$(docker buildx imagetools inspect "$image:$version" \
      --format '{{ index .Manifest.Annotations "org.opencontainers.image.revision" }} {{ index .Manifest.Annotations "org.opencontainers.image.version" }}' \
      2>"$temporary/inspect-error"); then
    if [[ "$published" != "$head $version" ]]; then
      echo 'Refusing to replace a version tag with different or missing source metadata' >&2
      exit 1
    fi
    echo "Version $version is already published from this commit"
  elif [[ $(cat "$temporary/inspect-error") == "ERROR: $image:$version: not found" ]]; then
    tags+=(--tag "$image:$version")
  else
    cat "$temporary/inspect-error" >&2
    echo 'Could not establish whether the version tag exists' >&2
    exit 1
  fi
fi
if [[ "$mode" == publish ]]; then
  if [[ "$head" == "$main" ]]; then
    tags+=(--tag "$image:build-$build_number" --tag "$image:latest")
  fi
  if ((${#tags[@]} == 0)); then
    exit 0
  fi
fi

# Official BuildKit images bundle QEMU for the Alpine ARM64 runtime stage.
docker buildx create --name "$builder" --driver docker-container --bootstrap
build_args=(--builder "$builder" --platform linux/amd64,linux/arm64
  --label "org.opencontainers.image.version=$version"
  --label "org.opencontainers.image.revision=$head"
  --annotation "index:org.opencontainers.image.version=$version"
  --annotation "index:org.opencontainers.image.revision=$head")
if [[ "$mode" == publish ]]; then
  build_args+=("${tags[@]}" --push)
else
  build_args+=(--output "type=oci,dest=$temporary/image.tar")
fi
docker buildx build "${build_args[@]}" .
