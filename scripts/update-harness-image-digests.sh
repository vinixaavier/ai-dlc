#!/usr/bin/env bash
set -Eeuo pipefail

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
source "$ROOT_DIR/scripts/agent-platform-versions.sh"
PUBLISH_REGISTRY="${PUBLISH_REGISTRY:-localhost:5002}"
CRANE_BIN="${CRANE_BIN:-$(go env GOPATH)/bin/crane}"
YQ_BIN="${YQ_BIN:-$(go env GOPATH)/bin/yq}"
if [[ ! -x "$YQ_BIN" ]]; then
  GOBIN="$(dirname -- "$YQ_BIN")" go install github.com/mikefarah/yq/v4@v4.47.2
fi
for harness in adk pi codex claude; do
  case "$harness" in
    adk) repository=kagent-dev/kagent/golang-adk; tag="$KAGENT_TAG" ;;
    pi) repository=ai-dlc/pi-harness; tag="$KAGENT_HARNESS_TAG" ;;
    *) repository="kagent-dev/kagent/$harness-harness"; tag="$KAGENT_HARNESS_TAG" ;;
  esac
  digest="$("$CRANE_BIN" digest "$PUBLISH_REGISTRY/$repository:$tag")"
  export HARNESS_IMAGE="localhost:5000/$repository:$tag@$digest"
  "$YQ_BIN" -i '(select(.kind == "Harness") | .spec.workload.image) = strenv(HARNESS_IMAGE)' \
    "$ROOT_DIR/argocd/apps/$harness-harness.yaml"
  printf '%s: %s\n' "$harness" "$HARNESS_IMAGE"
done
