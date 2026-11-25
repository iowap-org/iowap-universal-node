"""T-042: capability-manage toolset for iowap-node-mcp (5 node-HTTP surface tools).

Contracts proven:

* every tool drives the node CLI via ``run_node_cli`` (the one runner) with
  the T-042 subcommands; the argv shape is asserted per tool;
* CLI/runner failures come back as the T-274 error shape
  ``{'error': {'http_status', 'detail', 'raw'}}`` (client library helper,
  no copy) instead of raising;
* ``capability_delete`` without ``confirm=true`` raises BEFORE any spawn;
* the ``IOWAP_ALLOW_CAPS_MANAGE`` operator gate (csv; unset = all allowed)
  rejects gated tools before any spawn;
* ``capability_script_upload`` enforces the 2 MB cap BEFORE writing bytes,
  stages content through a temp file (``--file``), and passes the target
  filename as ``--name``;
* ``register_manage_tools`` attaches exactly MANAGE_TOOLS (the surface
  contract, 9 curated + 5 manage = 14, lives in test_t037_split).
"""

from __future__ import annotations

import asyncio
import json
from pathlib import Path
from typing import Any

import pytest

from iowap_client_mcp.nodecli import NodeCliError
from iowap_client_mcp.tools import _stderr_tail

from iowap_node_mcp import tools
from iowap_node_mcp.tools import (
    MANAGE_TOOLS,
    capability_create,
    capability_delete,
    capability_publish,
    capability_script_upload,
    capability_update,
    register_manage_tools,
)


class _FakeCli:
    """run_node_cli stub: records argv, returns payload or raises."""

    def __init__(self, payload: dict[str, Any] | None = None, exc: Exception | None = None):
        self.payload = payload or {}
        self.exc = exc
        self.calls: list[list[str]] = []

    async def __call__(self, args: list[str], timeout: float = 90.0, ok=(0, 1), home=None):
        self.calls.append(list(args))
        if self.exc is not None:
            raise self.exc
        return dict(self.payload)


@pytest.fixture()
def cli_ok(monkeypatch):
    fake = _FakeCli({"ok": True, "name": "x.cap", "version": "1.0.0", "published": False})
    monkeypatch.setattr(tools, "run_node_cli", fake)
    return fake


@pytest.fixture()
def cli_fail(monkeypatch):
    """A runner failure whose stderr tail carries a relay HTTP line (T-274)."""
    fake = _FakeCli(exc=NodeCliError(
        "node-cli exit 1 (capabilities create --name x.cap): "
        "capability create failed: HTTP 422: capability 'x.cap' already exists — "
        "delete it first"
    ))
    monkeypatch.setattr(tools, "run_node_cli", fake)
    return fake


def _run(coro):  # noqa: ANN001 — tiny asyncio helper for the sync pytest bodies
    return asyncio.run(coro)


# ── capability_create ───────────────────────────────────────────────────────


def test_create_maps_fields_to_cli_argv(cli_ok: _FakeCli) -> None:
    result = _run(capability_create({
        "name": "sys.info",
        "type": "process",
        "command": "cmd /c sysinfo.bat",
        "args": ["--plain", "x"],
        "max_parallel": 2,
        "cost": "3",
        "artifacts_glob": "out/*.txt",
        "version": "1.2.3",
        "publish": True,
    }))

    assert result == {"ok": True, "name": "x.cap", "version": "1.0.0", "published": False}
    assert cli_ok.calls[0][0:6] == [
        "capabilities", "create", "--name", "sys.info",
        "--type", "process",
    ]
    argv = cli_ok.calls[0]
    assert argv[argv.index("--command") + 1] == "cmd /c sysinfo.bat"
    assert argv[argv.index("--max-parallel") + 1] == "2"
    assert argv[argv.index("--cost") + 1] == "3"
    assert argv[argv.index("--artifacts-glob") + 1] == "out/*.txt"
    assert argv[argv.index("--version") + 1] == "1.2.3"
    # --args is a raw token slice: --args --plain x
    idx = argv.index("--args")
    assert argv[idx + 1:idx + 3] == ["--plain", "x"]
    assert "--publish" in argv


def test_create_requires_dict_and_name() -> None:
    with pytest.raises(TypeError, match="fields must be a JSON object"):
        _run(capability_create(["name", "x.cap"]))  # type: ignore[arg-type]
    with pytest.raises(ValueError, match="name"):
        _run(capability_create({"type": "process"}))


def test_create_rejects_unknown_fields_with_allowed_list() -> None:
    with pytest.raises(ValueError, match="bogus_field"):
        _run(capability_create({"name": "x.cap", "bogus_field": 1}))


def test_create_runner_failure_returns_t274_shape(cli_fail: _FakeCli) -> None:
    result = _run(capability_create({"name": "dup.cap", "type": "process",
                                      "command": "run x"}))

    assert result == {
        "error": {
            "http_status": 422,
            "detail": "capability 'x.cap' already exists — delete it first",
            "raw": ("capability create failed: HTTP 422: "
                    "capability 'x.cap' already exists — delete it first"),
        }
    }
    assert cli_fail.calls[0][0:4] == ["capabilities", "create", "--name", "dup.cap"]


# ── capability_update ───────────────────────────────────────────────────────


def test_update_maps_fields_and_keeps_name_route(cli_ok: _FakeCli) -> None:
    _run(capability_update("sys.info", {"type": "process", "command": "cmd /c run.bat"}))

    argv = cli_ok.calls[0]
    assert argv[:4] == ["capabilities", "update", "--name", "sys.info"]
    assert argv.count("--name") == 1  # --name rides once, the routing key


def test_update_pre_checks_name_immutability_and_empty_fields(cli_ok: _FakeCli) -> None:
    with pytest.raises(ValueError, match="name is immutable"):
        _run(capability_update("a.cap", {"name": "b.cap", "type": "process"}))
    with pytest.raises(ValueError, match="at least one capability field"):
        _run(capability_update("a.cap", {}))
    with pytest.raises(TypeError, match="fields"):
        _run(capability_update("a.cap", "type=process"))  # type: ignore[arg-type]
    assert cli_ok.calls == []  # nothing ever spawned


def test_update_rejects_unknown_fields() -> None:
    with pytest.raises(ValueError, match="cost_x"):
        _run(capability_update("a.cap", {"cost_x": 1}))


def test_update_runner_failure_returns_t274_shape(cli_ok: _FakeCli) -> None:
    cli_ok.exc = NodeCliError(
        "node-cli exit 1 (capabilities update --name a.cap): "
        "update failed: HTTP 404: capability not found"
    )
    result = _run(capability_update("a.cap", {"type": "process"}))

    assert result["error"]["http_status"] == 404
    assert "capability not found" in result["error"]["detail"]


# ── capability_delete (destructive, confirm) ────────────────────────────────


def test_delete_requires_confirm_true_before_spawn(cli_ok: _FakeCli) -> None:
    for bad in (False, None, 1):
        with pytest.raises(ValueError, match="confirm"):
            _run(capability_delete("x.cap", confirm=bad))  # type: ignore[arg-type]
    # note: confirm=1 is falsy-vs-True — the strict `is True` contract refuses it
    assert cli_ok.calls == []


def test_delete_runs_the_cli_when_confirmed(cli_ok: _FakeCli) -> None:
    result = _run(capability_delete("x.cap", confirm=True))

    assert result["ok"] is True
    assert cli_ok.calls == [["capabilities", "delete", "--name", "x.cap"]]


def test_delete_runner_failure_returns_t274_shape(cli_ok: _FakeCli) -> None:
    cli_ok.exc = NodeCliError(
        "node-cli exit 1 (capabilities delete --name x.cap): "
        "delete failed: HTTP 404: unknown capability 'x.cap'"
    )
    result = _run(capability_delete("x.cap", confirm=True))

    assert result["error"]["http_status"] == 404


# ── capability_publish ──────────────────────────────────────────────────────


def test_publish_true_and_false_map_to_the_pure_allowlist_toggle(cli_ok: _FakeCli) -> None:
    _run(capability_publish("x.cap", True))
    _run(capability_publish("x.cap", published=False))

    assert cli_ok.calls == [
        ["capabilities", "update", "--name", "x.cap", "--publish"],
        ["capabilities", "update", "--name", "x.cap", "--unpublish"],
    ]


def test_publish_rejects_non_bool() -> None:
    with pytest.raises(TypeError, match="boolean"):
        _run(capability_publish("x.cap", "true"))  # type: ignore[arg-type]


# ── capability_script_upload ────────────────────────────────────────────────


def test_script_upload_stages_temp_file_and_cleans_it_up(monkeypatch) -> None:
    """The bytes ride a temp file (``--file``) that exists during the run
    and is removed afterwards; the content is the UTF-8 encoding, the
    ``--name`` carries the TARGET filename, not the staged temp name."""
    staged: dict[str, Any] = {}

    async def fake_run(args: list[str], timeout: float = 90.0, ok=(0, 1), home=None):
        staged["argv"] = list(args)
        staged["path"] = Path(args[args.index("--file") + 1])
        assert staged["path"].read_bytes() == "rem script".encode("utf-8")  # existed mid-run
        assert args[args.index("--name") + 1] == "tool.bat"
        return {"ok": True, "stored": "bridge/scripts/tool.bat",
                "stored_name": "tool.bat", "name": "tool",
                "command": ["cmd", "/c", "scripts\\tool.bat"]}

    monkeypatch.setattr(tools, "run_node_cli", fake_run)
    result = _run(capability_script_upload("tool.bat", "rem script"))

    assert result["ok"] is True
    assert result["stored"] == "bridge/scripts/tool.bat"
    assert staged["argv"][0:2] == ["capabilities", "script-upload"]
    assert staged["path"].name.startswith("iowap-cap-script-")
    assert not staged["path"].exists()  # cleaned up after the call


def test_script_upload_enforces_size_cap_before_writing(monkeypatch) -> None:
    spawned: list[list[str]] = []

    async def fake_run(args: list[str], timeout: float = 90.0, ok=(0, 1), home=None):
        spawned.append(list(args))
        return {}

    monkeypatch.setattr(tools, "run_node_cli", fake_run)
    monkeypatch.setattr(tools, "_MAX_SCRIPT_BYTES", 10)
    with pytest.raises(ValueError, match="script too large"):
        _run(capability_script_upload("big.bat", "x" * 11))
    assert spawned == []  # refused BEFORE any spawn/disk write


def test_script_upload_validates_name_and_content_type() -> None:
    with pytest.raises(ValueError, match="bare filename"):
        _run(capability_script_upload("../tool.bat", "x"))
    with pytest.raises(TypeError, match="content"):
        _run(capability_script_upload("tool.bat", 123))  # type: ignore[arg-type]


def test_script_upload_runner_failure_returns_t274_shape(monkeypatch) -> None:
    async def fake_run(args: list[str], timeout: float = 90.0, ok=(0, 1), home=None):
        raise NodeCliError(
            "node-cli exit 1 (capabilities script-upload): "
            "script upload FAILED: extension '.txt' not allowed — one of "
            ".bat, .cmd, .ps1, .py, .sh, .exe"
        )

    monkeypatch.setattr(tools, "run_node_cli", fake_run)
    result = _run(capability_script_upload("tool.txt", "x"))

    assert result["error"] == {
        "http_status": None,
        "detail": ("script upload FAILED: extension '.txt' not allowed — "
                   "one of .bat, .cmd, .ps1, .py, .sh, .exe"),
        "raw": ("script upload FAILED: extension '.txt' not allowed — "
                "one of .bat, .cmd, .ps1, .py, .sh, .exe"),
    }


# ── the IOWAP_ALLOW_CAPS_MANAGE operator gate ───────────────────────────────


_CALLERS = {
    "capability_create": lambda: capability_create({"name": "x.cap", "type": "process"}),
    "capability_update": lambda: capability_update("x.cap", {"type": "process"}),
    "capability_delete": lambda: capability_delete("x.cap", confirm=True),
    "capability_publish": lambda: capability_publish("x.cap", True),
    "capability_script_upload": lambda: capability_script_upload("x.st", "data"),
}


def test_gate_unset_means_all_manage_tools_allowed(monkeypatch) -> None:
    """No env → every manage tool passes the gate (operator default open)."""
    monkeypatch.delenv(tools.NODE_MANAGE_ENV, raising=False)
    monkeypatch.setattr(tools, "run_node_cli", _FakeCli({"ok": True}))
    for caller in _CALLERS.values():
        result = _run(caller())
        assert result.get("ok") is True


def test_gate_csv_selects_the_allowed_tools_only(monkeypatch) -> None:
    fake = _FakeCli({"ok": True})
    monkeypatch.setattr(tools, "run_node_cli", fake)
    monkeypatch.setenv(tools.NODE_MANAGE_ENV, "capability_create, capability_publish")

    result = _run(capability_publish("x.cap", True))
    assert result["ok"] is True

    for gated in ("capability_update", "capability_delete", "capability_script_upload"):
        with pytest.raises(ValueError, match=tools.NODE_MANAGE_ENV):
            _run(_CALLERS[gated]())
    # only the one allowed publish call ever spawned — deletes never reach the CLI
    assert len(fake.calls) == 1


def test_gate_env_update_applies_live(monkeypatch) -> None:
    monkeypatch.setattr(tools, "run_node_cli", _FakeCli({"ok": True}))
    monkeypatch.setenv(tools.NODE_MANAGE_ENV, "capability_create")
    with pytest.raises(ValueError, match="IOWAP_ALLOW_CAPS_MANAGE"):
        _run(capability_publish("x.cap", True))
    monkeypatch.setenv(tools.NODE_MANAGE_ENV, "")  # empty csv = open again
    assert _run(capability_publish("x.cap", True))["ok"] is True


# ── registration / surface contract ─────────────────────────────────────────


def test_register_manage_tools_attaches_exactly_the_manage_set() -> None:
    from iowap_client_mcp.mcp_server import build_server

    server = build_server()
    assert server is not None
    register_manage_tools(server)
    names = sorted(tool.name for tool in asyncio.run(server.list_tools()))
    assert set(MANAGE_TOOLS).issubset(set(names))
    assert len(names) == 14  # 9 curated (client library) + 5 manage (T-042)
    assert len(MANAGE_TOOLS) == 5


def test_json_shapes_are_serializable() -> None:
    """The T-274 shape must ride the MCP envelope as plain JSON."""
    sample = {"error": {"http_status": 422, "detail": "d", "raw": "r"}}
    assert json.loads(json.dumps(sample)) == sample
    tail = _stderr_tail(NodeCliError("node-cli exit 1 (x): boom boom"))
    assert tail == "boom boom"
