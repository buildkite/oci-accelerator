#!/usr/bin/env bash
set +x
set -euo pipefail

# Called by image.sh after checkout validation and login. The pipeline serializes
# all publishers. Keep registry credentials scoped to image.sh's temporary config.
# shellcheck source=SCRIPTDIR/version.sh
. "$(dirname "$0")/version.sh"

main() {
  case "${1:-}" in
    publish) shift; publish "$@" ;;
    verify) shift; verify "$@" ;;
    *) echo 'Usage: release.sh publish|verify ...' >&2; return 1 ;;
  esac
}

# publish builds the version image unless it already exists, then advances latest
# when the version is newer than the release latest identifies.
publish() {
  local image=$1 version=$2 revision=$3 builder=$4
  local manifest digest latest latest_version latest_digest
  manifest=$(inspect "$image:$version")
  if [[ "$manifest" == null ]]; then
    docker buildx create --name "$builder" --driver docker-container --bootstrap
    docker buildx build --builder "$builder" --platform linux/amd64,linux/arm64 \
      --build-arg "VERSION=$version" \
      --label "org.opencontainers.image.version=$version" \
      --label "org.opencontainers.image.revision=$revision" \
      --annotation "index:org.opencontainers.image.version=$version" \
      --annotation "index:org.opencontainers.image.revision=$revision" \
      --tag "$image:$version" --push .
    manifest=$(inspect "$image:$version")
  fi

  # A retry must use the existing index, never rebuild or overwrite the version.
  digest=$(jq -er --arg version "$version" --arg revision "$revision" '
    select(.annotations["org.opencontainers.image.version"] == $version and
           .annotations["org.opencontainers.image.revision"] == $revision) |
    .digest' <<< "$manifest") || {
    echo 'Release image identity does not match VERSION and the commit' >&2
    return 1
  }

  latest=$(inspect "$image:latest")
  if [[ "$latest" != null ]]; then
    latest_version=$(jq -r '.annotations["org.opencontainers.image.version"] // ""' <<< "$latest")
    latest_digest=$(jq -r '.digest' <<< "$latest")
    # The first release replaces the legacy, unversioned main image.
    if [[ -n "$latest_version" ]]; then
      if [[ ! "$latest_version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
        echo 'latest has an invalid release version, refusing to replace it' >&2
        return 1
      fi
      if [[ "$version" == "$latest_version" ]]; then
        if [[ "$digest" != "$latest_digest" ]]; then
          echo 'latest has the same version but a different digest' >&2
          return 1
        fi
        echo 'latest already points to this release'
        return 0
      fi
      if ! version_greater "$version" "$latest_version"; then
        echo "Keeping newer latest release $latest_version"
        return 0
      fi
    fi
  fi
  # A single index source is copied byte-for-byte, retaining its digest.
  docker buildx imagetools create --tag "$image:latest" "$image@$digest"
}

# verify warns, without failing, when a build that did not change VERSION finds
# that the current version was never published, for example because Buildkite
# skipped the commit that changed it or its build failed.
verify() {
  local image=$1 version=$2 bump_commit=$3 manifest message
  manifest=$(inspect "$image:$version")
  if [[ "$manifest" != null ]]; then
    echo "Version $version is already published"
    return 0
  fi
  message="Version $version is not published. Retry the Buildkite build of $bump_commit, the commit that changed VERSION."
  echo "$message" >&2
  buildkite-agent annotate --style warning --context missing-release "$message"
}

inspect() {
  local reference=$1 manifest error_file
  error_file="$DOCKER_CONFIG/inspect-error"
  if ! manifest=$(docker buildx imagetools inspect "$reference" --format '{{json .Manifest}}' 2>"$error_file"); then
    # Only an explicit missing reference permits a new publication. Authentication,
    # rate limits, network errors, and other failures must stop the release.
    if grep -Fxq "ERROR: $reference: not found" "$error_file"; then
      printf 'null\n'
      return 0
    fi
    cat "$error_file" >&2
    return 1
  fi
  # Require a complete multi-platform index before reusing or promoting an image.
  jq -e '
    select(.schemaVersion == 2 and
      (.mediaType == "application/vnd.oci.image.index.v1+json" or
       .mediaType == "application/vnd.docker.distribution.manifest.list.v2+json") and
      (.digest | test("^sha256:[0-9a-f]{64}$")) and
      any(.manifests[]; .platform.os == "linux" and .platform.architecture == "amd64") and
      any(.manifests[]; .platform.os == "linux" and .platform.architecture == "arm64"))
  ' <<< "$manifest"
}

main "$@"
