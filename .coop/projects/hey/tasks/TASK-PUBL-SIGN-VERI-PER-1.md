---
id: TASK-PUBL-SIGN-VERI-PER-1
short_id: eb8a02d3c312
title: Publisher signature verification (per-scope trust anchor)
type: feature
status: done
created: 2026-07-13
updated: 2026-07-13
aliases: []
priority: p2
track: trust
acceptance:
  - each registry scope carries a publisher public key; manifests are signed;
    hey verifies the manifest signature against the scope key BEFORE trusting
    any artifact/checksum in it
  - fills the existing fetch.Verify SigSpec seam; tampered manifest or wrong key
    is rejected in tests
tests_required: []
comments:
  - at: 2026-07-13T16:35:51.191Z
    author: pkvsi
    body: "DECISION (user): hey owns its signing end-to-end. Use Go stdlib
      crypto/ed25519 (NOT external minisign/cosign), wrapped in hey's own
      signature envelope + hey keygen/sign/verify. Per-scope pinned public keys.
      No third-party tool dep so the protocol can evolve. Keep a seam for future
      immutability/transparency-log (Merkle/blockchain-anchored) verification."
  - at: 2026-07-13T17:17:33.159Z
    author: pkvsi
    body: "Refined: distributed trust, not one key. Envelope carries a LIST of
      signatures; scope gains a threshold (M-of-N quorum = judge & jury). v0
      ships 1-of-1 but the format is quorum-shaped. Layer 2 (later): append-only
      transparency log + per-sig Merkle inclusion proofs, ledger-anchored, so
      history can't be rewritten (hey included). Spec:
      docs/trust-and-signing-v0.md."
---
