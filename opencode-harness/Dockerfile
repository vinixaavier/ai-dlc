FROM golang:1.27-bookworm AS builder

WORKDIR /src/opencode-harness
COPY opencode-harness/go.mod opencode-harness/main.go ./
RUN go mod tidy && CGO_ENABLED=0 go build -trimpath -o /out/opencode-harness .

FROM node:22.19.0-bookworm-slim

ARG OPENCODE_VERSION=1.19.2
RUN apt-get update \
  && apt-get install -y --no-install-recommends ca-certificates \
  && rm -rf /var/lib/apt/lists/* \
  && npm install -g "opencode-ai@${OPENCODE_VERSION}" \
  && test "$(opencode --version)" = "${OPENCODE_VERSION}" \
  && npm cache clean --force \
  && rm -rf /root/.npm \
  && useradd --uid 65532 --create-home --home-dir /data --shell /usr/sbin/nologin kagent \
  && mkdir -p /data/workspace /data/opencode/config /data/opencode/data \
  && chown -R 65532:65532 /data

COPY --from=builder /out/opencode-harness /usr/local/bin/opencode-harness
COPY opencode-harness/opencode.json /etc/opencode/opencode.json

ENV HOME=/data \
    XDG_CONFIG_HOME=/data/opencode/config \
    XDG_DATA_HOME=/data/opencode/data \
    OPENCODE_CONFIG=/etc/opencode/opencode.json \
    OPENCODE_MODEL=agentgateway/qwen3.8-27b-nvfp4 \
    OPENCODE_VERSION=1.19.2 \
    PORT=80

RUN chown -R 65532:65532 /data
USER 65532:65532
WORKDIR /data/workspace
EXPOSE 80
ENTRYPOINT ["/usr/local/bin/opencode-harness"]
