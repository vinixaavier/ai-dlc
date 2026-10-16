# kagent-a2a

Tool A2A sobre **JSON-RPC HTTP** para chamar agentes do kagent **sem port-forward**,
via gateway `a2a-kagent.localhost` (kgateway → `kagent-controller:8083`).

Compatível com o controller `>= main-bf8afa56`: o control plane antigo
(`AgentInstanceService` gRPC) virou **`SessionService`** e o A2A passou a ser
servido como **JSON-RPC por agente** em `POST /agents/{namespace}/{name}`
(a2a-go v2, metodos PascalCase: `SendMessage`, `GetTask`, ...).

## Fluxo

Cada invocação cria uma conversa **nova** (a gateway cria a sessão
automaticamente, dedup por message id) e a descarta em seguida:

```
A2A "SendMessage" (JSON-RPC /agents/{ns}/{agent})
   └─ sessão auto-criada; response traz o contextId (= id da sessão)
SessionService/DeleteSession (gRPC-Web)  ← evita sessões zumbi
```

## Uso

```bash
# padrao (loki-specialist, conversa nova, descarta)
./kagent-a2a loki-specialist adk "ping"

# qualquer agente/task; agente pode ser "ns/name"
./kagent-a2a k8s-agent adk "liste os deployments do namespace prod"

# flags
./kagent-a2a --keep pi-default pi "que horas sao?"
  # nao descarta a sessao; reporta o contextId
./kagent-a2a --keep --context <contextId> pi-default pi "segunda mensagem"
  # retoma a conversa mantida
./kagent-a2a --quiet ...                              # só a resposta
./kagent-a2a --host http://1.2.3.4:80 ...             # outro proxy
./kagent-a2a --host-header grafana.localhost ...      # outro hostname

KAGENT_A2A_DEBUG=1 ./kagent-a2a ...                   # dump das chamadas
```

Notas:
- O argumento `harness` (2ª posicional) é aceito por compatibilidade com a
  CLI antiga, mas **ignorado**: no novo modelo o `Agent` já embute o harness
  (recurso `agents.api.kagent.dev`).
- Headers: `x-user-id` (auth) nas duas superfícies; `x-grpc-web: 1` só na
  chamada de DeleteSession.
- Multi-turno: `--keep` + `--context <id>` em todas as chamadas.

## Build

```bash
GOTOOLCHAIN=go1.27.0 go build -o kagent-a2a .
```

Dependências: só `github.com/google/uuid` (o protobuf do DeleteSession é
codificado na mão: campo 1, `session_id`).

## Rota de gateway

`a2a-kagent.localhost` → `kagent-controller:8083` vive em
`platform/kgateway/routes.yaml` (fonte de verdade; argocd aplica de
`admin/ai-dlc`). Timeouts de 30 min: A2A tem streams longos e o
timeout default do Envoy é 15 s.

## Protocolo

- A2A: `POST /agents/{namespace}/{name}` (JSON-RPC 2.0, a2a-go v2)
  - `SendMessage` com `message.role="ROLE_USER"`, `messageId` UUID,
    `parts=[{"kind":"text","text":...}]`
  - sem `contextId` → sessão nova; com → conversa retomada
- Limpeza: `POST /kagent.api.v1alpha1.SessionService/DeleteSession`
  (gRPC-Web binário: frame `0x00` + len(BE32) + protobuf { session_id })
