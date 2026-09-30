# ai-dlc

Local AI exploration platform running on Kind and managed with GitOps.

The goal is to run and observe AI CLIs, turn useful explorations into agents,
and connect those agents to anything: local CLIs, chats, Kubernetes or cron
events, repository issues, and other systems. Every flow is traced and measured
so we can evaluate whether it is effective.

## Architecture

- Kind: local Kubernetes runtime.
- Registry: an in-cluster, persistent HTTP OCI registry in the `registry`
  namespace. Argo CD and workloads consume it through the Kubernetes Service.
- Argo CD: declarative application reconciliation.
- kagent: Kubernetes-aware agents and Go ADK runtime.
- AgentGateway: A2A, MCP, and OpenAI-compatible routing.
- OpenTelemetry, Tempo, Prometheus, Loki, and Grafana: traces, metrics, logs.
- AgentRegistry: agent and MCP discovery.
- SGLang or vLLM: selectable RTX 5090 model runtime in the `sglang` namespace,
  managed by [`argocd/apps/llm-runtime.yaml`](argocd/apps/llm-runtime.yaml).

The host NVIDIA driver and Docker toolkit are prerequisites for the GPU-enabled
Kind node. The GPU Operator manages the Kubernetes device plugin, DCGM and
DCGM Exporter; SGLang/vLLM runs as a GPU Deployment inside Kind.

Before recreating Kind, enable the NVIDIA volume injection mode required by
GPU-enabled Docker nodes:

```bash
sudo nvidia-ctk runtime configure --runtime=docker --set-as-default --cdi.enabled
sudo nvidia-ctk config --set accept-nvidia-visible-devices-as-volume-mounts=true --in-place
sudo systemctl restart docker
```

This host-side step is intentionally not performed by GitOps.

## Bootstrap

The repository targets the cluster name `ai-dlc`. GPU-enabled nodes are created
through NVIDIA's `nvkind`; the bootstrap keeps the cluster recreation opt-in.

On a new machine with Docker, Kind, kubectl, Helm, and the custom images ready:

```bash
RECREATE_CLUSTER=true ./scripts/bootstrap-kind-gitops.sh
```

The script installs `nvkind` when needed, creates the GPU-enabled Kind cluster,
installs the in-cluster registry first, configures every Kind node's containerd
to use it over HTTP, publishes the local parent charts and required legacy
charts, installs Argo CD, and dispatches the Argo applications. The registry's
data is persisted in a 20Gi PVC. Argo then renders the parent charts and
resolves their upstream Helm dependencies. On WSL,
`NVKIND_SKIP_GPU_SETUP=true` (the default) applies the equivalent containerd
setup because the NVIDIA `/proc/driver/nvidia/params` file is not available
inside the worker container. The bootstrap exits after dispatching the Argo
applications; reconciliation and application health remain Argo's
responsibility.

Required local image:

```text
registry.registry.svc.cluster.local:5000/kagent-dev/kagent/golang-adk:0.10.1-otel

The substrate-enabled kagent controller also requires:

```text
registry.registry.svc.cluster.local:5000/kagent-dev/kagent/controller:0.10.1-otel
```

The controller image is built from the kagent `v0.10.1` source with the
runtime digest and tracing patch applied. Both images are loaded into every
Kind node by the bootstrap script.
```

The image is built from
[`patches/kagent-0.10.1-adk-otel.patch`](patches/kagent-0.10.1-adk-otel.patch).

Prometheus discovers the Substrate metrics endpoints on port `9090` for the
`ate-system` pods through the `substrate` scrape job in
[`argocd/apps/prometheus.yaml`](argocd/apps/prometheus.yaml).

## Endpoints

```text
http://aigw.localhost/v1/models
http://aigw.localhost/k8s-agent/.well-known/agent-card.json
http://analytics.localhost/ui/llm/analytics
http://grafana.localhost
http://argocd.localhost
```

For WSL or clients outside the Kind network, use the AgentGateway NodePort
(`30081`) through the control-plane IP.

### Windows browser hostnames

Run Notepad as Administrator and add this line to
`C:\Windows\System32\drivers\etc\hosts`:

```text
127.0.0.1 grafana.localhost argocd.localhost aigw.localhost analytics.localhost kagent.localhost agentregistry.localhost
```

Then open `http://argocd.localhost` or `http://grafana.localhost`. Kind maps
the local HTTP Gateway to `127.0.0.1:80`; direct AgentGateway clients use
`127.0.0.1:30081`.

## Tracing

The intended trace path is:

```text
AgentGateway -> kagent -> Go ADK -> AgentGateway /v1/chat/completions -> SGLang

The SGLang backend exposes the native AgentGateway API routes
`/v1/chat/completions` (OpenAI), `/v1/messages` (Anthropic), and
`/v1/responses` (Codex), plus `/v1/models` for model discovery.
```

In-cluster exporters use:

```text
http://otel-collector.otel.svc.cluster.local:4317
```

Use Grafana Explore or Tempo TraceQL to inspect the complete flow.

## Repository layout

- `argocd/apps/`: Argo CD Applications and chart configuration.
- `kind/`: declarative Kind configuration.
- `platform/llm-runtime/`: selectable SGLang/vLLM GPU Deployment and Service.
- `platform/`: Kubernetes gateways, routes, observability resources, and catalogs.
- `patches/`: reproducible source patch for the custom ADK image.
- `scripts/`: bootstrap and lifecycle automation.

Platform ownership is split across `agentgateway-platform`,
`kgateway-platform`, and `agentregistry-platform` Argo Applications.

The platform directories are packaged as OCI charts during bootstrap and
reconciled by their respective Argo Applications. AgentRegistry catalogs are
synced by an Argo hook because its catalog API is not a Kubernetes CRD.

## Validation

```bash
kubectl kustomize argocd/apps >/dev/null
bash -n scripts/bootstrap-kind-gitops.sh
kubectl -n argocd get applications.argoproj.io
```
