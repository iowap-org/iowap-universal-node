"""T-042: capability-manage toolset — the node-HTTP-surface-only write extension.

Five tools let an MCP-connected agent manage the container node's workspace
capability store (``<workspace>/bridge/providers.yaml``) exactly the way the
dashboard does — every tool drives the node CLI (``iowap-cli capabilities …``,
T-042 subcommands), which wraps the dashboard store functions verbatim:

* ``capability_create`` (fields) → ``capabilities create``
* ``capability_update`` (name, fields) → ``capabilities update``
* ``capability_delete`` (name, confirm) → ``capabilities delete`` — destructive,
  requires ``confirm=true`` (a falsy confirm raises before any spawn);
* ``capability_publish`` (name, published) → ``capabilities update
  --publish/--unpublish`` (field-less invocation = pure allowlist toggle,
  no version bump — the CLI publish-chain semantics);
* ``capability_script_upload`` (script_name, content) →
  ``capabilities script-upload`` with the bytes staged through a temp file
  (``--file``); the 2 MB cap is enforced HERE before any write.

Error transparency follows the T-274 shape: relay/classified runner failures
return ``{'error': {'http_status': int | None, 'detail', 'raw'}}`` instead of
raising, reusing the client library's own ``_relay_error_result`` helper (no
copy). Validation errors (shape, unknown field, gate, confirm, size cap)
still raise.

Scoping (SOFAT): the operator env ``IOWAP_ALLOW_CAPS_MANAGE`` (csv of tool
names; unset/empty = all five allowed) gates the whole set, read per call —
same semantics as ``IOWAP_ALLOW_CAPS`` for task_submit. Primary/legacy is
operator context, so the agent-token manage scope defaults to open.

T-035 P2: per-token scope check on top of the operator env gate — an
agent-token caller (identity from the T-035 auth context) needs its
``caps_manage`` flag in ``agents.json``; the primary bearer always passes
(operator context, flag rides no file). A denied agent gets the T-274
error shape ``{'error': {'http_status': None, 'detail': ..., 'raw':
'restricted for this token'}}`` from the tool result — the other tools of
the surface are unaffected (no session abort).
"""

from __future__ import annotations

import os
import tempfile
from collections.abc import Callable
from typing import Any

from iowap_client_mcp.nodecli import NodeCliError, run_node_cli
from iowap_client_mcp.tools import _relay_error_result

from . import agent_tokens
from .agent_tokens import PRIMARY_SUBJECT, current_identity

__all__ = [
    "MANAGE_TOOLS",
    "capability_create",
    "capability_delete",
    "capability_publish",
    "capability_script_upload",
    "capability_update",
    "register_manage_tools",
]

NODE_MANAGE_ENV = "IOWAP_ALLOW_CAPS_MANAGE"

MANAGE_TOOLS = (
    "capability_create",
    "capability_update",
    "capability_delete",
    "capability_publish",
    "capability_script_upload",
)

# The 2 MB cap mirrors the node dashboard's MAX_SCRIPT_BYTES (the store
# re-checks on CLI side; this pre-send check keeps bytes off disk first).
_MAX_SCRIPT_BYTES = 2 * 1024 * 1024

# MCP field key → CLI flag of `capabilities create/update` (T-042 slice).
_CREATE_FIELD_FLAGS: dict[str, str] = {
    "name": "--name",
    "type": "--type",
    "summary": "--summary",
    "command": "--command",
    "max_parallel": "--max-parallel",
    "cost": "--cost",
    "artifacts_glob": "--artifacts-glob",
    "version": "--version",
    "url_template": "--url-template",
    "method": "--method",
    "body_template": "--body-template",
    "result_path": "--result-path",
    "artifact_paths": "--artifact-paths",
    "health_url": "--health-url",
}
_LIST_FLAGS: dict[str, str] = {"args": "--args"}  # raw tokens, repeated


def _allowed_manage_tools() -> set[str] | None:
    """Operator allowlist from ``IOWAP_ALLOW_CAPS_MANAGE`` (csv), or None = open.

    Read per call so MCP-host configuration (``--env IOWAP_ALLOW_CAPS_MANAGE=…``)
    applies live; unset/empty means no restriction (primary/legacy is operator
    context, where manage defaults to allowed).
    """
    raw = os.environ.get(NODE_MANAGE_ENV, "")
    allowed = {entry.strip() for entry in raw.split(",") if entry.strip()}
    return allowed or None


def _manage_gate(tool_name: str) -> None:
    """Raise when the tool is excluded by the operator manage allowlist."""
    allowed = _allowed_manage_tools()
    if allowed and tool_name not in allowed:
        raise ValueError(
            f"capability manage tool {tool_name!r} not in {NODE_MANAGE_ENV} "
            f"allowlist — operator-set manage tools: {sorted(allowed)}"
        )


def _agent_scope_gate(tool_name: str) -> dict[str, Any] | None:
    """T-035 P2: deny ``capability_*`` to agent tokens without caps_manage.

    The identity comes from the per-request auth context (server-minted
    subject — not forgeable). ``primary``/no-request identities pass (the
    operator env keep being the only gate); a non-manage agent gets the
    T-274 error shape as the tool result, no raise — the other tools of
    the surface stay usable.
    """
    identity = current_identity()
    if identity == PRIMARY_SUBJECT:
        return None
    if agent_tokens.caps_manage_for(identity):
        return None
    return {
        "error": {
            "http_status": None,
            "detail": (
                f"capability {tool_name} is restricted for this token — "
                "the operator has not enabled caps_manage for this agent "
                "(dashboard: Agent-Tokens → Allow caps-manage)"
            ),
            "raw": "restricted for this token (caps_manage off)",
        }
    }


def _check_scope(tool_name: str) -> dict[str, Any] | None:
    """Combined gate: operator env allowlist, then the T-035 token scope."""
    _manage_gate(tool_name)
    return _agent_scope_gate(tool_name)


def _validate_str(name: str, value: Any) -> str:
    """One non-empty string argument (stripped); ValueError when malformed."""
    if not isinstance(value, str) or not value.strip():
        raise ValueError(f"{name} must be a non-empty string")
    return value.strip()


def _fields_to_cli_args(fields: dict[str, Any]) -> list[str]:
    """Map a fields dict onto ``capabilities create`` CLI flags.

    Unknown keys are rejected with the allowed key list (the CLI parser would
    only exit 2); ``publish`` becomes the CLI ``--publish`` store_true flag.
    """
    if not isinstance(fields, dict):
        raise TypeError("fields must be a JSON object (dict)")
    allowed = set(_CREATE_FIELD_FLAGS) | set(_LIST_FLAGS) | {"publish"}
    unknown = sorted(set(fields) - allowed)
    if unknown:
        raise ValueError(
            f"unknown field(s) {unknown} — allowed keys: {sorted(allowed)}"
        )
    argv: list[str] = []
    for key, flag in _CREATE_FIELD_FLAGS.items():
        value = fields.get(key)
        if value in (None, ""):
            continue
        argv += [flag, str(value)]
    for key, flag in _LIST_FLAGS.items():
        value = fields.get(key)
        if not value:
            continue
        if not isinstance(value, list) or not all(isinstance(t, str) for t in value):
            raise ValueError(f"{key} must be a list of strings")
        argv += [flag, *value]
    if fields.get("publish"):
        argv += ["--publish"]
    if not argv:
        raise ValueError("fields is empty — at least one capability field is required")
    return argv


def _error_or_payload(payload: Any) -> dict[str, Any]:
    """Normalize the CLI's JSON payload to a dict (defensive)."""
    return payload if isinstance(payload, dict) else {"cli_output": payload}


async def capability_create(fields: dict[str, Any]) -> dict[str, Any]:
    """Create one capability in the node's workspace store (dashboard rules).

    ``fields`` carries the store entry: ``name`` (required), ``type``
    (process|http), ``summary``, ``command``, ``args`` (extra argv tokens),
    ``max_parallel``, ``cost``, ``artifacts_glob``, ``version`` (X.Y.Z),
    the http exec fields (``url_template``, ``method``, ``body_template``,
    ``result_path``, ``artifact_paths``, ``health_url``), and ``publish``
    (true → also add the capability to the bridge allowlist). Validation is
    the dashboard store's (write-nothing-on-error, duplicate → "delete it
    first"). Returns the entry summary {name, version, published}; CLI
    failures come back as ``{'error': {...}}`` (T-274).
    """
    denied = _check_scope("capability_create")
    if denied is not None:
        return denied
    if not isinstance(fields, dict):
        raise TypeError("fields must be a JSON object (dict)")
    name = _validate_str("name", fields.get("name"))
    # --name rides explicitly; a (equal) fields.name is not re-emitted.
    flag_fields = {k: v for k, v in fields.items() if k != "name"}
    argv = ["capabilities", "create", "--name", name, *_fields_to_cli_args(flag_fields)]
    try:
        payload = await run_node_cli(argv, timeout=30.0)
    except NodeCliError as exc:
        return _relay_error_result(exc)  # T-274: no raw exception to the host
    return _error_or_payload(payload)


async def capability_update(name: str, fields: dict[str, Any]) -> dict[str, Any]:
    """Edit one capability in place — name immutable, version auto-bumps.

    ``name`` is the immutable routing key (the store refuses renames;
    explicit ``fields["name"]`` equal to ``name`` is accepted). Every edit
    patch-bumps the version unless a valid differing ``version`` is passed.
    Pass ``publish: true`` to also add the capability to the bridge
    allowlist. Returns the entry summary; failures as T-274 error result.
    """
    denied = _check_scope("capability_update")
    if denied is not None:
        return denied
    name = _validate_str("name", name)
    if not isinstance(fields, dict):
        raise TypeError("fields must be a JSON object (dict)")
    unknown = sorted(set(fields) - set(_CREATE_FIELD_FLAGS) - set(_LIST_FLAGS) - {"publish"})
    if unknown:
        allowed = sorted(set(_CREATE_FIELD_FLAGS) | set(_LIST_FLAGS) | {"publish"})
        raise ValueError(f"unknown field(s) {unknown} — allowed keys: {allowed}")
    if not fields:
        raise ValueError("fields is empty — at least one capability field is required")
    new_name = fields.get("name")
    if new_name and new_name.strip() != name:
        raise ValueError(
            f"name is immutable — {name!r} cannot be renamed to {new_name!r} "
            "(rename stays delete + create legitimate)"
        )
    # --name rides explicitly; the (equal) fields.name is not re-emitted.
    flag_fields = {k: v for k, v in fields.items() if k != "name"}
    argv = ["capabilities", "update", "--name", name, *_fields_to_cli_args(flag_fields)]
    try:
        payload = await run_node_cli(argv, timeout=30.0)
    except NodeCliError as exc:
        return _relay_error_result(exc)  # T-274
    return _error_or_payload(payload)


async def capability_delete(name: str, confirm: bool) -> dict[str, Any]:
    """Remove one capability from the node's workspace store — DESTRUCTIVE.

    Destructive; requires ``confirm=true`` (refuses to spawn otherwise).
    Returns {ok, name, deleted}; failures as T-274 error result.
    """
    denied = _check_scope("capability_delete")
    if denied is not None:
        return denied
    if confirm is not True:
        raise ValueError(
            "capability_delete is destructive — set confirm=true to remove "
            "the capability from the workspace store"
        )
    name = _validate_str("name", name)
    try:
        payload = await run_node_cli(
            ["capabilities", "delete", "--name", name], timeout=30.0
        )
    except NodeCliError as exc:
        return _relay_error_result(exc)  # T-274
    return _error_or_payload(payload)


async def capability_publish(name: str, published: bool = True) -> dict[str, Any]:
    """Toggle one capability's bridge publish state (allowlist, no version bump).

    ``published=true`` adds the capability to the bridge allowlist, false
    removes it — the field-less CLI update invocation, i.e. the dashboard
    publish/unpublish button semantics WITHOUT an entry version bump. Use
    ``capability_update(name, {…, 'publish': true})`` instead when the edit
    itself should publish. Failures as T-274 error result.
    """
    denied = _check_scope("capability_publish")
    if denied is not None:
        return denied
    if not isinstance(published, bool):
        raise TypeError("published must be a boolean")
    name = _validate_str("name", name)
    flag = "--publish" if published else "--unpublish"
    try:
        payload = await run_node_cli(
            ["capabilities", "update", "--name", name, flag], timeout=30.0
        )
    except NodeCliError as exc:
        return _relay_error_result(exc)  # T-274
    return _error_or_payload(payload)


async def capability_script_upload(script_name: str, content: str) -> dict[str, Any]:
    """Store one capability script under <workspace>/bridge/scripts.

    ``script_name`` must carry an allowed extension (.bat/.cmd/.ps1/.py/.sh/
    .exe; the store checks and collision-suffixes); ``content`` is the script
    text (str, sent as UTF-8 bytes — 2 MB cap enforced here BEFORE any write).
    Returns {ok, stored (workspace-relative), stored_name, name (slug),
    command (default argv template)}; failures as T-274 error result.
    """
    denied = _check_scope("capability_script_upload")
    if denied is not None:
        return denied
    script_name = _validate_str("script_name", script_name)
    if "/" in script_name or "\\" in script_name or ".." in script_name:
        raise ValueError("script_name must be a bare filename (no path segments)")
    if isinstance(content, str):
        data = content.encode("utf-8")
    elif isinstance(content, bytes):
        data = content
    else:
        raise TypeError("content must be a string (encoded as UTF-8 bytes)")
    if len(data) > _MAX_SCRIPT_BYTES:
        raise ValueError(
            f"script too large ({len(data)} bytes) — cap is "
            f"{_MAX_SCRIPT_BYTES // (1024 * 1024)} MB"
        )
    with tempfile.NamedTemporaryFile(prefix="iowap-cap-script-") as holder:
        holder.write(data)
        holder.flush()
        try:
            payload = await run_node_cli(
                [
                    "capabilities", "script-upload",
                    "--file", holder.name, "--name", script_name,
                ],
                timeout=30.0,
            )
        except NodeCliError as exc:
            return _relay_error_result(exc)  # T-274
    return _error_or_payload(payload)


def register_manage_tools(server: Any) -> None:
    """Attach the T-042 capability-manage toolset to an MCPServer instance.

    Node-HTTP-surface only (T-042 SOFAT): the stdio local-agent surface of
    iowap-client-mcp keeps the curated 9-tool set unchanged.
    """
    _t: Callable[..., Any] = server.tool
    _t(
        name="capability_create",
        description=(
            "Create an IOWAP capability in this node's workspace store "
            "(dashboard rules: name/type/command/etc. validated the same, "
            "write-nothing-on-error, duplicates rejected with 'delete it "
            "first'). fields carries name (required), type (process|http), "
            "summary, command, args, max_parallel, cost, artifacts_glob, "
            "version, http exec fields, publish. Operators can gate this via "
            "IOWAP_ALLOW_CAPS_MANAGE (csv env; unset = allowed). CLI "
            "failures return {'error': {http_status, detail, raw}}."
        ),
    )(capability_create)
    _t(
        name="capability_update",
        description=(
            "Edit an IOWAP capability in place (name immutable, version "
            "auto-bumps per edit; explicit differing version wins). Same "
            "field set as capability_create; publish=true also adds it to "
            "the bridge allowlist. Gated by IOWAP_ALLOW_CAPS_MANAGE; CLI "
            "failures return {'error': {http_status, detail, raw}}."
        ),
    )(capability_update)
    _t(
        name="capability_delete",
        description=(
            "Destructive; requires confirm=true. Removes an IOWAP capability "
            "from this node's workspace store (the bridge allowlist entry "
            "stays — same as the dashboard delete). Gated by "
            "IOWAP_ALLOW_CAPS_MANAGE; failures as {'error': {...}}."
        ),
    )(capability_delete)
    _t(
        name="capability_publish",
        description=(
            "Toggle an IOWAP capability's bridge publish state (allowlist "
            "membership; NO version bump — the dashboard publish button). "
            "published=false unpublishes. For an edit that publishes use "
            "capability_update(publish=true). Gated by IOWAP_ALLOW_CAPS_MANAGE."
        ),
    )(capability_publish)
    _t(
        name="capability_script_upload",
        description=(
            "Store a capability script in <workspace>/bridge/scripts "
            "(extension allowlist .bat/.cmd/.ps1/.py/.sh/.exe, 2 MB cap, "
            "collision suffixing). Returns the workspace-relative stored "
            "path, the slugified name and the default argv template to use "
            "as the command in capability_create. Gated by "
            "IOWAP_ALLOW_CAPS_MANAGE; failures as {'error': {...}}."
        ),
    )(capability_script_upload)
