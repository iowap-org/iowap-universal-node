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
- **Auth:** every request must carry `Authorization: Bearer` + either the
  primary token from `IOWAP_MCP_BEARER` (legacy full access), **or** a
  per-agent token minted by the node dashboard (T-035: verified against the
  SHA-256 hashes only agents store at `/data/agents/agents.json`; revoked
  tokens count as unknown). Anything else gets `401` +
  `WWW-Authenticate: Bearer`. The process **fails closed**: without the
  primary token (or with a blank value) it exits with an error instead of
  serving an unauthenticated endpoint. A missing/empty/corrupt agents
  store degrades to primary-only (legacy containers and fresh boots keep
  working — `env IOWAP_AGENTS_DIR` overrides the path for tests).
- **Only transport is HTTP.** `--http` is accepted (and a bare start
  serves HTTP too) to keep the container entrypoint command stable.

## Toolset — imported as a library

No duplicated tool code: `iowap-client-mcp` (>= 0.2.0) is a runtime
dependency and this server wires its toolset into the Streamable-HTTP
transport — the same 9 curated tools as the stdio path (`relay_status`,
`node_list`, `capabilities_list`, `task_list`, `task_submit`,
`task_result`, `artifact_download`, `artifact_link`, `docs`), with
`IOWAP_ALLOW_CAPS` and the artifact-overwrite refusal applied unchanged.

**T-042 (0.1.1):** the node-HTTP surface additionally serves the 5
capability-manage tools, defined in `iowap_node_mcp/tools.py` and
attached by `build_http_app` — `capability_create`, `capability_update`,
`capability_delete` (destructive; requires `confirm=true`),
`capability_publish`, `capability_script_upload` — all driven through the
node CLI (`iowap-cli capabilities …`, the dashboard store semantics),
with the T-274 error shape (`{'error': {http_status, detail, raw}}`) on
CLI/runner failures. Operators gate the whole set per tool name via env
`IOWAP_ALLOW_CAPS_MANAGE` (csv; unset = allowed — primary/legacy is
operator context; T-035 adds the per-agent `caps_manage` flag on top). The
stdio local-agent surface of `iowap-client-mcp` stays the
curated 9-tool set: this surface is a strict superset (14 via
`tools/list`).

**T-035 (0.1.2):** multi-bearer + token-bound scoping. The four
task/artifact tools ride token-bound versions on this surface — same
names, same schemas (`tools/list` stays 14): `task_submit` binds the
submitted task to the agent, `task_result` answers a foreign task with
the exact legacy `not_found` shape and records the artifacts a result
exposes, `artifact_download`/`artifact_link` admit only artifacts the
agent has seen (fail-closed T-274 `not found` error shape otherwise).
Agent tokens get `capability_*` only with `caps_manage: true` in the
agents store ("restricted for this token" error shape); the primary
bearer keeps full access with no binding at all (legacy semantics
unchanged — the stdio surface of iowap-client-mcp behaves exactly as
before).

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
