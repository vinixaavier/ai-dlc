---
name: agent-creator
description: Create specialized kagent ADK agents directly in Kubernetes using Harness and AgentTemplate resources with kubectl apply.
---

# Agent Creator

Use this skill when the user wants to create, specialize, clone, or modify an
ADK agent running on kagent.

The operation is cluster-first: produce and apply Kubernetes resources with
`kubectl apply`. Do not assume that editing a GitOps repository is required.

## Runtime model

kagent separates the runtime from the specialization:

- `Harness` is the execution runtime. For this skill, prefer the generic Go
  ADK/BYO Harness already installed in the cluster.
- `AgentTemplate` is the specialized agent: description, model, prompt, tools,
  and runtime label.

When a compatible ADK Harness already exists, create or update only an
`AgentTemplate`. Create a Harness only when the requested ADK runtime needs a
distinct image, command, model integration, or persistence configuration.

This environment currently uses `kagent.dev/v1alpha3`, with `Harness` and
`AgentTemplate` resources. Check the live API before applying anything; do not
blindly copy older examples using `Agent`, `AgentHarness`, or `SandboxAgent`.

## Discover before applying

Run read-only discovery first:

```bash
kubectl config current-context
kubectl api-resources | grep -Ei 'harness|agenttemplate|mcp'
kubectl -n kagent get harness
kubectl -n kagent get agenttemplate
kubectl -n kagent get modelconfig
kubectl -n kagent get remotemcpserver
```

Inspect the selected Harness and use its actual selector:

```bash
kubectl -n kagent get harness <harness-name> -o yaml
```

The AgentTemplate label must match `spec.allowedAgentTemplates.selector`.
The current generic ADK runtime is normally exposed by a Harness such as
`hello-substrate`, whose selector is `kagent.dev/runtime: hello-substrate`.
Never assume the label or Harness name without checking.

## Create a specialized AgentTemplate

Use an existing `ModelConfig` and MCP server whenever possible. Keep the
system prompt focused on the agent's role, operating scope, available tools,
safety boundaries, clarification conditions, and expected output.

Example:

```yaml
apiVersion: kagent.dev/v1alpha3
kind: AgentTemplate
metadata:
  name: prometheus-specialist
  namespace: kagent
  labels:
    kagent.dev/runtime: hello-substrate
spec:
  description: Investigates Prometheus metrics and time-series behavior
  modelConfig:
    name: default-model-config
  systemPrompt: |
    You are a Prometheus specialist. Investigate metric availability,
    targets, labels, time series, rates, aggregations, and alert symptoms.

    Use only the Prometheus tools attached to this agent. State the exact
    PromQL used, time range, assumptions, and whether the result is observed
    or inferred. Ask for the namespace, workload, service, metric, or time
    range when the request is ambiguous. Do not modify cluster resources.
  tools:
    - mcp:
        server:
          kind: RemoteMCPServer
          name: kagent-tool-server
        tools:
          - prometheus_query_tool
          - prometheus_query_range_tool
```

The `tools` list is an allowlist. When only a subset is wanted, list every
allowed tool explicitly. Do not omit it, because an omitted or empty list may
expose every tool from the MCP server.

Apply directly to Kubernetes:

```bash
kubectl apply -f agent-template.yaml
```

For an explicitly requested one-shot creation, `kubectl apply -f - <<'EOF'`
is also acceptable. Use a unique name and do not overwrite an existing
resource unless the user requested an update.

## Tools and permissions

Treat system prompts, MCP tools, runtime skills, and Harnesses as separate:

- the prompt defines behavior and policy;
- MCP bindings define executable capabilities;
- runtime skills are files/scripts loaded by the runtime;
- the Harness defines execution and isolation.

Prefer read-only tools for diagnostic specialists. If a tool can mutate state,
describe that capability and configure `requireApproval` when supported.
Never claim that a tool is available until it appears in the referenced
`RemoteMCPServer` status or live MCP discovery.

## Creating a new ADK Harness

Only create an ADK Harness when no compatible ADK runtime exists. Inspect the
existing runtime first:

```bash
kubectl -n kagent get harness <name> -o yaml
kubectl -n kagent get agenttemplate <name> -o yaml
```

Preserve the ADK runtime's required image, command, environment, substrate,
snapshot policy, and allowed-template selector. Confirm that the selected ADK
image consumes the generated `KAGENT_CONFIG_JSON` and
`KAGENT_AGENT_CARD_JSON` before relying on model or MCP fields.

## Verify after apply

```bash
kubectl -n kagent get agenttemplate <name> -o yaml
kubectl -n kagent describe agenttemplate <name>
```

Confirm the Harness reports selector acceptance, references resolve, and the
template reaches a ready/successful revision when those conditions exist.
For a safe functional test, invoke a read-only task:

```bash
kagent invoke --agent <name> --task "Run a read-only diagnostic" --stream
```

Report the namespace, AgentTemplate, Harness, model, tools, apply result, and
any readiness or permission issue. Do not report success solely because
`kubectl apply` returned zero; verify the resource status.

## Safety

- Do not delete or replace an existing Harness to create a specialization.
- Do not expose all MCP tools when the user requested a restricted set.
- Do not put API keys or credentials in AgentTemplate YAML.
- Do not apply mutating test tasks unless the user explicitly authorized them.
- If the requested runtime, model, tool, or selector does not exist, stop and
  report the concrete missing dependency.
