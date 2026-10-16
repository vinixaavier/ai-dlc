#!/usr/bin/env bash
set -euo pipefail

source_root="${1:?usage: patch-kagent-harness-cli-versions.sh /path/to/kagent/go [codex-version] [claude-version]}"
codex_version="${2:-0.163.0}"
claude_version="${3:-2.1.301}"

python3 - "$source_root" "$codex_version" "$claude_version" <<'PY'
from pathlib import Path
import sys

root = Path(sys.argv[1])
replacements = {
    root / "harness/codex/config/config.go": ('PinnedCodexVersion = "', sys.argv[2]),
    root / "harness/claude/config/config.go": ('PinnedClaudeVersion                 = "', sys.argv[3]),
}
for path, (prefix, version) in replacements.items():
    text = path.read_text()
    start = text.index(prefix) + len(prefix)
    end = text.index('"', start)
    updated = text[:start] + version + text[end:]
    path.write_text(updated)
    print(f"{path}: {version}")

dockerfiles = {
    root / "harness/codex/Dockerfile": {
        "CODEX_CLI_VERSION": sys.argv[2],
        "CODEX_CLI_AMD64_SHA256": "582646c6bf0561eb0de540f7fc85b9121b0091a826bc6e96830b7f1722122182",
        "CODEX_CLI_ARM64_SHA256": "9d8ecf2d19d3e184877983902d6c85de48b3888dcb72c382a8a0c975845927fb",
    },
    root / "harness/claude/Dockerfile": {
        "CLAUDE_CODE_VERSION": sys.argv[3],
        "CLAUDE_CODE_AMD64_SHA256": "081a7219cf075e5c5d2a39796517ae2fa06610d99f3498e7ee5c577f0da687fa",
        "CLAUDE_CODE_ARM64_SHA256": "d0a6b24d157d061cd5c406eb4b8f3d4016b646c8802b47babd3625c636e4433d",
    },
}
for path, args in dockerfiles.items():
    lines = path.read_text().splitlines()
    for key, value in args.items():
        prefix = f"ARG {key}="
        matches = [index for index, line in enumerate(lines) if line.startswith(prefix)]
        if len(matches) != 1:
            raise SystemExit(f"expected one {prefix} line in {path}; found {len(matches)}")
        lines[matches[0]] = f"{prefix}{value}"
    path.write_text("\n".join(lines) + "\n")
    print(f"{path}: CLI versions and checksums updated")
PY
