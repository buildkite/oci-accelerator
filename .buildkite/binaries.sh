#!/usr/bin/env bash
set -euo pipefail

# Run from the repository root in the pinned Go container. Release eligibility
# and version progression are validated separately by image.sh.
version=$(cat VERSION)
if [[ ! "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo 'VERSION must be a vMAJOR.MINOR.PATCH archive version' >&2
  exit 1
fi

# Keep even failed builds outside the checkout so publication's strict clean
# checkout check is unaffected. Upload before the EXIT trap removes the files.
temporary=$(mktemp -d /tmp/oci-accelerator-binaries.XXXXXXXX)
trap 'rm -rf "$temporary"' EXIT
mkdir "$temporary/artifacts" "$temporary/bin"
export CGO_ENABLED=0

for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  echo "--- Packaging $target"
  GOOS=${target%/*} GOARCH=${target#*/} go build -trimpath -buildvcs=false \
    -o "$temporary/bin/oci-accelerator" ./cmd/oci-accelerator
  tar --format=ustar --mtime=@0 --owner=0 --group=0 --numeric-owner --mode=0755 \
    -C "$temporary/bin" -cf - oci-accelerator \
    | gzip -n > "$temporary/artifacts/oci-accelerator-${target%/*}-${target#*/}-$version.tar.gz"
done

(
  cd "$temporary/artifacts"
  sha256sum *.tar.gz > "oci-accelerator-$version.SHA256SUMS"
  buildkite-agent artifact upload '*'
)
