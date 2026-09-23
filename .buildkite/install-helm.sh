#!/usr/bin/env bash
set -euo pipefail
destination=${1:?usage: install-helm.sh DESTINATION_DIRECTORY}
version=4.3.0
case "$(uname -m)" in
  x86_64) arch=amd64; checksum=86584a54def73570558f66f5111cc53dfed56689637ae32c1201205d494f54fb ;;
  aarch64) arch=arm64; checksum=31c5794dd55c66a51e6b7d2e2ac7a114ae8b1de41ff1d9ba51748ac973b06a08 ;;
  *) echo 'Unsupported Helm architecture' >&2; exit 1 ;;
esac
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT
curl --fail --location --silent --show-error --retry 3 --max-time 120 \
  "https://get.helm.sh/helm-v${version}-linux-${arch}.tar.gz" -o "$temporary/helm.tar.gz"
echo "$checksum  $temporary/helm.tar.gz" | sha256sum --check
tar -xzf "$temporary/helm.tar.gz" -C "$temporary"
mkdir -p "$destination"
install -m 0755 "$temporary/linux-$arch/helm" "$destination/helm"
