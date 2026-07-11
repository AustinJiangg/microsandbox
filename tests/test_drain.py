"""Stage 25 -- graceful node drain (real VMs), gated on MSB_TEST_DISCOVERY=1.

Drain is a dynamic-fleet operation (Decision D5): the Redis service registry is what carries a
node's self-reported status, so drain runs under the SAME gate as the Stage-24 discovery e2e --
MSB_TEST_DISCOVERY=1 flips the fixture to orchestrator --register + api --node-discovery redis.
Unset (the default), this module skips and the ordinary suite runs on static discovery, unchanged.

The scenario proves drain's load-bearing properties against a real VM on the single real
orchestrator node:
  (a) a drained node is excluded from NEW placements -- it is the only node, so create returns 503;
  (b) a sandbox already on it keeps working and stays reachable (drain != eviction: the data path
      routes by the catalog, independent of placement);
  (c) resume reverses it -- the node goes active and new placements succeed again.

Running two real orchestrators on one box is not an E2B concept (each E2B orchestrator is a
machine), so -- as in Stage 23/24 -- multi-node spread is covered by the in-process integration
test (cmd/api/placement_integration_test.go); here we exercise the end-to-end channel through a real
VM. See docs/STAGE25_DESIGN.md.
"""

import json
import os
import time
import urllib.error
import urllib.request

import pytest

from microsandbox import Sandbox

pytestmark = pytest.mark.skipif(
    os.environ.get("MSB_TEST_DISCOVERY", "") in ("", "0"),
    reason="set MSB_TEST_DISCOVERY=1 to run the api with --node-discovery redis (drain needs it, D5)",
)

API_KEY = "msb_dev_key"  # the fixture-seeded dev key (default team)


def _get_nodes(base_url):
    """The api's live fleet view (GET /nodes); each node carries its Stage-25 status."""
    req = urllib.request.Request(base_url + "/nodes", headers={"X-API-Key": API_KEY})
    with urllib.request.urlopen(req, timeout=5) as r:
        return json.load(r)["nodes"]


def _status_of(base_url, node_id):
    for n in _get_nodes(base_url):
        if n["id"] == node_id:
            return n["status"]
    return None


def _post(base_url, path, expect_status=None):
    """POST with the dev key; return the HTTP status code. Never raises on an HTTP error status (we
    assert on the code), only on a transport failure. The node id in a drain path contains a colon
    (the gRPC address), which is a valid path char, so no escaping is needed."""
    req = urllib.request.Request(base_url + path, method="POST", headers={"X-API-Key": API_KEY})
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            code = r.status
    except urllib.error.HTTPError as e:
        code = e.code
    if expect_status is not None:
        assert code == expect_status, f"POST {path} -> {code} (want {expect_status})"
    return code


def _wait(pred, timeout=12.0, interval=0.2):
    deadline = time.time() + timeout
    while time.time() < deadline:
        if pred():
            return True
        time.sleep(interval)
    return False


def test_drain_excludes_node_but_keeps_serving(control_plane):
    base_url = control_plane
    nodes = _get_nodes(base_url)
    assert nodes, "the api should have discovered its orchestrator via Redis"
    node_id = nodes[0]["id"]
    assert _status_of(base_url, node_id) == "active", f"the node should start active: {nodes}"

    # A stateful sandbox already placed on the (only) node -- it must survive the drain.
    sb = Sandbox(base_url=base_url)
    try:
        sb.run_code("x = 21")
        assert sb.run_code("print(x * 2)").stdout.strip() == "42"

        # Drain the node -> 202 Accepted (recorded; effective on the next heartbeat + reconcile).
        _post(base_url, f"/nodes/{node_id}/drain", expect_status=202)
        assert _wait(lambda: _status_of(base_url, node_id) == "draining"), \
            "GET /nodes should show the node draining within a heartbeat + reconcile"

        # (a) The draining node is the only node, so a new placement has nowhere to land -> 503
        # (ErrNoNode). This proves drain removes it from NEW placements without touching the fleet.
        assert _post(base_url, "/sandboxes") == 503, \
            "creating while the only node drains should be 503 (no eligible node)"

        # (b) The existing sandbox is untouched -- its stateful kernel still runs (drain != eviction).
        assert sb.run_code("print(x + 21)").stdout.strip() == "42"

        # (c) Resume reverses it: the node goes active and new placements boot again.
        _post(base_url, f"/nodes/{node_id}/resume", expect_status=202)
        assert _wait(lambda: _status_of(base_url, node_id) == "active"), \
            "the node should return to active after resume"
        sb2 = Sandbox(base_url=base_url)
        try:
            assert sb2.run_code("print(1 + 1)").stdout.strip() == "2"
        finally:
            sb2.close()
    finally:
        # Never leave the session-shared node drained for later tests, then drop the sandbox.
        _post(base_url, f"/nodes/{node_id}/resume")
        sb.close()
