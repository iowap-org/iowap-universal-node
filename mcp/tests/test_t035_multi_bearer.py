"""T-035: multi-bearer auth + token-bound scoping on the node-HTTP surface.

The auth-Matrix contract (each row proven END-TO-END over a live Streamable
HTTP server against a store fixture):

* primary env token      → authenticated, identity ``primary`` (full access);
* valid agent token      → authenticated, subject = agent id, ``last_used``
  touched on the call;
* revoked agent token    → 401 (revoked == unknown, never resurrected);
* unknown agent token    → 401;
* empty/absent store     → agent tokens 401, primary keeps working (legacy
  containers / fresh boot: primary-only mode);
* no bearer at all       → 401 (fail-closed unchanged).

Identity-flow proof: the server-minted subject rides the SDK auth context
into the tool impls — submit binds the task to the agent, a foreign/
unbound ``task_result`` returns the EXACT legacy unknown-task shape, a
bound result records the artifacts it carries, ``artifact_download``/
``artifact_link`` admit only SEEEN artifacts, and the T-042
``capability_*`` tools enforce ``caps_manage`` two-sided (agent without
flag → 'restricted for this token' error shape; primary always passes).
tools/list stays exactly 14 (names unchanged; schemas identical).
"""

from __future__ import annotations

import asyncio
import hashlib
import json
import threading
import time
import urllib.error
import urllib.request
from contextlib import contextmanager
from pathlib import Path
from typing import Any

import pytest

from iowap_node_mcp import agent_tokens, binding, tools
from iowap_node_mcp.server import build_http_app

INITIALIZE = {
    "jsonrpc": "2.0",
    "id": 1,
    "method": "initialize",
    "params": {
        "protocolVersion": "2025-06-18",
        "capabilities": {},
        "clientInfo": {"name": "t035-test", "version": "0"},
    },
}
INITIALIZED_NOTIFICATION = {"jsonrpc": "2.0", "method": "notifications/initialized"}

AGENT_ID = "agt-1a2b3c4d"
OTHER_ID = "agt-feedbeef"
PRIMARY = "iw_mcp_primary_shared_token"
AGENT_TOKEN = "iw_agt_agent_x_token"
OTHER_TOKEN = "iw_agt_other_agent_token"


def _sha(raw: str) -> str:
    return hashlib.sha256(raw.encode()).hexdigest()


def _agents_store(caps_manage: bool = False) -> dict[str, Any]:
    return {
        "version": 1,
        "agents": [
            {
                "id": AGENT_ID, "name": "agent-x", "token_sha256": _sha(AGENT_TOKEN),
                "created_at": "2026-11-25T00:00:00+00:00", "last_used": None,
                "caps_manage": caps_manage, "revoked": False,
            },
            {
                "id": OTHER_ID, "name": "other-y", "token_sha256": _sha(OTHER_TOKEN),
                "created_at": "2026-11-25T00:00:00+00:00", "last_used": None,
                "caps_manage": False, "revoked": True,
            },
        ],
    }


# ---------------------------------------------------------------------------
# HTTP helpers (mirror test_t004_http — self-contained, no import coupling)
# ---------------------------------------------------------------------------


def _post(url, body, *, token=None, session=None):
    data = json.dumps(body).encode()
    headers = {"Content-Type": "application/json",
               "Accept": "application/json, text/event-stream"}
    if token is not None:
        headers["Authorization"] = f"Bearer {token}"
    if session is not None:
        headers["Mcp-Session-Id"] = session
    req = urllib.request.Request(url, data=data, headers=headers, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return resp.status, resp.read().decode()
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read().decode()


def _post_session(base_url, body, *, token, session):
    """Like _post but reads the Mcp-Session-Id response header on 200."""
    data = json.dumps(body).encode()
    headers = {"Content-Type": "application/json",
               "Accept": "application/json, text/event-stream",
               "Authorization": f"Bearer {token}"}
    if session is not None:
        headers["Mcp-Session-Id"] = session
    req = urllib.request.Request(f"{base_url}/mcp", data=data, headers=headers, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return resp.status, resp.headers.get("Mcp-Session-Id"), resp.read().decode()
    except urllib.error.HTTPError as exc:
        return exc.code, None, exc.read().decode()


@contextmanager
def _http_server(app):
    import uvicorn

    config = uvicorn.Config(app, host="127.0.0.1", port=0, log_level="warning")
    server = uvicorn.Server(config)
    thread = threading.Thread(target=server.run, daemon=True)
    thread.start()
    try:
        deadline = time.monotonic() + 15
        while not server.started:
            if time.monotonic() > deadline:
                raise TimeoutError("uvicorn did not start")
            time.sleep(0.05)
        port = server.servers[0].sockets[0].getsockname()[1]
        yield f"http://127.0.0.1:{port}"
    finally:
        server.should_exit = True
        thread.join(timeout=10)


def _open_session(base_url, token):
    status, session, body = _post_session(base_url, INITIALIZE, token=token, session=None)
    assert status == 200, (status, body)
    assert session, body
    status, _, _ = _post_session(
        base_url, INITIALIZED_NOTIFICATION, token=token, session=session
    )
    assert status == 202, (status, body)
    return f"{base_url}/mcp", session


def _call(url, session, token, name, args, req_id=42):
    status, body = _post(
        url,
        {"jsonrpc": "2.0", "id": req_id, "method": "tools/call",
         "params": {"name": name, "arguments": args}},
        token=token,
        session=session,
    )
    assert status == 200, (status, body)
    result = json.loads(body)["result"]
    text = result["content"][0]["text"]
    return result, json.loads(text)


# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------


class _FakeCli:
    """run_node_cli stub shared across the server process (records argv)."""

    def __init__(self, payloads: dict[str, Any] | None = None):
        self.payloads = payloads or {}
        self.calls: list[list[str]] = []

    async def __call__(self, args, timeout=90.0, ok=(0, 1), home=None):
        self.calls.append(list(args))
        return dict(self.payloads.get(
            tuple(args[:2]), self.payloads.get("default", {"ok": True})))


@pytest.fixture()
def store(tmp_path, monkeypatch):
    """agents.json + bindings.json under a tmp agents dir (env override)."""
    d = tmp_path / "agents-store"
    d.mkdir()
    monkeypatch.setenv("IOWAP_AGENTS_DIR", str(d))
    monkeypatch.delenv("IOWAP_MCP_BEARER", raising=False)
    (d / "agents.json").write_text(json.dumps(_agents_store()))
    return d


@pytest.fixture()
def stub_cli(store, monkeypatch):
    """Stub the client library's runner (imported where the tools use it)."""
    from iowap_client_mcp import tools as client_tools

    fake = _FakeCli({
        ("task", "submit"): {"task_id": "TASK-1", "status": "queued"},
        ("task", "result"): {
            "task": {"status": "completed", "claimed_by": "N1", "owner_node_id": "N1"},
            "stages": [
                {"stage_id": "s1", "status": "completed",
                 "result": {"status": "completed",
                            "result": {"artifacts": [
                                {"artifact_id": "art-1", "name": "out.png"},
                                {"artifact_id": "art-2", "name": "log.txt"},
                            ]}}},
            ],
        },
        ("artifact", "download"): {"ok": True, "written": True},
        ("artifact", "link"): {"url": "http://relay/dl", "expires_at": "x",
                               "max_downloads": 1},
    })
    monkeypatch.setattr(client_tools, "run_node_cli", fake)
    return fake


# ---------------------------------------------------------------------------
# Auth matrix (end-to-end over live HTTP)
# ---------------------------------------------------------------------------

def test_auth_matrix_primary_and_agent_and_missing(store) -> None:
    app = build_http_app(PRIMARY)
    with _http_server(app) as base_url:
        # primary bearer: unchanged legacy behavior
        url, session = _open_session(base_url, PRIMARY)
        assert _post(url, {"jsonrpc": "2.0", "id": 9, "method": "tools/list"},
                     token=PRIMARY, session=session)[0] == 200
    # a valid agent token opens its own session
    app2 = build_http_app(PRIMARY)
    with _http_server(app2) as base_url:
        _open_session(base_url, AGENT_TOKEN)


def test_auth_matrix_revoked_and_unknown_are_401(store) -> None:
    app = build_http_app(PRIMARY)
    with _http_server(app) as base_url:
        for bad in (OTHER_TOKEN, "iw_agt_totally_unknown", "not-even-a-token"):
            status, _, _ = _post_session(
                base_url, INITIALIZE, token=bad, session=None
            )
            assert status == 401, bad
        # no bearer at all — the fail-closed default
        req = urllib.request.Request(f"{base_url}/mcp", data=json.dumps(INITIALIZE).encode(),
                                     headers={"Content-Type": "application/json",
                                              "Accept": "application/json, text/event-stream"})
        with pytest.raises(urllib.error.HTTPError) as excinfo:
            urllib.request.urlopen(req, timeout=10)
        assert excinfo.value.code == 401


def test_auth_matrix_empty_store_is_primary_only_legacy(tmp_path, monkeypatch) -> None:
    """Missing agents.json → primary-only mode (legacy containers, fresh boot)."""
    d = tmp_path / "empty-store"
    d.mkdir()
    monkeypatch.setenv("IOWAP_AGENTS_DIR", str(d))
    app = build_http_app(PRIMARY)
    with _http_server(app) as base_url:
        status, _, _ = _post_session(base_url, INITIALIZE, token=AGENT_TOKEN, session=None)
        assert status == 401
        status, _, body = _post_session(base_url, INITIALIZE, token=PRIMARY, session=None)
        assert status == 200, body


def test_auth_matrix_corrupt_store_is_primary_only(tmp_path, monkeypatch) -> None:
    d = tmp_path / "corrupt-store"
    d.mkdir()
    monkeypatch.setenv("IOWAP_AGENTS_DIR", str(d))
    (d / "agents.json").write_text("{corrupt json")
    app = build_http_app(PRIMARY)
    with _http_server(app) as base_url:
        assert _post_session(base_url, INITIALIZE, token=AGENT_TOKEN, session=None)[0] == 401
        assert _post_session(base_url, INITIALIZE, token=PRIMARY, session=None)[0] == 200


def test_agent_call_touches_last_used(store) -> None:
    app = build_http_app(PRIMARY)
    with _http_server(app) as base_url:
        _open_session(base_url, AGENT_TOKEN)
    entry = next(a for a in agent_tokens.load_agents() if a["id"] == AGENT_ID)
    assert entry["last_used"] is not None


# ---------------------------------------------------------------------------
# tools/list = 14 (names + schemas unchanged)
# ---------------------------------------------------------------------------


def test_tools_list_still_14_with_identical_task_schemas(store, stub_cli) -> None:
    from iowap_client_mcp.tools import ALL_TOOLS

    app = build_http_app(PRIMARY)
    with _http_server(app) as base_url:
        url, session = _open_session(base_url, PRIMARY)
        status, body = _post(url, {"jsonrpc": "2.0", "id": 2, "method": "tools/list"},
                             token=PRIMARY, session=session)
        assert status == 200
        listed = {t["name"]: t for t in json.loads(body)["result"]["tools"]}
        assert set(listed) == set(ALL_TOOLS) | set(tools.MANAGE_TOOLS)
        assert len(listed) == 14
        # the wrapped four keep the library's schemas (param sets identical)
        assert {"capability", "payload"} <= set(
            listed["task_submit"]["inputSchema"]["properties"])
        assert {"task_id"} == set(listed["task_result"]["inputSchema"]["properties"])


# ---------------------------------------------------------------------------
# Identity flow: submit → binding → result → artifact sight
# ---------------------------------------------------------------------------


def test_submit_binds_task_for_agent_not_for_primary(store, stub_cli) -> None:
    app = build_http_app(PRIMARY)
    with _http_server(app) as base_url:
        url, session = _open_session(base_url, AGENT_TOKEN)
        result, payload = _call(url, session, AGENT_TOKEN, "task_submit",
                                {"capability": "some.cap", "payload": {}})
        assert payload["task_id"] == "TASK-1"
        assert agent_tokens.tasks_for(AGENT_ID) == ["TASK-1"]

        # the primary path records nothing (legacy full-access semantics)
        url, session = _open_session(base_url, PRIMARY)
        _call(url, session, PRIMARY, "task_submit",
              {"capability": "some.cap", "payload": {}}, req_id=43)
        assert agent_tokens.tasks_for(AGENT_ID) == ["TASK-1"]
        assert "primary" not in agent_tokens.load_bindings()


def test_failed_submit_binds_nothing(store, monkeypatch) -> None:
    from iowap_client_mcp import tools as client_tools
    from iowap_client_mcp.nodecli import NodeCliError

    async def boom(args, timeout=90.0, ok=(0, 1), home=None):
        raise NodeCliError(
            "node-cli exit 1 (task submit --stage x:y): HTTP 400: bad payload")

    monkeypatch.setattr(client_tools, "run_node_cli", boom)
    app = build_http_app(PRIMARY)
    with _http_server(app) as base_url:
        url, session = _open_session(base_url, AGENT_TOKEN)
        _, payload = _call(url, session, AGENT_TOKEN, "task_submit",
                           {"capability": "x.y", "payload": {}})
        assert "error" in payload
    assert agent_tokens.tasks_for(AGENT_ID) == []


def test_foreign_task_result_is_not_found_shape(store, stub_cli) -> None:
    """An unbound task_id must be indistinguishable from an unknown task."""
    app = build_http_app(PRIMARY)
    with _http_server(app) as base_url:
        url, session = _open_session(base_url, AGENT_TOKEN)
        _, payload = _call(url, session, AGENT_TOKEN, "task_result",
                           {"task_id": "TASK-OTHER-9"})
        assert payload == {
            "task_id": "TASK-OTHER-9",
            "status": "not_found",
            "hint": (
                "task not readable from this node's identity — since T-227 "
                "admin, the owner node, and the sender (submitted_by) "
                "can read it; submit from the owning/sending node's token"
            ),
        }


def test_bound_task_result_records_artifacts_seen(store, stub_cli) -> None:
    app = build_http_app(PRIMARY)
    with _http_server(app) as base_url:
        url, session = _open_session(base_url, AGENT_TOKEN)
        # bind first via the wrapped submit (record_task)
        _call(url, session, AGENT_TOKEN, "task_submit",
              {"capability": "some.cap", "payload": {}})
        _, payload = _call(url, session, AGENT_TOKEN, "task_result",
                           {"task_id": "TASK-1"})
        assert payload["task_id"] == "TASK-1"
        assert payload["result"]["artifacts"][0]["artifact_id"] == "art-1"
        assert set(binding.extract_artifact_ids(payload)) == {"art-1", "art-2"}
        seen = agent_tokens.load_bindings()[AGENT_ID]["seen_artifacts"]
        assert set(seen) == {"art-1", "art-2"}
        assert seen["art-1"] == ["TASK-1"]

        # primary sees everything without any binding work
        url, session = _open_session(base_url, PRIMARY)
        _, payload = _call(url, session, PRIMARY, "task_result",
                           {"task_id": "TASK-FOREIGN"}, req_id=44)
        assert "artifacts" in json.dumps(payload)


def test_artifact_download_and_link_admit_only_seen(store, stub_cli) -> None:
    app = build_http_app(PRIMARY)
    with _http_server(app) as base_url:
        url, session = _open_session(base_url, AGENT_TOKEN)
        _call(url, session, AGENT_TOKEN, "task_submit",
              {"capability": "some.cap", "payload": {}})
        _call(url, session, AGENT_TOKEN, "task_result", {"task_id": "TASK-1"})

        _, seen = _call(url, session, AGENT_TOKEN, "artifact_download",
                        {"artifact_id": "art-1", "output_path": "/tmp/never-written-real"})
        assert "error" not in seen
        _, seen_link = _call(url, session, AGENT_TOKEN, "artifact_link",
                             {"artifact_id": "art-2"})
        assert seen_link.get("url") == "http://relay/dl"

        _, foreign = _call(url, session, AGENT_TOKEN, "artifact_download",
                           {"artifact_id": "art-never-seen",
                            "output_path": "/tmp/never-written-real"})
        assert foreign == {
            "error": {
                "http_status": 404,
                "detail": "artifact 'art-never-seen' not found",
                "raw": ("artifact art-never-seen not found "
                        "(or not in this token's binding view)"),
            }
        }
        _, foreign_link = _call(url, session, AGENT_TOKEN, "artifact_link",
                                {"artifact_id": "art-never-seen"})
        assert foreign_link["error"]["http_status"] == 404

        # primary is never gated
        url, session = _open_session(base_url, PRIMARY)
        status, body = _post(url, {"jsonrpc": "2.0", "id": 77, "method": "tools/call",
                                   "params": {"name": "artifact_link",
                                              "arguments": {"artifact_id": "art-anything"}}},
                             token=PRIMARY, session=session)
        assert status == 200
        assert "error" not in json.loads(json.loads(body)["result"]["content"][0]["text"])


# ---------------------------------------------------------------------------
# caps_manage gate (T-042 tools, two-sided)
# ---------------------------------------------------------------------------


def _as_agent(identity: str):
    """Set the SDK auth contextvar for a direct (non-HTTP) unit call."""
    from contextlib import contextmanager

    from mcp.server.auth.middleware.auth_context import auth_context_var
    from mcp.server.auth.middleware.bearer_auth import AuthenticatedUser
    from mcp.server.auth.provider import AccessToken

    @contextmanager
    def _ctx():
        user = AuthenticatedUser(AccessToken(
            token="raw", client_id="c", scopes=[], expires_at=None, subject=identity))
        token = auth_context_var.set(user)
        try:
            yield
        finally:
            auth_context_var.reset(token)

    return _ctx


def _run(coro):
    return asyncio.run(coro)


def test_caps_manage_gate_denies_agent_without_flag(store, monkeypatch) -> None:
    fake = _FakeCli({"default": {"ok": True}})
    monkeypatch.setattr(tools, "run_node_cli", fake)
    with _as_agent(AGENT_ID)():
        result = _run(tools.capability_create({"name": "x.cap", "type": "process"}))
    assert result["error"]["detail"].startswith("capability capability_create is restricted")
    assert result["error"]["raw"] == "restricted for this token (caps_manage off)"
    assert result["error"]["http_status"] is None
    assert fake.calls == []  # nothing ever spawned


def test_caps_manage_gate_allows_agent_with_flag(store, monkeypatch) -> None:
    (store / "agents.json").write_text(
        json.dumps({"version": 1,
                    "agents": [{**_agents_store(caps_manage=True)["agents"][0]}]}))
    fake = _FakeCli({"default": {"ok": True}})
    monkeypatch.setattr(tools, "run_node_cli", fake)
    with _as_agent(AGENT_ID)():
        result = _run(tools.capability_create({"name": "x.cap", "type": "process"}))
    assert result["ok"] is True


def test_caps_manage_gate_primary_always_passes(store, monkeypatch) -> None:
    fake = _FakeCli({"default": {"ok": True}})
    monkeypatch.setattr(tools, "run_node_cli", fake)
    # no auth context at all (direct call) AND explicit primary subject
    result = _run(tools.capability_create({"name": "x.cap", "type": "process"}))
    assert result["ok"] is True
    with _as_agent(agent_tokens.PRIMARY_SUBJECT)():
        result = _run(tools.capability_publish("x.cap", True))
    assert result["ok"] is True


def test_operator_env_gate_still_takes_precedence(store, monkeypatch) -> None:
    """The csv allowlist keeps raising (T-042 semantics unchanged)."""
    monkeypatch.setenv(tools.NODE_MANAGE_ENV, "capability_publish")
    fake = _FakeCli({"default": {"ok": True}})
    monkeypatch.setattr(tools, "run_node_cli", fake)
    with _as_agent(AGENT_ID)():
        with pytest.raises(ValueError, match=tools.NODE_MANAGE_ENV):
            _run(tools.capability_create({"name": "x", "type": "process"}))
        # publish IS in the allowlist, but the agent still lacks caps_manage
        result = _run(tools.capability_publish("x.cap", True))
    assert result["error"]["raw"] == "restricted for this token (caps_manage off)"


def test_caps_gate_never_breaks_other_tools(store, stub_cli) -> None:
    """A manage-denied agent still gets submit/result (no session abort)."""
    app = build_http_app(PRIMARY)
    with _http_server(app) as base_url:
        url, session = _open_session(base_url, AGENT_TOKEN)
        _, denied = _call(url, session, AGENT_TOKEN, "capability_create",
                          {"fields": {"name": "y.cap", "type": "process"}})
        assert "error" in denied
        _, submitted = _call(url, session, AGENT_TOKEN, "task_submit",
                             {"capability": "some.cap", "payload": {}})
        assert submitted["task_id"] == "TASK-1"
        _, listed = _call(url, session, AGENT_TOKEN, "task_result",
                          {"task_id": "TASK-1"})
        assert listed["task_id"] == "TASK-1"


def test_2mb_cap_and_confirm_untouched(store, monkeypatch) -> None:
    """The T-042 raw guards ride the same functions (primary path)."""
    async def no_spawn(args, timeout=90.0, ok=(0, 1), home=None):  # pragma: no cover
        raise AssertionError("must not spawn")

    monkeypatch.setattr(tools, "run_node_cli", no_spawn)
    with pytest.raises(ValueError, match="confirm"):
        _run(tools.capability_delete("x.cap", confirm=False))
    monkeypatch.setattr(tools, "_MAX_SCRIPT_BYTES", 4)
    with pytest.raises(ValueError, match="script too large"):
        _run(tools.capability_script_upload("big.bat", "xxxxx"))


# ---------------------------------------------------------------------------
# Store/extract unit contracts (no HTTP)
# ---------------------------------------------------------------------------


def test_extract_artifact_ids_shapes(store) -> None:
    assert binding.extract_artifact_ids({}) == []
    assert binding.extract_artifact_ids({"result": {"artifacts": []}}) == []
    assert binding.extract_artifact_ids({
        "stages": [{"result": {"status": "completed",
                               "result": {"artifacts": [{"artifact_id": "a1"}]}}}],
    }) == ["a1"]
    # dedupe + task-level echo
    assert binding.extract_artifact_ids({
        "artifacts": [{"artifact_id": "a1"}, {"artifact_id": "a2"}],
        "result": {"artifacts": [{"artifact_id": "a1"}]},
    }) == ["a1", "a2"]


def test_binding_gates_fail_closed(store) -> None:
    assert agent_tokens.can_read_task(AGENT_ID, "") is False  # empty task_id
    assert agent_tokens.can_read_task(AGENT_ID, "unbound-task") is False
    assert agent_tokens.can_read_artifact(AGENT_ID, "never-seen") is False
    # primary: full access both sides, zero binding entries
    assert agent_tokens.can_read_task(agent_tokens.PRIMARY_SUBJECT, "anything")
    assert agent_tokens.can_read_artifact(agent_tokens.PRIMARY_SUBJECT, "anything")
    assert agent_tokens.record_task(agent_tokens.PRIMARY_SUBJECT, "x") is False
    assert agent_tokens.record_artifacts(agent_tokens.PRIMARY_SUBJECT, "x", ["a"]) is False


def test_current_identity_defaults_to_primary() -> None:
    """Outside a live request the identity is primary (legacy semantics)."""
    from mcp.server.auth.middleware.auth_context import auth_context_var

    if auth_context_var.get() is not None:  # pragma: no cover — parallel pollution
        auth_context_var.set(None)
    assert agent_tokens.current_identity() == agent_tokens.PRIMARY_SUBJECT


def test_store_schema_shares_the_node_contracts(store) -> None:
    """The JSON contracts the MCP process reads == the dashboard writes."""
    data = json.loads((store / "agents.json").read_text())
    assert set(data["agents"][0]) == {
        "id", "name", "token_sha256", "created_at", "last_used",
        "caps_manage", "revoked",
    }
    agent_tokens.record_task(AGENT_ID, "tt-1")
    agent_tokens.record_artifacts(AGENT_ID, "tt-1", ["aa-1"])
    store_obj = json.loads((store / "bindings.json").read_text())
    assert set(store_obj) == {"version", "bindings"}
    assert set(store_obj["bindings"][AGENT_ID]) == {"task_ids", "seen_artifacts"}
    assert store_obj["bindings"][AGENT_ID]["task_ids"] == ["tt-1"]


# ---------------------------------------------------------------------------
# The SAME example files as the node repo (byte-identical pinned copies in
# iowap-node/tests/fixtures/agent_tokens/) — both loaders must read the exact
# same content; any schema drift breaks one of the two suites.
# ---------------------------------------------------------------------------

_AGENTS_EXAMPLE_SHA256 = "b920cc5b5d0bc22ed419760a5127ac70d99011988e3c506031cd32428a5c4e72"
_BINDINGS_EXAMPLE_SHA256 = "5a90e3a5a155f432ecba9b3a5613bafad4aa77dd5007c964bab8898fc30e73c8"

import hashlib as _hashlib  # local to the contract test block


def test_shared_example_files_roundtrip(tmp_path, monkeypatch) -> None:
    fixtures = Path(__file__).resolve().parent / "fixtures" / "agent_tokens"

    # byte-identity with the node-package copies is the drift guard
    assert (_hashlib.sha256((fixtures / "agents.json").read_bytes()).hexdigest()
            == _AGENTS_EXAMPLE_SHA256)
    assert (_hashlib.sha256((fixtures / "bindings.json").read_bytes()).hexdigest()
            == _BINDINGS_EXAMPLE_SHA256)

    monkeypatch.setenv("IOWAP_AGENTS_DIR", str(fixtures))
    # verification against the shared example (same tokens as the node suite)
    assert agent_tokens.verify("iw_agt_raw_of_old_scoped_agent") is None  # revoked
    entry = agent_tokens.verify("iw_agt_raw_of_ops_robot")
    assert entry is not None and entry["name"] == "ops-robot"
    assert agent_tokens.caps_manage_for("agt-5678abcd") is True
    assert agent_tokens.caps_manage_for("agt-00001234") is False

    # binding view: art-101 seen via TASK-1A + TASK-2B in the example
    assert agent_tokens.can_read_artifact("agt-00001234", "art-101")
    assert not agent_tokens.can_read_artifact("agt-00001234", "art-999")
    assert agent_tokens.tasks_for("agt-00001234") == ["TASK-1A", "TASK-2B"]
    assert agent_tokens.can_read_task("primary", "TASK-3C")  # primary full access
