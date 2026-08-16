# hey services — a local runtime agent for offline heypkv.ai

Status: design. Nothing in this document is implemented; it defines the
target shape for `hey up`, the discovery hub, the security envelope, the
named-hosts/CA tier, edge AI, and the agentic tier, and shows exactly which
existing hey machinery each piece reuses. See [north-star.md](../north-star.md)
for the ecosystem this sits inside and [feature-set-v1.md](../feature-set-v1.md)
/ [hey-kernel-brief.md](../hey-kernel-brief.md) for the two prior specs this
document supersedes and unifies.

## 1. Vision & principles

hey installs with one command — `curl -fsSL https://kitsy.ai/hey.sh | sh`
(Linux/macOS) or `irm https://kitsy.ai/hey.ps1 | iex` (Windows), mirrored at
`heypkv.ai/hey.sh` / `heypkv.ai/hey.ps1` — and that is the only manual
install step in the entire stack (README.md: "hey itself is the only thing
you ever install by hand"). Once installed, hey is not just an app runner:
it is the **local runtime agent** for the machine. `hey up <service>` starts
local servers — an LLM, postgres, a cache, an agent host — and every
heypkv.ai/kitsy.ai surface (web app or desktop app) talks to them instead of
a heypkv.ai server, for a genuinely offline experience. The mechanism is
deliberately not heypkv-exclusive: heypkv defaults ship built in, but every
service, port, and origin policy is overridable through `hey config`, so a
developer with no relationship to heypkv can point `hey up postgres` or
`hey up ai` at their own project.

Principles (extending [north-star.md](../north-star.md) §Principles, not
replacing them):

1. **Offline-first, local-first.** Every service works fully with zero
   network after `hey up`. heypkv.ai's cloud is an upgrade, never a
   requirement — this document is what makes north-star's rung 2→3 jump
   ("+ hey services" → "+ hey kernel") real.
2. **Zero-complexity install, zero-complexity discovery.** No Docker, no
   `docker-compose.yml`, no manual port bookkeeping. One command starts a
   service; one HTTP call (`GET /services`) tells any caller — a browser
   tab, the desktop app, another local service — what's running and where.
3. **heypkv-default, user-repurposable.** The hub ships a built-in
   heypkv/kitsy catalog (§8) but that catalog is *data*, exactly like
   `internal/registry`'s embedded `default.json` and `internal/svc`'s
   embedded `packs.json` are data today — override files and env vars take
   precedence over the embedded default, never the reverse (§4).
4. **No hacks.** `hey` already refuses non-HTTPS downloads, requires a
   SHA-256 for every artifact, and binds every service to `127.0.0.1` by
   pack-review policy, not user config
   ([pack-manifest-v0.md](../pack-manifest-v0.md) §Instance layout &
   invariants). Everything this document adds extends that discipline;
   nothing bypasses it.
5. **Lean core, pluggable everything.** The hub, the Service contract, and
   the security envelope are the only new *concrete* machinery. Every
   individual service stays a driver-interpreted pack or a contract-native
   app, exactly as [feature-set-v1.md](../feature-set-v1.md) already
   commits to: "lifecycle logic lives in a handful of Go drivers; everything
   specific to postgres or python lives in data-only packs."

This document formalizes and merges two things that exist on paper but not
as one system: `internal/svc` (the provisioner — shipped, proven on
postgres today, §8) and [hey-kernel-brief.md](../hey-kernel-brief.md) (the
`hey up` spike idea — origin-gated `/hey/info`, pairing tokens, PNA
preflight, never built). It adds: one discovery hub with a stable port and
a multi-service `/services` list (§4), a Service contract every service
implements (§3), a security envelope built once and shared by every
service (§5), a named-hosts + local CA tier (§6), and two new service kinds
— edge AI and the agentic host — that need new drivers but the same
contract (§8).

## 2. Architecture overview

```
   browser tab (heypkv.ai / kitsy.ai, or a third-party dev's own site)
   fetch() + X-Hey-Client header, Origin-checked, PNA preflight  |    desktop app:
                                    |                             |    native fetch,
                                    v                             |    no CORS/PNA
   +--------------------------------------------------------------+---------+
   |                       hey discovery hub — 127.0.0.1:8525 (§4)          |
   |  GET /services  GET /services/<name>  GET /healthz                     |
   |  security envelope (§5): CORS allow-list, custom header, PNA preflight |
   |  reverse proxy (§6, phase 2): hey.local/<service>, <service>.hey —     |
   |    TLS via local CA, routes by Host/SNI to the service port            |
   +-----------------------+------------------------+-----------------------+
             registers/health-polls                  registers/health-polls
                             v                                    v
   +----------------------------------+   +------------------------------------+
   | svc-managed (internal/svc)       |   | contract-native (internal/state +  |
   |  archive-exec: postgres (shipped)|   | internal/contract, same as UI      |
   |  model-server: ai/qwen-phi (new) |   | apps today)                        |
   |  agent-host:   agent (new)       |   |  ai (OpenAI-compatible HTTP)       |
   |                                   |   |  agent (MCP host)                  |
   |                                   |   |  registry — npm (shipped)          |
   +-----------------+-----------------+   +------------------+------------------+
                     v                                        v
   ~/.hey/svc/<inst>/bin,data,logs,svc.json(0600)   ~/.hey/registry/, ~/.hey/artifacts/
                                                     models/ (new: content-addressed,
                                                     resumable — §8)
```

Two lifecycle managers already exist and neither goes away: `internal/svc`
(pack-driven processes with their own wire protocol — postgres is the
proof) and `internal/state` + `internal/contract` (HTTP-native processes
speaking hey's existing [app-contract-v0.md](../app-contract-v0.md)
handshake — how UI apps like `djin ui` work today). The hub is a **third,
thin layer above both**: it doesn't replace `svc.json` or `procs.json`, it
polls and republishes them as one `/services` view, and owns CORS, named
hosts, and TLS. `ai`, `agent`, and `registry` are HTTP-native and slot into
the existing UI-app path with almost no new lifecycle code; `postgres` (and
future wire-protocol services) stay on `internal/svc`.

## 3. The Service contract

`internal/pkgreg.Format` is the existing precedent for "one small interface,
concrete shared machinery behind it" — a package ecosystem (npm today)
implements `Name()`/`Mount()`/`List()`/`Clear()` and gets proxying, auth
caching, and logging for free from `pkgreg.Core`
(`internal/pkgreg/pkgreg.go`). `Service` follows the same shape, one layer
above `svc.Pack`:

```go
// Service is the seam a local capability implements to appear in the hub's
// /services list. It does not replace pack or app-contract lifecycle
// (internal/svc, internal/state+internal/contract still own process
// start/stop/health) — it is the hub-facing metadata neither has a field
// for today: protocol, browser bridge, CORS policy, MCP.
type Service interface {
	// Name is the hub-visible name ("postgres", "ai", "agent", "registry")
	// — also the svc.Up/internal/state key and the hey.local/<name> path.
	Name() string

	// Lifecycle names which process manager owns this service: "svc"
	// (internal/svc drivers) or "contract" (internal/state +
	// internal/contract, the existing UI-app handshake path).
	Lifecycle() string

	// Protocol is what's listening on Port(): "postgres-wire", "http",
	// "openai-http" (ai), "mcp".
	Protocol() string

	// Bridge, if non-nil, is the HTTP surface hey serves so a browser —
	// which cannot open a raw postgres-wire socket — can use this service.
	// nil means Protocol() is already browser-speakable HTTP.
	Bridge() *Bridge

	// CORS is this service's additive origin policy on top of the hub
	// default allow-list (§5). Empty means "hub default only".
	CORS() CORSPolicy

	// MCP reports the service's Model Context Protocol endpoint, if any,
	// so `hey up agent` can compose it as a tool (§8).
	MCP() (endpoint string, ok bool)
}

// Bridge describes hey's browser-facing translation of a native protocol.
type Bridge struct {
	Path string // e.g. "/postgres/bridge" — the hub's reverse proxy for
	            // wire-protocol services, or the service's own HTTP surface.
	Kind string // "http-query-api" (postgres: scoped, parameterized query,
	            // no DDL/superuser reachable from a browser), "openai-
	            // compatible" (ai, Bridge is nil in practice), "mcp" (agent).
}

type CORSPolicy struct {
	AddOrigins []string // additive to the hub default allow-list (§5)
}
```

Concretely, this is a **data-only extension of the existing pack schema**,
not a parallel manifest — [pack-manifest-v0.md](../pack-manifest-v0.md)'s
whole philosophy is "packs travel the same trust pipeline as apps... a new
service is a data-only change." `packs.json` v1 adds four optional fields
next to the ones postgres already uses (`driver`, `kind`, `start`, `ready`,
`conn`, `stop` — see `internal/svc/pack.go`):

```jsonc
{
  "postgres": {
    "pack": "postgres", "driver": "archive-exec", "kind": "service",
    // ...existing fields unchanged (internal/svc/packs.json today)...
    "protocol": "postgres-wire",
    "bridge": { "path": "/postgres/bridge", "kind": "http-query-api" },
    "cors": { "add_origins": [] },
    "mcp": false
  }
}
```

Contract-native services (`ai`, `agent`, `registry`) don't live in
`packs.json` today — they'd need the same four fields added to their
registry entry (`internal/registry.App`, which today only carries
`Description`/`Source`/`UICommands`) or a small sibling `services.json`.
Which gets extended is an implementation decision, not a design one; either
way `validatePack`/`validateApp`-style validation rejects malformed entries
as it does today. Process lifecycle for `Lifecycle()=="svc"` reuses
`internal/svc.Up`/`Start`/`Stop`/`WaitReady` and `internal/proc.Detach`/
`Alive`/`KillTree`/`TermSignal` unchanged; for `Lifecycle()=="contract"` it
reuses `internal/state.Put`/`Prune` and `internal/contract
.WaitHandshakeFromLog`/`Healthy`/`Shutdown` unchanged. The hub adds nothing
to either process manager — it only reads `svc.json`/`procs.json` and
republishes them.

## 4. Discovery hub spec

### Port

**8525.** Chosen deliberately, by exclusion:

- Inside the IANA **User Ports** range (1024–49151) — no admin/root needed
  to bind it on Windows, Linux, or macOS, matching the "user-configurable
  range" requirement.
- Outside the **Dynamic/Private** range (49152–65535), so an OS's ephemeral
  allocator (which hey relies on for UI-app `--port 0` binding, per
  [app-contract-v0.md](../app-contract-v0.md)) can never collide with it.
- Distinct from hey's other two fixed local ports: `hey registry`'s default
  4874 (`cmd/hey/registry.go`) and crow's fixed 9999 (referenced from
  `internal/pkgreg/lifecycle.go`'s doc comment and `@heypkv/crow`'s
  `GraphClientOptions.sources` fallback chain in the webapp).
- Outside `internal/svc`'s own instance port range, 5432–5600
  (`internal/svc/instance.go`, `portRangeLo`/`portRangeHi`) — postgres
  instances and the hub can never fight over the same block.
- Avoids common local dev-tool defaults: 3000/4000/5000/5173 (JS dev
  servers), 5432/6379 (postgres/redis), 8000/8080/8888/9000/9090/9200,
  11211 (memcached), 27017 (mongo).
- Spot-checked against the IANA Service Name and Port Number Registry: no
  listed assignment at time of writing. `HEY_HUB_PORT` / `--hub-port`
  override it regardless — a future collision is a config change, not a
  breaking one, same discipline as `HEY_REGISTRY`/`HEY_PACKS`.

### Endpoints

```
GET /services            -> the full catalog + live state (below)
GET /services/<name>     -> one service's record, 404 if unknown/not up
GET /healthz             -> hub's own liveness (200, no body needed)
```

This is [hey-kernel-brief.md](../hey-kernel-brief.md)'s single-capability
`GET /hey/info` generalized into a multi-service list — the kernel brief's
"info/discovery" v0 capability becomes `/services` verbatim, with pairing
and per-origin state layered on by the security envelope (§5) rather than
baked into the endpoint shape.

`/services` response — one `svc`-lifecycle entry (postgres, wire protocol,
needs a bridge URL) and one `contract`-lifecycle entry (ai, already HTTP):

```json
{
  "hey_hub": 0, "hub_version": "0.3.0",
  "services": [
    { "name": "postgres", "pack": "postgres", "lifecycle": "svc",
      "protocol": "postgres-wire", "port": 5432,
      "bridge_url": "http://127.0.0.1:8525/postgres/bridge",
      "health": "healthy", "version": "16.9.0", "pid": 41232,
      "started": "2026-08-17T09:00:00Z" },
    { "name": "ai", "pack": "ai-qwen-phi", "lifecycle": "contract",
      "protocol": "openai-http", "port": 51234,
      "url": "http://127.0.0.1:51234", "health": "healthy",
      "version": "qwen2.5-3b-instruct-q4", "mcp": false }
  ]
}
```

### Registration mechanism

The hub is itself a hey-managed background daemon, using the exact pidfile +
TCP-liveness pattern `internal/pkgreg/lifecycle.go` already implements for
`hey registry` — `PidInfo{PID, Port, Store}`, a JSON pidfile
(`~/.hey/hub/hub.pid`), and `ServerUp(port)` as the liveness probe. That doc
comment already says this "mirror[s] crow's ergonomics" — the hub is a
third instance of a pattern now used twice in production.

`hey up <service>` composes three existing calls:

1. Ensures the hub is running — spawns it (`internal/svc.Start`'s detach
   path) if `ReadPidFile`+`ServerUp` says it isn't.
2. Provisions/starts the service through whichever lifecycle it declares —
   `svc.Up(svcDir, name, pack, version, nil)` for pack-based services
   (unchanged from `cmd/hey/svc.go`'s `svcUp` today), or the existing
   UI-app run path (port handshake via `internal/contract`) for
   contract-native ones.
3. Upserts the record into the hub's table — an in-memory map rebuilt by
   polling `svc.ListInstances`/`state.Load` plus live health
   (`svc.Alive`/`contract.Healthy`), not a separate persisted store. The
   hub is a **view**, not a second source of truth, avoiding drift bugs
   where the hub's copy of "is postgres running" disagrees with `svc.json`.

`hey svc up postgres` keeps working — `hey up postgres` becomes the general
spelling and `svc` stays a recognized alias, so the migration costs nothing
for the one service already shipped.

### Config file & `hey config`

Same precedence convention as `registry.json`/`packs.json`
([registry-v0.md](../registry-v0.md)/[pack-manifest-v0.md](../pack-manifest-v0.md)):
`HEY_HUB_CONFIG` (env, path or https URL) → `~/.hey/hub.json` → embedded
default. Config sets are wholesale — no merging, same as today's registry
and packs resolution.

```jsonc
// ~/.hey/hub.json
{
  "hey_hub": 0,
  "port": 8525,
  "cors": { "allow_origins": ["https://heypkv.ai", "https://*.heypkv.ai",
                                "https://kitsy.ai", "https://*.kitsy.ai"] },
  "services": {
    "postgres": { "cors": { "add_origins": ["https://example.com"] } },
    "ai":       { "disabled": false }
  }
}
```

```
hey config get [<key>]              # e.g. hey config get cors.allow_origins
hey config set <key> <value>        # e.g. hey config set services.postgres.port 5433
hey config edit                     # opens $EDITOR on ~/.hey/hub.json
```

Keys are dotted paths into the JSON shape above (`hub.port`,
`cors.allow_origins`, `services.<name>.cors.add_origins`,
`services.<name>.port`, `services.<name>.disabled`). The per-service CORS
override doubles as the `--cors` flag's persistence: `hey up postgres
--cors https://example.com` is sugar for `hey config set
services.postgres.cors.add_origins '["https://example.com"]'` before
provisioning.

## 5. Security envelope

### Threat model

[hey-kernel-brief.md](../hey-kernel-brief.md) already named the core risk
correctly: **"localhost is not a security boundary."** Any open browser tab
— not just heypkv.ai — can attempt requests against `127.0.0.1`. The hub and
every service must defend against:

- **Drive-by cross-origin requests** — a malicious/compromised page issues
  a `fetch()` or plain `<form>` POST at a local port, hoping for a readable
  response (leak) or a side effect even blind (a no-cors POST).
- **DNS rebinding** — a page's origin resolves to a public IP at
  fetch-check time, then a request's Host header is rebound to
  `127.0.0.1`; Origin checks alone don't catch this without Host validation.
- **Mixed content / non-secure-context blocks** — a `https://heypkv.ai` page
  fetching `http://127.0.0.1:<port>` is mixed-content-flagged by some
  browsers (Safari is stricter than Chrome/Firefox about a loopback-HTTP
  secure-context exception) — a compatibility failure as much as a
  security one, and the reason §6 exists.
- **Private Network Access** — Chrome's PNA requires an explicit preflight
  opt-in before a public-website origin may reach a private/loopback
  address at all — already flagged as a hard requirement in the kernel brief.

### Envelope, built once

The decisions below apply uniformly, as one middleware chain, to the hub's
own endpoints *and* every service's HTTP surface (contract-native
listeners, and the bridge endpoints the hub proxies for wire-protocol
services). This is new machinery — `internal/pkgreg`'s registry server has
**no** CORS/origin gating today (it's built for CLI/npm-client consumption
over trusted loopback, not browser access) — the only thing reused from it
is the Go idiom, `pkgreg.LoggingMiddleware`'s `http.Handler`-wrapping
pattern (`internal/pkgreg/pkgreg.go`), which the envelope follows.

1. **Default origin allow-list**: `heypkv.ai`, `kitsy.ai`, and their
   subdomains, plus a small fixed set of localhost dev ports (configurable,
   not `localhost:*`). Per-service **additive** override via `hey up
   <service> --cors <origin>` or `hey config set
   services.<name>.cors.add_origins` (§4) — so one service can widen its
   own access without weakening every other service's default.
2. **Required custom header.** Every request must carry a fixed custom
   header (e.g. `X-Hey-Client: 1`). Per the Fetch spec, a non-simple header
   always triggers a CORS preflight — a plain HTML `<form>` POST and a
   `fetch()` in `mode: "no-cors"` can neither one set custom headers, so
   neither can ever reach a mutating endpoint, blind or otherwise. The
   browser is structurally forced to ask via `OPTIONS` first, and the
   envelope's Origin check runs before the real request is dispatched.
3. **Private-Network-Access preflight.** The envelope answers
   `Access-Control-Request-Private-Network: true` with
   `Access-Control-Allow-Private-Network: true` only for allow-listed
   Origins — same gate as CORS, applied to the PNA preflight specifically.
4. **Loopback-only binding, always.** Not new — [pack-manifest-v0.md](../pack-manifest-v0.md)'s
   existing invariant ("Packs must bind loopback; enforced by pack review,
   not user config") generalized to every service kind.
5. **Host header validation.** The hub and every service reject requests
   whose Host header isn't `127.0.0.1:<port>`, `localhost:<port>`, or (once
   §6 ships) an issued `hey.local`/`<service>.hey` name — closing the DNS
   rebinding gap Origin-checking alone leaves open.

## 6. Named-hosts tier

**Phase one ships without this tier.** `127.0.0.1:<port>` (or the hub-
proxied `127.0.0.1:8525/<service>/...`) works immediately, needs no
elevation, and is the default for every service in §8 at P1 (§9).

**Phase two (fast-follow)** adds `https://hey.local/<service>` and/or
`https://<service>.hey` as a first-class tier, not a cosmetic alias:

1. **hosts-file entries.** hey writes a clearly-delimited, hey-managed block
   into `/etc/hosts` (Linux/macOS) or
   `C:\Windows\System32\drivers\etc\hosts` (Windows) — `# hey-managed`/
   `# end hey-managed` markers, so `hey hosts install` rewrites only its
   own section and is idempotent. Needs elevation on every OS: a `sudo`
   re-exec on Linux/macOS, a UAC-elevated helper on Windows — the same
   class of one-time prompt mkcert and Docker Desktop already ask for.
2. **Local CA (mkcert pattern).** `hey ca install` generates a CA keypair
   under `~/.hey/ca/` (private key 0600, mirroring `svc.json`'s
   credential-file discipline) and installs the CA cert into OS and browser
   trust stores — Windows via `certutil`, macOS Keychain via `security
   add-trusted-cert`, Linux `update-ca-certificates` plus the NSS database
   Firefox/Chromium read there. This is mkcert's cross-platform trust-store
   code path, cited as concrete prior art to port, not reinvent.
3. **Per-host certs**, minted on demand by `hey ca`/the hub, signed by the
   local CA, for `hey.local` and every `<service>.hey`, cached under
   `~/.hey/ca/certs/`.
4. **hey's reverse proxy** — the hub's HTTP surface with a TLS listener
   added — terminates these certs and routes by SNI/Host to the right port.

### Why this tier matters (not just naming)

A locally-trusted CA turns every named hey host into a **genuine secure
context** in every browser, Safari included. Today, a `https://heypkv.ai`
page calling `http://127.0.0.1:<port>` hits two walls: Safari is materially
stricter than Chrome/Firefox about granting loopback HTTP an implicit
secure-context exception, so some capability-gated web APIs refuse to
engage; and mixed-content blocking can stop the fetch outright.
`https://<service>.hey`, signed by a CA the browser already trusts, makes
both disappear — page and service are both https, no scheme mismatch to
police. It also simplifies Private-Network-Access, whose behavior differs
by whether the target is itself a secure context — a trusted local
`https://` endpoint is the easier target to reason about than plain-HTTP
loopback.

## 7. App-side integration

heypkv.ai/kitsy.ai apps already follow a **neutral-facade, runtime-bound-
implementation** architecture, documented in the webapp repo's
`docs/runtime-platform-facades.md` and `docs/hub-runtime-architecture.md`:
product code imports `@heypkv/platform`, `@heypkv/hub`, `@heypkv/analytics`
— never a runtime implementation package directly — and each runtime host
(web, RN, desktop) aliases those neutral specifiers to `-web`/`-rn`/
`-desktop` packages at build time. hey services plugs into **two** of these
facades as new backend variants; it invents no new architecture on the app
side.

### AI: a third routing lane, not a new pattern

`@heypkv/platform`'s AI contract (`platform/ai`) already defines
`PlatformAiProviderKind = "edge-local" | "server-vinci"` and a router,
`resolvePlatformAiProvider({ entitlements, isAuthenticated, policy })`,
consumed by `@heypkv/platform-web`'s `usePlatformAi` hook. Today that's a
two-way switch: `edge-local` dispatches to a dedicated Web Worker running a
WASM/WebGPU model in-browser; `server-vinci` calls HEYPKV's hosted cloud
models (`server-vinci` is literally named after the "vinci" codename this
document's vision section retires in favor of real model names — legacy
type name, not a promise about which model runs).

hey services adds a **third `PlatformAiProviderKind`** — `"hey-local"` —
routed ahead of `edge-local` in `DEFAULT_PLATFORM_AI_ROUTE_POLICY` when a
healthy `ai` service is present. Selecting it needs one new capability that
doesn't exist yet: a discovery probe. `@heypkv/crow`'s existing
`GraphClientOptions.sources` is the pattern to copy exactly — an ordered
list of base URLs (today: CDN → `http://localhost:9999/...` → bundled
fallback), first-to-answer-its-manifest wins. `usePlatformAi`'s init gets
the same shape: probe `http://127.0.0.1:8525/services` (short timeout, the
`X-Hey-Client` header from §5) → `ai` healthy → route `hey-local` → else
the existing `edge-local` worker → `server-vinci` stays the entitlement-
gated premium lane, unaffected. No UI code changes: `usePlatformAi`'s public
contract is unchanged, only the routing table gains a rung — matching
`hub-runtime-architecture.md`'s own stated direction ("baseline browser AI
is the default AI lane; premium HEYPKV-hosted models are a future
entitlement-gated lane behind the same neutral contract"): hey-local slots
in *ahead* of baseline, same contract.

### Storage: the seam already has a placeholder

`@heypkv/hub`'s managed-data capability (`hub/index`'s `data.ts` /
`relational.ts`) defines `HubDatabaseProvider.openDatabase()` with policies
`managed` / `local-only` / `cloud-required`
(`docs/hub-runtime-architecture.md` §Data posture vocabulary), backed today
by `createSqlJsHubDatabaseProvider()` (SQL.js + IndexedDB, web), Expo SQLite
(RN), or SQL.js over a preload bridge (desktop). Business — "the reference
implementation for portable HEYPKV applications"
(`docs/business/business-runtime-template.md`) — already names the
substitution seam this document needs: *"A SQLite-compatible Hey capability
can therefore replace SQL.js or native SQLite without changing
`BusinessDataSource` at all... A future Hey PostgreSQL integration must
either supply a tested compatibility implementation beneath `HubDatabase`,
or implement the same semantic `BusinessDataSource` through its own
repositories and migrations."* `@heypkv/hub-web` already ships the exact
stub this fills — `heyDatabase.ts`'s `createHeyHubDatabaseProvider()`,
today throwing `HubDatabaseProviderNotSupportedError("hey", ..., "Hey local
databases are not supported in the Web runtime yet.")`. That function is
where postgres's bridge (§8) connects: when `hub.data`'s policy is `managed`
and the discovery probe finds a healthy `postgres` service,
`createHeyHubDatabaseProvider()` opens a `HubDatabase` backed by the HTTP
query bridge instead of throwing — Business's repositories/migrations
decide whether postgres is SQLite-*compatible* enough or needs its own
path, exactly as that doc anticipates. The policy vocabulary itself is
untouched.

### Discovery client sketch

```ts
// New code (does not exist today) — lives alongside platform-web /
// platform-desktop's other runtime-owned backends, following crow's
// GraphClientOptions.sources probe-in-order idiom.
interface HeyServiceInfo {
  name: string; protocol: string; port: number;
  url?: string; bridgeUrl?: string; health: "healthy" | "unhealthy";
}

async function probeHeyHub(timeoutMs = 300): Promise<HeyServiceInfo[] | null> {
  try {
    const res = await fetch("http://127.0.0.1:8525/services", {
      headers: { "X-Hey-Client": "1" }, signal: AbortSignal.timeout(timeoutMs),
    });
    return res.ok ? (await res.json()).services : null;
  } catch {
    return null; // hey not installed / hub not running — fall through
  }
}

async function getService(name: string): Promise<HeyServiceInfo | null> {
  const services = await probeHeyHub();
  return services?.find(s => s.name === name && s.health === "healthy") ?? null;
}
```

Native `fetch` on the desktop app isn't subject to Fetch-spec CORS, so
`getService` is identical code across `platform-web` and `platform-desktop`
— only the calling context differs, matching the neutral-facade rule that
runtime differences live in the implementation, not the contract.

## 8. Service catalog v1

### `ai` — edge AI (new)

Real default models, not a placeholder codename: **Qwen2.5-Instruct** and
**Phi-4-mini / Phi-3.5-mini** as the two default families, in size tiers
matching north-star's "bring your own hardware" premise —

| Tier | Params | Target hardware |
|---|---|---|
| tiny | ~0.5–1.5B | low-RAM MSME hardware, the north-star baseline |
| small (default) | ~3B | typical laptop/desktop, balanced quality/speed |
| medium (opt-in) | ~7–8B | capable machines only, not auto-selected |

Served llama.cpp-server-style: a fetched, checksummed `llama-server`-
compatible binary (same trust pipeline as postgres — HTTPS-only, pinned
SHA-256, per-`{os}_{arch}` artifacts), run with templated flags via a new
**`model-server` driver** alongside `archive-exec` in `internal/svc`,
exposing an OpenAI-compatible `/v1/chat/completions` / `/v1/completions` /
`/v1/embeddings` surface (`Protocol()` = `"openai-http"`, no `Bridge` needed
— browsers already speak HTTP/JSON to it, unlike postgres).

Model weights are large (multi-GB) files distributed through hey's existing
artifact machinery — but that machinery needs two capabilities it doesn't
have yet, called out explicitly rather than assumed: `internal/fetch
.Download` today does a single-shot download capped at 512 MB
(`maxArchiveBytes`) with no HTTP Range/resume support, and every existing
store (`internal/pkgreg`'s npm store, `internal/svc`'s per-instance `bin/`)
is namespaced by package/pack name, not content-addressed. Model artifacts
need **resumable, range-based downloads** (multi-GB files over home
internet get interrupted) and **content-addressing by digest** (so the same
`qwen2.5-3b-q4` weight isn't re-fetched or re-stored once per pack that
references it — genuine dedup). Both extend, rather than replace, the
existing checksum-verified, HTTPS-only pipeline.

### `agent` — the agentic tier (new)

`hey up agent`: model + tools + memory, not just inference. Architecturally
the agent is an **MCP host**: `@heypkv/crow` already ships an MCP server
(`crow mcp`) exposing graph data, so the agent composes it — plus the
postgres bridge (wrapped as a tool) and local files (the kernel brief's
planned "fs blobs" v0 capability) — into offline agentic workflows for
heypkv.ai's AI surfaces, matching `hub-runtime-architecture.md`'s own
target shape for agentic AI: *"UI composer/sheet -> neutral platform AI
contract -> runtime AI implementation -> retrieval sources and typed tools
-> local state and/or remote providers."* `agent`'s `MCP()` method (§3)
means other services can, in turn, treat the agent as a composable MCP
endpoint. Needs a new **`agent-host` driver** — whether it ships *inside*
the hey binary (like `internal/pkgreg`'s registry server) or as its own
fetched artifact is open (§10). Serves the same `openai-http`-shaped
contract as `ai` plus an `mcp` endpoint, so `usePlatformAi`'s `hey-local`
lane (§7) can treat `agent` as a drop-in upgrade over plain `ai`.

### `postgres` — the shipped exemplar

Already real, already shipping (`internal/svc/packs.json`,
`cmd/hey/svc.go`, proven end-to-end by task `TASK-POST-SERV-PACK-HEY-1`
against Windows `psql`): pinned EDB binary archives for
`windows_amd64`/`darwin_amd64` at `16.9.0`, `initdb` with generated
`scram-sha-256` credentials via a transient 0600 pwfile
(`internal/svc/lifecycle.go`'s `runInit`), started on `127.0.0.1:{port}`
inside `internal/svc`'s `5432`–`5600` range, health-checked with
`pg_isready`, stopped gracefully with `pg_ctl -m fast` and a `KillTree`
fallback. Folding it in requires exactly one new thing: its **Bridge**
(§3) — browsers cannot open a raw postgres-wire socket, so it needs a
scoped HTTP query API (`Kind: "http-query-api"`, `Path: "/postgres/
bridge"`) — parameterized-statement-only, no DDL or superuser path
reachable from a browser origin, namespaced per calling origin per the
kernel brief's "hard per-origin namespaces" invariant. The desktop app,
with OS-level TCP access and no CORS/PNA to satisfy, skips the bridge and
speaks postgres-wire directly via `hey up postgres`'s printed connection
string (`svc.Conn`) — decision #4's web/desktop parity split, made concrete
on the one service that needs it in v1.

### `registry` (npm) — proof the pattern already works

`hey registry` (`cmd/hey/registry.go` + `internal/pkgreg`) is cited here as
**evidence the underlying pattern is production-proven**: loopback-only
HTTP service, pidfile-based background lifecycle, a driver (`Format`)
shipping in the hey binary with all ecosystem-specific logic, a data-only
store namespaced per format. Its `Protocol()` is plain `"http"`, no
`Bridge` needed — npm clients already speak HTTP. If the hub proxies it, it
would need an explicit `CORS()` (today it has none, by design — a
CLI/npm-client tool, not browser-facing) rather than inheriting a default.

## 9. Phasing

**P1 — hub + Service contract + `hey up ai` + platform/ai backend.**
Loopback only, no named hosts, no CA. Extend `packs.json`'s schema with
`protocol`/`bridge`/`cors`/`mcp` (§3); build the hub as a new package
(mirroring `internal/pkgreg`'s pidfile pattern, §4) with `/services` and
the security envelope (§5) as shared middleware; add the `model-server`
driver and ship `ai`/Qwen+Phi (§8); reserve `up`, `config`, `services` in
`internal/registry.Reserved` before any of this lands, as the kernel brief
already flagged for `up`; land the `hey-local` `PlatformAiProviderKind` and
`probeHeyHub` client (§7).

**P2 — named hosts + local CA.** `hey.local`/`<service>.hey`, hosts-file
management (`hey hosts install`), mkcert-pattern trust-store installation
(`hey ca install`), per-host cert minting, TLS termination added to the
hub's reverse proxy (§6).

**P3 — postgres bridge + first business app wired end-to-end.** Ship
`postgres`'s `Bridge` (§8), wire `createHeyHubDatabaseProvider()` (§7) to
it for real, and prove decision #4 (web/desktop parity) in production on
Business — this is the concrete step that closes north-star's rung 2→3 gap.

**P4 — agentic/MCP host.** `hey up agent` (§8), composing crow's MCP server
+ the postgres bridge + files into offline agentic workflows behind the
same `platform/ai` contract extended in P1.

## 10. Open questions

1. **Elevation UX per OS (§6).** hosts-file writes and CA trust-store
   installs both need one-time elevated access. Candidates: a `sudo`
   re-exec (Unix), a small persistent elevated helper service vs. a
   one-shot UAC prompt (Windows), and `pkexec`/PolicyKit variability across
   Linux distros. Needs a concrete choice per OS before P2, not a generic
   "prompt for admin" placeholder.
2. **Model licensing and real size validation (§8).** Qwen2.5's license
   terms vary by size tier; Phi ships under MIT. Confirm redistribution
   terms for bundling vs. fetch-on-first-use, and validate the tiny/small
   tier boundaries against actual MSME-class hardware rather than
   developer laptops — north-star's "bring your own CPU/memory" premise is
   only honest if `tiny` genuinely runs acceptably on it.
3. **Multi-user machines.** `~/.hey` is per-user today (`internal/home`).
   Does the hub become one instance per OS user (separate ports, no
   conflict but no sharing either), or does it need a shared system-wide
   mode? The named-hosts/CA tier gets materially harder with multiple OS
   users needing the same hosts-file and trust-store entries — likely needs
   an admin-installed system-wide variant as a documented alternative, not
   an assumption that per-user is always right.
4. **Resource limits.** Nothing today caps how many `hey up` services run
   concurrently, or bounds their combined CPU/RAM/disk footprint. A laptop
   running postgres + a 7B model + the agent host simultaneously has no
   guardrail. Needs a policy (soft warnings, a configurable cap, or outright
   refusal past a threshold) before P1 ships broadly, not after.
5. **Reserved-namespace gap found during this review.** `internal/registry
   .Reserved` (app names) and `internal/svc`'s pack-name validation
   (`validatePack`) are two separate namespaces today with **no** collision
   check between them — nothing stops a pack named e.g. `ps` or an app named
   `postgres` from shadowing a hub-known name. The hub introduces a *third*
   namespace (service instance names, user-chosen via `--name`). These need
   one unified reservation/collision check before `hey up` ships, not three
   independently-validated lists that can silently disagree.
6. **Where does the agent process live (§8).** Built into the hey binary
   like `internal/pkgreg`'s registry server (fast, but grows hey's own
   binary and ties agent updates to hey releases), or fetched as its own
   pack/artifact like postgres (independent update cadence, but loses the
   "zero extra download" property `ai`/`registry` have). Affects both
   binary-size and update-velocity trade-offs and should be decided before
   the `agent-host` driver is built, not discovered mid-implementation.

## 11. Decisions (P1 build)

Resolutions to §10's open questions, made during the P1 implementation
(hub + `hey up` + vinci model-server + postgres bridge). Recorded here so
they're not re-litigated at P2/P3/P4 without cause.

1. **Elevation (§10 Q1) — setup grants only, never at service runtime.**
   Elevated access is confined to *one-time setup* the P2 named-hosts/CA tier
   needs (`hey hosts install`'s hosts-file write, `hey ca install`'s
   trust-store install) — never to running a service. Every `hey up
   <service>` process, P1 through P4, runs under the invoking user's normal
   permissions, exactly like every `internal/svc`-managed process does
   today; no service's pack `start` command may assume or request elevation.
   This wasn't a live question for P1 (loopback-only, no named hosts), but is
   binding on P2's design.
2. **Model tiers (§10 Q2) — progressive, P1 ships the mechanism.** Qwen2.5-
   3B-Instruct Q4_K_M is the default ("small") tier; a low-RAM ("tiny", ~1.5B)
   tier and better-than-default ("medium") tiers are selectable per-pack via
   `hey up vinci --model <tier>` (`Pack.Models`/`DefaultModel`,
   `internal/svc.ResolveModel`) — implemented and tested (with fake artifacts
   — see §12). What's deferred: automatic tier selection by probing available
   RAM. That's a fast-follow, not a P1 gap — `--model` is the escape hatch
   until it lands. Real per-tier pins (§10 Q2's licensing/sizing validation)
   are still open; see §12.
3. **Hub scope (§10 Q3) — per-user by default, confirmed.** The hub binds
   one instance per OS user via `~/.hey` (`HEY_HOME`), matching
   `internal/home`'s existing per-user model — unchanged, not reconsidered.
   A system-wide/shared mode remains a later, explicit opt-in if multi-user
   machines need it, not something P1 attempts.
4. **Resource limits (§10 Q4) — policy-driven, not a blind cap; still not
   built.** The direction is a *policy* (soft warnings informed by actual
   available RAM/disk/CPU before starting a heavy service like vinci's medium
   tier) rather than a fixed count-of-services ceiling — but no such policy
   ships in P1. `hey up vinci` does check free disk space before a model
   download (skips/warns rather than filling the disk) as a narrow, concrete
   instance of this principle; a general resource-aware admission policy
   across all services is P-later work.
5. **Namespace unification (§10 Q5) — resolved.** `internal/namespace`
   (`CheckCollisions`, `CheckName`) is the single collision check across
   `internal/registry.Reserved`, registry app names, and `internal/svc` pack
   names — the hub's own service names need no fourth check because a hub
   service's `Name()` is always exactly a pack name (default) or a
   user-chosen `--name` that collides-checks against the same three-way set
   (`CheckName`, called from `hey up` before provisioning). `up`, `down`,
   `hub`, `config`, `services` were added to `Reserved` ahead of this
   landing, per §9's own instruction.
6. **Agent process location (§10 Q6) — not decided in P1.** P1 does not
   build the `agent-host` driver (P4 scope); this question stays open until
   that work starts, but the direction leans toward the same "fetched
   artifact" pattern as vinci's model-server driver (§12), consistent with
   decision #7's architecture directive.
7. **Architecture directive: hey stays a thin coordinator (new, P1).**
   Every service-specific behavior — postgres's init/start/stop templates,
   vinci's server flags and model choice — lives in pack *data*
   (`packs.json`) or a *fetched artifact* (the EDB archive, the pinned
   `llama-server` binary, the GGUF weight), never compiled into the hey
   binary. This mirrors `buddy` (an installations plugin over fetched
   bundles) and `runner` (a thin app-running facade over fetched binaries):
   `internal/svc`'s two drivers (`archive-exec`, `model-server`) are
   generic, reusable machinery with zero postgres- or llama.cpp-specific
   code; only the hub itself (routing, the security envelope, the postgres
   wire-protocol bridge) is new *concrete* Go machinery in the binary, and
   that machinery is service-agnostic by construction — a third pack could
   reuse either driver with no hub changes.

## 12. P1 build report

What actually shipped, and what's still a pin-only placeholder — see the
implementation's own commit for the full detail; this is the durable record.

- **Hub.** `internal/hub`: `/healthz`, `/services`, `/services/<name>`,
  `/svc/<name>/*` (reverse proxy or bridge dispatch by pack kind), the §5
  security envelope as one middleware (`Envelope`), a pidfile lifecycle
  (`hey hub start|stop|status`) mirroring `internal/pkgreg`'s registry
  pattern. `hey up` auto-starts it. The catalog is keyed by *instance* name
  (not pack name) so a custom `hey up postgres --name pg2` gets its own
  correctly-routed `/svc/pg2/...` entry — pack name and instance name are
  two different things throughout the hub, not assumed equal.
- **`hey up` / `hey down`.** `cmd/hey/up.go`: multi-service (`hey up vinci
  postgres`), `--name`, `--model <tier>`, first-run model-download progress
  UX. `hey svc up` keeps working unchanged.
- **vinci.** `internal/svc`'s new `model-server` driver reuses the
  `archive-exec` provisioning pipeline for the `llama-server` binary and
  adds one step, `EnsureModel`, for the separately-pinned GGUF weight:
  resumable (HTTP Range, `internal/fetch.DownloadResumable`), checksummed,
  content-addressed under `~/.hey/artifacts/models/<sha256>/<file>` (dedup
  across packs/instances that reference the same weight). **Binary and
  model pins are TODO** — `packs.json`'s `vinci` pack ships the obviously-
  fake `PendingPinSHA256` placeholder for every platform artifact and the
  default model; `EnsureModel`/`Pack.Artifact` refuse to fetch a pending pin
  with a loud, specific error rather than downloading something unverified.
  Real pins were not guessed in, per this build's explicit instruction.
- **postgres bridge.** `internal/pgwire` (new, zero-dependency, hand-rolled
  postgres wire protocol v3 + SCRAM-SHA-256, including a from-scratch
  PBKDF2-HMAC-SHA256 since `golang.org/x/crypto` isn't a hey dependency) +
  `internal/hub/bridge_postgres.go`. The wire contract is the app-side
  client's (`@heypkv/hub-web`'s `heyDatabase.ts`), not services.md's
  original sketch: `{rows: [{col: val}]}`/`{rowsAffected, lastInsertId?}`
  response shapes, `X-Hey-Session`-pinned connections so multi-request
  transactions really commit/rollback, per-`{namespace,name}` postgres
  schema isolation, and `{"$bytes": base64}` for bytea/Uint8Array. Verified
  against a real postgres instance (SCRAM auth, transactions, bytea
  round-trip, namespace isolation) — see the commit's test suite and report.
  **Known gap:** the bridge's statement blocklist is a prefix check, not a
  real SQL parser, and the pack's generated credentials are postgres's
  bootstrap *superuser* (initdb's `-U` always is) — a properly scoped,
  non-superuser bridge role is flagged, not built, in P1.
