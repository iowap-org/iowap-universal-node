"""T-037 split contracts specific to iowap-node-mcp 0.1.0."""

from __future__ import annotations

import asyncio

from iowap_node_mcp.server import BEARER_ENV, main


def test_bare_start_defaults_to_http(monkeypatch, capsys) -> None:
    """Bare invocation serves HTTP too — the package's only transport.

    The container entrypoint passes --http explicitly, but the default
    must not silently do something else (e.g. stdio, the client package's
    transport). Proven through the fail-closed gate: no Bearer env set →
    exit 1 with the Bearer-env error, identical to --http.
    """
    monkeypatch.delenv(BEARER_ENV, raising=False)
    assert main([]) == 1
    err = capsys.readouterr().err
    assert BEARER_ENV in err
    assert "refusing" in err.lower() or "fail" in err.lower()


def test_toolset_is_the_client_library_not_a_copy() -> None:
    """T-037 invariant: the HTTP surface wires the client package's toolset.

    The server must be built through iowap-client-mcp's build_server
    (same MCPServer builder the stdio server uses) — no vendored copy.
    T-042: the bare client-library surface stays the curated 9 tools;
    the node-HTTP surface is that toolset PLUS the 5 capability-manage
    tools (iowap_node_mcp.tools, node-cli driven) → 14 on this package.
    """
    import iowap_client_mcp.mcp_server as client_mod
    import iowap_client_mcp.tools as client_tools_mod

    import iowap_node_mcp.server as server_mod
    from iowap_node_mcp.tools import MANAGE_TOOLS, register_manage_tools

    assert server_mod.build_server is client_mod.build_server

    names = sorted(tool.name for tool in asyncio.run(client_mod.build_server().list_tools()))
    assert set(names) == set(client_tools_mod.ALL_TOOLS)
    assert len(names) == 9

    surface = client_mod.build_server()
    register_manage_tools(surface)
    surface_names = sorted(tool.name for tool in asyncio.run(surface.list_tools()))
    assert set(surface_names) == set(client_tools_mod.ALL_TOOLS) | set(MANAGE_TOOLS)
    assert len(surface_names) == 14


def test_build_http_app_attaches_the_manage_toolset(monkeypatch) -> None:
    """T-042: the HTTP app carries 14 tools — 9 curated + 5 capability-manage.

    build_http_app() builds the client toolset AND attaches the node-only
    capability-manage set before streaming it; the Bearer verifier rides in
    (fail-closed wiring unchanged).
    """
    from iowap_client_mcp.mcp_server import build_server as client_build

    from iowap_node_mcp import server as server_mod
    from iowap_node_mcp.tools import MANAGE_TOOLS, register_manage_tools

    real = client_build()
    captured: dict[str, object] = {}
    seen_verifier: list[object] = []

    def fake_build(token_verifier=None, auth=None):
        seen_verifier.append(token_verifier)
        return real

    def spy_app(host: str, json_response: bool):
        captured["host"] = host
        captured["json_response"] = json_response
        return ("spy-app", host, json_response)

    monkeypatch.setattr(server_mod, "build_server", fake_build)
    monkeypatch.setattr(real, "streamable_http_app", spy_app)
    app = server_mod.build_http_app("some-bearer-token", host="1.2.3.4", port=99)

    assert app == ("spy-app", "1.2.3.4", True)
    assert captured == {"host": "1.2.3.4", "json_response": True}
    assert len(seen_verifier) == 1 and seen_verifier[0] is not None
    names = sorted(tool.name for tool in asyncio.run(real.list_tools()))
    assert len(names) == 14
    assert set(MANAGE_TOOLS).issubset(names)
    assert register_manage_tools.__module__ == "iowap_node_mcp.tools"
