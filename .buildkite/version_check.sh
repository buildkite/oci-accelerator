#!/usr/bin/env bash
set -euo pipefail

# Validate the VERSION file and, when this build changes it relative to main,
# require a version strictly greater than the one on origin/main. A build that
# does not touch VERSION passes even if main has since moved ahead.
# shellcheck source=SCRIPTDIR/version.sh
. "$(dirname "$0")/version.sh"

version=$(read_version VERSION)

# Full history is needed to find where this build diverged from main.
fetch_args=(--quiet --no-tags)
if [[ $(git rev-parse --is-shallow-repository) == true ]]; then
  fetch_args+=(--unshallow)
fi
git fetch "${fetch_args[@]}" origin +refs/heads/main:refs/remotes/origin/main

base=$(git merge-base HEAD refs/remotes/origin/main)
base_version=$(git show "$base:VERSION" 2>/dev/null || true)
if [[ "$version" == "$base_version" ]]; then
  echo "VERSION $version is unchanged"
  exit 0
fi

main_version=$(git show refs/remotes/origin/main:VERSION 2>/dev/null || true)
if [[ -n "$main_version" ]]; then
  main_version=$(parse_version "$main_version")
  if ! version_greater "$version" "$main_version"; then
    echo "VERSION $version must be greater than $main_version on main" >&2
    exit 1
  fi
fi
echo "VERSION changes to $version"
