# a2a-ping

Conversa **A2A puro** direto no gateway A2A do kagent — sem passar pela CLI.
É o mesmo fluxo que `kagent invoke` usa por baixo, só que exposto como binário
reusável.

## O que ele faz

1. Abre um `kubectl port-forward svc/kagent-controller 8083` (o gateway é
   ClusterIP, não é acessível de fora do cluster).
2. Cria uma **AgentInstance nova** via gRPC API (conversa nova, não pega
   instância existente).
3. Envia um `SendMessage` A2A no gateway (gRPC `A2AService`), roteado pelo
   header `x-kagent-agent-instance-id`, autenticado por `x-user-id`.
4. Imprime a resposta e **descarta a instância** (não vira zumbi).

## Uso

```bash
./a2a-ping [template] [harness] "task..."
# ex.:
./a2a-ping loki-specialist adk "ping"
./a2a-ping k8s-agent adk "lista os pods do namespace kagent"
./a2a-ping pi-default pi "que horas são?"
```

Args:
- `template` (default `loki-specialist`): nome do AgentTemplate
- `harness` (default `adk`): nome do Harness
- `task` (default `ping`): texto enviado ao agente

## Rebuild

O module do kagent exige Go 1.27 (repositório local em
`/tmp/pi-github-repos/.../go`, apontado por `replace` no `go.mod`).

```bash
export GOTOOLCHAIN=go1.27.0
GOFLAGS=-mod=mod go build -o a2a-ping .
```

## Por que funciona

- Gateway A2A = gRPC `A2AService` (métodos `SendMessage`, `GetTask`,
  `ListTasks`, ...) na porta `8083` do `kagent-controller`.
- Roteamento por instância: header `x-kagent-agent-instance-id`.
- Auth: header `x-user-id` (default `admin@kagent.dev`).
- Criação da instância: gRPC `AgentInstanceService.CreateAgentInstance`
  (mesmo endpoint 8083, só que no serviço de control plane).
