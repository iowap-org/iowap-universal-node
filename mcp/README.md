# iowap-node-mcp

The **container-HTTP MCP surface** for IOWAP — one half of the T-037
package split (stdio-Local vs. Container-HTTP, DEC 2026-11-25). This
package lives in [iowap-org/iowap-universal-node](https://github.com/iowap-org/iowap-universal-node)
under `mcp/` (repo scope: universal-node flavour — container-MCP + compose).

`iowap-client-mcp` stays the local-agent path (MCP stdio, the agent spawns
`iowap-mcp` on the same host as the node); this package is the container
surface: the same curated IOWAP toolset served over the MCP Streamable
HTTP transport at `/mcp` behind Bearer auth, designed to run inside the
iowap node container.

## Container usage

The `iowap-node-base` image (iowap-docker) installs this package and its
entrypoint starts the server bearer-gated on port 7781:

```bash
# inside the container, after IOWAP_MCP_BEARER is set by the operator/
# dashboard (fail closed — without a token the process refuses to start):
iowap-node-mcp --http --host 0.0.0.0 --port 7781
```

- **Endpoint:** `http://<container>:7781/mcp` (the universal-node compose
  profile publishes it as `127.0.0.1:7781` on the host — loopback-only).
- **Auth:** every request must carry `Authorization: Bearer <token>`,
  token from `IOWAP_MCP_BEARER`; anything else gets `401` +
  `WWW-Authenticate: Bearer`. The process **fails closed**: without the
  token (or with a blank value) it exits with an error instead of serving
  an unauthenticated endpoint.
- **Only transport is HTTP.** `--http` is accepted (and a bare start
  serves HTTP too) to keep the container entrypoint command stable.

## Toolset — imported as a library

No duplicated tool code: `iowap-client-mcp` (>= 0.2.0) is a runtime
dependency and this server wires its toolset into the Streamable-HTTP
transport — the same 9 tools either way (`relay_status`, `node_list`,
`capabilities_list`, `task_list`, `task_submit`, `task_result`,
`artifact_download`, `artifact_link`, `docs`), with `IOWAP_ALLOW_CAPS`
and the artifact-overwrite refusal applied unchanged. For tools, agents
and MCP clients the surface is identical to before the split; only the
package and release cadence differ.

Local agents (same host as the node) should keep using the stdio package:
[iowap-client-mcp](https://github.com/iowap-org/iowap-client-mcp).

## MCP probe (two-step handshake)

The Streamable-HTTP transport requires the session handshake — a
single-shot curl always looks broken:

```bash
# 1) initialize — the response carries the Mcp-Session-Id header
curl -sS -D- -H "Authorization: Bearer $TOKEN" \
     -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
     -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe","version":"0"}}}' \
     http://127.0.0.1:7781/mcp
# 2) every follow-up call posts WITH that session id:
#    -H "Mcp-Session-Id: <session>"  → tools/list etc.
```

## Development

```bash
python -m venv .venv && .venv/bin/pip install -e ".[dev]"
.venv/bin/pytest
```

## License

MIT — see [LICENSE](LICENSE).
