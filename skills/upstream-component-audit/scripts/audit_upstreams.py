#!/usr/bin/env python3
"""Read-only upstream audit for Argo-managed platform components."""

from __future__ import annotations

import argparse
import json
import re
import sys
import urllib.error
import urllib.request
from datetime import datetime, timezone
from pathlib import Path

GITHUB_RE = re.compile(r"https://github\.com/([^/\s#]+/[^/\s#]+)")
SHA_TAG_RE = re.compile(r"^main-([0-9a-f]{7,40})$")
SEMVER_RE = re.compile(r"^[vV]?\d+\.\d+(?:\.\d+)?(?:[-+][0-9A-Za-z.-]+)?$")
TAG_RE = re.compile(r"^\s*tag:\s*[\"']?([^\s#\"']+)")
REPO_RE = re.compile(r"^\s*repository:\s*[\"']?([^\s#\"']+)")

# Official repositories for vendored charts/components whose local Chart.yaml
# does not carry a GitHub source URL. Keep these mappings explicit: they are
# preferred over guessing from an image or chart name.
OFFICIAL_REPOS = {
    "agentgateway": "agentgateway/agentgateway",
    "agentregistry": "agentregistry-dev/agentregistry",
    "gpu-operator": "NVIDIA/gpu-operator",
    "kgateway": "kgateway-dev/kgateway",
}


def github(path: str) -> object:
    request = urllib.request.Request(
        f"https://api.github.com/{path}",
        headers={"Accept": "application/vnd.github+json", "User-Agent": "codex-upstream-component-audit"},
    )
    with urllib.request.urlopen(request, timeout=20) as response:
        return json.load(response)


def repo_from_chart(chart: Path) -> str | None:
    candidates = [chart, *sorted(chart.parent.glob("charts/*/Chart.yaml"))]
    for candidate in candidates:
        match = GITHUB_RE.search(candidate.read_text(encoding="utf-8", errors="replace"))
        if match:
            return match.group(1).removesuffix(".git")
    return None


def map_image_repo(image_repo: str, chart_source: str | None) -> str | None:
    """Map local registry repositories to GitHub source repositories."""
    mappings = {
        "kagent-dev/substrate": "agent-substrate/substrate",
        "kagent-dev/kmcp": "kagent-dev/kmcp",
        "kagent-dev/kagent/tools": "kagent-dev/tools",
        "kagent-dev/tools": "kagent-dev/tools",
        "kagent-dev/kagent": "kagent-dev/kagent",
        "lmsysorg/sglang": "sgl-project/sglang",
        "vllm/vllm-openai": "vllm-project/vllm",
    }
    # Check the more specific tools prefix before the kagent prefix.
    for prefix in (
        "kagent-dev/kagent/tools", "kagent-dev/tools", "kagent-dev/substrate",
        "kagent-dev/kmcp", "kagent-dev/kagent", "lmsysorg/sglang",
        "vllm/vllm-openai",
    ):
        if prefix in image_repo:
            return mappings[prefix]
    return chart_source


def chart_version_pins(path: Path) -> list[dict[str, str]]:
    """Read versions of vendored dependency charts as upstream release pins."""
    pins: list[dict[str, str]] = []
    for chart in sorted(path.glob("charts/*/Chart.yaml")):
        text = chart.read_text(encoding="utf-8", errors="replace")
        version = re.search(r"^version:\s*[\"']?([^\s#\"']+)", text, re.MULTILINE)
        if not version:
            continue
        source = repo_from_chart(chart)
        source = source or OFFICIAL_REPOS.get(chart.parent.name)
        if source and (SEMVER_RE.match(version.group(1)) or version.group(1).startswith("v")):
            pins.append({"tag": version.group(1), "repo": source})
    return pins


def discover(root: Path) -> list[dict[str, object]]:
    components: list[dict[str, object]] = []
    paths: set[Path] = set()
    direct: list[dict[str, object]] = []
    for app in sorted((root / "argocd" / "apps").glob("*.yaml")):
        text = app.read_text(encoding="utf-8", errors="replace")
        repo_match = re.search(r"^\s*repoURL:\s*https://github\.com/([^\s#]+?)(?:\.git)?\s*$", text, re.MULTILINE)
        revision_match = re.search(r"^\s*targetRevision:\s*([^\s#]+)\s*$", text, re.MULTILINE)
        if repo_match and revision_match:
            direct.append({
                "name": app.stem,
                "path": str(app.relative_to(root)),
                "source": repo_match.group(1),
                "pins": [{"tag": revision_match.group(1), "repo": repo_match.group(1)}],
            })
        match = re.search(r"^\s*path:\s*(platform/[^\s#]+)\s*$", text, re.MULTILINE)
        if match:
            paths.add(root / match.group(1))
    if not paths:
        paths = set((root / "platform").glob("*/"))

    for path in sorted(paths):
        chart = path / "Chart.yaml"
        if not chart.is_file():
            continue
        source = repo_from_chart(chart) or OFFICIAL_REPOS.get(path.name)
        pins: list[dict[str, str]] = []
        values = path / "values.yaml"
        if values.is_file():
            current_repo = ""
            for line in values.read_text(encoding="utf-8", errors="replace").splitlines():
                repo_match = REPO_RE.match(line)
                if repo_match:
                    current_repo = repo_match.group(1)
                tag_match = TAG_RE.match(line)
                if tag_match:
                    tag = tag_match.group(1)
                    if SHA_TAG_RE.match(tag) or SEMVER_RE.match(tag):
                        pins.append({"tag": tag, "repo": map_image_repo(current_repo, source) or ""})
                image_match = re.match(r"^\s{2}(sglang|vllm):\s*([^\s#]+)", line)
                if image_match and ":" in image_match.group(2):
                    image_repo, tag = image_match.group(2).rsplit(":", 1)
                    pins.append({"tag": tag, "repo": map_image_repo(image_repo, source) or ""})
        if not pins:
            pins.extend(chart_version_pins(path))
        unique: list[dict[str, str]] = []
        seen: set[tuple[str, str]] = set()
        for pin in pins:
            key = (pin["repo"], pin["tag"])
            if key not in seen:
                seen.add(key)
                unique.append(pin)
        components.append({"name": path.name, "path": str(path.relative_to(root)), "source": source, "pins": unique})
    return direct + components


def compare_commit(repo: str, tag: str) -> dict[str, object]:
    short_sha = SHA_TAG_RE.match(tag).group(1)  # type: ignore[union-attr]
    commit = github(f"repos/{repo}/commits/{short_sha}")
    pinned_sha = commit["sha"]
    comparison = github(f"repos/{repo}/compare/{pinned_sha}...main")
    return {
        "mode": "commits",
        "repo": repo,
        "pin": tag,
        "pinned_sha": pinned_sha,
        "upstream_sha": comparison.get("base_commit", {}).get("sha"),
        "ahead_by": comparison.get("ahead_by"),
        "behind_by": comparison.get("behind_by"),
        "commits": [
            {"sha": item["sha"], "subject": item["commit"]["message"].splitlines()[0], "url": item["html_url"]}
            for item in comparison.get("commits", [])
        ],
    }


def version_key(value: str) -> tuple[tuple[int, ...], int, tuple[tuple[int, object], ...]]:
    """Sort SemVer-like tags, including prereleases deterministically."""
    match = re.match(r"^[vV]?(\d+(?:\.\d+)*)(?:-([0-9A-Za-z.-]+))?", value)
    if not match:
        return (), 0, ()
    base = tuple(int(part) for part in match.group(1).split("."))
    prerelease = match.group(2)
    if not prerelease:
        return base, 1, ()
    tokens: list[tuple[int, object]] = []
    for token in prerelease.split("."):
        tokens.append((0, int(token)) if token.isdigit() else (1, token))
    return base, 0, tuple(tokens)


def compare_tag(repo: str, tag: str) -> dict[str, object]:
    releases = github(f"repos/{repo}/releases?per_page=100")
    tags = github(f"repos/{repo}/tags?per_page=100")
    candidates = [item.get("tag_name", "") for item in releases if item.get("tag_name")]
    candidates += [item.get("name", "") for item in tags if item.get("name")]
    candidates = [item for item in set(candidates) if SEMVER_RE.match(item)]
    latest = max(candidates, key=version_key) if candidates else None
    return {
        "mode": "tags",
        "repo": repo,
        "pin": tag,
        "latest_tag": latest,
        "update_available": bool(latest and version_key(latest) > version_key(tag)),
        "url": f"https://github.com/{repo}/releases" if latest else None,
    }


def audit_component(component: dict[str, object]) -> list[dict[str, object]]:
    pins = component["pins"]
    if not pins:
        reason = "no image tag, commit pin, or chart version found" if component.get("source") else "no upstream source or image pin found"
        return [{"mode": "unresolved", "repo": component.get("source"), "reason": reason}]
    findings: list[dict[str, object]] = []
    for pin in pins:
        repo = pin["repo"]
        if not repo or not re.fullmatch(r"[^/]+/[^/]+", repo):
            findings.append({"mode": "unresolved", "pin": pin["tag"], "reason": "upstream repository could not be inferred"})
            continue
        if not SHA_TAG_RE.match(pin["tag"]) and not SEMVER_RE.match(pin["tag"]):
            findings.append({"mode": "unresolved", "repo": repo, "pin": pin["tag"], "reason": "custom or non-semantic tag cannot be compared with GitHub releases"})
            continue
        try:
            findings.append(compare_commit(repo, pin["tag"]) if SHA_TAG_RE.match(pin["tag"]) else compare_tag(repo, pin["tag"]))
        except (urllib.error.HTTPError, urllib.error.URLError, TimeoutError, KeyError, json.JSONDecodeError) as error:
            findings.append({"mode": "error", "repo": repo, "pin": pin["tag"], "reason": str(error)})
    return findings


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--json", action="store_true", help="emit machine-readable JSON")
    args = parser.parse_args()
    root = Path.cwd()
    report = {"checked_at": datetime.now(timezone.utc).isoformat(timespec="seconds"), "components": []}
    for component in discover(root):
        report["components"].append({**component, "findings": audit_component(component)})
    if args.json:
        print(json.dumps(report, indent=2))
        return 0
    print(f"Upstream audit: {report['checked_at']}")
    for component in report["components"]:
        print(f"\n## {component['name']} ({component['path']})")
        for finding in component["findings"]:
            mode = finding["mode"]
            if mode == "commits":
                print(f"- {finding['repo']} {finding['pin']}: {finding['ahead_by']} commit(s) ahead on upstream main")
                for item in finding["commits"]:
                    print(f"  - {item['sha'][:10]} {item['subject']} ({item['url']})")
            elif mode == "tags":
                status = "update available" if finding["update_available"] else "current"
                print(f"- {finding['repo']} {finding['pin']}: {status}; latest={finding['latest_tag']}")
            else:
                details = " ".join(
                    str(finding[key]) for key in ("repo", "pin") if finding.get(key)
                )
                prefix = f" {details}:" if details else ":"
                print(f"- {mode}{prefix} {finding.get('reason', 'unknown reason')}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
