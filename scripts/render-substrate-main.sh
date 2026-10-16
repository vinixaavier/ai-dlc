#!/usr/bin/env bash
set -Eeuo pipefail

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
source "$ROOT_DIR/scripts/agent-platform-versions.sh"
PUBLISH_REGISTRY="${PUBLISH_REGISTRY:-localhost:5002}"
IMAGE_REPO="registry.registry.svc.cluster.local:5000/kagent-dev/substrate"
CRANE_BIN="${CRANE_BIN:-$(go env GOPATH)/bin/crane}"
export ROOT_DIR PUBLISH_REGISTRY IMAGE_REPO CRANE_BIN SUBSTRATE_TAG

python3 - <<'PY'
import os
import pathlib
import re
import subprocess

root = pathlib.Path(os.environ["ROOT_DIR"])
source = root / "platform/substrate/upstream/ate-install"
provider = root / "platform/substrate/upstream/egress-credential-injection"
paths = [
    source / "kind-agentgateway",
    source / "kind/postgres",
    source / "agentgateway-egress",
    provider,
]
parts = []
for path in paths:
    parts.append(subprocess.check_output(
        ["kubectl", "kustomize", "--load-restrictor", "LoadRestrictionsNone", str(path)],
        text=True,
    ))
for path in [
    source / "pod-certificate-controller.yaml",
    source / "sandboxconfig-gvisor.yaml",
    source / "sandboxconfig-validation.yaml",
]:
    parts.append(path.read_text())

egress_block = '''credentialProviders:
- uriAuthority: "k8s.io"
  target:
    host: "k8s-credential-provider.ate-system.svc:50051"
    policies:
      backendTLS:
        hostname: "k8s-credential-provider.ate-system.svc"
        cert: /run/podidentity.podcert.ate.dev/credential-bundle.pem
        key: /run/podidentity.podcert.ate.dev/credential-bundle.pem
        root: /run/servicedns-ca/trust-bundle.pem'''

documents = []
seen = {}
positions = {}
for part in parts:
    for raw in re.split(r"(?m)^---\s*$", part):
        doc = raw.strip()
        if not doc or not re.search(r"(?m)^apiVersion:", doc):
            continue
        doc = re.sub(
            r"(?m)^(\s*)#ATE_AGENTGATEWAY_CREDENTIAL_PROVIDERS\s*$",
            lambda m: "\n".join(m.group(1) + line for line in egress_block.splitlines()),
            doc,
        )
        api = kind = name = namespace = ""
        in_metadata = False
        for line in doc.splitlines():
            if line.startswith("apiVersion:"):
                api = line.split(":", 1)[1].strip().strip('"')
            elif line.startswith("kind:"):
                kind = line.split(":", 1)[1].strip().strip('"')
            elif line == "metadata:":
                in_metadata = True
            elif in_metadata:
                if line and not line[0].isspace():
                    in_metadata = False
                elif re.match(r"^  name:", line):
                    name = line.split(":", 1)[1].strip().strip('"')
                elif re.match(r"^  namespace:", line):
                    namespace = line.split(":", 1)[1].strip().strip('"')
        if not (api and kind and name):
            raise SystemExit(f"cannot identify rendered resource:\n{doc[:500]}")
        key = (api, kind, namespace, name)
        if key in seen:
            # The egress overlay applies an additional component patch to its
            # shared ConfigMap, so prefer its later rendering of that object.
            if seen[key] != doc:
                documents[positions[key]] = doc
                seen[key] = doc
            continue
        seen[key] = doc
        positions[key] = len(documents)
        documents.append(doc)

digest_cache = {}
def digest(name):
    if name not in digest_cache:
        ref = f"{os.environ['PUBLISH_REGISTRY']}/kagent-dev/substrate/{name}:{os.environ['SUBSTRATE_TAG']}"
        digest_cache[name] = subprocess.check_output(
            [os.environ["CRANE_BIN"], "digest", ref], text=True
        ).strip()
    return digest_cache[name]

rendered = "\n---\n".join(documents) + "\n"
version = os.environ["SUBSTRATE_TAG"]
suffix = re.sub(r"[^a-z0-9]", "-", version.lower()).strip("-")
rendered = rendered.replace("${SUBSTRATE_VERSION_SUFFIX}", suffix)
rendered = rendered.replace("${SUBSTRATE_VERSION}", version)
def replace_ko(match):
    path = match.group(1)
    name = path.rsplit("/", 1)[-1]
    return f"image: {os.environ['IMAGE_REPO']}/{name}:{os.environ['SUBSTRATE_TAG']}@{digest(name)}"
rendered, ko_count = re.subn(r"image: ko://github\.com/agent-substrate/substrate/([^\s]+)", replace_ko, rendered)
if ko_count == 0:
    raise SystemExit("no Substrate ko image references found")

agentgateway_version = "v0.0.0-alpha.168b05d6"
agentgateway_ref = f"cr.agentgateway.dev/agentgateway:{agentgateway_version}@sha256:5e8384d1fcfdc443a3ad5155ea6a64dc7953bde162df7f5f8e131a5c19ae08a3"
agentgateway_digest = subprocess.check_output(
    [os.environ["CRANE_BIN"], "digest", f"{os.environ['PUBLISH_REGISTRY']}/kagent-dev/substrate/agentgateway:{agentgateway_version}"],
    text=True,
).strip()
agentgateway_local = f"{os.environ['IMAGE_REPO']}/agentgateway:{agentgateway_version}@{agentgateway_digest}"
rendered = rendered.replace(agentgateway_ref, agentgateway_local)
if agentgateway_ref in rendered:
    raise SystemExit("could not replace the pinned AgentGateway image")
if "ko://" in rendered or "#ATE_AGENTGATEWAY_CREDENTIAL_PROVIDERS" in rendered:
    raise SystemExit("unresolved Substrate image or egress credential marker remains")

out = root / "platform/substrate/main.yaml"
out.write_text(rendered)
print(f"Rendered {len(documents)} resources and {ko_count} Substrate images to {out}")
PY
