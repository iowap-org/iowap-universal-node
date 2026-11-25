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
    """
    import iowap_client_mcp.mcp_server as client_mod

    import iowap_node_mcp.server as server_mod

    assert server_mod.build_server is client_mod.build_server

    from iowap_client_mcp.tools import ALL_TOOLS

    names = sorted(tool.name for tool in asyncio.run(client_mod.build_server().list_tools()))
    assert set(names) == set(ALL_TOOLS)
    assert len(names) == 9
