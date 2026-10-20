# AgentRegistry catalog

AgentRegistry is populated through `POST /v0/apply`; it is not a Kubernetes
CRD and there is no `ar.dev` CRD in this cluster. The
`agentregistry-platform` Argo Application syncs both catalog streams through a
hook Job after the AgentRegistry service is healthy.

The Kubernetes Service exposes the API on port `12121` and the MCP endpoint on
port `31313`. From inside the cluster, the application listens on port `8080`:

```bash
pod=$(kubectl -n agentregistry get pods \
  -l app.kubernetes.io/name=agentregistry,app.kubernetes.io/component=server \
  -o jsonpath='{.items[0].metadata.name}')
kubectl -n agentregistry exec -i -c agentregistry "$pod" -- \
  sh -c 'curl -sS -X POST -H "Content-Type: application/yaml" \
    --data-binary @- http://127.0.0.1:8080/v0/apply' < platform/agentregistry/kagent-catalog.yaml
```

| File | Contents |
|---|---|
| `kagent-catalog.yaml` | The kagent A2A agents exposed through AgentGateway. |
| `mcp-servers.yaml` | The remote MCP servers fronted by AgentGateway. |

## Current limitations

- AgentRegistry v0.4.0 does not accept the local OpenAI-compatible model in its
  model provider enum. The SGLang model remains registered through
  AgentGateway instead.
- AgentRegistry v0.4.0 has no Kubernetes catalog CRD; the Argo hook applies the
  streams through `/v0/apply`.
- The upstream server image does not contain `git`, so repository-backed skills
  require a custom image with Git or an OCI/package source.

The bootstrap publishes the chart and Argo runs the catalog sync automatically:

```bash
./scripts/bootstrap-kind-gitops.sh
```
