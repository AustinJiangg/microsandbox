"""Stage 24 -- dynamic node discovery (real VMs), gated on MSB_TEST_DISCOVERY=1.

With MSB_TEST_DISCOVERY=1 the control-plane fixture runs the orchestrator with --register and the
api with --node-discovery redis, so the api learns its fleet ONLY through the Redis service
registry -- no static --nodes flag. These cases prove that (a) discovery-driven placement boots a
real VM, and (b) a node whose registry key lapses (a crashed orchestrator) is evicted from the
api's fleet by the reconcile loop.

Unset (the default), the whole module skips and the ordinary suite runs on static discovery,
unchanged. See docs/STAGE24_DESIGN.md.
"""

import json
import os
import socket
import time
import urllib.request

import pytest

from microsandbox import Sandbox

# The fixture flips to dynamic discovery only under this env var; otherwise these cases are moot
# (the api would be on static discovery), so skip the module.
pytestmark = pytest.mark.skipif(
    os.environ.get("MSB_TEST_DISCOVERY", "") in ("", "0"),
    reason="set MSB_TEST_DISCOVERY=1 to run the api with --node-discovery redis (dynamic discovery)",
)

API_KEY = "msb_dev_key"  # the fixture-seeded dev key (default team)
REDIS_ADDR = "127.0.0.1:6379"  # ensure_redis() always provisions Redis here


def _get_nodes(base_url: str):
    """The api's live view of its discovered fleet (GET /nodes, Stage 24)."""
    req = urllib.request.Request(base_url + "/nodes", headers={"X-API-Key": API_KEY})
    with urllib.request.urlopen(req, timeout=5) as r:
        return json.load(r)["nodes"]


def _node_ids(base_url: str) -> set:
    return {n["id"] for n in _get_nodes(base_url)}


def _wait(pred, timeout: float = 10.0, interval: float = 0.2) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        if pred():
            return True
        time.sleep(interval)
    return False


def _redis_set_px(addr: str, key: str, value: str, px_ms: int) -> None:
    """Minimal raw-RESP `SET key value PX px_ms` so the test needs no python redis dependency. Used
    to inject a fake orchestrator registration that then TTL-expires (a stand-in for a crash)."""
    host, port = addr.split(":")
    parts = ["SET", key, value, "PX", str(px_ms)]
    cmd = f"*{len(parts)}\r\n" + "".join(f"${len(p)}\r\n{p}\r\n" for p in parts)
    with socket.create_connection((host, int(port)), timeout=3) as s:
        s.sendall(cmd.encode())
        resp = s.recv(64)
    assert resp.startswith(b"+OK"), f"redis SET failed: {resp!r}"


def test_discovery_boots_sandbox(control_plane):
    """The api discovered its orchestrator only through the Redis registry (no --nodes), and
    discovery-driven placement boots a real VM: GET /nodes shows a ready node and code runs in it."""
    base_url = control_plane
    nodes = _get_nodes(base_url)
    assert nodes, "the api should have discovered at least one orchestrator via Redis"
    assert any(n["ready"] for n in nodes), f"a discovered node should be ready: {nodes}"

    sb = Sandbox(base_url=base_url)
    try:
        assert sb.run_code("print(6 * 7)").stdout.strip() == "42"
    finally:
        sb.close()


def test_node_ttl_eviction(control_plane):
    """A node whose registry key lapses (a crashed orchestrator that stopped heartbeating) is
    evicted from the api's fleet by the reconcile loop. We inject a FAKE node key with a short TTL
    -- rather than killing the session-shared real orchestrator -- and watch GET /nodes pick it up,
    then drop it once the TTL expires. This exercises the same reconcile eviction path a real crash
    would (E2B's keepInSync deregister)."""
    base_url = control_plane
    fake_id = "disco-test-fake:19999"
    key = f"msb:node:{fake_id}"
    info = json.dumps({"ID": fake_id, "GRPC": "127.0.0.1:19999", "Proxy": "127.0.0.1:15999"})

    # Register the fake node with a 2s TTL and never refresh it (a crash: one write, no heartbeat).
    _redis_set_px(REDIS_ADDR, key, info, 2000)
    assert _wait(lambda: fake_id in _node_ids(base_url)), \
        "the api's reconcile should discover the injected node within a poll"

    # The key expires (no heartbeat) -> the next reconcile drops the node from the fleet.
    assert _wait(lambda: fake_id not in _node_ids(base_url), timeout=12.0), \
        "a node that stopped heartbeating should be evicted from the api's fleet"

    # The real orchestrator is untouched: at least one ready node remains.
    assert any(n["ready"] for n in _get_nodes(base_url)), "the real orchestrator should still be discovered"
