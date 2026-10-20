#!/usr/bin/env bash

set -Eeuo pipefail

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-ai-dlc}"
NVKIND_CONFIG_TEMPLATE="${NVKIND_CONFIG_TEMPLATE:-${ROOT_DIR}/kind/nvkind-config-template.yaml}"
NVKIND_CONFIG_VALUES="${NVKIND_CONFIG_VALUES:-${ROOT_DIR}/kind/nvkind-config-values.yaml}"
NVKIND_BIN="${NVKIND_BIN:-/tmp/ai-dlc-tools/nvkind}"
RECREATE_CLUSTER="${RECREATE_CLUSTER:-false}"
NVKIND_SKIP_GPU_SETUP="${NVKIND_SKIP_GPU_SETUP:-true}"
ARGOCD_VERSION="${ARGOCD_VERSION:-v3.5.2}"
ARGOCD_MANIFEST_URL="${ARGOCD_MANIFEST_URL:-https://raw.githubusercontent.com/argoproj/argo-cd/${ARGOCD_VERSION}/manifests/install.yaml}"
REGISTRY_NAMESPACE="${REGISTRY_NAMESPACE:-registry}"
REGISTRY_SERVICE="${REGISTRY_SERVICE:-registry.${REGISTRY_NAMESPACE}.svc.cluster.local}"
REGISTRY_PORT="${REGISTRY_PORT:-5000}"
REGISTRY_CLUSTER_IP="${REGISTRY_CLUSTER_IP:-10.96.200.10}"
REGISTRY_NODE_PORT="${REGISTRY_NODE_PORT:-30500}"
REGISTRY_HOST_PORT="${REGISTRY_HOST_PORT:-5002}"
REGISTRY_INTERNAL="${REGISTRY_INTERNAL:-${REGISTRY_SERVICE}:${REGISTRY_PORT}}"
CUSTOM_ADK_SOURCE_IMAGE="${CUSTOM_ADK_SOURCE_IMAGE:-172.17.0.1:5001/kagent-dev/kagent/golang-adk:0.10.1-otel}"
CUSTOM_CONTROLLER_SOURCE_IMAGE="${CUSTOM_CONTROLLER_SOURCE_IMAGE:-172.17.0.1:5001/kagent-dev/kagent/controller:0.10.1-otel}"
REGISTRY_IMAGE="${REGISTRY_IMAGE:-registry:2}"
NVIDIA_SMOKE_TEST_IMAGE="${NVIDIA_SMOKE_TEST_IMAGE:-nvidia/cuda:12.8.1-base-ubuntu24.04}"
GITEA_ADMIN_USERNAME="${GITEA_ADMIN_USERNAME:-admin}"
GITEA_ADMIN_PASSWORD="${GITEA_ADMIN_PASSWORD:-ai-dlc-local-admin}"
GITEA_REPOSITORY_URL="${GITEA_REPOSITORY_URL:-http://gitea-http.gitea.svc.cluster.local:3000/admin/ai-dlc.git}"
GITEA_BOOTSTRAP_URL="${GITEA_BOOTSTRAP_URL:-http://127.0.0.1:30090}"
HELM_BIN="${HELM_BIN:-helm}"

log() {
  printf '[bootstrap] %s\n' "$*"
}

die() {
  printf '[bootstrap] ERROR: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

SUDO_KEEPALIVE_PID=""
cleanup() {
  if [[ -n "$SUDO_KEEPALIVE_PID" ]]; then
    kill "$SUDO_KEEPALIVE_PID" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

for command_name in docker kind kubectl curl nvidia-ctk sudo; do
  require_command "$command_name"
done
require_command "$HELM_BIN"
require_command go
require_command git

[[ -f "$NVKIND_CONFIG_TEMPLATE" ]] || die "nvkind config template not found: $NVKIND_CONFIG_TEMPLATE"
[[ -f "$NVKIND_CONFIG_VALUES" ]] || die "nvkind config values not found: $NVKIND_CONFIG_VALUES"
[[ -d "$ROOT_DIR/argocd/apps" ]] || die "Argo application directory not found"

if [[ ! -x "$NVKIND_BIN" ]]; then
  require_command go
  log "Installing nvkind into $NVKIND_BIN"
  mkdir -p "$(dirname -- "$NVKIND_BIN")"
  GOBIN="$(dirname -- "$NVKIND_BIN")" go install github.com/NVIDIA/nvkind/cmd/nvkind@latest
fi

if ! docker info >/dev/null 2>&1; then
  die "Docker is not running"
fi

log "Configuring the NVIDIA container runtime for CDI and volume injection"
if sudo -n -v >/dev/null 2>&1; then
  (while true; do sudo -n -v; sleep 60; done) >/dev/null 2>&1 &
  SUDO_KEEPALIVE_PID="$!"
  sudo nvidia-ctk runtime configure --runtime=docker --set-as-default --cdi.enabled
  sudo nvidia-ctk config \
    --set accept-nvidia-visible-devices-as-volume-mounts=true \
    --in-place
  sudo systemctl restart docker
elif docker info --format '{{json .Runtimes}}' | grep -q '"nvidia"'; then
  log "NVIDIA Docker runtime is already configured; continuing without sudo"
else
  die "NVIDIA runtime is not configured and sudo is not currently authorized"
fi

if ! docker info >/dev/null 2>&1; then
  die "Docker is not running after NVIDIA runtime configuration"
fi

log "Verifying NVIDIA GPU access through Docker"
docker run --rm --gpus all "$NVIDIA_SMOKE_TEST_IMAGE" \
  nvidia-smi --query-gpu=name,driver_version --format=csv,noheader \
  || die "Docker cannot access an NVIDIA GPU through nvidia-ctk"

if kind get clusters | grep -Fxq "$KIND_CLUSTER_NAME"; then
  if [[ "$RECREATE_CLUSTER" == "true" ]]; then
    log "Deleting existing cluster $KIND_CLUSTER_NAME"
    kind delete cluster --name "$KIND_CLUSTER_NAME"
  else
    log "Cluster $KIND_CLUSTER_NAME already exists; set RECREATE_CLUSTER=true to replace it"
  fi
fi

if ! kind get clusters | grep -Fxq "$KIND_CLUSTER_NAME"; then
  log "Creating GPU-enabled cluster $KIND_CLUSTER_NAME with nvkind"
  nvkind_gpu_setup_args=()
  if [[ "$NVKIND_SKIP_GPU_SETUP" == "true" ]]; then
    nvkind_gpu_setup_args+=(--skip-gpu-setup)
  fi
  "$NVKIND_BIN" cluster create \
    --name "$KIND_CLUSTER_NAME" \
    --config-template "$NVKIND_CONFIG_TEMPLATE" \
    --config-values "$NVKIND_CONFIG_VALUES" \
    "${nvkind_gpu_setup_args[@]}" \
    --wait 120s
fi

kubectl config use-context "kind-${KIND_CLUSTER_NAME}" >/dev/null

if [[ "$NVKIND_SKIP_GPU_SETUP" == "true" ]]; then
  log "Applying nvkind GPU node setup workaround for WSL"
  for node in $(kind get nodes --name "$KIND_CLUSTER_NAME"); do
    if ! docker inspect "$node" --format '{{range .Mounts}}{{println .Destination}}{{end}}' \
      | grep -Fxq '/var/run/nvidia-container-devices/all'; then
      continue
    fi
    docker exec "$node" bash -ceu '
      if ! command -v nvidia-ctk >/dev/null 2>&1; then
        apt-get update
        apt-get install -y --no-install-recommends ca-certificates curl gpg
        install -d -m 0755 /usr/share/keyrings
        curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey \
          | gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
        curl -fsSL https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list \
          | sed "s#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g" \
          > /etc/apt/sources.list.d/nvidia-container-toolkit.list
        apt-get update
        apt-get install -y --no-install-recommends nvidia-container-toolkit
      fi
      nvidia-ctk runtime configure --runtime=containerd --config-source=file
      systemctl restart containerd
    '
  done
fi

kubectl apply -f - <<'EOF'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: nvidia
handler: nvidia
EOF
kubectl wait --for=condition=Ready nodes --all --timeout=180s
for node in $(kind get nodes --name "$KIND_CLUSTER_NAME"); do
  if docker inspect "$node" --format '{{range .Mounts}}{{println .Destination}}{{end}}' \
    | grep -Fxq '/var/run/nvidia-container-devices/all'; then
    kubectl label node "$node" nvidia.com/gpu.present=true --overwrite >/dev/null
    kubectl label node "$node" feature.node.kubernetes.io/pci-10de.present=true --overwrite >/dev/null
  fi
done

log "Loading the registry image into every Kind node"
docker image inspect "$REGISTRY_IMAGE" >/dev/null 2>&1 \
  || docker pull "$REGISTRY_IMAGE"
if ! kind load docker-image "$REGISTRY_IMAGE" --name "$KIND_CLUSTER_NAME"; then
  log "kind load failed; importing the single-platform OCI archive directly"
  registry_archive="$(mktemp /tmp/ai-dlc-registry.XXXXXX.tar)"
  repacked_registry_image="ai-dlc-bootstrap-registry:2"
  repacked_registry_container="ai-dlc-bootstrap-registry-repack"
  docker rm -f "$repacked_registry_container" >/dev/null 2>&1 || true
  docker create --name "$repacked_registry_container" "$REGISTRY_IMAGE" >/dev/null
  docker commit "$repacked_registry_container" "$repacked_registry_image" >/dev/null
  docker rm "$repacked_registry_container" >/dev/null
  docker save --output "$registry_archive" "$repacked_registry_image"
  for node in $(kind get nodes --name "$KIND_CLUSTER_NAME"); do
    docker exec -i "$node" ctr --namespace=k8s.io images import \
      --local \
      --base-name "docker.io/library/ai-dlc-bootstrap-registry" \
      --snapshotter=overlayfs \
      - < "$registry_archive"
    docker exec "$node" ctr --namespace=k8s.io images tag \
      "docker.io/library/$repacked_registry_image" \
      "docker.io/library/$REGISTRY_IMAGE"
  done
  rm -f "$registry_archive"
fi

log "Configuring containerd for the in-cluster HTTP registry"
for node in $(kind get nodes --name "$KIND_CLUSTER_NAME"); do
  docker exec "$node" sh -c 'grep -q "config_path =.*certs.d" /etc/containerd/config.toml || sed -i "/^\[plugins.\"io.containerd.grpc.v1.cri\"\]$/a\\  [plugins.\"io.containerd.grpc.v1.cri\".registry]\n  config_path = \"/etc/containerd/certs.d\"" /etc/containerd/config.toml'
  docker exec "$node" sh -c "mkdir -p /etc/containerd/certs.d/${REGISTRY_INTERNAL}"
  docker exec -i "$node" sh -c "cat > /etc/containerd/certs.d/${REGISTRY_INTERNAL}/hosts.toml" <<EOF
server = "http://${REGISTRY_CLUSTER_IP}:${REGISTRY_PORT}"

[host."http://${REGISTRY_CLUSTER_IP}:${REGISTRY_PORT}"]
  capabilities = ["pull", "resolve", "push"]
EOF
  docker exec "$node" sh -c 'kill -HUP "$(pidof containerd)"'
done

log "Installing the in-cluster OCI registry before Argo CD"
kubectl apply --server-side --force-conflicts -f "$ROOT_DIR/registry/registry.yaml"
kubectl rollout status deployment/registry -n "$REGISTRY_NAMESPACE" --timeout=180s

REGISTRY_CONTROL_PLANE_IP="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' \
  "${KIND_CLUSTER_NAME}-control-plane")"
REGISTRY_PUSH_REPOSITORY="${REGISTRY_PUSH_REPOSITORY:-oci://${REGISTRY_CONTROL_PLANE_IP}:${REGISTRY_NODE_PORT}/charts}"
wait_for_registry_endpoint() {
  local endpoint="$1"
  local attempt
  for attempt in $(seq 1 30); do
    if curl --silent --show-error --fail --max-time 5 "$endpoint" >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  return 1
}
wait_for_registry_endpoint "http://${REGISTRY_CONTROL_PLANE_IP}:${REGISTRY_NODE_PORT}/v2/" \
  || die "in-cluster registry is not reachable through NodePort ${REGISTRY_NODE_PORT}"
wait_for_registry_endpoint "http://127.0.0.1:${REGISTRY_HOST_PORT}/v2/" \
  || die "host publishing port ${REGISTRY_HOST_PORT} is unavailable; recreate Kind with the current kind config"

if docker image inspect "$CUSTOM_ADK_SOURCE_IMAGE" >/dev/null 2>&1; then
  log "Loading the custom Go ADK image into every Kind node"
  kind load docker-image "$CUSTOM_ADK_SOURCE_IMAGE" --name "$KIND_CLUSTER_NAME"
  docker tag "$CUSTOM_ADK_SOURCE_IMAGE" "localhost:${REGISTRY_HOST_PORT}/kagent-dev/kagent/golang-adk:0.10.1-otel"
  docker push "localhost:${REGISTRY_HOST_PORT}/kagent-dev/kagent/golang-adk:0.10.1-otel"
else
  die "custom image $CUSTOM_ADK_SOURCE_IMAGE is not present locally; build it before bootstrapping"
fi

if docker image inspect "$CUSTOM_CONTROLLER_SOURCE_IMAGE" >/dev/null 2>&1; then
  log "Loading the custom kagent controller image into every Kind node"
  kind load docker-image "$CUSTOM_CONTROLLER_SOURCE_IMAGE" --name "$KIND_CLUSTER_NAME"
  docker tag "$CUSTOM_CONTROLLER_SOURCE_IMAGE" "localhost:${REGISTRY_HOST_PORT}/kagent-dev/kagent/controller:0.10.1-otel"
  docker push "localhost:${REGISTRY_HOST_PORT}/kagent-dev/kagent/controller:0.10.1-otel"
else
  die "custom image $CUSTOM_CONTROLLER_SOURCE_IMAGE is not present locally; build it before bootstrapping"
fi

push_helm_chart_http() {
  local package_file="$1"
  local repository="$2"
  local registry_base registry_path repository_path chart_version
  local config_file manifest_file layer_digest config_digest location upload_url

  registry_path="${repository#oci://}"
  registry_base="http://${registry_path%%/*}"
  repository_path="${registry_path#*/}/$(basename -- "$package_file" | sed -E 's/-[^-]+\.tgz$//')"
  chart_version="$(basename -- "$package_file" | sed -E 's/^.+-([^-]+)\.tgz$/\1/')"
  config_file="$(mktemp /tmp/ai-dlc-helm-config.XXXXXX.json)"
  manifest_file="$(mktemp /tmp/ai-dlc-helm-manifest.XXXXXX.json)"
  printf '{}\n' > "$config_file"
  config_digest="sha256:$(sha256sum "$config_file" | awk '{print $1}')"
  layer_digest="sha256:$(sha256sum "$package_file" | awk '{print $1}')"

  for blob in "$config_file|$config_digest" "$package_file|$layer_digest"; do
    blob_file="${blob%|*}"
    blob_digest="${blob##*|}"
    location="$(curl --silent --show-error --fail -D - -o /dev/null \
      -X POST "$registry_base/v2/$repository_path/blobs/uploads/" \
      | awk 'BEGIN{IGNORECASE=1} /^Location:/{sub(/^[^:]+:[[:space:]]*/, ""); gsub(/\r/, ""); print; exit}')"
    [[ -n "$location" ]] || die "failed to start OCI upload for $blob_file"
    case "$location" in
      http://*\?*) upload_url="${location}&digest=$blob_digest" ;;
      http://*) upload_url="${location}?digest=$blob_digest" ;;
      /*) upload_url="${registry_base}${location}?digest=$blob_digest" ;;
      *) die "unexpected OCI upload location: $location" ;;
    esac
    curl --silent --show-error --fail -X PUT --upload-file "$blob_file" "$upload_url" >/dev/null
  done

  printf '{"schemaVersion":2,"config":{"mediaType":"application/vnd.cncf.helm.config.v1+json","digest":"%s","size":3},"layers":[{"mediaType":"application/vnd.cncf.helm.chart.content.v1.tar+gzip","digest":"%s","size":%s}]}\n' \
    "$config_digest" "$layer_digest" "$(stat -c '%s' "$package_file")" > "$manifest_file"
  curl --silent --show-error --fail -X PUT \
    -H 'Content-Type: application/vnd.oci.image.manifest.v1+json' \
    --data-binary "@$manifest_file" \
    "$registry_base/v2/$repository_path/manifests/$chart_version" >/dev/null
}

publish_platform_chart() {
  local chart_name="$1"
  local package_dir package_file chart_metadata package_name package_version

  package_dir="$(mktemp -d /tmp/ai-dlc-chart.XXXXXX)"
  chart_metadata="$("$HELM_BIN" show chart "$ROOT_DIR/platform/$chart_name")"
  package_name="$(printf '%s\n' "$chart_metadata" | awk -F': ' '$1 == "name" {print $2; exit}')"
  package_version="$(printf '%s\n' "$chart_metadata" | awk -F': ' '$1 == "version" {print $2; exit}')"
  [[ -n "$package_name" && -n "$package_version" ]] \
    || die "failed to read platform chart metadata: $chart_name"
  package_file="$package_dir/${package_name}-${package_version}.tgz"
  tar --transform="s,^${chart_name},${package_name}," \
    -czf "$package_file" \
    -C "$ROOT_DIR/platform" "$chart_name"
  push_helm_chart_http "$package_file" "$REGISTRY_PUSH_REPOSITORY"
}

log "Publishing only the bootstrap parent chart to the in-cluster registry"
publish_platform_chart gitea

if ! kubectl get namespace argocd >/dev/null 2>&1; then
  log "Creating the argocd namespace"
  kubectl create namespace argocd
fi

log "Installing or reconciling Argo CD $ARGOCD_VERSION"
kubectl apply --server-side --force-conflicts \
  -n argocd \
  -f "$ARGOCD_MANIFEST_URL"

kubectl wait --for=condition=Established \
  crd/applications.argoproj.io \
  --timeout=180s
kubectl rollout status statefulset/argocd-application-controller \
  -n argocd --timeout=300s
kubectl rollout status deployment/argocd-server \
  -n argocd --timeout=300s

argocd_server_args="$(kubectl -n argocd get deployment argocd-server \
  -o jsonpath='{.spec.template.spec.containers[0].args[*]}')"
if [[ "$argocd_server_args" != *"--insecure"* ]]; then
  if [[ -n "$argocd_server_args" ]]; then
    kubectl -n argocd patch deployment argocd-server --type=json \
      -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--insecure"}]'
  else
    kubectl -n argocd patch deployment argocd-server --type=json \
      -p='[{"op":"add","path":"/spec/template/spec/containers/0/args","value":["--insecure"]}]'
  fi
kubectl rollout status deployment/argocd-server \
    -n argocd --timeout=300s
fi

log "Bootstrapping Gitea through Argo"
kubectl apply --server-side --force-conflicts -f "$ROOT_DIR/argocd/apps/gitea.yaml"
for attempt in $(seq 1 90); do
  gitea_sync="$(kubectl -n argocd get application gitea \
    -o jsonpath='{.status.sync.status}' 2>/dev/null || true)"
  gitea_health="$(kubectl -n argocd get application gitea \
    -o jsonpath='{.status.health.status}' 2>/dev/null || true)"
  if [[ "$gitea_sync" == "Synced" && "$gitea_health" == "Healthy" ]]; then
    break
  fi
  if [[ "$attempt" == 90 ]]; then
    die "Gitea did not become Synced/Healthy before Git repository bootstrap"
  fi
  sleep 2
done
kubectl -n gitea rollout status deployment/gitea --timeout=300s

log "Creating the local GitOps repository in Gitea"
curl --silent --show-error --fail \
  --user "$GITEA_ADMIN_USERNAME:$GITEA_ADMIN_PASSWORD" \
  -H 'Content-Type: application/json' \
  -X POST "$GITEA_BOOTSTRAP_URL/api/v1/user/repos" \
  -d '{"name":"ai-dlc","description":"ai-dlc GitOps repository","private":true,"auto_init":false}' \
  >/dev/null 2>&1 || true

gitea_basic_auth="$(printf '%s:%s' "$GITEA_ADMIN_USERNAME" "$GITEA_ADMIN_PASSWORD" | base64 | tr -d '\n')"
git -C "$ROOT_DIR" rev-parse --verify HEAD >/dev/null 2>&1 \
  || die "the bootstrap source directory must be a Git repository with at least one commit"
git -C "$ROOT_DIR" \
  -c "http.extraHeader=Authorization: Basic $gitea_basic_auth" \
  push "$GITEA_REPOSITORY_URL" HEAD:main >/dev/null

log "Registering the Gitea repository in Argo CD"
kubectl -n argocd create secret generic repo-gitea \
  --from-literal=type=git \
  --from-literal=url="$GITEA_REPOSITORY_URL" \
  --from-literal=username="$GITEA_ADMIN_USERNAME" \
  --from-literal=password="$GITEA_ADMIN_PASSWORD" \
  --dry-run=client -o yaml \
  | kubectl apply --server-side --force-conflicts -f -
kubectl -n argocd label secret repo-gitea \
  argocd.argoproj.io/secret-type=repository --overwrite >/dev/null

log "Applying Argo root Application"
kubectl apply --server-side --force-conflicts -f "$ROOT_DIR/argocd/root-app.yaml"

log "Argo Applications dispatched; reconciliation continues asynchronously"
kubectl -n argocd get applications.argoproj.io \
  -o custom-columns='NAME:.metadata.name,SYNC:.status.sync.status,HEALTH:.status.health.status'
log "Bootstrap dispatch completed for Kind cluster $KIND_CLUSTER_NAME"
