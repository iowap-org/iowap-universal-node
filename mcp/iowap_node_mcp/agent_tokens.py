"""T-035: agent-token store readers for iowap-node-mcp — the MCP-side half.

The node package's ``nodes/common/agent_tokens.py`` OWNS the store API
(create/show-once, revoke, caps toggle); this module re-implements ONLY the
read + binding side that the MCP process needs, per the T-037 split rule:
the mcp/ package must never import the node package. Both sides share the
IDENTICAL JSON contracts (``agents.json`` / ``bindings.json``, same field
names, same schema) and both package suites pin the same example files, so
a schema drift breaks one of the suites instead of live traffic.

Storage resolver mirrors the node side exactly (T-050 included):
``IOWAP_AGENTS_DIR`` env override (wins unprobed, per call) > the dedicated
token volume ``/data/agents`` when it exists AND passes a WRITE probe
(mkdir + tmp write + unlink — ``os.access()`` alone misses a root-owned
named volume, which surfaced as ``PermissionError [Errno 13]:
/data/agents/agents.json.tmp`` in the Ronny-Log 2026-11-27) > the
``~/.relay/agents`` fallback with a ONE-TIME warning; the resolution is
cached once per process (probe-once). Files are read FRESH per call — the
dashboard and this process both write (last-writer-wins per file,
DEC-documented); writes go through tmp+os.replace.

Identity (T-035 P1b): :func:`current_identity` lifts the SDK access token
(set by :class:`server.MultiBearerVerifier` in the auth middleware
contextvar) into the ``agent_id | "primary"`` string the tool impls use.
Outside a live HTTP request (tests, plain calls) it returns ``"primary"``
— the operator/legacy semantics everything had before T-035.
"""

from __future__ import annotations

import hashlib
import json
import logging
import os
from pathlib import Path
from typing import Any

PRIMARY_SUBJECT = "primary"

VOLUME_AGENT_DIR = Path("/data/agents")
AGENTS_FILE = "agents.json"
BINDINGS_FILE = "bindings.json"

# T-050: probe-once per process — mirrors nodes/common/agent_tokens.py.
_AGENTS_DIR_CACHE: Path | None = None

logger = logging.getLogger("iowap-node-mcp.tokens")


# ---------------------------------------------------------------------------
# Store resolution + JSON I/O (same contracts as nodes/common/agent_tokens.py)
# ---------------------------------------------------------------------------


def agents_dir() -> Path:
    """``IOWAP_AGENTS_DIR`` > ``/data/agents`` (write-probed) > ``~/.relay/agents``.

    T-050: mirrors ``nodes/common/agent_tokens.py`` 1:1 (no import) — the
    volume candidate must pass a real write probe (``mkdir`` + tmp write +
    unlink; ``os.access()`` alone misses a root-owned named volume).
    Unwritable → relay-dir fallback with a one-time warning, and the whole
    resolution is cached on first call (probe-once per process — a later
    chown needs a process restart). The env override wins unprobed, per
    call (tests, exotic installs).
    """
    override = os.environ.get("IOWAP_AGENTS_DIR", "").strip()
    if override:
        return Path(override)
    global _AGENTS_DIR_CACHE
    if _AGENTS_DIR_CACHE is None:
        _AGENTS_DIR_CACHE = _resolve_agents_dir()
    return _AGENTS_DIR_CACHE


def _resolve_agents_dir() -> Path:
    """Volume dir if a real write probe passes; relay-dir fallback otherwise."""
    if VOLUME_AGENT_DIR.is_dir() and _dir_accepts_writes(VOLUME_AGENT_DIR):
        return VOLUME_AGENT_DIR
    fallback = _relay_fallback_dir()
    logger.warning(
        "agents store: %s not writable — using relay-dir fallback %s",
        VOLUME_AGENT_DIR, fallback,
    )
    return fallback


def _relay_fallback_dir() -> Path:
    """``~/.relay/agents`` — volume-less containers and probe failures."""
    return Path.home() / ".relay" / "agents"


def _dir_accepts_writes(path: Path) -> bool:
    """T-050 write probe: actually ``mkdir`` + write + unlink a tmp file.

    ``os.access()`` is not trustworthy here (root-owned dirs report odd
    outcomes; ACLs are invisible to it) — the probe does what the store
    itself would do, so the resolver sees the exact failure mode.
    """
    try:
        path.mkdir(parents=True, exist_ok=True)
        probe = path / f".iowap-write-probe-{os.getpid()}"
        probe.write_text("probe", encoding="utf-8")
        probe.unlink(missing_ok=True)
        return True
    except OSError:
        return False


def _reset_agents_dir_cache() -> None:
    """Test seam: forget the probe-once resolution (next call re-probes)."""
    global _AGENTS_DIR_CACHE
    _AGENTS_DIR_CACHE = None


def agents_path() -> Path:
    return agents_dir() / AGENTS_FILE


def bindings_path() -> Path:
    return agents_dir() / BINDINGS_FILE


def _read_json(path: Path, default: dict[str, Any]) -> dict[str, Any]:
    """Read one store file; corrupt/invalid → ``default`` (fail to primary-only)."""
    if not path.exists():
        return default
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError) as exc:
        logger.warning("agent-token store %s unreadable (%s) — primary-only mode", path, exc)
        return default
    if not isinstance(data, dict):
        logger.warning("agent-token store %s is not an object — primary-only mode", path)
        return default
    return data


def _write_json(path: Path, data: dict[str, Any]) -> None:
    """Atomic write (tmp + 0600 + os.replace) — the store API's write shape."""
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_text(json.dumps(data, indent=2) + "\n", encoding="utf-8")
    os.chmod(tmp, 0o600)
    os.replace(tmp, path)


def load_agents() -> list[dict[str, Any]]:
    """Known agents (same entry shape as the node store); corrupt → ``[]``."""
    agents = _read_json(agents_path(), {"version": 1, "agents": []}).get("agents")
    if not isinstance(agents, list):
        return []
    return [a for a in agents if isinstance(a, dict)]


def load_bindings() -> dict[str, dict[str, Any]]:
    """The binding map ``agent_id -> {task_ids, seen_artifacts}``; corrupt → ``{}``."""
    bindings = _read_json(bindings_path(), {"version": 1, "bindings": {}}).get("bindings")
    if not isinstance(bindings, dict):
        return {}
    return {k: v for k, v in bindings.items() if isinstance(v, dict)}


# ---------------------------------------------------------------------------
# Token verification (P1b) — primary env-token OR sha256'd agent token
# ---------------------------------------------------------------------------

def hash_token(raw: str) -> str:
    """SHA-256 hex digest of the raw bearer (the only value at rest)."""
    return hashlib.sha256(raw.encode("utf-8")).hexdigest()


def verify(raw: str) -> dict[str, Any] | None:
    """Resolve a raw token to its unrevoked agent entry, else ``None``."""
    if not isinstance(raw, str) or not raw:
        return None
    digest = hash_token(raw.strip())
    for entry in load_agents():
        if entry.get("revoked"):
            continue
        if isinstance(entry.get("token_sha256"), str) and entry["token_sha256"] == digest:
            return entry
    return None


def caps_manage_for(agent_id: str) -> bool:
    """The T-042 ``capability_*`` gate flag of one agent (False fail-closed)."""
    entry = next((a for a in load_agents() if a.get("id") == agent_id), None)
    if entry is None:
        return False
    return entry.get("caps_manage") is True


def touch(agent_id: str) -> bool:
    """Update ``last_used`` on every valid agent-token call (best-effort)."""
    agents = load_agents()
    for entry in agents:
        if entry.get("id") == agent_id:
            entry["last_used"] = _utcnow()
            try:
                _write_json(agents_path(), {"version": 1, "agents": agents})
            except OSError as exc:
                logger.warning("touch %s: write failed (%s)", agent_id, exc)
                return False
            return True
    return False


def _utcnow() -> str:
    from datetime import UTC, datetime

    return datetime.now(UTC).isoformat()


# ---------------------------------------------------------------------------
# Task/artifact binding (P2a) — the scoping view an agent is allowed to see
# ---------------------------------------------------------------------------


def _ensure_entry(bindings: dict[str, dict[str, Any]], agent_id: str) -> dict[str, Any]:
    """Normalize one binding entry {task_ids, seen_artifacts} (defensive shape)."""
    entry = bindings.get(agent_id)
    if not isinstance(entry, dict):
        entry = {}
    task_ids = [t for t in entry.get("task_ids", []) if isinstance(t, str)]
    seen_raw = entry.get("seen_artifacts")
    seen: dict[str, list[str]] = {}
    if isinstance(seen_raw, dict):
        for art_id, tasks in seen_raw.items():
            if isinstance(art_id, str) and isinstance(tasks, list):
                seen[art_id] = [t for t in tasks if isinstance(t, str)]
    return {"task_ids": task_ids, "seen_artifacts": seen}


def record_task(agent_id: str, task_id: str) -> bool:
    """Bind one submitted task to its agent (never for the primary path)."""
    if not _is_agent(agent_id) or not task_id:
        return False
    bindings = load_bindings()
    entry = _ensure_entry(bindings, agent_id)
    if task_id not in entry["task_ids"]:
        entry["task_ids"].append(task_id)
    bindings[agent_id] = entry
    try:
        _write_json(bindings_path(), {"version": 1, "bindings": bindings})
    except OSError as exc:
        logger.warning("record_task %s: write failed (%s)", agent_id, exc)
        return False
    return True


def record_artifacts(agent_id: str, task_id: str, artifact_ids: list[str]) -> bool:
    """Record the artifacts an agent HAS SEEN via one task's result."""
    if not _is_agent(agent_id) or not task_id:
        return False
    clean = [a for a in (artifact_ids or []) if isinstance(a, str) and a]
    if not clean:
        return False  # nothing seen — no entry written
    bindings = load_bindings()
    entry = _ensure_entry(bindings, agent_id)
    for art_id in clean:
        seen = entry["seen_artifacts"].setdefault(art_id, [])
        if not isinstance(seen, list):
            seen = []
            entry["seen_artifacts"][art_id] = seen
        if task_id not in seen:
            seen.append(task_id)
    bindings[agent_id] = entry
    try:
        _write_json(bindings_path(), {"version": 1, "bindings": bindings})
    except OSError as exc:
        logger.warning("record_artifacts %s: write failed (%s)", agent_id, exc)
        return False
    return True


def tasks_for(agent_id: str) -> list[str]:
    """Tasks bound to one agent (primary keeps no binding view)."""
    if not _is_agent(agent_id):
        return []
    entry = load_bindings().get(agent_id, {})
    task_ids = entry.get("task_ids") if isinstance(entry, dict) else []
    return [t for t in (task_ids or []) if isinstance(t, str)]


def can_read_task(agent_id: str, task_id: str) -> bool:
    """Primary → full access (legacy semantics); agents → own tasks only."""
    if not _is_agent(agent_id):
        return True  # primary/legacy — IOWAP_MCP_BEARER sees everything
    if not task_id:
        return False
    return task_id in tasks_for(agent_id)


def can_read_artifact(agent_id: str, artifact_id: str) -> bool:
    """Primary → full access; agents → artifacts seen via their own results."""
    if not _is_agent(agent_id):
        return True
    if not artifact_id:
        return False
    entry = load_bindings().get(agent_id, {})
    seen = entry.get("seen_artifacts") if isinstance(entry, dict) else {}
    return isinstance(seen, dict) and artifact_id in seen


# ---------------------------------------------------------------------------
# Identity (the P1b context-scoped flow) — proven per-call in the tests
# ---------------------------------------------------------------------------


def _is_agent(agent_id: str) -> bool:
    """True for actual agent ids; ``primary`` or empty mean the operator path."""
    return bool(agent_id) and agent_id != PRIMARY_SUBJECT


def current_access_token() -> Any | None:
    """The SDK access token of the live HTTP request, or ``None`` outside one.

    SDK-native path (mcp 2.3.0): the Bearer middleware stores the resolved
    ``AccessToken`` in an auth contextvar; tool impls read it right here.
    """
    try:
        from mcp.server.auth.middleware.auth_context import get_access_token
    except ImportError:  # the mcp SDK is a hard dep, but stay import-safe
        return None
    try:
        return get_access_token()
    except Exception:  # noqa: BLE001 — outside a request the var is unset
        return None


def current_identity() -> str:
    """``agent_id | "primary"`` for the calling context.

    The subject is server-minted (set by :class:`server.MultiBearerVerifier`
    on VERIFIED tokens only) — a caller cannot forge it. Without a live
    request token (tests, bare function calls) → ``"primary"``.
    """
    token = current_access_token()
    if token is None:
        return PRIMARY_SUBJECT
    subject = getattr(token, "subject", None)
    if isinstance(subject, str) and subject:
        return subject
    return PRIMARY_SUBJECT
