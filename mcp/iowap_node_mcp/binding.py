"""T-035 P2a: token-bound scoping for the four task/artifact client tools.

``iowap-client-mcp``'s tools (9, curated) are transport-agnostic and stay
UNTOUCHED — the stdio local-agent surface keeps its exact behavior. This
module installs the TOKEN-BOUND versions on the node-HTTP surface only:
after :func:`iowap_node_mcp.server` builds the shared server, the four
write/read task tools are swapped for wrappers (same names, same schemas —
``tools/list`` stays 14, no new tool) that delegate to the library
impl and add node-owned enforcement:

* ``task_submit``: on a successful submit, binds ``task_id`` to the agent
  (``bindings.json``); the primary path records nothing; an error response
  (the T-274 shape) binds nothing;
* ``task_result``: agents may read only their bound tasks; anything else
  returns EXACTLY the legacy unknown-task response (``status: "not_found"``
  + hint, the T-227/T-005g semantics — foreign tasks are indistinguishable
  from unknown ones). A readable result records the artifacts it carries;
* ``artifact_download`` / ``artifact_link``: agents must have SEEN the
  artifact (via a recorded result); otherwise the T-274 not-found error
  shape (fail-closed).

The primary/legacy bearer keeps FULL access with no binding (DEC: the
migration never breaks; legacy tasks stay readable for everyone).
"""

from __future__ import annotations

from collections.abc import Callable
from typing import Any

from iowap_client_mcp import tools as client_tools

from . import agent_tokens
from .agent_tokens import PRIMARY_SUBJECT, current_identity

__all__ = [
    "SCOPED_TOOLS",
    "extract_artifact_ids",
    "install_scoping",
]

# The toolset swapped on the HTTP surface (names unchanged, count unchanged).
SCOPED_TOOLS = ("task_submit", "task_result", "artifact_download", "artifact_link")

# The exact legacy unknown-task response the client library returns for a
# task the node identity cannot read (T-227/T-005g) — the agent-facing
# "foreign" answer must be indistinguishable from an unknown task_id.
_NOT_FOUND_HINT = (
    "task not readable from this node's identity — since T-227 "
    "admin, the owner node, and the sender (submitted_by) "
    "can read it; submit from the owning/sending node's token"
)

# T-274 error shape (not-found-like) for artifacts outside the caller's view.
def _artifact_denied(artifact_id: str) -> dict[str, Any]:
    return {
        "error": {
            "http_status": 404,
            "detail": f"artifact '{artifact_id}' not found",
            "raw": f"artifact {artifact_id} not found (or not in this token's binding view)",
        }
    }


def extract_artifact_ids(result: dict[str, Any]) -> list[str]:
    """Collect artifact ids a task_result payload says the agent has seen.

    Bridge-capability stage results carry the upload echoes under an
    ``artifacts`` key (``[{"artifact_id": ...}, ...]`` — nodes/common/
    bridge.py). The walk stays defensive: any list under any ``artifacts``
    key, any dict entry with a str ``artifact_id``, task-level and
    stage-level shapes, exactly-once ids, unknown shapes ignored silently.
    """
    ids: list[str] = []

    def _walk(node: Any) -> None:
        if isinstance(node, dict):
            if isinstance(node.get("artifact_id"), str) and node["artifact_id"]:
                ids.append(node["artifact_id"])
            for value in node.values():
                if isinstance(value, (dict, list)):
                    _walk(value)
        elif isinstance(node, list):
            for item in node:
                _walk(item)

    _walk(result)
    seen: set[str] = set()
    ordered = [eid for eid in ids if not (eid in seen or seen.add(eid))]
    return ordered


# ---------------------------------------------------------------------------
# Wrappers (identical signatures → identical tool schemas on the wire)
# ---------------------------------------------------------------------------


async def scoped_task_submit(
    capability: str,
    payload: dict[str, Any],
    owner_node_id: str | None = None,
    wait_seconds: int = 0,
    name: str | None = None,
    session_id: str | None = None,
) -> dict[str, Any]:
    """``task_submit`` + ``record_task`` on success (primary records nothing)."""
    result = await client_tools.task_submit(
        capability,
        payload,
        owner_node_id=owner_node_id,
        wait_seconds=wait_seconds,
        name=name,
        session_id=session_id,
    )
    identity = current_identity()
    if identity != PRIMARY_SUBJECT and "error" not in result and result.get("task_id"):
        agent_tokens.record_task(identity, str(result["task_id"]))
    return result


async def scoped_task_result(task_id: str) -> dict[str, Any]:
    """``task_result`` with the binding gate + artifact recording.

    Foreign/unknown-identity tasks return the EXACT legacy unknown-task
    shape — the binding never widens the not_found semantics, it just
    narrows who may see what.
    """
    identity = current_identity()
    if identity != PRIMARY_SUBJECT and not agent_tokens.can_read_task(identity, task_id):
        return {"task_id": task_id, "status": "not_found", "hint": _NOT_FOUND_HINT}
    result = await client_tools.task_result(task_id)
    if identity != PRIMARY_SUBJECT and "error" not in result:
        # record what this result exposes (bridge caps echo artifact ids);
        # a payload without artifacts records nothing (harmless no-op).
        try:
            agent_tokens.record_artifacts(
                identity, task_id, extract_artifact_ids(result)
            )
        except (OSError, ValueError) as exc:  # binding never breaks a read
            agent_tokens.logger.warning(
                "record_artifacts(%s, %s) failed: %s", identity, task_id, exc
            )
    return result


async def scoped_artifact_download(
    artifact_id: str,
    output_path: str,
    overwrite: bool = False,
) -> dict[str, Any]:
    """``artifact_download`` gated by the seen-artifacts binding view."""
    identity = current_identity()
    if identity != PRIMARY_SUBJECT and not agent_tokens.can_read_artifact(identity, artifact_id):
        return _artifact_denied(artifact_id)
    return await client_tools.artifact_download(
        artifact_id, output_path, overwrite=overwrite
    )


async def scoped_artifact_link(
    artifact_id: str,
    ttl_seconds: int | None = None,
    max_downloads: int | None = None,
) -> dict[str, Any]:
    """``artifact_link`` gated by the seen-artifacts binding view (T-274 shape)."""
    identity = current_identity()
    if identity != PRIMARY_SUBJECT and not agent_tokens.can_read_artifact(identity, artifact_id):
        return _artifact_denied(artifact_id)
    return await client_tools.artifact_link(
        artifact_id, ttl_seconds=ttl_seconds, max_downloads=max_downloads
    )


_WRAPPER_ATTR = "iowap_t035_scoped"  # idempotence marker (double-install guard)


def install_scoping(server: Any) -> None:
    """Swap the four client-library tools for their token-bound versions.

    Same names + same descriptions (reused from the registered Tool objects)
    → ``tools/list`` stays exactly 14 with identical schemas; the manager's
    ``add_tool`` keeps duplicates, so the swap is ``remove_tool`` first.
    Idempotent: already-scoped tools are left untouched.
    """
    wrappers: dict[str, Callable[..., Any]] = {
        "task_submit": scoped_task_submit,
        "task_result": scoped_task_result,
        "artifact_download": scoped_artifact_download,
        "artifact_link": scoped_artifact_link,
    }
    manager = server._tool_manager
    for name, wrapper in wrappers.items():
        tool = manager.get_tool(name)
        if tool is None:
            raise ValueError(f"cannot scope unknown tool {name!r}")
        if getattr(tool.fn, _WRAPPER_ATTR, False):
            continue  # idempotent — this server already carries the wrappers
        description = tool.description
        manager.remove_tool(name)
        # add_tool directly = exactly what server.tool() does under the hood;
        # the wrapper carries the identical signature → identical wire schema.
        wrapper_with_marker = wrapper
        setattr(wrapper_with_marker, _WRAPPER_ATTR, True)
        manager.add_tool(wrapper_with_marker, name=name, description=description)
