---
id: TASK-HEY-INST-RUN-APP-BUN-1
short_id: 37b23b0cb77b
title: "hey install/run app bundles: archive/appimage/binary/installer/link +
  --temp/--location"
type: feature
status: in_review
created: 2026-07-13
updated: 2026-07-13
aliases: []
priority: p2
track: deploy
acceptance:
  - "install per kind: archive->ExtractTree+launch, appimage/binary->chmod+run,
    installer->hand to OS, link->open URL; launch per interface
    (window=launch&return, hey-contract=existing runUI)"
  - hey run --temp installs to a throwaway dir and cleans up on exit; --location
    installs to a caller path; default ~/.hey/apps/<id>/<version>
  - end-to-end test against a synthetic manifest + a placeholder bundle built
    in-test (no network, no real GUI)
tests_required: []
origin:
  authority_refs:
    - docs/deployment-manifest-v0.md
  derived_refs: []
---
