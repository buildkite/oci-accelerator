#!/usr/bin/env bash
# Run against a locally built image. Docker must be accessible to this user.
set -euo pipefail
image=${1:?usage: container_smoke.sh IMAGE}
temporary=$(mktemp -d)
name="oci-accelerator-smoke-$$"
cleanup() {
  docker rm -f "$name" >/dev/null 2>&1 || true
  docker volume rm "$name" >/dev/null 2>&1 || true
  rm -rf "$temporary"
}
trap cleanup EXIT

cat > "$temporary/config.yaml" <<'YAML'
listen: 0.0.0.0:3128
registries: [registry-1.docker.io]
ca_dir: /var/lib/oci-accelerator/ca
cache:
  dir: /var/lib/oci-accelerator/cache
YAML
chmod 0755 "$temporary"
chmod 0444 "$temporary/config.yaml"
docker volume create "$name" >/dev/null
# This ownership step is also needed for Kubernetes volumes, whose mount roots
# need not inherit the image's ownership. Never change the process to run as root.
docker run --rm --user 0:0 --entrypoint sh \
  -v "$name:/var/lib/oci-accelerator" "$image" \
  -ec 'chown 10001:10001 /var/lib/oci-accelerator; chmod 0700 /var/lib/oci-accelerator'
docker run -d --name "$name" --read-only --cap-drop ALL \
  --security-opt no-new-privileges \
  -p 127.0.0.1::3128 \
  -v "$temporary/config.yaml:/etc/oci-accelerator.yaml:ro" \
  -v "$name:/var/lib/oci-accelerator" \
  "$image" --config /etc/oci-accelerator.yaml >/dev/null
test "$(docker exec "$name" id -u)" = 10001
docker exec "$name" test -s /etc/ssl/certs/ca-certificates.crt
fetch_ca() {
  local address
  address=$(docker port "$name" 3128/tcp)
  curl --fail --silent --show-error --retry 15 --retry-connrefused \
    --retry-delay 1 --max-time 5 "http://$address/ca.crt" > "$1"
  grep -q 'BEGIN CERTIFICATE' "$1"
}
fetch_ca "$temporary/before.crt"
docker stop --time 30 "$name" >/dev/null
test "$(docker inspect --format '{{.State.ExitCode}}' "$name")" = 0
docker start "$name" >/dev/null
fetch_ca "$temporary/after.crt"
cmp "$temporary/before.crt" "$temporary/after.crt"
echo 'PASS: non-root, read-only root filesystem, trust roots, persistent CA, graceful restart'
