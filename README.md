# iowap-universal-node

The **universal-node flavour** of the IOWAP stack: everything needed to run
a self-contained universal node container — the container-MCP surface
(`mcp/`) and the compose profile (`compose/`). Split out of
iowap-org/iowap-docker by the T-037 package split (MCP-package-split DEC,
2026-11-25); merging the bridge bootstrap into this repo follows as its own
task.

## Layout

| Path    | What lives here |
|---------|-----------------|
| `mcp/` | The PyPI package **iowap-node-mcp** — Streamable HTTP `/mcp` + Bearer auth over the iowap-client-mcp toolset (see [mcp/README.md](mcp/README.md)). Released via `wheel-v*` tags. |
| `compose/` | `docker-compose.universal-node.yml` — the universal-node compose profile (node image + loopback-published 7781/7783 + workspace/state volumes), moved from iowap-org/iowap-docker. |

Releases: `wheel-v<version>` tags publish only the `mcp/` package wheel;
the node image itself continues to be built/published from
iowap-org/iowap-docker (base image `ghcr.io/iowap-org/iowap-node-base`).
