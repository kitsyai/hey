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
hey version                        print hey's version
```

Flags (before the app name): `--registry <path|https-url>`, `--no-browser`,
`--timeout <dur>`. Environment: `HEY_HOME`, `HEY_REGISTRY`, `GH_TOKEN`.

## Development

```
go test ./...                                  # unit tests (no network)
HEY_INTEGRATION=1 go test ./cmd/hey -run Integration -v   # real fetch + UI lifecycle
go build -o hey ./cmd/hey
```

`internal/testapp` is the reference implementation of the app contract used
by the integration tests.
