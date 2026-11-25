"""Smoke tests for the iowap-node-mcp package (T-037)."""

from __future__ import annotations

import importlib
import tomllib
from importlib import metadata
from pathlib import Path

import iowap_node_mcp
from iowap_node_mcp.server import PACKAGE, build_http_app, main

REPO_ROOT = Path(__file__).resolve().parents[1]


def _mcp_unavailable(module_name: str, *args, **kwargs):
    if module_name == "mcp" or module_name.startswith("mcp."):
        raise ImportError(f"No module named '{module_name}'", name=module_name)
    return importlib.import_module(module_name)


def test_version_consistency() -> None:
    pyproject = tomllib.loads((REPO_ROOT / "pyproject.toml").read_text())
    declared = pyproject["project"]["version"]
    assert iowap_node_mcp.__version__ == declared
    assert metadata.version("iowap-node-mcp") == declared


def test_main_version_flag_exits_zero(capsys) -> None:
    assert main(["--version"]) == 0
    assert capsys.readouterr().out.strip() == iowap_node_mcp.__version__


def test_entry_point_constant_matches_package() -> None:
    assert PACKAGE == "iowap-node-mcp"


def test_build_server_via_client_library() -> None:
    """build_server comes from iowap-client-mcp (T-037 library import)."""
    import iowap_client_mcp.mcp_server as client_mod

    assert build_http_app.__globals__["build_server"] is client_mod.build_server
    server = client_mod.build_server()
    from mcp.server.mcpserver import MCPServer

    assert isinstance(server, MCPServer)
    assert server.name == "iowap"
