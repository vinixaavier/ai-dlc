import { readdirSync, readFileSync, writeFileSync } from "node:fs";

const root = "/root/.pi/agent/npm/node_modules/pi-otel/dist";

function patch(file, replacements) {
  const path = `${root}/${file}`;
  let source = readFileSync(path, "utf8");
  for (const [from, to] of replacements) {
    if (!source.includes(from)) throw new Error(`pi-otel patch target not found: ${file}: ${from}`);
    source = source.replace(from, to);
  }
  writeFileSync(path, source);
}

// The actor is suspended immediately after agent_end. Flush pi-otel's own
// BatchSpanProcessor at that lifecycle boundary instead of adding another
// telemetry provider in the harness.
patch("otel/sdk.js", [[
  "export async function shutdownSdk() {",
  `export async function forceFlushSdk() {
    if (!sdk) return;
    try {
        const provider = trace.getTracerProvider();
        await provider.forceFlush?.();
    }
    catch {
        // best effort: the actor may be suspended during collector outages
    }
}
export async function shutdownSdk() {`,
]]);

patch("index.js", [[
  "import { initSdk, probeEndpoint, shutdownSdk } from \"./otel/sdk.js\";",
  "import { forceFlushSdk, initSdk, probeEndpoint, shutdownSdk } from \"./otel/sdk.js\";",
], [
  "tracker?.endInteraction();\n    });",
  "tracker?.endInteraction();\n        await forceFlushSdk();\n    });",
]]);

// Keep the pi-otel interaction/LLM context active while Pi's provider starts
// its HTTP request, so the fetch wrapper below injects traceparent downstream.
patch("spans.js", [[
  "    hasOpenLlmRequest() {",
  "    activeLlmContext() {\n        return this.llm?.ctx ?? this.turn?.ctx ?? this.interaction?.ctx ?? null;\n    }\n    hasOpenLlmRequest() {",
]]);

patch("index.js", [[
  'import { basename } from "node:path";',
  'import { basename } from "node:path";\nimport { context as otelContext, propagation } from "@opentelemetry/api";',
], [
  "    let shellPropagationOn = false;",
  `    let shellPropagationOn = false;
    let activeProviderContext = null;
    function installProviderFetchWrapper() {
        const currentFetch = globalThis.fetch;
        if (typeof currentFetch !== "function" || currentFetch.__piOtelWrapped) return;
        const wrappedFetch = (...args) => {
            if (!activeProviderContext) return currentFetch(...args);
            const carrier = {};
            propagation.inject(activeProviderContext, carrier);
            const spanContext = trace.getSpan(activeProviderContext)?.spanContext?.();
            if (spanContext?.traceId && spanContext?.spanId) {
                carrier.traceparent = "00-" + spanContext.traceId + "-" + spanContext.spanId + "-" + (spanContext.traceFlags ?? 0).toString(16).padStart(2, "0");
            }
            const input = args[0];
            const init = args[1] ? { ...args[1] } : {};
            const baseHeaders = init.headers ?? (
                typeof Request !== "undefined" && input instanceof Request
                    ? input.headers
                    : undefined
            );
            const headers = new Headers(baseHeaders);
            if (carrier.traceparent) headers.set("traceparent", carrier.traceparent);
            if (carrier.tracestate) headers.set("tracestate", carrier.tracestate);
            init.headers = headers;
            return otelContext.with(activeProviderContext, () => currentFetch(input, init));
        };
        wrappedFetch.__piOtelWrapped = true;
        globalThis.fetch = wrappedFetch;
    }
    installProviderFetchWrapper();`,
], [
  "        tracker?.startLlmRequest(typeof model === \"string\" ? model : undefined, ctx.model?.provider);",
  "        tracker?.startLlmRequest(typeof model === \"string\" ? model : undefined, ctx.model?.provider);\n        activeProviderContext = tracker?.activeLlmContext() ?? null;\n        installProviderFetchWrapper();",
], [
  "        tracker?.setLlmAttrs(attrs);\n        tracker?.noteAssistantMessage(msg);",
  "        tracker?.setLlmAttrs(attrs);\n        activeProviderContext = null;\n        tracker?.noteAssistantMessage(msg);",
]]);

// Pi's bundled OpenAI-compatible adapter may receive a provider-specific
// `options.fetch` captured before this extension is loaded. Force the actual
// model request through globalThis.fetch, where the chat span propagation
// wrapper is installed.
const piBundleRoot = "/usr/local/lib/node_modules/@earendil-works/pi-coding-agent/dist/bundle/chunks";
for (const file of readdirSync(piBundleRoot)) {
  if (!file.endsWith(".js")) continue;
  const path = `${piBundleRoot}/${file}`;
  const source = readFileSync(path, "utf8");
  const patched = source.replaceAll("options?.fetch??globalThis.fetch", "globalThis.fetch");
  if (patched !== source) writeFileSync(path, patched);
}

// The RPC actor can deliver the final assistant message without emitting the
// extension's agent_end lifecycle event. Close the interaction at the actual
// final assistant message so invoke_agent/pi.turn are exported as a hierarchy.
patch("index.js", [[
  "        tracker?.endLlmRequest();\n        if (finish === \"error\") {",
  `        tracker?.endLlmRequest();
        const hasToolCall = Array.isArray(msg.content) && msg.content.some((part) =>
            part && typeof part === "object" &&
            ["toolCall", "tool_call", "tool_use"].includes(part.type));
        if (!hasToolCall && finish !== "error") {
            tracker?.endInteraction();
            await forceFlushSdk();
        }
        if (finish === "error") {`,
]]);

console.log("patched pi-otel for pre-suspend flush and downstream traceparent propagation");
