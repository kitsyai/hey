---
id: TASK-HEY-DEPL-V1-MANI-CLI-1
short_id: 0ae8e5fbb583
title: hey.deploy.v1 manifest client + scope/URL resolution
type: feature
status: in_progress
created: 2026-07-13
updated: 2026-07-13
aliases: []
priority: p2
track: deploy
acceptance:
  - resolves <ref> as @scope/id (registry scopes map -> manifest_url template),
    a direct https manifest URL, or an existing github-release app name -- all
    coexist without breaking guten/djin
  - parses+validates hey.deploy.v1, selects the artifact matching current
    os/arch, enforces sha256; clear error when no artifact matches
  - unit tests via httptest against a synthetic manifest
tests_required: []
origin:
  authority_refs:
    - docs/deployment-manifest-v0.md
  derived_refs: []
---
