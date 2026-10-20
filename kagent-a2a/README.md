# kagent-a2a

Tool A2A sobre **gRPC-Web** para chamar agentes do kagent **sem port-forward**,
via gateway `a2a-kagent.localhost` (kgateway → `kagent-controller:8083`).

Cada invocação cria uma conversa **nova** e a descarta em seguida:

```
CreateAgentInstance ─► A2A SendMessage ─► DeleteAgentInstance
   (control plane)       (lf.a2a.v1)        (evita instâncias zumbi)
```

## Uso

```bash
# padrao (loki-specialist, harness adk, conversa nova, descarta)
./kagent-a2a loki-specialist adk "ping"

# qualquer agente/task
./kagent-a2a k8s-agent adk "liste os deployments do namespace prod"

# flags
./kagent-a2a --keep pi-default pi "que horas são?"   # mantém a instância
./kagent-a2a --quiet ...                              # só a resposta
./kagent-a2a --host http://1.2.3.4:80 ...             # outro proxy
./kagent-a2a --host-header grafana.localhost ...      # outro hostname

KAGENT_A2A_DEBUG=1 ./kagent-a2a ...                   # dump dos frames
```

Padrões: `--host http://127.0.0.1` + `--host-header a2a-kagent.localhost`.
O proxy local na porta 80 roteia pelo header Host para o kgateway; usar
127.0.0.1 + Host explícito funciona sem resolver `a2a-kagent.localhost`
(Go/Python não resolvem `*.localhost` nesse WSL; o curl resolve).

## Build

```bash
GOTOOLCHAIN=go1.27.0 go build -o kagent-a2a .
# requer /home/vinic/ai-dlc/repos/kagent (replace directive no go.mod)
```

## Rota de gateway

`a2a-kagent.localhost` → `kagent-controller:8083` vive em
`platform/kgateway/routes.yaml` (fonte de verdade; argocd aplica de
`admin/ai-dlc`). Timeouts de 30 min: gRPC-Web tem streams longos e o
timeout default do Envoy é 15 s.

## Protocolo (gRPC-Web binário)

- POST `/<service>/<method>` com `x-grpc-web: 1`,
  `Content-Type: application/grpc-web+proto`
- corpo: frame `0x00` + len(BE32) + bytes protobuf
- resposta: frame de mensagem + frame de trailers (`0x80`)
- headers de roteamento: `x-user-id` (auth) e
  `x-kagent-agent-instance-id` (rota A2A → instância)

Servidos pelo controller (`core/internal/grpcserver`): gRPC nativo h2c
**e** o mesmo mux com wrapper gRPC-Web (HTTP/1.1) — é esse que atravessa
o kgateway.
