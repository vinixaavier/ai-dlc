---
name: upstream-component-audit
description: Audit deployed components in a GitOps codebase against their upstream repositories, comparing main-based builds by commit count and tagged builds by upstream release/tag.
metadata:
  short-description: Compare components with upstream tags and commits
---

# Upstream component audit

Use this skill when the user asks whether the components in this repository are
up to date with upstream, how many commits a build is behind, or what changed
since the pinned build.

Run the bundled auditor from the repository root:

```bash
python3 skills/upstream-component-audit/scripts/audit_upstreams.py
```

The auditor discovers Argo-managed `platform/*` applications, reads each
component's `Chart.yaml`, vendored dependency Charts, and top-level
`values.yaml`, inspects image pins, and also audits Argo applications that
point directly at a GitHub repository with `repoURL` and `targetRevision` (for
example, a CRD application pinned to `v1.6.2`). When a local Chart omits its
GitHub URL, use the official repository URL from the Chart metadata, embedded
documentation, or the explicit official mapping in the bundled script. Do not
leave a component unresolved merely because its source is not repeated in the
top-level platform Chart.
It uses GitHub's API for public upstream repositories. A component is handled
according to its pin:

- `main-<sha>` is compared with upstream `main`; report `ahead_by`, the pinned
  commit, and every later commit.
- A semantic image tag such as `v0.10.1` or `0.10.1` is compared with the
  newest upstream release/tag and report the available version.
- A vendored Chart's semantic `version` is compared with the newest upstream
  release/tag when it is the component's upstream Chart version.
- A custom or non-semantic image tag is reported as `unresolved` with its
  resolved official repository and the reason that GitHub releases cannot
  compare it; do not invent a version or commit count.

Inspect `unresolved` entries before presenting results. They should now mean a
custom/non-semantic pin, a private registry, or genuinely missing metadata—not
merely that the source URL was absent from the top-level Chart. Resolve
mappings manually when possible and clearly label assumptions.

The report must distinguish the build pin, the upstream reference and check
time, the number of commits or version updates available, and the subject of
each intervening commit. Include direct upstream links when available.

Do not modify manifests, image tags, lock files, or the worktree. Network
access is read-only. If GitHub rate limiting or a private repo prevents a
check, report that component as `error` with the reason instead of treating it
as current.
