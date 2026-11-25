"""IOWAP node MCP server — the container-HTTP surface (T-037).

This package serves the Streamable-HTTP MCP endpoint that runs inside the
iowap node container: ``POST /mcp`` (SDK-native ``streamable_http_app`` from
the mcp package — no hand-rolled ASGI) behind a static operator Bearer
token from ``IOWAP_MCP_BEARER``. The process refuses to start without that
token (fail closed): it never serves an unauthenticated MCP endpoint.

The toolset is NOT duplicated here (T-037 split): it is imported as a
library from ``iowap-client-mcp``, so both surfaces — the local-agent
stdio server (``iowap-mcp``, package iowap-client-mcp) and this
container-HTTP server — share exactly the same curated 9-tool set,
envelope semantics and guards. Local agents that live on the same host as
the node use the stdio package directly and do not need this server.

Server name is kept as ``iowap`` so MCP clients see the same identity
either way.
"""

from __future__ import annotations

import argparse
import hmac
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


class StaticBearerVerifier:
    """Exact-match verifier for the operator-set shared Bearer token.

    Implements the SDK ``TokenVerifier`` protocol (duck-typed — the mcp SDK
    only requires ``async verify_token``). Uses constant-time comparison and
    grants no scopes/resources; the token is a shared secret, not an identity.
    """

    def __init__(self, token: str) -> None:
        self._token = token.encode()

    async def verify_token(self, token: str) -> Any | None:
        from mcp.server.auth.provider import AccessToken

        if hmac.compare_digest(token.encode(), self._token):
            return AccessToken(
                token=token,
                client_id="iowap-node-mcp-bearer",
                scopes=[],
                expires_at=None,
                subject="operator-bearer",
            )
        return None


def build_http_app(
    bearer_token: str, *, host: str = DEFAULT_HTTP_HOST, port: int = DEFAULT_HTTP_PORT
) -> Any:
    """Return the SDK-native Streamable-HTTP Starlette app (endpoint /mcp).

    Wires the static Bearer verifier through the SDK's own auth middleware
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
    server = build_server(token_verifier=StaticBearerVerifier(bearer_token), auth=auth)
    if server is None:
        raise RuntimeError(
            "the 'mcp' package is not installed — "
            "install with: pip install 'iowap-node-mcp'"
        )
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
