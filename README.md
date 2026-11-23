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
   come from `providers.yaml`. The **workspace file** takes precedence:
   if `<workspace>/bridge/providers.yaml` exists (the channel the node
   dashboard writes capability definitions through, board T-021/T-022),
   it overrides the config-dir registry. A missing or empty file is fine:
   the daemon serves an **empty manifest** rather than crashing. A broken
   file makes the daemon refuse to start — loud, not silent. See
   [Providers](#providers-providersyaml) below and
   `providers.example.yaml`.

   **Workspace provider watch (T-022):** after startup the daemon polls
   both files every 10 s (plain polling, no fsnotify — Windows-simple).
   The workspace file appearing, changing or disappearing hot-reloads the
   registry without a daemon restart — the manifest and exec routing
   follow immediately, health loops are restarted for the new provider
   set. A broken edit logs a warning and keeps the **last good registry**
   (never crashes, never serves an empty manifest over a broken file);
   when the workspace file disappears, the config-dir registry
   (`IOWAP_PROVIDERS_FILE`) takes over again.
4. **Bridge API** on `127.0.0.1:7780` (below), plus the **tray icon**
   (Windows — see below).

> **Onboarding moved into the node container (v0.2.0, board DEC #8).** The
> daemon no longer ships an onboarding web UI and no longer displays the MCP
> bearer. Node registration and the one-time bearer/snippet display live in
> the node-side onboarding UI at `http://127.0.0.1:7783` (served inside the
> `iowap-universal-node` container).

## Tray & exe icon (Windows, board T-023, v0.3.1)

The daemon adds a system tray icon (`fyne.io/systray` — pure Go on
Windows, no CGO). The tray runs in its own goroutine and is best-effort:
any tray failure or panic is logged, the bridge daemon keeps serving.

Since v0.3.1 the tray shows the real IOWAP icon (multi-size
`assets/iowap.ico`, 16–256 px — host tower in cornflower blue, cluster
tower in lavender, segmented arch on a dark plate), embedded into the
binary via `go:embed`. The same asset is compiled into the exe as a
Windows resource (`rsrc_windows_amd64.syso`, generated with
`github.com/akavel/rsrc`), so Explorer, Taskbar and shortcuts show the
icon as well. The tray embeds a byte-identical copy of the asset
(`internal/tray/icon.ico`) because `go:embed` cannot reference parent
directories — `internal/tray/icon_test.go` fails the build when the two
drift apart.

**Status widget** (disabled header lines in the menu, refreshed every
5 s): container state (`running`/`stopped` via the Docker Engine),
node registration (probed from the node dashboard on 7783 — the blank
setup form means "not registered"), daemon version and the pulled node
image digest.

**Menu:**

* **Open Dashboard** — opens `http://127.0.0.1:7783` in the default
  browser (same helper as the bootstrap hand-off).
* **Restart docker container** — stops and starts the
  `iowap-universal-node` container as-is (no recreate, tokens/bearers
  untouched — consistent with the bootstrap's existing-container path).
* **Check for updates** — informational only in v1 (nothing is
  downloaded or executed):
  * container image: the local repo digest (`ghcr.io/iowap-org/
    iowap-node-base`) vs. the registry's current digest (registry v2
    API, anonymous pull token);
  * the exe: the daemon's own version vs. the GitHub latest release of
    `iowap-org/iowap-bridge-daemon`.
  The result ("update available" / "up to date" / reason it could not
  check) appears as a status line and in the log.
* **Quit** — confirmation dialog **"Stop the node container too?"**:
  *Yes* stops the container and exits the daemon; *No* exits the daemon
  and leaves the node running. The daemon exits even when the container
  stop fails (the failure is logged).

On non-Windows platforms the tray is a no-op stub (the daemon stays
buildable and CGO-free; CI runs the full suite there).

| Port  | Purpose                                        | Binding     |
|-------|------------------------------------------------|-------------|
| 7780  | Bridge manifest/exec API (node → daemon)       | 127.0.0.1   |
| 7781  | HTTP MCP endpoint (agents → node container)    | 127.0.0.1   |
| 7783  | Node onboarding UI + bearer/snippet display (node container, board T-017) | 127.0.0.1   |

## Console window & daemon.log (v0.3.1)

A double-click on the released exe starts the daemon **windowless**: the
release build uses the `windowsgui` subsystem (release.yml,
`-ldflags "-s -w -H=windowsgui"`), so the OS never creates a console — no
window, no flash, just the tray icon. This was a deliberate choice over
the console-subsystem + `ShowWindow(SW_HIDE)` alternative, which creates
a console first and hides it (visible flash on some Windows versions).

Debugging is opt-in per run:

* `iowap-bridge.exe --debug` (command-line flag), or
* `IOWAP_BRIDGE_DEBUG=1` (environment variable, truthy values
  `1`/`true`/`yes`/`on`)

With debug set, the daemon allocates a console (`AllocConsole` in
`internal/console`) and all bootstrap/tray/watch messages appear there.

Without the flag the same messages additionally go to a log file next to
the config dir:

```
%LOCALAPPDATA%\iowap-bridge\daemon.log
```

Append mode, simple rotation at 5 MB (the current file becomes
`daemon.log.1`). Logging setup is best-effort — an unwritable log dir
never keeps the daemon from starting. Note: a local dev build (`go run` /
`go build` without `-H=windowsgui`) has a normal console attached; there
`--debug` reuses it and a non-debug run still shows its console.

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
| `IOWAP_PROVIDERS_FILE` | no       | `<configdir>/iowap-bridge/providers.yaml` | Provider registry (board T-019). Overridden by `<workspace>/bridge/providers.yaml` when that file exists (workspace watch, board T-022). Missing/empty → empty manifest. |
| `IOWAP_BRIDGE_DEBUG`   | no       | unset                     | Show the console window and log verbosely to stderr (v0.3.1). Truthy values: `1`/`true`/`yes`/`on`. Equivalent to the `--debug` flag. Without it, logs go to `%LOCALAPPDATA%\iowap-bridge\daemon.log`. |
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
  "provider": { "name": "iowap-bridge", "host_os": "windows", "version": "0.3.0" },
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
