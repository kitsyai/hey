---
id: TASK-SIGN-RELE-MINI-VERI-1
short_id: 1da7de37eec7
title: Sign releases with minisign and verify in hey
type: feature
status: todo
created: 2026-07-12
updated: 2026-07-12
aliases: []
priority: p2
track: distribution
acceptance:
  - goreleaser signs checksums.txt with minisign in the release workflow
  - registry gains minisign_pubkey field; fetch.Verify SigSpec seam implemented
    and enforced when key present
  - tampered checksums.txt is rejected in a test
tests_required: []
---
