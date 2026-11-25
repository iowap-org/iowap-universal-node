# iowap-universal-node

The **universal-node flavour** of the IOWAP stack: the container-MCP surface
for the universal node (`mcp/`). Split out of iowap-org/iowap-docker by the
T-037 package split (MCP-package-split DEC, 2026-11-25); merging the bridge
bootstrap into this repo follows as its own task. The universal-node
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
| `mcp/` | The PyPI package **iowap-node-mcp** — Streamable HTTP `/mcp` + Bearer auth over the iowap-client-mcp toolset (see [mcp/README.md](mcp/README.md)). Released via `wheel-v*` tags. |

The universal-node compose profile (which runs the
ghcr.io/iowap-org/iowap-universal-node image built from `mcp/`) lives in
[iowap-org/iowap-docker](https://github.com/iowap-org/iowap-docker) under
`universal-node/` (storage service-dir pattern, T-037). CI here runs the
`mcp/` pytest suite (pr-tests); there is no wheel-publish workflow — the
image build is the release.
