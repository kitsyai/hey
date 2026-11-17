# hey pack manifest v0

A **pack** describes one local service (postgres, a cache, object storage) as
*data* — never code. Every pack is executed by a **driver** that already ships
inside the hey binary. v1 ships exactly one driver, `archive-exec`; adding a
new service is a data-only change (a new pack), not a code change.

Packs travel the same trust pipeline as apps: HTTPS-only downloads, a pinned
SHA-256 for every per-platform artifact, zip-slip-safe extraction. The default
pack set is embedded in the binary (`internal/svc/packs.json`); an override
file at `~/.hey/packs.json` (or `HEY_PACKS`) replaces it wholesale, identical
format, so a hosted pack registry needs no code change.

## Resolution precedence

1. `HEY_PACKS` environment variable (path or `https://` URL)
2. `~/.hey/packs.json`
3. embedded default

Pack sets do not merge; the first found wins entirely. URL overrides must be
`https://`.

## Schema

```jsonc
{
  "hey_packs": 0,                          // format version
  "packs": {
    "postgres": {
      "pack": "postgres",
      "driver": "archive-exec",            // the generic service driver
      "kind": "service",                   // "service" (default) | "runtime"
      "versions": {
        "16.4.0": {
          "artifacts": {                   // keyed by "{os}_{arch}" (GOOS_GOARCH)
            "windows_amd64": {
              "url": "https://.../postgresql-16.4-windows-x64-binaries.zip",
              "sha256": "0123…",
              "bin_subdir": "pgsql/bin"    // dir INSIDE the archive holding the exes
            },
            "linux_amd64": {
              "url": "https://.../postgresql-16.4-linux-x64-binaries.tar.gz",
              "sha256": "89ab…",
              "bin_subdir": "pgsql/bin"
            }
          }
        }
      },
      "init":  [ "{bin}/initdb -D {data} -U {user} --pwfile {pwfile} -E UTF8" ],
      "start": "{bin}/postgres -D {data} -p {port}",
      "ready": { "tcp": "127.0.0.1:{port}", "timeout_seconds": 60 },
      "conn":  "postgresql://{user}:{password}@127.0.0.1:{port}/postgres",
      "stop":  { "command": "{bin}/pg_ctl stop -D {data} -m fast", "signal": "term", "grace_seconds": 20 }
    }
  }
}
```

### Fields

- `hey_packs` — format version. hey rejects a pack set newer than it
  understands ("update hey"); unknown fields are ignored.
- `pack` — the pack's canonical name; must equal its key and contain no
  `/ \ @` or spaces.
- `driver` — the service driver in the hey binary. v0 accepts only
  `archive-exec`; any other value is rejected with a clear message (the seam
  for future drivers).
- `kind` — `service` (a long-running managed process, the default) or
  `runtime` (a fetch-verify-exec artifact with no lifecycle; see
  feature-set-v1 §2). v0's driver implements `service`.
- `stateless` — optional, default `false`. `true` declares that instances
  keep no data worth preserving (e.g. `dsc`, a signer in front of a USB
  token). Then `hey up` moves an existing instance to a newer pinned version
  by replacing its binaries in place (same name and port); without it, an
  existing instance keeps its version until it is removed. Never set it on a
  pack with data (postgres).
- `versions` — map of semantic version → the artifacts for that version.
- `artifacts` — map of `"{os}_{arch}"` (Go's `GOOS_GOARCH`, e.g.
  `windows_amd64`, `linux_amd64`, `darwin_arm64`) → one artifact:
  - `url` — HTTPS archive (`.zip` or `.tar.gz`). Redirects are followed; the
    final hop is streamed to a temp file with a size cap.
  - `sha256` — mandatory lower-hex digest of the downloaded archive.
    Verification is not skippable.
  - `bin_subdir` — path *inside* the extracted archive to the directory that
    contains the executables. `{bin}` resolves to
    `<instance>/bin/<bin_subdir>`. Omit (or `""`) when the executables sit at
    the archive root.

### Lifecycle specs

All of the following are **command templates** (see *Templating* below).

- `init` — an array of commands run **exactly once**, the first time an
  instance is provisioned (e.g. `initdb`). A `.hey-initialized` marker in the
  data dir guards re-runs; a failed init leaves no marker so it retries.
- `start` — a single command, launched as a **detached, managed process**
  (reusing hey's `internal/proc` spawn). Its stdout+stderr stream to
  `logs/service.log`. The PID is recorded in `svc.json`.
- `ready` — the health check, polled until it passes or `timeout_seconds`
  (default 30) elapses. Exactly one of:
  - `tcp` — a `host:port` that must accept a TCP connection.
  - `command` — a command that must exit 0.
- `conn` — a connection-string template printed by `hey svc conn`. Data only;
  the driver never parses it.
- `stop` — how to shut the service down, tried in order:
  1. `command` — a graceful stop command (e.g. `pg_ctl … -m fast`), if set;
  2. else `signal` — `term` (default), `int`, or `kill`, delivered to the
     process (best-effort on Windows, where a detached console process cannot
     receive Unix-style signals);
  3. after `grace_seconds` (default 15) the driver force-kills the whole
     process tree (`proc.KillTree`) as a guaranteed fallback.

## Templating

Commands are tokenized with shell-style double-quote handling (`-k ""` yields
an empty argument), then each token has these variables substituted. Because
substitution happens **after** tokenization, values containing spaces (Windows
paths) are safe.

| Variable     | Value                                                       |
|--------------|-------------------------------------------------------------|
| `{bin}`      | `<instance>/bin/<bin_subdir>` — the extracted executables   |
| `{data}`     | `<instance>/data` — the durable data directory              |
| `{port}`     | the instance's allocated port                               |
| `{user}`     | the generated service username                              |
| `{password}` | the generated service password                              |
| `{pwfile}`   | a transient 0600 file holding only the password (init only) |

The first token of every command is the executable. On Windows, if that path
does not exist but `path + ".exe"` does, the `.exe` form is used — so a single
manifest (`{bin}/initdb`) works on every platform.

## Instance layout & invariants

```
~/.hey/svc/<instance>/
  bin/            extracted, verified binaries (per pack version)
  data/           the user's data — NEVER touched by upgrades or cache clean
  logs/service.log
  svc.json        pack, version, port, generated credentials, state (mode 0600)
```

- **127.0.0.1 only.** Packs must bind loopback; this is enforced by pack
  review, not user config.
- **Generated credentials.** `{user}`/`{password}` are generated at init,
  never defaulted. `svc.json` (which stores them) is mode `0600`.
- **Stable ports.** A port is allocated from a local range at first `up` and
  recorded in `svc.json`; it stays fixed across restarts.
- **Data durability.** `data/` survives stop/start and pack-version upgrades;
  `hey svc rm` refuses to delete it without `--purge-data` and a confirmation.

## Reserved for later

`container-less composite` drivers (packs depending on other packs) and a
`minisign_pubkey` per pack (artifact signatures) are named here but unused in
v0. Checksums protect download integrity, not publisher authenticity —
signatures are the roadmap item shared with the apps trust chain.
