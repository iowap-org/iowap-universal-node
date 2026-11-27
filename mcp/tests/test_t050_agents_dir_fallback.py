"""T-050 mirror: the mcp-side store resolver carries the SAME write probe.

The node package's ``nodes/common/agent_tokens.py`` owns the store; this
package re-implements the read side per the T-037 split rule (no import).
T-050 changed BOTH sides' resolver to probe the volume candidate with a
real write (mkdir + tmp write + unlink) — these tests pin the mirror:

* ``IOWAP_AGENTS_DIR`` env override wins unprobed, per call;
* a writable volume dir is chosen (and the probe leaves no litter);
* an unwritable (root-owned, simulated via ``chmod 0o555``) volume falls
  back to ``~/.relay/agents`` with EXACTLY ONE warning (probe-once per
  process — the cache holds, no retry storm);
* a later permission change does not flip the cached resolution.
"""

from __future__ import annotations

import logging
import os

import pytest

from iowap_node_mcp import agent_tokens as at

_NON_ROOT_POSIX = os.name == "posix" and os.geteuid() != 0


@pytest.fixture()
def probe_env(tmp_path, monkeypatch):
    """No volume, no env override, fresh probe cache per test."""
    monkeypatch.delenv("IOWAP_AGENTS_DIR", raising=False)
    monkeypatch.setattr(at, "VOLUME_AGENT_DIR", tmp_path / "definitely-absent")
    monkeypatch.setattr(at, "_AGENTS_DIR_CACHE", None)
    return tmp_path


def test_env_override_wins_unprobed(probe_env, monkeypatch) -> None:
    override = probe_env / "explicit"
    monkeypatch.setenv("IOWAP_AGENTS_DIR", str(override))
    assert at.agents_dir() == override


def test_writable_volume_is_chosen_and_probe_leaves_no_litter(
    probe_env, tmp_path, monkeypatch
) -> None:
    volume = tmp_path / "data" / "agents"
    volume.mkdir(parents=True)
    monkeypatch.setattr(at, "VOLUME_AGENT_DIR", volume)
    assert at.agents_dir() == volume
    assert list(volume.iterdir()) == []  # probe tmp file cleaned up


@pytest.mark.skipif(not _NON_ROOT_POSIX, reason="chmod-based unwritable test")
def test_unwritable_volume_falls_back_and_logs_exactly_once(
    probe_env, tmp_path, monkeypatch, caplog
) -> None:
    volume = tmp_path / "data" / "agents"  # root-owned-volume simulation
    volume.mkdir(parents=True)
    monkeypatch.setattr(at, "VOLUME_AGENT_DIR", volume)
    fallback = tmp_path / "relay" / "agents"
    monkeypatch.setattr(at, "_relay_fallback_dir", lambda: fallback)
    volume.chmod(0o555)

    with caplog.at_level(logging.WARNING, logger="iowap-node-mcp.tokens"):
        first = at.agents_dir()
        second = at.agents_dir()

    assert first == second == fallback
    warnings = [r for r in caplog.records
                if r.name == "iowap-node-mcp.tokens" and r.levelno == logging.WARNING]
    assert len(warnings) == 1  # probe-once — the second call is a cache hit
    assert "not writable" in warnings[0].getMessage()
    assert "/data/agents" in warnings[0].getMessage()
    volume.chmod(0o755)


@pytest.mark.skipif(not _NON_ROOT_POSIX, reason="chmod-based unwritable test")
def test_probe_result_is_cached_even_if_access_changes_later(
    probe_env, tmp_path, monkeypatch
) -> None:
    volume = tmp_path / "data" / "agents"
    volume.mkdir(parents=True)
    monkeypatch.setattr(at, "VOLUME_AGENT_DIR", volume)
    assert at.agents_dir() == volume
    volume.chmod(0o555)
    assert at.agents_dir() == volume  # cache, no re-probe mid-process
    volume.chmod(0o755)
