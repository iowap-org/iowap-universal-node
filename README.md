# iowap-universal-node

The **universal-node flavour** of the IOWAP stack: the container-MCP surface
for the universal node (`mcp/`). Split out of iowap-org/iowap-docker by the
T-037 package split (MCP-package-split DEC, 2026-11-25); the bridge
daemon followed as its own task — subtree-merged into this repo with full
history (T-039, 2026-11-25, see `bridge/` below). The universal-node
compose profile lives in iowap-org/iowap-docker (storage pattern —
`universal-node/docker-compose.yml`).

**Distribution:** iowap-node-mcp is **not published on PyPI** — the code is
baked into the universal-node image via the `universal-node/mcp/` gitlink in
iowap-org/iowap-docker (storage pattern; the authoritative source stays this
repo). The version lives in `mcp/pyproject.toml`; only the release step was
dropped (owner call 2026-11-25). Local-agent packages keep the PyPI path
(`iowap-client-mcp` etc.).

## Layout

| Path    | What lives here |
|---------|-----------------|
| `mcp/` | The PyPI package **iowap-node-mcp** — Streamable HTTP `/mcp` + Bearer auth over the iowap-client-mcp toolset (see [mcp/README.md](mcp/README.md)). Not published: baked into the node image via the iowap-docker gitlink. |
| `bridge/` | The Go **bridge daemon** — git subtree of iowap-org/iowap-bridge-daemon (full history preserved, T-039). exe releases via `bridge-v*` tags. |

The universal-node compose profile (which runs the
ghcr.io/iowap-org/iowap-universal-node image built from `mcp/`) lives in
[iowap-org/iowap-docker](https://github.com/iowap-org/iowap-docker) under
`universal-node/` (storage service-dir pattern, T-037). CI here runs the
`mcp/` pytest suite (pr-tests) and the `bridge/` Go gate (vet/gofmt/test,
bridge-tests); there is no wheel-publish workflow — the image build is the
release. The bridge daemon ships its exe releases from this repo via
`bridge-v*` tags.

## bridge/ — the bridge daemon

`bridge/` is the
[iowap-org/iowap-bridge-daemon](https://github.com/iowap-org/iowap-bridge-daemon)
codebase, merged here as a **git subtree with full history** (T-039,
2026-11-25): `git log --follow bridge/go.mod` walks the original daemon
history across the subtree merge (merge parents). The old repo is archived —
this repo is now the single home for bridge code and its release pipeline.
Daemon contracts stay untouched: the image reference the exe pulls
(`ghcr.io/iowap-org/iowap-node-base:latest`) lives until the T-040 image flip
switches it to the universal-node image, and the Nextcloud delivery paths are
unchanged.

**Release flow:** a `bridge-vX.Y.Z` tag (only that prefix — the old daemon
`v*` tags are retired here to stay collision-free against the `wheel-v*`
tags of the mcp/ era) triggers `release.yml`:

1. cross-build `iowap-bridge-{windows,linux}-amd64(.exe)` (windowsgui
   subsystem for windows) with sha256sums,
2. attach the assets to the GitHub release,
3. mirror them to Nextcloud: stable `Shared/iowap-bridge/` paths (latest,
   overwrite) plus a per-version archive folder
   `Shared/iowap-bridge/bridge-vX.Y.Z/` with its own `VERSION.txt` that is
   never overwritten.

Needs the repo secrets `NEXTCLOUD_HOST` / `NEXTCLOUD_USERNAME` /
`NEXTCLOUD_PASSWORD`. A manual `workflow_dispatch` run serves as a build-only
smoke test: it builds both binaries but creates no release and does not
touch Nextcloud (both publish steps gate on tag refs).
