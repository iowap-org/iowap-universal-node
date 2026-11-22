# iowap-bridge-daemon

IOWAP Bridge Daemon — native host-side capability provider (Manifest/Exec
contract) for the universal node container; first target: Windows
(Docker Desktop), Go.

The daemon exposes host applications to an IOWAP node running in a
container on the same host. Capabilities are **config-driven** (board
T-019): a `providers.yaml` registry instantiates providers of the types
`comfyui` (the built-in Stable Diffusion integration), `http` (any REST
endpoint) and `process` (host commands) — without a daemon rebuild. The
node polls the manifest (pull model) and forwards claimed tasks via
HTTP; both sides share a workspace directory for artifact hand-off.

**Status: private until first release.**

## Startup flow

**No env setup needed — the daemon provisions its tokens.** A fresh
machine works with a plain exe double-click: the daemon generates its
bridge token and MCP bearer itself (v0.1.1, DEC #5/#6). The fail-closed
rule still holds — the server never serves without a valid Bearer — but
the daemon no longer *demands* the token from the user; it resolves it:

1. **Token resolution** (`internal/tokenres`) — in order:
   * explicit env (`IOWAP_BRIDGE_TOKEN` / `IOWAP_MCP_BEARER`, power-user
     override), else
   * **existing container**: tokens are reused from the
     `iowap-universal-node` container env (docker inspect) — the container
     IS the secret store, so a daemon restart beside a running node
     (autostart, PC reboot) keeps everything working without persisting a
     secret, else
   * **generated** (crypto/rand, `iw_br_…` / `iw_mcp_…`). Only if
     generation fails does the daemon exit 1.
2. **Bootstrap** — the daemon checks the Docker Engine (`client.FromEnv`:
   Windows named pipe / unix socket). Unreachable → actionable error
   ("install or start Docker Desktop") and exit 1. Container handling:
   * not existing → created via the **vendored iowap-docker compose
     profile** (`internal/bootstrap/docker-compose.universal-node.yml`,
     provenance-marked — the profile file is the single source of truth,
     board DEC #7) with `docker compose -f <file> up -d`, injecting
     `IOWAP_BRIDGE_TOKEN`, `IOWAP_MCP_BEARER` and `IOWAP_WORKSPACE_DIR`;
   * existing but stopped → started as-is (its env — and with it the
     already-configured MCP bearer — stays untouched);
   * already running → skipped.

   Every step is announced on the console ("checking Docker Engine…",
   "checking container state…", "pulling image and starting container…",
   "container ready"). A successful bootstrap ends by opening the node's
   registration page `http://127.0.0.1:7783` in the default browser — also
   in the "container was already running" case (the page then shows the
   node status). Best-effort: if no browser can be launched, the URL is
   printed as the last console line instead; the daemon never crashes over
   a failed browser launch.
3. **Provider registry** (`internal/registry`, board T-019) — capabilities
   come from `providers.yaml` in the config dir (env `IOWAP_PROVIDERS_FILE`,
   default `<configdir>/iowap-bridge/providers.yaml`). A missing or empty
   file is fine: the daemon serves an **empty manifest** rather than
   crashing. A broken file (bad YAML, unknown type, missing required
   fields) makes the daemon refuse to start — loud, not silent. See
   [Providers](#providers-providersyaml) below and
   `providers.example.yaml`.
4. **Bridge API** on `127.0.0.1:7780` (below).

> **Onboarding moved into the node container (v0.2.0, board DEC #8).** The
> daemon no longer ships an onboarding web UI and no longer displays the MCP
> bearer. Node registration and the one-time bearer/snippet display live in
> the node-side onboarding UI at `http://127.0.0.1:7783` (served inside the
> `iowap-universal-node` container).

| Port  | Purpose                                        | Binding     |
|-------|------------------------------------------------|-------------|
| 7780  | Bridge manifest/exec API (node → daemon)       | 127.0.0.1   |
| 7781  | HTTP MCP endpoint (agents → node container)    | 127.0.0.1   |
| 7783  | Node onboarding UI + bearer/snippet display (node container, board T-017) | 127.0.0.1   |

## Build & Run

Requires Go 1.23+ (or Docker: `docker run --rm -v "$PWD":/app -w /app golang:1.23 go build ./cmd/iowap-bridge`).

```sh
go run ./cmd/iowap-bridge
```

### Environment

| Variable               | Required | Default                   | Description                                                        |
|------------------------|----------|---------------------------|--------------------------------------------------------------------|
| `IOWAP_BRIDGE_TOKEN`   | no       | auto-provisioned          | Power-user override. By default the daemon resolves the token itself (env → existing container env → generated, `iw_br_…`). The bridge server stays fail-closed: every endpoint requires this Bearer. The node container receives the same token as `IOWAP_BRIDGE_TOKEN` (injected at container creation). |
| `IOWAP_MCP_BEARER`     | no       | auto-provisioned          | Power-user override for the MCP bearer agents must present on port 7781. Generated at container creation and displayed once by the node-side onboarding UI (port 7783); reused from the container env on daemon restarts. |
| `IOWAP_BRIDGE_PORT`    | no       | `7780`                    | TCP port. The daemon binds `127.0.0.1` only — never a foreign interface. |
| `IOWAP_WORKSPACE_DIR`  | no       | `./workspace`             | Workspace root; must be the same directory the node container mounts at `/workspace`. |
| `IOWAP_PROVIDERS_FILE` | no       | `<configdir>/iowap-bridge/providers.yaml` | Provider registry (board T-019). Missing/empty → empty manifest. |
| `IOWAP_MCP_PORT`       | —        | removed (v0.2.0)          | Was UI-snippet-only; the compose profile owns the real 7781 mapping. |

## Providers (providers.yaml)

Every entry becomes one manifest capability. Common fields:
`name` (capability name, pattern `^[a-z][a-z0-9_.-]{1,63}$`, node-side
`host.` prefix), `summary`, `long_run`, `input_schema` (JSON Schema for
the params object), `workspace` (optional subdirectory of the workspace
root this provider's tasks run in; self-creating).

### `type: comfyui`

The built-in ComfyUI integration as a configurable case — same behavior
as the pre-0.2.0 hard-coded provider (capability `comfy.generate`,
`/prompt` submit + `/history` poll + `/view` downloads, health probe with
`/system_stats` fallback, start-on-demand):

```yaml
providers:
  - type: comfyui
    url: http://127.0.0.1:8188
    start_cmd: "C:/ComfyUI/run_nvidia_gpu.bat"   # optional
    long_run: true
```

### `type: http` — generic REST provider

`health_url` is probed (GET, 2xx = ok; down after 3 consecutive failures;
without it the capability is always healthy). `{{param}}` and
`{{task_id}}` placeholders are substituted in `url_template`, header
values and `body_template` (strings as-is, numbers/booleans textually,
objects as compact JSON; an unknown variable is a terminal
`invalid_params` error). `result_path` selects the response field that
becomes the exec result (dot path, empty = whole response object);
`artifact_paths` are dot paths to workspace-relative file path strings in
the response.

```yaml
providers:
  - name: render.doc
    type: http
    health_url: http://127.0.0.1:9000/health
    exec:
      method: POST
      url_template: "http://127.0.0.1:9000/api/{{task_id}}/render"
      headers: { X-Api-Key: "local-secret" }
      body_template: '{"file": "{{file}}"}'
      result_path: result
      artifact_paths: ["result.output_path"]
```

Error mapping mirrors the ComfyUI provider: 400 → `invalid_params`
(terminal), other non-2xx → `capability.failed` (terminal), unreachable →
`provider.unavailable` (retryable).

### `type: process` — host command

> **Security:** this type executes arbitrary local commands with the
> daemon's privileges — it is the deliberate "invoke scripts" layer, and
> the daemon operator is responsible for every configured command. Do not
> accept `providers.yaml` content from untrusted sources.

Params are substituted into the argv templates (`command` entries support
`{{param}}` / `{{task_id}}`) **and** piped to the process stdin as JSON
(`stdin_json: false` disables the pipe). The process runs with its
working directory set to the (provider-scoped) workspace. Files matching
`artifacts_glob` (workspace-relative) become artifacts. Non-zero exit →
`capability.failed` (terminal, stderr tail in the message); unstartable
command → `provider.unavailable` (retryable); timeout → `timeout`
(retryable).

```yaml
providers:
  - name: script.cleanup
    type: process
    command: ["python", "C:/scripts/cleanup.py", "--target", "{{target}}"]
    artifacts_glob: "out/*.png"
```

See `providers.example.yaml` for a complete commented example.

## Endpoints

All endpoints require `Authorization: Bearer <IOWAP_BRIDGE_TOKEN>` and
return `401` + `WWW-Authenticate: Bearer` otherwise (constant-time token
comparison).

### `GET /v1/manifest`

```json
{
  "version": 1,
  "provider": { "name": "iowap-bridge", "host_os": "windows", "version": "0.2.0" },
  "capabilities": [
    {
      "name": "comfy.generate",
      "summary": "Generate images with the local ComfyUI installation (Stable Diffusion)",
      "input_schema": { "type": "object", "properties": { "workflow": { "type": "object" } }, "required": ["workflow"] },
      "long_run": true,
      "health": "ok"
    }
  ]
}
```

`health` is `ok | down`; the ComfyUI health probe runs every 15 s and the
capability reports `down` after 3 consecutive failed probes (the node
then stops publishing the capability).

### `POST /v1/exec/{name}`

Request: `{ "task_id": "...", "params": { ... }, "timeout_s": 3600 }`.

For `comfy.generate`, `params` must be `{"workflow": <ComfyUI API-format
workflow>}` (the JSON from ComfyUI's "Save (API Format)"). The daemon
submits it to ComfyUI's `/prompt`, polls `/history` until completion,
downloads the outputs via `/view` and writes them under
`<workspace>/<task_id>/<filename>`.

Response (success): `{"status": "completed", "result": {...}, "artifacts": [{"name": "...", "path": "..."}]}`
— artifact paths are workspace-relative, readable by the node through the
shared workspace volume.

Response (task failure, HTTP 200): `{"status": "failed", "error": {"code": "...", "message": "...", "retryable": false}}`

| Error code              | Retryable | Meaning                                   |
|-------------------------|-----------|-------------------------------------------|
| `capability.failed`     | no        | The task failed while executing            |
| `invalid_params`        | no        | Params violate the capability's schema     |
| `provider.unavailable`  | yes       | The host application is not reachable      |
| `timeout`               | yes       | The task did not finish within `timeout_s` |

Unknown capability → HTTP 404.

## Releases

Release binaries (`windows/amd64`, `linux/amd64`, built with
`-trimpath -ldflags "-s -w"`) are attached to GitHub releases on `v*`
tags. Every asset has a sibling `.sha256` file — **verify the checksum
before running a downloaded binary**:

```sh
sha256sum -c iowap-bridge-windows-amd64.exe.sha256
```
