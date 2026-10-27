#!/usr/bin/env bash

# Sourced by image.sh only for an approved version release. The caller owns
# version, head, and temporary, including cleanup of credentials and downloads.
github_prepare() {
  local manifest files matches name target
  github_repo=buildkite/oci-accelerator
  github_api="repos/$github_repo"
  github_directory="$temporary/releases"
  mkdir "$github_directory"
  : "${BUILDKITE_BUILD_ID:?Publishing binaries requires BUILDKITE_BUILD_ID}"
  buildkite-agent artifact download --build "$BUILDKITE_BUILD_ID" --step binaries '*' "$github_directory"

  github_files=()
  for target in linux-amd64 linux-arm64 darwin-amd64 darwin-arm64; do
    github_files+=("oci-accelerator-$target-$version.tar.gz")
  done
  manifest="oci-accelerator-$version.SHA256SUMS"
  github_files+=("$manifest")
  files=("$github_directory/"*)
  if ((${#files[@]} != ${#github_files[@]})); then
    echo 'Expected exactly four release archives and one checksum manifest' >&2
    return 1
  fi
  for name in "${github_files[@]}"; do
    [[ -f "$github_directory/$name" && ! -L "$github_directory/$name" ]] || return 1
  done
  # Compare with independently computed checksums, not filenames supplied by the
  # manifest. This also rejects missing, duplicate, or unexpected records.
  (cd "$github_directory" && LC_ALL=C sha256sum *.tar.gz) > "$temporary/checksums"
  if ! cmp -s "$temporary/checksums" "$github_directory/$manifest"; then
    echo 'Release artifact checksums do not match the manifest' >&2
    return 1
  fi

  export GH_TOKEN
  GH_TOKEN=$(buildkite-agent secret get --format default OCI_ACCELERATOR_GITHUB_RELEASE_TOKEN)
  [[ -n "$GH_TOKEN" ]] || return 1
  github_verify_tag
  gh api --hostname github.com --paginate --slurp "$github_api/releases?per_page=100" > "$temporary/github-releases"
  matches=$(jq --arg version "$version" '[.[][] | select(.tag_name == $version)]' "$temporary/github-releases")
  if [[ $(jq length <<< "$matches") -gt 1 ]]; then
    echo 'Multiple GitHub releases have the requested version' >&2
    return 1
  fi
  if [[ $(jq length <<< "$matches") == 0 ]]; then
    gh api --hostname github.com --method POST "$github_api/releases" \
      -f "tag_name=$version" -f "target_commitish=$head" -f "name=$version" \
      -F draft=true -F generate_release_notes=true > "$temporary/github-release"
  else
    jq '.[0]' <<< "$matches" > "$temporary/github-release"
  fi
  github_id=$(jq -er '.id' "$temporary/github-release")
  github_draft=$(jq -er '.draft | tostring' "$temporary/github-release")
  if ! jq -e --arg version "$version" --arg head "$head" \
    '.tag_name == $version and .prerelease == false and
     (if .draft then .target_commitish == $head else true end)' "$temporary/github-release" >/dev/null; then
    echo 'GitHub release does not match this version and commit' >&2
    return 1
  fi
  if [[ "$github_draft" == false && "$github_tag_present" == false ]]; then
    echo 'Published GitHub release has no matching Git tag' >&2
    return 1
  fi
  github_stage_assets
}

github_publish() {
  github_verify_tag
  if [[ "$github_tag_present" == false ]]; then
    gh api --hostname github.com --method POST "$github_api/git/refs" \
      -f "ref=refs/tags/$version" -f "sha=$head" >/dev/null
    github_verify_tag
    [[ "$github_tag_present" == true ]] || return 1
  fi
  if [[ "$github_draft" == true ]]; then
    # An older release may finish after a newer one. Let GitHub select latest
    # using semantic version and commit date, not publication order.
    gh api --hostname github.com --method PATCH "$github_api/releases/$github_id" \
      -F draft=false -f make_latest=legacy >/dev/null
  fi
}

github_verify_tag() {
  local refs sha ref commit= peeled=
  refs=$(git ls-remote --tags origin "refs/tags/$version" "refs/tags/$version^{}")
  while read -r sha ref; do
    case "$ref" in
      "refs/tags/$version") commit=$sha ;;
      "refs/tags/$version^{}") peeled=$sha ;;
    esac
  done <<< "$refs"
  github_tag_present=false
  if [[ -n "$commit" ]]; then
    github_tag_present=true
    if [[ "${peeled:-$commit}" != "$head" ]]; then
      echo 'Refusing to replace a Git tag pointing at another commit' >&2
      return 1
    fi
  fi
}

github_stage_assets() {
  local assets matches name asset id
  gh api --hostname github.com --paginate --slurp "$github_api/releases/$github_id/assets?per_page=100" \
    > "$temporary/github-assets"
  assets=$(jq 'add' "$temporary/github-assets")
  if ! jq -e --args '. as $assets | all($assets[]; .name as $name | $ARGS.positional | index($name) != null)' \
    "${github_files[@]}" <<< "$assets" >/dev/null; then
    echo 'GitHub release contains unexpected assets' >&2
    return 1
  fi
  for name in "${github_files[@]}"; do
    matches=$(jq --arg name "$name" '[.[] | select(.name == $name)]' <<< "$assets")
    case $(jq length <<< "$matches") in
      0)
        if [[ "$github_draft" != true ]]; then
          echo 'Published GitHub release is missing an asset' >&2
          return 1
        fi
        asset=$(gh api --hostname github.com --method POST \
          "https://uploads.github.com/$github_api/releases/$github_id/assets?name=$name" \
          --header 'Content-Type: application/octet-stream' --input "$github_directory/$name")
        ;;
      1) asset=$(jq '.[0]' <<< "$matches") ;;
      *) echo 'GitHub release contains duplicate asset names' >&2; return 1 ;;
    esac
    if ! jq -e --arg name "$name" '.name == $name and .state == "uploaded"' <<< "$asset" >/dev/null; then
      echo 'GitHub release asset upload is incomplete' >&2
      return 1
    fi
    id=$(jq -er '.id' <<< "$asset")
    gh api --hostname github.com "$github_api/releases/assets/$id" \
      --header 'Accept: application/octet-stream' > "$temporary/github-asset"
    if ! cmp -s "$temporary/github-asset" "$github_directory/$name"; then
      echo 'Refusing to replace a GitHub release asset with different bytes' >&2
      return 1
    fi
  done
}
