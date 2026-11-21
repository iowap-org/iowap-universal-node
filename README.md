# iowap-bridge-daemon

IOWAP Bridge Daemon — native host-side capability provider (Manifest/Exec
contract) for the universal node container; first target: Windows
(Docker Desktop), Go.

The daemon exposes host applications (first target: a local ComfyUI
installation) to an IOWAP node running in a container on the same host.
The node polls the manifest (pull model) and forwards claimed tasks via
HTTP; both sides share a workspace directory for artifact hand-off.

**Status: private until first release.**

## Build & Run

Requires Go 1.23+ (or Docker: `docker run --rm -v "$PWD":/app -w /app golang:1.23 go build ./cmd/iowap-bridge`).

```sh
go run ./cmd/iowap-bridge
```

### Environment

| Variable               | Required | Default                   | Description                                                        |
|------------------------|----------|---------------------------|--------------------------------------------------------------------|
| `IOWAP_BRIDGE_TOKEN`   | yes      | —                         | Bearer token for all endpoints. **Fail-closed:** if unset/empty the daemon exits before opening a socket. The node container receives the same token as `IOWAP_BRIDGE_TOKEN` (injected at container creation). |
| `IOWAP_BRIDGE_PORT`    | no       | `7780`                    | TCP port. The daemon binds `127.0.0.1` only — never a foreign interface. |
| `IOWAP_WORKSPACE_DIR`  | no       | `./workspace`             | Workspace root; must be the same directory the node container mounts at `/workspace`. |
| `IOWAP_COMFYUI_URL`    | no       | `http://127.0.0.1:8188`   | ComfyUI base URL.                                                  |
| `IOWAP_COMFYUI_CMD`    | no       | —                         | Optional command to (re)start ComfyUI when a health probe fails. The daemon launches it and waits for the server to become healthy. |

## Endpoints

All endpoints require `Authorization: Bearer <IOWAP_BRIDGE_TOKEN>` and
return `401` + `WWW-Authenticate: Bearer` otherwise (constant-time token
comparison).

### `GET /v1/manifest`

```json
{
  "version": 1,
  "provider": { "name": "iowap-bridge", "host_os": "windows", "version": "0.1.0" },
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
