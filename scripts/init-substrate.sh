#!/usr/bin/env bash
set -Eeuo pipefail

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
source "$ROOT_DIR/scripts/agent-platform-versions.sh"
SOURCE_DIR="${AGENT_PLATFORM_SOURCE_DIR:-$ROOT_DIR/vendor/agent-platform}"
SUBSTRATE_SOURCE="$SOURCE_DIR/substrate"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-ai-dlc}"

[[ -d "$SUBSTRATE_SOURCE/.git" ]] || {
  echo 'Run scripts/publish-agent-platform.sh substrate before initializing Substrate' >&2
  exit 1
}
[[ "$(git -C "$SUBSTRATE_SOURCE" rev-parse HEAD)" == "$SUBSTRATE_COMMIT" ]] || {
  echo "Substrate source must be $SUBSTRATE_COMMIT" >&2
  exit 1
}
mkdir -p "$SOURCE_DIR/bin"
go -C "$SUBSTRATE_SOURCE" build -o "$SOURCE_DIR/bin/ate-setup" ./cmd/ate-setup

kubectl --context "kind-$KIND_CLUSTER_NAME" create namespace ate-system --dry-run=client -o yaml | kubectl --context "kind-$KIND_CLUSTER_NAME" apply -f -
for resource in jwt-authority-pool actor-id-ca-pool actor-id-ca-certs egress-mitm-ca-pool podcertificate-controller-cas api-server-env-vars api-authentication-config; do
  (cd "$SUBSTRATE_SOURCE" && "$SOURCE_DIR/bin/ate-setup" --kind --context "kind-$KIND_CLUSTER_NAME" create "$resource")
done
