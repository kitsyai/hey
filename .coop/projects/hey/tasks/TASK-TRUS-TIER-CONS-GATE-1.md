---
id: TASK-TRUS-TIER-CONS-GATE-1
short_id: d5c671f50ad8
title: Trust tiers + consent gate (registered scope vs direct URL)
type: feature
status: done
created: 2026-07-13
updated: 2026-07-13
aliases: []
priority: p2
track: trust
acceptance:
  - installs from a registered signed scope run without prompt; direct https
    manifest URLs are treated UNTRUSTED (checksum-only, no authenticity) and
    require explicit consent or --allow-untrusted with a clear warning
tests_required: []
---
