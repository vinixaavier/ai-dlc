#!/usr/bin/env bash
set -Eeuo pipefail

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
source "$ROOT_DIR/scripts/agent-platform-versions.sh"
SOURCE_DIR="${AGENT_PLATFORM_SOURCE_DIR:-$ROOT_DIR/vendor/agent-platform}"
PUBLISH_REGISTRY="${PUBLISH_REGISTRY:-localhost:5002}"
BUILD_PLATFORM="${BUILD_PLATFORM:-linux/amd64}"
BUILD_ARCH="${BUILD_PLATFORM#linux/}"
CRANE_BIN="${CRANE_BIN:-$(go env GOPATH)/bin/crane}"
BUILDX_BUILDER="${BUILDX_BUILDER:-kagent-builder-v0.23.0}"
FORCE_REBUILD="${FORCE_REBUILD:-false}"

for command in docker git gh go curl make; do
  command -v "$command" >/dev/null || { echo "Missing command: $command" >&2; exit 1; }
done
if [[ ! -x "$CRANE_BIN" ]]; then
  GOBIN="$(dirname -- "$CRANE_BIN")" go install github.com/google/go-containerregistry/cmd/crane@v0.20.7
fi
mkdir -p "$SOURCE_DIR"

prepare_source() {
  local component="$1" repository="$2" commit="$3" destination="$SOURCE_DIR/$1"
  local fetch_ref="${4:-$commit}"
  if [[ ! -d "$destination/.git" ]]; then
    gh repo clone "$repository" "$destination" -- --no-checkout --filter=blob:none
    git -C "$destination" fetch --depth=1 origin "$fetch_ref"
    git -C "$destination" checkout --detach "$commit"
  fi
  [[ "$(git -C "$destination" rev-parse HEAD)" == "$commit" ]] || {
    echo "Wrong source revision: $destination (expected $commit); use a new AGENT_PLATFORM_SOURCE_DIR" >&2
    exit 1
  }
}

image_needed() {
  [[ "$FORCE_REBUILD" == true ]] || ! "$CRANE_BIN" digest "$1" >/dev/null 2>&1
}

build_image() {
  local image="$1" commit="$2" dockerfile="$3" context="$4"
  shift 4
  if image_needed "$image"; then
    docker buildx build --builder "$BUILDX_BUILDER" --load --platform "$BUILD_PLATFORM" \
      --label "org.opencontainers.image.revision=$commit" \
      --tag "$image" --file "$dockerfile" "$@" "$context"
    docker push "$image"
  fi
  "$CRANE_BIN" digest "$image"
}

publish_kagent() {
  prepare_source kagent kagent-dev/kagent "$KAGENT_COMMIT"
  local source="$SOURCE_DIR/kagent" image flags
  flags="-X github.com/kagent-dev/kagent/go/core/internal/version.Version=$KAGENT_TAG -X github.com/kagent-dev/kagent/go/core/internal/version.GitCommit=$KAGENT_COMMIT"
  for component in controller golang-adk; do
    image="$PUBLISH_REGISTRY/kagent-dev/kagent/$component:$KAGENT_TAG"
    local package=core/cmd/controller/main.go
    [[ "$component" != golang-adk ]] || package=adk/cmd/main.go
    build_image "$image" "$KAGENT_COMMIT" "$source/go/Dockerfile" "$source/go" \
      --build-arg "BUILD_PACKAGE=$package" --build-arg "VERSION=$KAGENT_TAG" --build-arg "LDFLAGS=$flags"
  done
  build_image "$PUBLISH_REGISTRY/kagent-dev/kagent/ui:$KAGENT_TAG" "$KAGENT_COMMIT" \
    "$source/ui/Dockerfile" "$source/ui" --build-arg "VERSION=$KAGENT_TAG"
  for component in codex claude; do
    build_image "$PUBLISH_REGISTRY/kagent-dev/kagent/$component-harness:$KAGENT_HARNESS_TAG" "$KAGENT_COMMIT" \
      "$ROOT_DIR/$component-harness/Dockerfile" "$ROOT_DIR" \
      --build-context "kagent=$source" \
      --build-arg "BUILDPLATFORM=$BUILD_PLATFORM" \
      --build-arg "TARGETPLATFORM=$BUILD_PLATFORM" \
      --build-arg "TARGETARCH=$BUILD_ARCH" --build-arg "TARGETOS=linux"
  done
  build_image "$PUBLISH_REGISTRY/ai-dlc/pi-harness:$KAGENT_HARNESS_TAG" "$KAGENT_COMMIT" \
    "$ROOT_DIR/pi-harness/Dockerfile" "$ROOT_DIR" --build-context "kagent=$source"
}

publish_tools() {
  prepare_source tools kagent-dev/tools "$TOOLS_COMMIT"
  local source="$SOURCE_DIR/tools"
  local image="$PUBLISH_REGISTRY/kagent-dev/kagent/tools:$TOOLS_TAG"
  if image_needed "$image"; then
    build_image "$image" "$TOOLS_COMMIT" "$source/Dockerfile" "$source" \
      --build-arg "VERSION=$TOOLS_TAG" \
      --build-arg TOOLS_ISTIO_VERSION=1.31.1 \
      --build-arg TOOLS_ARGO_ROLLOUTS_VERSION=1.10.0 \
      --build-arg TOOLS_KUBECTL_VERSION=1.37.0 \
      --build-arg TOOLS_HELM_VERSION=4.3.0 \
      --build-arg TOOLS_CILIUM_VERSION=0.20.0
    return
  fi
  "$CRANE_BIN" digest "$image"
}

publish_substrate() {
  prepare_source substrate "$SUBSTRATE_REPO" "$SUBSTRATE_COMMIT" "$SUBSTRATE_TAG"
  local source="$SOURCE_DIR/substrate"
  make -C "$source" build-images \
    "KO_DOCKER_REPO=$PUBLISH_REGISTRY/kagent-dev/substrate" \
    "VERSION=$SUBSTRATE_TAG" "KO_DEFAULTPLATFORMS=$BUILD_PLATFORM"
  local agentgateway_image="$PUBLISH_REGISTRY/kagent-dev/substrate/agentgateway:v0.0.0-alpha.168b05d6"
  if image_needed "$agentgateway_image"; then
    "$CRANE_BIN" copy --platform "$BUILD_PLATFORM" \
      cr.agentgateway.dev/agentgateway:v0.0.0-alpha.168b05d6@sha256:5e8384d1fcfdc443a3ad5155ea6a64dc7953bde162df7f5f8e131a5c19ae08a3 \
      "$agentgateway_image"
  fi
  "$CRANE_BIN" digest "$agentgateway_image"
}

publish_agentgateway() {
  prepare_source agentgateway agentgateway/agentgateway "$AGENTGATEWAY_COMMIT"
  local source="$SOURCE_DIR/agentgateway" controller="$PUBLISH_REGISTRY/agentgateway-controller:$AGENTGATEWAY_TAG"
  build_image "$PUBLISH_REGISTRY/agentgateway:$AGENTGATEWAY_TAG" "$AGENTGATEWAY_COMMIT" \
    "$source/Dockerfile" "$source" --build-arg "VERSION=$AGENTGATEWAY_TAG" \
    --build-arg "GIT_REVISION=$AGENTGATEWAY_COMMIT"
  if image_needed "$controller"; then
    make -C "$source/controller" agentgateway-controller-docker \
      IMAGE_REGISTRY="$PUBLISH_REGISTRY" VERSION="$AGENTGATEWAY_TAG" GOARCH="$BUILD_ARCH" \
      BUILDX_BUILD="docker buildx build --builder $BUILDX_BUILDER --label org.opencontainers.image.revision=$AGENTGATEWAY_COMMIT"
    docker push "$controller"
  fi
  "$CRANE_BIN" digest "$controller"
}

publish_agentregistry() {
  prepare_source agentregistry agentregistry-dev/agentregistry "$AGENTREGISTRY_COMMIT"
  local source="$SOURCE_DIR/agentregistry"
  local image="$PUBLISH_REGISTRY/agentregistry-dev/agentregistry/server:$AGENTREGISTRY_TAG"
  local flags="-s -w -X github.com/agentregistry-dev/agentregistry/pkg/version.Version=$AGENTREGISTRY_TAG -X github.com/agentregistry-dev/agentregistry/pkg/version.GitCommit=$AGENTREGISTRY_COMMIT -X github.com/agentregistry-dev/agentregistry/pkg/version.BuildDate=$(date -u +%Y-%m-%d)"
  build_image "$image" "$AGENTREGISTRY_COMMIT" "$source/docker/server.Dockerfile" "$source" \
    --build-arg "LDFLAGS=$flags"
}

curl --fail --silent --show-error "http://$PUBLISH_REGISTRY/v2/" >/dev/null
docker buildx inspect "$BUILDX_BUILDER" >/dev/null 2>&1 || \
  docker buildx create --name "$BUILDX_BUILDER" --driver docker-container --driver-opt network=host
docker buildx inspect --bootstrap "$BUILDX_BUILDER" >/dev/null

if [[ "$#" == 0 ]]; then
  set -- kagent tools substrate agentgateway agentregistry
fi
for component in "$@"; do
  case "$component" in
    kagent) publish_kagent ;;
    tools) publish_tools ;;
    substrate) publish_substrate ;;
    agentgateway) publish_agentgateway ;;
    agentregistry) publish_agentregistry ;;
    *) echo "Unknown component: $component (kagent, tools, substrate, agentgateway, agentregistry)" >&2; exit 1 ;;
  esac
done
