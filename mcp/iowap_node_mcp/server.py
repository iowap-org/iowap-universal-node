"""IOWAP node MCP server — the container-HTTP surface (T-037).

This package serves the Streamable-HTTP MCP endpoint that runs inside the
iowap node container: ``POST /mcp`` (SDK-native ``streamable_http_app`` from
the mcp package — no hand-rolled ASGI) behind Bearer auth (T-035
multi-bearer: the operator token from ``IOWAP_MCP_BEARER`` stays
primary/legacy and agent tokens minted by the node dashboard verify
against ``agents.json``). The process refuses to start without the
primary token (fail closed): it never serves an unauthenticated MCP
endpoint.

The toolset is NOT duplicated here (T-037 split): it is imported as a
library from ``iowap-client-mcp``, so both surfaces — the local-agent
stdio server (``iowap-mcp``, package iowap-client-mcp) and this
container-HTTP server — share exactly the same curated 9-tool set,
envelope semantics and guards. Local agents that live on the same host as
the node use the stdio package directly and do not need this server.
T-042 adds the 5 capability-manage tools (node surface only); T-035 adds
token-bound scoping around the four task/artifact tools (same names, same
schemas — tools/list stays 14).

Server name is kept as ``iowap`` so MCP clients see the same identity
either way.
"""

from __future__ import annotations

import argparse
import hmac
import logging
import os
import sys
from importlib import metadata
from typing import Any

from iowap_client_mcp.mcp_server import build_server

PACKAGE = "iowap-node-mcp"
SERVER_NAME = "iowap"
BEARER_ENV = "IOWAP_MCP_BEARER"
PORT_ENV = "IOWAP_MCP_PORT"
DEFAULT_HTTP_PORT = 7781
DEFAULT_HTTP_HOST = "127.0.0.1"


class MultiBearerVerifier:
    """SDK ``TokenVerifier`` over the node's multi-bearer model (T-035 P1b).

    Accepted = the primary env token (exact match, legacy full-access
    semantics unchanged) OR an agent token minted by the node dashboard
    (SHA-256 verify against the store's ``agents.json``, unrevoked only —
    revoked tokens are simply unknown). The resolved identity rides the
    SDK access token's ``subject`` (``"primary" | "agt-<hex>"``) into the
    per-request auth context the tool impls read; a missing/empty/corrupt
    store degrades to primary-only — legacy containers and fresh boots
    keep working, fail-closed stays intact.
    """

    def __init__(self, primary_token: str) -> None:
        self._primary = primary_token.encode()
        from . import agent_tokens as _tokens

        self._tokens = _tokens

    async def verify_token(self, token: str) -> Any | None:
        from mcp.server.auth.provider import AccessToken

        if hmac.compare_digest(token.encode(), self._primary):
            return AccessToken(
                token=token,
                client_id="iowap-node-mcp-bearer",
                scopes=[],
                expires_at=None,
                subject=self._tokens.PRIMARY_SUBJECT,
            )
        entry = self._tokens.verify(token)
        if entry is not None:
            # touch() on every valid agent-token call (T-035 P1b) — best
            # effort, the auth decision is already made (never fail the
            # request over a store write).
            try:
                self._tokens.touch(str(entry["id"]))
            except Exception as exc:  # a failed touch never fails a verified call
                logging.getLogger(__name__).debug(
                    "agent token touch failed for %s: %s", agent_id, exc)
            agent_id = str(entry["id"])
            return AccessToken(
                token=token,
                client_id=f"iowap-agent:{agent_id}",
                scopes=[],
                expires_at=None,
                subject=agent_id,
            )
        return None


# Back-compat name (T-035 renames the verifier; the class is new enough in
# the wild that the old alias costs nothing).
StaticBearerVerifier = MultiBearerVerifier


def build_http_app(
    bearer_token: str, *, host: str = DEFAULT_HTTP_HOST, port: int = DEFAULT_HTTP_PORT
) -> Any:
    """Return the SDK-native Streamable-HTTP Starlette app (endpoint /mcp).

    Wires the multi-bearer verifier through the SDK's own auth middleware
    (``AuthSettings`` + ``TokenVerifier``) — no hand-built ASGI scaffold, so
    401 handling, WWW-Authenticate and session management stay SDK-owned.
    The MCP server instance itself (and with it the curated toolset) is
    built by the iowap-client-mcp library (T-037 split).
    ``validate_token_resource=False`` because the exact-match verifier itself
    is the full audience check for a shared secret.
    """
    from mcp.server.auth.settings import AuthSettings
    from pydantic import AnyHttpUrl

    base_url = AnyHttpUrl(f"http://{host}:{port}")
    auth = AuthSettings(
        issuer_url=base_url,
        resource_server_url=base_url,
        validate_token_resource=False,
    )
    server = build_server(token_verifier=MultiBearerVerifier(bearer_token), auth=auth)
    if server is None:
        raise RuntimeError(
            "the 'mcp' package is not installed — "
            "install with: pip install 'iowap-node-mcp'"
        )
    # T-042: attach the node-only capability-manage toolset (5 tools, see
    # iowap_node_mcp.tools) on top of the client library's curated 9 — the
    # node-HTTP surface is a strict superset of the stdio local-agent surface.
    from iowap_node_mcp.tools import register_manage_tools

    register_manage_tools(server)
    # T-035 P2a: swap the four task/artifact tools for their token-bound
    # versions (same names, same schemas — tools/list stays 14).
    from iowap_node_mcp.binding import install_scoping

    install_scoping(server)
    return server.streamable_http_app(host=host, json_response=True)


def resolve_port(port: int | None) -> int:
    """CLI ``--port`` wins over ``IOWAP_MCP_PORT``; default 7781."""
    if port is not None:
        return port
    raw = os.environ.get(PORT_ENV, "").strip()
    if not raw:
        return DEFAULT_HTTP_PORT
    try:
        return int(raw)
    except ValueError as exc:
        raise ValueError(
            f"{PORT_ENV}={raw!r} is not an integer port number"
        ) from exc


def _run_http(host: str, port: int) -> int:
    """Start the HTTP transport — fail closed without a Bearer token env."""
    bearer = os.environ.get(BEARER_ENV, "").strip()
    if not bearer:
        print(
            f"error: {BEARER_ENV} is not set — refusing to start the HTTP "
            "transport (fail closed: never serve an unauthenticated MCP "
            "endpoint). Set a strong token in the container environment.",
            file=sys.stderr,
        )
        return 1
    try:
        port = resolve_port(port)
        app = build_http_app(bearer, host=host, port=port)
    except (ValueError, RuntimeError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1
    import uvicorn

    uvicorn.run(app, host=host, port=port, log_level="warning")
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="iowap-node-mcp",
        description=(
            "IOWAP node MCP server — Streamable HTTP at /mcp (the container "
            "surface) with Bearer auth from the IOWAP_MCP_BEARER env (fail "
            "closed). The toolset is imported as a library from "
            "iowap-client-mcp (T-037); local agents use the stdio server "
            "iowap-mcp instead."
        ),
    )
    parser.add_argument("--version", action="store_true", help="print version and exit")
    parser.add_argument(
        "--http",
        action="store_true",
        help=(
            "serve Streamable HTTP at /mcp (the only transport of this "
            "package — the flag is accepted, and a bare start serves HTTP "
            "as well, to keep the container entrypoint command stable)"
        ),
    )
    parser.add_argument(
        "--host",
        default=DEFAULT_HTTP_HOST,
        help=f"HTTP bind host (default: {DEFAULT_HTTP_HOST}; use 0.0.0.0 in containers)",
    )
    parser.add_argument(
        "--port",
        type=int,
        default=None,
        help=f"HTTP port (default: {PORT_ENV} env or {DEFAULT_HTTP_PORT})",
    )
    args = parser.parse_args(argv)

    if args.version:
        print(metadata.version(PACKAGE))
        return 0

    # Only transport is HTTP: bare invocations serve too (see --http help).
    return _run_http(args.host, args.port)


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
