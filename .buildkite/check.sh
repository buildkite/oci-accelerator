#!/usr/bin/env bash
set -euo pipefail

# Run from the repository root, just like the Buildkite Docker plugin.
go version

echo '--- Checking Go formatting'
unformatted=$(gofmt -l version.go cmd internal integration)
if [[ -n "$unformatted" ]]; then
  printf 'Run gofmt on these files:\n%s\n' "$unformatted" >&2
  exit 1
fi

echo '--- Running go vet'
go vet ./...

echo '--- Building and validating the example configuration'
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT
# This disposable validation binary needs no VCS stamp. The Docker container's
# user can differ from the checkout owner, which Git rejects as unsafe.
go build -buildvcs=false -o "$temporary/oci-accelerator" ./cmd/oci-accelerator
"$temporary/oci-accelerator" --env-file .env.example --check
