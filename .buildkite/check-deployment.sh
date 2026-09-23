#!/usr/bin/env bash
set -euo pipefail
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT
bash .buildkite/install-helm.sh "$temporary"
export PATH="$temporary:$PATH"
helm lint deploy/helm/oci-accelerator --strict --set image.tag=validation
OCI_ACCELERATOR_DEPLOYMENT_TESTS=1 go test -count=1 ./integration \
  -run 'Test(PlainDeploymentSafety|HelmDeploymentParity|HelmDeploymentOptions|HelmBootstrapNamesStayBoundedAcrossRevisions)$'
