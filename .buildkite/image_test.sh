#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
export PATH="$tmp/bin:$PATH"
export GIT_CONFIG_GLOBAL="$tmp/gitconfig" GIT_CONFIG_NOSYSTEM=1
unset BUILDKITE_BRANCH BUILDKITE_TAG BUILDKITE_COMMIT BUILDKITE_BUILD_NUMBER BUILDKITE_PULL_REQUEST
mkdir "$tmp/bin"

cat >"$tmp/bin/buildkite-agent" <<'EOF'
#!/usr/bin/env bash
printf 'secret\n' >>"$TEST_LOG"
printf token
EOF
cat >"$tmp/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -eu
printf 'docker %s\n' "$*" >>"$TEST_LOG"
case " $* " in
  *' login '*) cat >/dev/null; exit "${LOGIN_STATUS:-0}" ;;
  *' imagetools inspect '*)
    ref=${4}
    key=${ref##*:}
    variable="MANIFEST_${key//[^a-zA-Z0-9]/_}"
    result=${!variable:-missing}
    case "$result" in
      missing_then:*)
        marker="$DOCKER_CONFIG/seen-${key//[^a-zA-Z0-9]/_}"
        if [[ ! -e "$marker" ]]; then touch "$marker"; echo "ERROR: $ref: not found" >&2; exit 1; fi
        cat "${result#missing_then:}" ;;
      missing) echo "ERROR: $ref: not found" >&2; exit 1 ;;
      auth) echo 'ERROR: unauthorized' >&2; exit 1 ;;
      network) echo 'ERROR: network timeout' >&2; exit 1 ;;
      *) cat "$result" ;;
    esac ;;
  *' buildx build '*) exit "${BUILD_STATUS:-0}" ;;
esac
EOF
chmod +x "$tmp/bin/"*

failures=0
pass() { printf 'ok - %s\n' "$1"; }
fail() {
  printf 'not ok - %s\n' "$1" >&2
  failures=$((failures + 1))
}
run() {
  local name=$1 expected=$2 output status=0
  shift 2
  : >"$TEST_LOG"
  output=$("$@" 2>&1) || status=$?
  if (((status == 0 && expected == 0) || (status != 0 && expected != 0))); then
    pass "$name"
  else
    fail "$name (status $status, wanted $expected): $output"
  fi
}
contains() {
  local name=$1 needle=$2
  if grep -Fq -- "$needle" "$TEST_LOG"; then pass "$name"; else fail "$name (missing $needle)"; fi
}
not_contains() {
  local name=$1 needle=$2
  if ! grep -Fq -- "$needle" "$TEST_LOG"; then pass "$name"; else fail "$name (unexpected $needle)"; fi
}

export TEST_LOG="$tmp/actions"
git config --global user.name 'Image Test'
git config --global user.email image-test@example.invalid
git init -q --bare "$tmp/origin.git"
git -C "$tmp/origin.git" symbolic-ref HEAD refs/heads/main
git init -q -b main "$tmp/seed"
cp "$root/.buildkite/image.sh" "$root/.buildkite/release.sh" "$tmp/seed/"
mkdir "$tmp/seed/.buildkite"
mv "$tmp/seed/image.sh" "$tmp/seed/release.sh" "$tmp/seed/.buildkite/"
echo one >"$tmp/seed/content"
git -C "$tmp/seed" add .
git -C "$tmp/seed" commit -qm one
old=$(git -C "$tmp/seed" rev-parse HEAD)
git -C "$tmp/seed" tag -a v0.9.0 -m old
echo two >>"$tmp/seed/content"
git -C "$tmp/seed" commit -qam two
tip=$(git -C "$tmp/seed" rev-parse HEAD)
git -C "$tmp/seed" tag -a v0.10.0 -m current
git -C "$tmp/seed" tag v0.11.0
git -C "$tmp/seed" checkout -qb off-main
echo off-main >>"$tmp/seed/content"
git -C "$tmp/seed" commit -qam off-main
off_main=$(git -C "$tmp/seed" rev-parse HEAD)
git -C "$tmp/seed" tag -a v0.12.0 -m off-main
git -C "$tmp/seed" checkout -q main
git -C "$tmp/seed" remote add origin "$tmp/origin.git"
git -C "$tmp/seed" push -q origin main --tags
git clone -q --depth 1 "file://$tmp/origin.git" "$tmp/work"

cd "$tmp/work"
run 'check needs no credentials' 0 bash .buildkite/image.sh check
not_contains 'check does not request secret' secret

run 'main publishes' 0 env BUILDKITE_BRANCH=main BUILDKITE_COMMIT="$tip" BUILDKITE_BUILD_NUMBER=12 bash .buildkite/image.sh publish
contains 'main publishes immutable build tag' 'buildkite/oci-accelerator:build-12'
contains 'main publishes edge' 'buildkite/oci-accelerator:edge'
not_contains 'main never publishes latest' 'buildkite/oci-accelerator:latest'

assert_precredential_rejection() {
  local name=$1
  shift
  run "$name" 1 env "$@" bash .buildkite/image.sh publish
  not_contains "$name is before credentials" secret
}
assert_precredential_rejection 'PR rejected' BUILDKITE_PULL_REQUEST=4 BUILDKITE_BRANCH=main BUILDKITE_COMMIT="$tip" BUILDKITE_BUILD_NUMBER=1
assert_precredential_rejection 'tag build rejected for development' BUILDKITE_BRANCH=main BUILDKITE_TAG=v0.10.0 BUILDKITE_COMMIT="$tip" BUILDKITE_BUILD_NUMBER=1
assert_precredential_rejection 'commit mismatch rejected' BUILDKITE_BRANCH=main BUILDKITE_COMMIT="$old" BUILDKITE_BUILD_NUMBER=1
echo dirty >untracked
assert_precredential_rejection 'dirty checkout rejected' BUILDKITE_BRANCH=main BUILDKITE_COMMIT="$tip" BUILDKITE_BUILD_NUMBER=1
rm untracked

git fetch -q origin refs/tags/v0.9.0
git checkout -q "$old"
run 'stale main skips' 0 env BUILDKITE_BRANCH=main BUILDKITE_COMMIT="$old" BUILDKITE_BUILD_NUMBER=2 bash .buildkite/image.sh publish
not_contains 'stale main skips credentials' secret
git checkout -q main

manifest() { jq -n --arg v "$1" --arg r "$2" --arg d "sha256:$3" '{schemaVersion:2,mediaType:"application/vnd.oci.image.index.v1+json",digest:$d,annotations:{"org.opencontainers.image.version":$v,"org.opencontainers.image.revision":$r},manifests:[{platform:{os:"linux",architecture:"amd64"}},{platform:{os:"linux",architecture:"arm64"}}]}' >"$4"; }
hex_a=$(printf 'a%.0s' {1..64})
hex_b=$(printf 'b%.0s' {1..64})
manifest 0.10.0 "$tip" "$hex_a" "$tmp/v010"
manifest 0.9.0 old "$hex_b" "$tmp/v09"
manifest 0.10.0 "$tip" "$hex_b" "$tmp/v010b"
manifest 1.0.0 x "$hex_b" "$tmp/v1"

# Release ancestry and tag shape are checked after unshallowing and before login.
for tag in v01.2.3 v1.2 v1.2.3-rc.1 v1.2.3+build 1.2.3; do
  run "reject noncanonical $tag" 1 env BUILDKITE_TAG="$tag" BUILDKITE_COMMIT="$tip" bash .buildkite/image.sh release
  not_contains 'invalid tag stops before credentials' secret
done
run 'release PR rejected' 1 env BUILDKITE_TAG=v0.10.0 BUILDKITE_COMMIT="$tip" BUILDKITE_PULL_REQUEST=42 bash .buildkite/image.sh release
not_contains 'release PR stops before credentials' secret
run 'lightweight release rejected' 1 env BUILDKITE_TAG=v0.11.0 BUILDKITE_COMMIT="$tip" bash .buildkite/image.sh release
not_contains 'lightweight tag stops before credentials' secret
run 'tag pointing to different commit rejected' 1 env BUILDKITE_TAG=v0.9.0 BUILDKITE_COMMIT="$tip" bash .buildkite/image.sh release
not_contains 'tag mismatch stops before credentials' secret
git fetch -q origin refs/tags/v0.12.0
git checkout -q "$off_main"
run 'off-main release rejected' 1 env BUILDKITE_TAG=v0.12.0 BUILDKITE_COMMIT="$off_main" bash .buildkite/image.sh release
not_contains 'off-main tag stops before credentials' secret
git checkout -q "$old"
manifest 0.9.0 "$old" "$hex_b" "$tmp/old-release"
run 'older main commit is releasable' 0 env BUILDKITE_TAG=v0.9.0 BUILDKITE_COMMIT="$old" MANIFEST_0_9_0="$tmp/old-release" MANIFEST_latest="$tmp/v010" bash .buildkite/image.sh release
contains 'older main release passes ancestry gate' secret
git checkout -q main
run 'valid annotated release reaches credentials' 0 env BUILDKITE_TAG=v0.10.0 BUILDKITE_COMMIT="$tip" MANIFEST_0_10_0="$tmp/v010" MANIFEST_latest="$tmp/v010" bash .buildkite/image.sh release
contains 'valid release reads credentials' secret
# The manifest tests below exercise release.sh directly after image.sh's checkout gate.
release_case() {
  local name=$1 expected=$2 version=$3 revision=$4 current=$5 latest=$6
  shift 6
  mkdir -p "$tmp/dock"
  run "$name" "$expected" env DOCKER_CONFIG="$tmp/dock" "MANIFEST_${version//./_}=$current" "MANIFEST_latest=$latest" "$@" bash .buildkite/release.sh example/image "$version" "$revision" builder
}

release_case 'existing matching retry' 0 0.10.0 "$tip" "$tmp/v010" "$tmp/v010"
not_contains 'matching retry does not rebuild' 'buildx build'
release_case 'mismatched identity refused' 1 0.10.0 wrong "$tmp/v010" missing
not_contains 'identity refusal does not rebuild' 'buildx build'
release_case '0.10 promotes over 0.9' 0 0.10.0 "$tip" "$tmp/v010" "$tmp/v09"
contains 'promotion uses digest identity' "example/image@sha256:$hex_a"
release_case '0.9 cannot replace 0.10' 0 0.9.0 old "$tmp/v09" "$tmp/v010"
not_contains 'older minor is not promoted' 'imagetools create'
release_case 'patch cannot replace major' 0 0.10.0 "$tip" "$tmp/v010" "$tmp/v1"
not_contains 'lower major is not promoted' 'imagetools create'
release_case 'same version digest conflict' 1 0.10.0 "$tip" "$tmp/v010" "$tmp/v010b"
jq 'del(.annotations)' "$tmp/v09" >"$tmp/legacy"
release_case 'legacy latest is promoted over' 0 0.10.0 "$tip" "$tmp/v010" "$tmp/legacy"
contains 'legacy promotion happens' 'imagetools create'

manifest 0.10.1 "$tip" "$hex_a" "$tmp/patch"
release_case 'higher patch advances latest' 0 0.10.1 "$tip" "$tmp/patch" "$tmp/v010"
contains 'patch promotion happens' 'imagetools create'
release_case 'lower patch retains latest' 0 0.10.0 "$tip" "$tmp/v010" "$tmp/patch"
not_contains 'lower patch is not promoted' 'imagetools create'
release_case 'higher major advances latest' 0 1.0.0 x "$tmp/v1" "$tmp/v010"
contains 'major promotion happens' 'imagetools create'
manifest bad x "$hex_b" "$tmp/invalid-latest"
release_case 'invalid latest version refused' 1 0.10.0 "$tip" "$tmp/v010" "$tmp/invalid-latest"
not_contains 'invalid latest is not replaced' 'imagetools create'
jq '.manifests |= .[:1]' "$tmp/v010" >"$tmp/incomplete"
release_case 'incomplete release refused' 1 0.10.0 "$tip" "$tmp/incomplete" missing
not_contains 'incomplete release cannot be promoted' 'imagetools create'

rm -f "$tmp/dock/seen-0_10_0"
release_case 'new release builds with metadata' 0 0.10.0 "$tip" "missing_then:$tmp/v010" missing
contains 'release builds both platforms' '--platform linux/amd64,linux/arm64'
contains 'release supplies version label' '--label org.opencontainers.image.version=0.10.0'
contains 'release supplies revision annotation' "--annotation index:org.opencontainers.image.revision=$tip"

release_case 'failed build stops publication' 1 0.10.0 "$tip" missing missing BUILD_STATUS=1
not_contains 'failed build has no promotion' 'imagetools create'
release_case 'retry after version publication' 0 0.10.0 "$tip" "$tmp/v010" missing
not_contains 'publication retry does not rebuild' 'buildx build'
contains 'publication retry promotes latest' 'imagetools create'
release_case 'registry auth fails closed' 1 0.10.0 "$tip" auth missing
not_contains 'auth error cannot build' 'buildx build'
release_case 'registry network fails closed' 1 0.10.0 "$tip" network missing
not_contains 'network error cannot build' 'buildx build'
release_case 'latest inspection failure stops promotion' 1 0.10.0 "$tip" "$tmp/v010" network
not_contains 'latest inspection error cannot promote' 'imagetools create'

if ((failures)); then exit 1; fi
