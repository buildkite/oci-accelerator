#!/usr/bin/env bash
set -euo pipefail

# Unit tests for the shared version helpers.
# shellcheck source=SCRIPTDIR/version.sh
. "$(dirname "$0")/version.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

failures=0
pass() { printf 'ok - %s\n' "$1"; }
fail() {
  printf 'not ok - %s\n' "$1" >&2
  failures=$((failures + 1))
}
expect() {
  local name=$1 expected=$2 status=0
  shift 2
  "$@" >/dev/null 2>&1 || status=$?
  if (((status == 0 && expected == 0) || (status != 0 && expected != 0))); then
    pass "$name"
  else
    fail "$name (status $status, wanted $expected)"
  fi
}

# Canonical versions parse, and everything else is rejected.
for valid in 0.0.0 0.1.0 1.2.3 10.20.30 123456789012345678901234567890.0.0; do
  expect "parse $valid" 0 parse_version "$valid"
done
for invalid in '' v1.2.3 1.2 1.2.3.4 01.2.3 1.02.3 1.2.03 1.2.3-rc.1 1.2.3+build ' 1.2.3' '1.2.3 ' $'1.2.3\r' $'1.2.3\n1.2.4' 1..3 a.b.c; do
  expect "reject $(printf %q "$invalid")" 1 parse_version "$invalid"
done

# read_version accepts a trailing newline but nothing else around the version.
printf '1.2.3\n' >"$tmp/VERSION"
if [[ $(read_version "$tmp/VERSION") == 1.2.3 ]]; then pass 'read trailing newline'; else fail 'read trailing newline'; fi
printf '1.2.3\r\n' >"$tmp/VERSION"
expect 'read rejects CRLF' 1 read_version "$tmp/VERSION"
expect 'read rejects missing file' 1 read_version "$tmp/missing"

# Comparisons are numeric per component, never lexical across components.
for pair in '0.0.1 0.0.0' '0.1.0 0.0.9' '1.0.0 0.99.99' '0.10.0 0.9.0' '0.0.10 0.0.9' \
  '10.0.0 9.0.0' '100000000000000000000.0.0 99999999999999999999.0.0'; do
  read -r a b <<< "$pair"
  expect "$a > $b" 0 version_greater "$a" "$b"
  expect "$b not > $a" 1 version_greater "$b" "$a"
done
expect 'equal is not greater' 1 version_greater 1.2.3 1.2.3

if ((failures)); then exit 1; fi
