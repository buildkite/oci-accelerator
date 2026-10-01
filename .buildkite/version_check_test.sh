#!/usr/bin/env bash
set -euo pipefail

# Exercise version_check.sh against disposable local origins with private Git config.
root=$(cd "$(dirname "$0")/.." && pwd)
script=$root/.buildkite/version_check.sh
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
export HOME=$tmp GIT_CONFIG_GLOBAL="$tmp/gitconfig" GIT_CONFIG_NOSYSTEM=1
git config --global user.name 'Version Test'
git config --global user.email version-test@example.invalid
git config --global init.defaultBranch main

failures=0
pass() { printf 'ok - %s\n' "$1"; }
fail() {
  printf 'not ok - %s\n' "$1" >&2
  failures=$((failures + 1))
}

# check runs the script on a branch of a fresh clone and asserts its status and message.
check() {
  local name=$1 origin=$2 branch=$3 expected=$4 message=$5 output status=0
  rm -rf "$tmp/work"
  git clone -q --depth 1 --branch "$branch" "file://$origin" "$tmp/work"
  output=$(cd "$tmp/work" && bash "$script" 2>&1) || status=$?
  if (((status == 0) != (expected == 0))); then
    fail "$name (status $status, wanted $expected): $output"
  elif [[ "$output" != *"$message"* ]]; then
    fail "$name (missing '$message'): $output"
  else
    pass "$name"
  fi
}

# commit_version records VERSION and a content change on the current branch.
commit_version() {
  printf '%s\n' "$1" >VERSION
  echo "$1 on $(git branch --show-current)" >>content
  git add VERSION content
  git commit -qm "$1"
}

# Main moves from 0.1.0 to 0.2.0. Branches start before or after that bump.
git init -q "$tmp/seed"
cd "$tmp/seed"
commit_version 0.1.0
git branch behind
commit_version 0.2.0
for branch in unchanged bump downgrade malformed; do git branch "$branch"; done
git checkout -q behind
git checkout -qb behind-unchanged
echo change >>content
git commit -qam change
git checkout -q behind
git checkout -qb behind-equal
commit_version 0.2.0
git checkout -q behind
git checkout -qb behind-bump
commit_version 0.3.0
git checkout -q unchanged
echo change >>content
git commit -qam change
git checkout -q bump
commit_version 0.2.1
git checkout -q downgrade
commit_version 0.1.5
git checkout -q malformed
commit_version v0.3.0
git checkout -q main
git init -q --bare "$tmp/origin.git"
git push -q --all "$tmp/origin.git"

check 'main is unchanged' "$tmp/origin.git" main 0 'VERSION 0.2.0 is unchanged'
check 'unchanged branch passes' "$tmp/origin.git" unchanged 0 'is unchanged'
check 'branch behind main without a bump passes' "$tmp/origin.git" behind-unchanged 0 'VERSION 0.1.0 is unchanged'
check 'greater version passes' "$tmp/origin.git" bump 0 'VERSION changes to 0.2.1'
check 'branch behind main with a greater bump passes' "$tmp/origin.git" behind-bump 0 'VERSION changes to 0.3.0'
check 'version equal to main rejected' "$tmp/origin.git" behind-equal 1 'must be greater than 0.2.0'
check 'downgrade rejected' "$tmp/origin.git" downgrade 1 'VERSION 0.1.5 must be greater than 0.2.0'
check 'malformed version rejected' "$tmp/origin.git" malformed 1 'canonical MAJOR.MINOR.PATCH'

# The first VERSION file on a main that has none passes.
git init -q "$tmp/first"
cd "$tmp/first"
echo one >content
git add content
git commit -qm one
git checkout -qb add-version
commit_version 0.1.0
git init -q --bare "$tmp/first.git"
git push -q --all "$tmp/first.git"
check 'first VERSION passes' "$tmp/first.git" add-version 0 'VERSION changes to 0.1.0'

# An unreachable origin fails closed.
rm -rf "$tmp/work"
git clone -q --branch bump "file://$tmp/origin.git" "$tmp/work"
git -C "$tmp/work" remote set-url origin "$tmp/missing.git"
status=0
(cd "$tmp/work" && bash "$script" >/dev/null 2>&1) || status=$?
if ((status != 0)); then pass 'unreachable origin fails'; else fail 'unreachable origin fails'; fi

if ((failures)); then exit 1; fi
