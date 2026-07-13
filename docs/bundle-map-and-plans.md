# hey: bundle map, plans, and the orchestrator north star

hey is a **lean orchestrator**, not a doer. It performs no domain work of its
own. It knows *which native tool* does a job and *in what order* to run tools —
and nothing about the jobs themselves. djin files GST returns; hey does not.
guten renders invoices; hey does not. nmap scans a network; hey does not. hey
downloads the right **native bundle** for the exact platform (unlike Docker's
hypervisor/containers or brew/apt's images), runs it, and relays the result.

What stays small is hey. What grows is *data*: the map of bundles, and the
library of plans. This document records the intended shape so the growth stays
disciplined.

## Three layers (keep them separate)

1. **Bundle map / registry** — pure data. Namespaced, trusted-publisher
   manifests: `@scope/tool` → a `hey.deploy.v1` native bundle per os/arch (see
   [deployment-manifest-v0.md](deployment-manifest-v0.md)). This is the
   knowledge graph of *what can run*. Already in place: registry `scopes` +
   the deployment manifest contract.
2. **Plan layer** — declarative recipes mapping an *intent* → a sequence of tool
   invocations, with tool-presence detection, platform-conditional steps, and
   consent gates for sensitive actions. The **free/deterministic tier runs
   pre-authored plans** — hey executes a known recipe. Plans are the asset that
   compounds. Sketch: [plan v0](#plan-v0-sketch-draft).
3. **AI planner (paid)** — when no pre-authored plan fits, an AI layer selects
   tools from the registry and composes a plan. Crucially it emits the **same
   declarative plan format** the deterministic executor runs, so AI *proposes*
   and the audited/consent-gated executor *disposes*. Intelligence is a
   differentiator on top, never the foundation.

## Trusted-publisher namespaces

Reserve now: **`@heypkv`**, **`@kitsy`**. Add well-known publishers later
(`@google`, `@meta`, …). The model is exactly **Homebrew-cask / winget / scoop**:
hey *curates and verifies genuine official signed artifacts*; it is not a
partnership or an authorization from the publisher. A scope entry is one line —
a `manifest_url` template — and the only publisher-specific data hey ever holds.

(`@heypkv` is registered today; `@kitsy` and others get a scope line once their
manifest hosting URL is known — hey never guesses infrastructure URLs.)

## Trust vs. sandbox — the honest tradeoff

Native bundles are hey's edge over Docker, but native means **no container
isolation**. So safety in v1 is **trust-based**: verified publisher + mandatory
sha256 (+ future signature/notarization) + explicit user consent + revocation.
A trusted bundle runs with full machine access — that is trust, not isolation.
OS-level sandboxing (macOS seatbelt, Windows AppContainer, Linux
bubblewrap/namespaces) is a deliberate later capability. The product promise is
"hey runs *trusted* native tools"; "sandboxed" must not be implied before it is
built.

## Plan v0 sketch (draft)

A plan is declarative data hey executes step by step. Draft shape — to be
refined before implementation:

```jsonc
{
  "hey_plan": 1,
  "intent": "scan-local-devices",
  "consent": "Scan your local network for devices?",   // gate before running
  "steps": [
    { "tool": "@heypkv/netscan", "when_missing": "install",
      "platforms": ["macos","windows","linux"],
      "run": ["scan", "--subnet", "{subnet}"],
      "capture": "json" }
    // a step may name a registry tool (installed on demand) OR a known system
    // tool (nmap) that hey only *detects* and asks consent to use — never
    // silently installs system-wide.
  ],
  "output": "devices"
}
```

Example intents to seed the library:

- `scan-local-devices` → a scanner tool (registry bundle, or detected `nmap`).
- `file-gstr` → `@heypkv/djin gstr1 build …`.
- `make-invoice` → `@kitsy/guten batch …`.

Design rules for plan v0: every step is either a registry tool (installed
on demand, verified) or a *detected* system tool (never auto-installed
system-wide); sensitive steps (network scan, install, filesystem writes outside
`~/.hey`) require consent; the same format is what the paid AI planner emits.

## Where this is going

hey-core stays a small, boring, trustworthy executor: resolve → verify →
install → run → clean up, plus a plan executor. Everything that makes it feel
like "an AI-native assistant on your machine" — the bundle map, the plans, the
AI planner — is pluggable, data-described, and grows without growing hey.
