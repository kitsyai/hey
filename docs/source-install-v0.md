# Source install — `hey.source.v1` (v0)

A tool can be installed straight from its own repo, without publishing a release
bundle, by checking a small manifest (`hey.json`) into the repo root. buddy reads
it and installs a native executable — hey never learns the tool's internals.

```
hey keeper auth --name gh-acme --token-file tok.txt   # once, for a private repo
hey buddy install acme/tool --cred gh-acme
tool <args>                # runs directly (PATH shim)
hey runner run tool <args> # equivalent
```

(`--cred` is only needed for a private repo; a public repo installs without it.)

## How it works

1. buddy fetches `hey.json` from the repo via the GitHub **contents API** with the
   keeper token (`Accept: application/vnd.github.raw`). This reads a single file
   from a private repo without cloning the whole repository.
2. It picks the `prebuilt` entry for the running platform (`<os>/<arch>`), fetches
   that checked-in binary the same way, and verifies its `sha256` if given.
3. It installs the binary to `~/.hey/apps/<id>/<version>/<id>[.exe]`, records a
   `source` bundle in `meta.json`, and writes a PATH shim next to the `hey`
   binary so `<id> …` works. The shim delegates to `hey runner run <id>`, which
   resolves the current installed version — so it never goes stale on update.

## Manifest

```json
{
  "hey_manifest": "hey.source.v1",
  "id": "tool",
  "version": "1.2.0",
  "prebuilt": {
    "windows/amd64": { "path": "releases/1.2.0/tool-windows-amd64.exe", "sha256": "…" },
    "darwin/arm64":  { "path": "releases/1.2.0/tool-darwin-arm64", "sha256": "…" },
    "linux/amd64":   { "path": "releases/1.2.0/tool-linux-amd64", "sha256": "…" }
  },
  "build": {
    "toolchain": "go",
    "min_version": "1.26",
    "command": "go build -o {out} ./cmd/tool",
    "out": "bin/tool{exe}"
  },
  "launch": { "args": [] }
}
```

- **`prebuilt`** — the working path today: a native executable checked into the
  repo, per platform, keyed by `<os>/<arch>`. `sha256` is optional but
  recommended. Keep the binaries in a tracked directory — mind repo `.gitignore`
  rules such as a bare `bin/` that match at any depth.
- **`build`** — declares how to build from source when a toolchain is present.
  **Declared in v1; buddy's build execution ships in a later version.** hey never
  builds on its own: only when the manifest says how *and* the toolchain is
  present *and* the user opts in. (A tool whose Go workspace, `go.work`, references
  sibling repos won't build from a bare clone — for those, ship a prebuilt.)

`{exe}` expands to `.exe` on Windows and empty elsewhere.

## Update

```
hey buddy update tool  # buddy owns install + update for source bundles
hey buddy update       # update every buddy-installed tool
hey update tool        # equivalent (the general updater routes here too)
```

`hey buddy update <id>` re-fetches the repo's `hey.json` and reinstalls when the
platform binary has changed. A change is detected **either** way:

- **bump `version`** in `hey.json` (and, by convention, drop the new binaries
  under `releases/<new-version>/`) — the standard path; or
- **rebuild + push the binary at the same version** (its `sha256` changes) — a
  dev hotfix; hey compares the on-disk binary's hash to the manifest's and
  reinstalls if they differ.

If neither the version nor the binary changed, update is a no-op
(`tool is already up to date`). The PATH shim always points through
`hey runner run <id>`, which resolves the current installed version, so no shim
rewrite is needed after an update.

## Versions

`hey.json` names exactly one version — the **top**. install and update both
converge to it: `hey buddy install <owner/repo>` fetches whatever `version` +
`prebuilt` the manifest currently declares, and `hey buddy update` re-reads the
manifest and moves to its top. There is no floating "latest" for hey to guess —
the repo's `hey.json` *is* the pointer, and moving it (1.2.0 → 1.3.0 → …) is how
a producer publishes.

What happens to older versions:

- **In the repo:** keep each build under its own `releases/<version>/` for
  provenance and rollback. The manifest only points at one of them at a time.
- **On the machine:** each installed version lives in its own
  `~/.hey/apps/<id>/<version>/`; `meta.json` records which one is `current`.
  Updating to 1.3.0 leaves 1.2.0's directory in place (cheap rollback) and just
  repoints `current`. `hey remove <id>` clears them.

**Rollback** is just moving the pointer back: set `hey.json`'s `version` (and
shas) to the older build and push — `hey buddy update` on each machine steps
back to it. (Installing an arbitrary pinned version, e.g.
`hey buddy install acme/tool@1.2.0`, would need the manifest to list multiple
versions or a per-version manifest — not in v0.)
