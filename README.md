# hey

`hey` fetches, verifies, and runs single-binary apps from the heypkv/kitsy
ecosystem — think `npx`/`uvx`, but for Go binaries with embedded web UIs. No
Node, no Java, no installer.

```
hey guten render -t 'Hi {{ name }}' -d '{"name":"Ada"}'   # fetch on demand, then run
hey djin ui                                               # fetch, verify, start UI, open browser
hey ps                                                    # what's running
hey stop djin
```

## Install

One line — hey itself is the only thing you ever install by hand:

```powershell
# Windows
irm https://kitsy.ai/hey.ps1 | iex
```

```sh
# Linux / macOS
curl -fsSL https://kitsy.ai/hey.sh | sh
```

Both scripts resolve the latest release, verify the SHA-256 checksum, and
put `hey` on your PATH (Windows: `%LOCALAPPDATA%\Programs\hey`; Unix:
`/usr/local/bin` if writable, else `~/.local/bin`). Until the kitsy.ai
endpoints go live, substitute
`https://raw.githubusercontent.com/kitsyai/hey/main/install/hey.ps1` (or
`.sh`). Or install manually: grab the archive for your platform from
[Releases](https://github.com/kitsyai/hey/releases), verify it against
`checksums.txt`, and drop the binary on your PATH. Pin a version with
`HEY_VERSION=0.1.0`; relocate with `HEY_INSTALL_DIR`.

With a Go toolchain you can also install from source:

```sh
go install github.com/kitsyai/hey/cmd/hey@latest
```

## How it works

- **Resolve** — app names come from a registry (embedded by default, see
  [docs/registry-v0.md](docs/registry-v0.md)). Versions resolve against
  GitHub Releases with monorepo tag-prefix support; resolutions are cached
  for 24h. `hey app@1.2.3` pins a version and never touches the API.
- **Verify** — downloads are HTTPS-only and checked against the release's
  goreleaser SHA-256 `checksums.txt`. A mismatch aborts the install; there is
  no override. (Signature verification is the v1 roadmap item.)
- **Run** — ordinary commands run in the foreground with your terminal, and
  exit codes pass through. UI commands (per
  [docs/app-contract-v0.md](docs/app-contract-v0.md)) start detached: hey
  waits for the app's port handshake, health-checks it, records it, and opens
  your browser.

Binaries and state live under `~/.hey` (`HEY_HOME` to relocate):

```
~/.hey/bin/<app>/<version>/   verified binaries
~/.hey/logs/<app>.log         UI app output
~/.hey/state/procs.json       running UI apps
~/.hey/registry.json          optional registry override
```

## Commands

```
hey <app>[@version] [args...]      run an app (fetched on demand)
hey run <app>[@version] [args...]  explicit run form
hey install <app>[@version]        fetch + verify without running
hey update [<app>]                 re-resolve latest and fetch if newer
hey ls                             list installed apps
hey ps                             list running UI apps
hey stop <app>                     stop a running UI app
hey which <app>                    print path of the installed binary
hey cache clean [<app>]            remove cached binaries
hey up <service> [<service2> ...]  bring up a local service (postgres, vinci, ...)
hey down <service>                 stop it
hey hub <start|stop|status>        the local services discovery hub
hey version                        print hey's version
```

Flags (before the app name): `--registry <path|https-url>`, `--no-browser`,
`--timeout <dur>`. Environment: `HEY_HOME`, `HEY_REGISTRY`, `GH_TOKEN`.

## Local package registry

`hey registry` runs a local, loopback-only npm registry so a teammate can
publish a work-in-progress `@heypkv`/`@kitsy` package and have another app
install it exactly like it came from the real registry — real tarball, real
`package.json`, no symlinks. It overlays local publishes on top of the real
upstream (resolved from `~/.npmrc`): anything not published locally proxies
straight through, auth included.

```
hey registry start  [--port 4874] [--store <dir>]   start in the background
hey registry serve  [--port 4874] [--store <dir>]   run in the foreground
hey registry stop                                   stop the background server
hey registry status                                 running?, port, store, published count
hey registry ls                                     list locally-published package@version
hey registry clear  [<pkg>[@<version>]]              drop local versions
```

Team flow:

```
hey self update; hey registry start
# in the family repo:
pnpm publish --registry http://127.0.0.1:4874           # 0.x.y-dev.N, never a real release
# in the consuming app repo, for the session only:
#   point the @scope registry at 127.0.0.1:4874 (project .npmrc), then:
pnpm add @heypkv/whatever
# ...test, iterate...
hey registry stop                                        # back to pure GAR/npmjs
```

This is the "real artifact" loop, distinct from an app repo's `pnpm
dev:link` (a symlinked fast loop for iterating inside one repo) — reach for
`dev:link` when you're editing in place, and for `hey registry` when you
need to prove the actual published tarball installs and works.

Store layout is namespaced per package format (npm today; more may follow):
`~/.hey/registry/npm/<pkg>/{metadata.json,tarballs/<file>.tgz}` — every
locally-published package is a plain directory, inspectable without any
tooling. Upstream resolution reads only `@scope:registry=` and
`//host/path/:tokenHelper=` lines out of `~/.npmrc` — token/authToken lines
are never read or logged.

## hey up — local services

`hey up` is the two-command offline story: install `hey`, then `hey up
<service>` — a local postgres, an edge-AI model server ("vinci") — and every
heypkv.ai/kitsy.ai surface (or your own app) talks to it instead of a cloud
server. No Docker, no manual port bookkeeping. Full design:
[docs/design/services.md](docs/design/services.md).

```
hey up postgres              # provision + start postgres, register with the hub
hey up vinci                 # provision + start a local model server (default tier)
hey up vinci --model tiny    # a smaller model tier for low-RAM machines
hey up vinci postgres        # bring up more than one at once
hey down postgres            # stop it (alias: hey svc stop postgres)
hey hub status                # is the hub up, what's registered
```

The first `hey up` call also starts **the discovery hub** in the background
(`hey hub start`/`stop`/`status` manage it directly), a loopback-only server
on `127.0.0.1:8525` (`HEY_HUB_PORT` to override) that is the single front
door for every service:

```
GET  /healthz                        hub liveness
GET  /services                       what's up: name, protocol, port, health, urls
GET  /services/<name>                one service's record (404 if never provisioned)
ANY  /svc/<name>/...                 reverse-proxied to the service (vinci's OpenAI-
                                      compatible /v1/chat/completions etc.)
POST /svc/postgres/query             parameterized SELECT/read -> {"rows": [...]}
POST /svc/postgres/exec              parameterized write/DDL/txn -> {"rowsAffected", ...}
```

A browser tab can reach any of this directly — `/services` is how
`@heypkv/hub-web`'s `createHeyHubDatabaseProvider()` and `platform-web`'s
`hey-local` AI lane discover a running hub — behind one security envelope
applied uniformly (docs/design/services.md §5): an origin allow-list
(`heypkv.ai`/`kitsy.ai` + subdomains by default, additive per-service via
`hey up <service> --cors <origin>`), a required `X-Hey-Client` header that
forces a CORS preflight on every real request, Private-Network-Access
preflight support, Host-header validation, and loopback-only binding. The
desktop app, with OS-level access and no CORS to satisfy, can skip straight
to the raw instance port `/services` reports.

**postgres** is the proven exemplar: pinned EDB binaries, `scram-sha-256`
credentials generated at first `up` (never defaulted), a stable port in
`5432`-`5600`. Since a browser can't open a raw postgres-wire socket, the hub
speaks it for you — `/svc/postgres/query`/`/exec` is a real postgres-wire
client (`internal/pgwire`, hand-rolled to keep `hey` at zero third-party Go
dependencies) with per-`{namespace,name}` schema isolation and one pinned
connection per `X-Hey-Session` so a client's `BEGIN`/`COMMIT`/`ROLLBACK`
compose into a real transaction. It is a *bridge*, not a general SQL
gateway: role/permission/superuser and filesystem-affecting statements are
refused regardless of endpoint.

**vinci** is heypkv's local model-serving brand (Qwen/Phi are the models
underneath, not the product name) — a pinned `llama-server` binary plus a
resumable, checksummed, content-addressed model download
(`~/.hey/artifacts/models/`, deduplicated by digest), exposed
OpenAI-compatible at `/svc/vinci/v1/...`. **The exact upstream binary/model
pins are not yet set** — `internal/svc/packs.json`'s `vinci` pack ships with
placeholder URLs/hashes and `hey up vinci` refuses to run them with a loud,
specific error rather than silently downloading something unverified; see
the pack's `TODO-PIN-REQUIRED` markers for exactly what needs pinning.

## Development

```
go test ./...                                  # unit tests (no network)
HEY_INTEGRATION=1 go test ./cmd/hey -run Integration -v   # real fetch + UI lifecycle
go build -o hey ./cmd/hey
```

`internal/testapp` is the reference implementation of the app contract used
by the integration tests.

## License

Apache License 2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE). Developed by
HEYPKV INNOVATIONS PRIVATE LIMITED. On contributions, see
[CONTRIBUTING.md](CONTRIBUTING.md) (the project is not accepting external pull
requests at this time).

hey is the open client for the kitsy.ai ecosystem. It's a deliberately thin
runtime: it resolves a manifest, verifies bytes, and runs a native binary. It
carries no product-specific knowledge — that lives in the tools it runs and in
the modules (keeper, buddy, runner) plugged into it.
