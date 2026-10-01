#!/usr/bin/env bash
set -euo pipefail

# Isolate the test in a temporary directory with private Git config and a clean Buildkite environment.
root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
export PATH="$tmp/bin:$PATH"
export GIT_CONFIG_GLOBAL="$tmp/gitconfig" GIT_CONFIG_NOSYSTEM=1
unset BUILDKITE_BRANCH BUILDKITE_TAG BUILDKITE_COMMIT BUILDKITE_BUILD_NUMBER BUILDKITE_PULL_REQUEST
mkdir "$tmp/bin"

# Stub buildkite-agent so every secret read and annotation is recorded in the test log.
cat >"$tmp/bin/buildkite-agent" <<'EOF'
#!/usr/bin/env bash
case "$1" in
  secret) printf 'secret\n' >>"$TEST_LOG"; printf token ;;
  *) printf 'buildkite-agent %s\n' "$*" >>"$TEST_LOG" ;;
esac
EOF

# Stub docker to log each call and return canned manifests or registry errors.
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

# Test helpers that run commands, check exit status, and search the action log.
failures=0
pass() { printf 'ok - %s\n' "$1"; }
fail() {
  printf 'not ok - %s\n' "$1" >&2
  failures=$((failures + 1))
}
run() {
  local name=$1 expected=$2 status=0
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
said() {
  local name=$1 needle=$2
  if [[ "$output" == *"$needle"* ]]; then pass "$name"; else fail "$name (output lacks $needle): $output"; fi
}

# Build a fixture origin whose main history introduces VERSION, bumps it, leaves
# it unchanged, and downgrades it, plus an off-main branch that bumps it.
export TEST_LOG="$tmp/actions"
git config --global user.name 'Image Test'
git config --global user.email image-test@example.invalid
git init -q --bare "$tmp/origin.git"
git -C "$tmp/origin.git" symbolic-ref HEAD refs/heads/main
git init -q -b main "$tmp/seed"
mkdir "$tmp/seed/.buildkite"
cp "$root/.buildkite/image.sh" "$root/.buildkite/release.sh" "$root/.buildkite/version.sh" "$tmp/seed/.buildkite/"
echo one >"$tmp/seed/content"
git -C "$tmp/seed" add .
git -C "$tmp/seed" commit -qm before-version
commit_content() {
  echo "$1" >>"$tmp/seed/content"
  git -C "$tmp/seed" commit -qam "$1"
  git -C "$tmp/seed" rev-parse HEAD
}
commit_version() {
  printf '%s\n' "$1" >"$tmp/seed/VERSION"
  git -C "$tmp/seed" add VERSION
  commit_content "version $1 on $(git -C "$tmp/seed" branch --show-current)"
}
first=$(commit_version 0.9.0)
plain=$(commit_content plain)
bump=$(commit_version 0.10.0)
down=$(commit_version 0.9.5)
tip=$(commit_content tip)
git -C "$tmp/seed" checkout -qb off-main
off_main=$(commit_version 1.0.0)
git -C "$tmp/seed" checkout -q main
git -C "$tmp/seed" remote add origin "$tmp/origin.git"
git -C "$tmp/seed" push -q origin main off-main

# clone_at makes a fresh shallow checkout of one commit, like a Buildkite agent.
clone_at() {
  cd "$tmp"
  rm -rf "$tmp/work"
  git clone -q --depth 1 "file://$tmp/origin.git" "$tmp/work"
  cd "$tmp/work"
  git fetch -q --depth 1 origin "$1"
  git checkout -q "$1"
}

# Check mode builds without reading credentials.
clone_at "$tip"
run 'check needs no credentials' 0 bash .buildkite/image.sh check
not_contains 'check does not request secret' secret
contains 'check embeds development version' "--build-arg VERSION=0.9.5-dev+${tip:0:12}"

# Main builds publish build-N and edge tags but never latest.
run 'main publishes' 0 env BUILDKITE_BRANCH=main BUILDKITE_COMMIT="$tip" BUILDKITE_BUILD_NUMBER=12 bash .buildkite/image.sh publish
contains 'main publishes immutable build tag' 'buildkite/oci-accelerator:build-12'
contains 'main publishes edge' 'buildkite/oci-accelerator:edge'
not_contains 'main never publishes latest' 'buildkite/oci-accelerator:latest'
contains 'main embeds development version' "--build-arg VERSION=0.9.5-dev+${tip:0:12}"

# A malformed VERSION file stops check mode before building.
for bad in 'v0.1.0' '0.1' '01.0.0' '0.1.0 extra'; do
  printf '%s\n' "$bad" >VERSION
  run "VERSION $bad rejected" 1 bash .buildkite/image.sh check
  said "VERSION $bad explains" 'canonical MAJOR.MINOR.PATCH'
  not_contains "VERSION $bad creates no builder" 'buildx create'
done
git checkout -q VERSION

# Invalid publish and release contexts fail before any secret is read.
assert_precredential_rejection() {
  local name=$1 mode=$2
  shift 2
  run "$name" 1 env "$@" bash .buildkite/image.sh "$mode"
  not_contains "$name is before credentials" secret
}
for mode in publish release; do
  assert_precredential_rejection "$mode PR rejected" "$mode" BUILDKITE_PULL_REQUEST=4 BUILDKITE_BRANCH=main BUILDKITE_COMMIT="$tip" BUILDKITE_BUILD_NUMBER=1
  assert_precredential_rejection "$mode tag build rejected" "$mode" BUILDKITE_BRANCH=main BUILDKITE_TAG=v0.10.0 BUILDKITE_COMMIT="$tip" BUILDKITE_BUILD_NUMBER=1
  assert_precredential_rejection "$mode feature branch rejected" "$mode" BUILDKITE_BRANCH=feature BUILDKITE_COMMIT="$tip" BUILDKITE_BUILD_NUMBER=1
  assert_precredential_rejection "$mode commit mismatch rejected" "$mode" BUILDKITE_BRANCH=main BUILDKITE_COMMIT="$plain" BUILDKITE_BUILD_NUMBER=1
  echo dirty >untracked
  assert_precredential_rejection "$mode dirty checkout rejected" "$mode" BUILDKITE_BRANCH=main BUILDKITE_COMMIT="$tip" BUILDKITE_BUILD_NUMBER=1
  rm untracked
done

# A build of an older main commit skips development publishing without reading credentials.
clone_at "$plain"
run 'stale main skips' 0 env BUILDKITE_BRANCH=main BUILDKITE_COMMIT="$plain" BUILDKITE_BUILD_NUMBER=2 bash .buildkite/image.sh publish
not_contains 'stale main skips credentials' secret

# Write fake multi-platform image index manifests for the docker stub to return.
manifest() { jq -n --arg v "$1" --arg r "$2" --arg d "sha256:$3" '{schemaVersion:2,mediaType:"application/vnd.oci.image.index.v1+json",digest:$d,annotations:{"org.opencontainers.image.version":$v,"org.opencontainers.image.revision":$r},manifests:[{platform:{os:"linux",architecture:"amd64"}},{platform:{os:"linux",architecture:"arm64"}}]}' >"$4"; }
hex_a=$(printf 'a%.0s' {1..64})
hex_b=$(printf 'b%.0s' {1..64})
manifest 0.10.0 "$tip" "$hex_a" "$tmp/v010"
manifest 0.9.0 old "$hex_b" "$tmp/v09"
manifest 0.10.0 "$tip" "$hex_b" "$tmp/v010b"
manifest 1.0.0 x "$hex_b" "$tmp/v1"
manifest 0.10.0 "$bump" "$hex_a" "$tmp/bump-release"
manifest 0.9.0 "$first" "$hex_b" "$tmp/first-release"

# main_build runs image.sh in the given mode from a fresh shallow checkout of a main commit.
main_build() {
  local name=$1 expected=$2 mode=$3 commit=$4
  shift 4
  clone_at "$commit"
  run "$name" "$expected" env BUILDKITE_BRANCH=main BUILDKITE_COMMIT="$commit" BUILDKITE_BUILD_NUMBER=3 "$@" bash .buildkite/image.sh "$mode"
}

# The commit that changes VERSION publishes it, even when main has moved on.
main_build 'older bump commit releases' 0 release "$bump" MANIFEST_0_10_0="missing_then:$tmp/bump-release" MANIFEST_latest="$tmp/first-release"
contains 'bump release reads credentials' secret
contains 'bump release builds both platforms' '--platform linux/amd64,linux/arm64'
contains 'bump release embeds exact version' '--build-arg VERSION=0.10.0'
contains 'bump release records its commit' "--annotation index:org.opencontainers.image.revision=$bump"
contains 'bump release advances latest' "docker.io/buildkite/oci-accelerator@sha256:$hex_a"
main_build 'first VERSION releases' 0 release "$first" MANIFEST_0_9_0="missing_then:$tmp/first-release"
contains 'first VERSION builds' '--build-arg VERSION=0.9.0'
main_build 'bump retry reuses the image' 0 release "$bump" MANIFEST_0_10_0="$tmp/bump-release" MANIFEST_latest="$tmp/bump-release"
not_contains 'bump retry does not rebuild' 'buildx build'
main_build 'bump with foreign image refused' 1 release "$bump" MANIFEST_0_10_0="$tmp/v010" MANIFEST_latest=missing
said 'foreign image explains' 'does not match VERSION and the commit'
not_contains 'foreign image is not rebuilt' 'buildx build'

# Builds that do not change VERSION verify it was published and never build.
main_build 'unchanged VERSION verifies' 0 release "$plain" MANIFEST_0_9_0="$tmp/first-release"
said 'unchanged VERSION reports publication' 'Version 0.9.0 is already published'
not_contains 'unchanged VERSION does not build' 'buildx build'
not_contains 'published version is not annotated' 'annotate'
main_build 'missed release warns without failing' 0 release "$tip" MANIFEST_0_9_5=missing
contains 'missed release annotates a warning' 'buildkite-agent annotate --style warning --context missing-release'
contains 'missed release names the bump commit' "$down"
not_contains 'missed release does not build' 'buildx build'
main_build 'missed release check fails closed' 1 release "$tip" MANIFEST_0_9_5=network
not_contains 'registry error is not annotated' 'annotate'

# Downgrades, off-main commits, and malformed versions fail before credentials.
main_build 'downgrade rejected' 1 release "$down"
said 'downgrade explains' 'VERSION 0.9.5 must be greater than the previous 0.10.0'
not_contains 'downgrade stops before credentials' secret
main_build 'off-main release rejected' 1 release "$off_main"
said 'off-main release explains' 'Release requires a build commit in main history'
not_contains 'off-main release stops before credentials' secret
bad=$(commit_version v1.0.0)
git -C "$tmp/seed" push -q origin main
for mode in publish release; do
  main_build "malformed VERSION stops $mode" 1 "$mode" "$bad"
  said "malformed VERSION explains in $mode" 'canonical MAJOR.MINOR.PATCH'
  not_contains "malformed VERSION in $mode stops before credentials" secret
done
cd "$tmp"

# The manifest tests below exercise release.sh directly after image.sh's checkout gate.
release_case() {
  local name=$1 expected=$2 version=$3 revision=$4 current=$5 latest=$6
  shift 6
  mkdir -p "$tmp/dock"
  run "$name" "$expected" env DOCKER_CONFIG="$tmp/dock" "MANIFEST_${version//./_}=$current" "MANIFEST_latest=$latest" "$@" bash "$tmp/seed/.buildkite/release.sh" publish example/image "$version" "$revision" builder
}

# Existing releases are reused or refused, and latest only moves to a newer version.
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

# Patch and major version comparisons, plus invalid or incomplete manifests.
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

# A new release builds both platforms with version and revision metadata.
rm -f "$tmp/dock/seen-0_10_0"
release_case 'new release builds with metadata' 0 0.10.0 "$tip" "missing_then:$tmp/v010" missing
contains 'release builds both platforms' '--platform linux/amd64,linux/arm64'
contains 'release supplies version label' '--label org.opencontainers.image.version=0.10.0'
contains 'release embeds exact version' '--build-arg VERSION=0.10.0'
contains 'release supplies revision annotation' "--annotation index:org.opencontainers.image.revision=$tip"

# Build failures, retries, and registry errors fail closed without rebuilding or promoting.
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

# Fail the script if any assertion failed.
if ((failures)); then exit 1; fi
