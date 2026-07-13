---
id: TASK-HEY-MOBI-NEAR-DEVI-1
short_id: 6d762ae14bd3
title: "hey mobile: nearby devices + apk push (adb) + open (link)"
type: feature
status: in_review
created: 2026-07-13
updated: 2026-07-13
aliases: []
priority: p2
track: deploy
acceptance:
  - hey mobile devices lists adb devices (USB + same-network); clear guidance if
    adb missing
  - hey mobile push <ref> resolves the android/package artifact, verifies
    sha256, adb -s <device> install; hey open <ref> opens a link artifact
  - flow tested against a mock adb on PATH (no real device)
tests_required: []
origin:
  authority_refs:
    - docs/deployment-manifest-v0.md
  derived_refs: []
---
