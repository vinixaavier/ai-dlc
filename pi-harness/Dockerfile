FROM golang:1.27-bookworm AS builder

WORKDIR /src/pi-harness
COPY vendor/kagent/go /src/kagent
COPY pi-harness/go.mod pi-harness/main.go ./
RUN go mod tidy && CGO_ENABLED=0 go build -o /out/pi-harness .

FROM node:22-bookworm-slim

RUN apt-get update \
  && apt-get install -y --no-install-recommends ca-certificates \
  && rm -rf /var/lib/apt/lists/* \
  && npm install -g @earendil-works/pi-coding-agent@0.85.1 \
  && npm cache clean --force \
  && rm -rf /root/.npm

COPY --from=builder /out/pi-harness /usr/local/bin/pi-harness
COPY pi-harness/models.json /root/.pi/agent/models.json
COPY pi-harness/settings.json /root/.pi/agent/settings.json
COPY pi-harness/patch-pi-otel.mjs /tmp/patch-pi-otel.mjs

# pi-otel: OpenTelemetry extension for pi, installed as a user package so it
# loads without project trust. The endpoint is also baked into settings.json:
# ATE snapshot restoration can omit Harness env vars from the actor workload.
RUN mkdir -p /root/.pi/agent/npm \
  && cd /root/.pi/agent/npm \
  && echo '{"name":"pi-extensions","private":true}' > package.json \
  && npm install --no-audit --no-fund pi-otel@0.3.0 \
  && node /tmp/patch-pi-otel.mjs \
  && npm cache clean --force \
  && rm -rf /root/.npm /tmp/patch-pi-otel.mjs

ENV PI_CODING_AGENT_DIR=/root/.pi/agent
ENV PORT=8080
ENV PI_PROVIDER=agentgateway
ENV PI_MODEL=qwen3.8-27b-nvfp4
ENV PI_API_KEY=sglang-local

EXPOSE 80
ENTRYPOINT ["/usr/local/bin/pi-harness"]
