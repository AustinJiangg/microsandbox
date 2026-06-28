"""Stage 16: auth (X-API-Key -> team) on the api, and team-scoped resources.

The api now authenticates every lifecycle request with an X-API-Key that resolves to a team;
sandboxes are owned by that team and list/delete are scoped to it. The conftest seeds two keys
-- msb_dev_key (team "default", the suite-wide key) and msb_team_b_key (team "team_b") -- so
these tests can prove a missing/wrong key is rejected and that one team cannot see or delete
another team's sandboxes.

These cases drive the api directly (lifecycle only), but go through the control_plane fixture,
so they auto-skip as a group without firecracker / KVM / the networking privilege.
"""

import json
import urllib.error
import urllib.request

import pytest

from microsandbox import Sandbox

DEV_KEY = "msb_dev_key"  # team "default" -- the suite-wide key the conftest exports
TEAM_B_KEY = "msb_team_b_key"  # team "team_b"


def _api(base_url: str, method: str, path: str, key: str | None = None):
    """One raw lifecycle request to the api with an optional X-API-Key, returning (status, body).

    Bypasses any env proxy (the WSL autoproxy would otherwise grab the loopback hop), exactly as
    test_ports.py does for its direct requests."""
    headers = {}
    if key is not None:
        headers["X-API-Key"] = key
    req = urllib.request.Request(base_url + path, method=method, headers=headers)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(req, timeout=10) as resp:
            raw = resp.read()
            return resp.status, (json.loads(raw) if raw else {})
    except urllib.error.HTTPError as exc:
        raw = exc.read()
        return exc.code, (json.loads(raw) if raw else {})


def test_missing_key_is_401(control_plane):
    status, body = _api(control_plane, "GET", "/sandboxes", key=None)
    assert status == 401
    assert "API" in body.get("error", "")


def test_wrong_key_is_401(control_plane):
    status, _ = _api(control_plane, "GET", "/sandboxes", key="totally-wrong-key")
    assert status == 401


def test_health_stays_open(control_plane):
    # /health is the one unauthenticated route (load balancers / the fixture probe it).
    status, _ = _api(control_plane, "GET", "/health", key=None)
    assert status == 200


def test_sdk_without_key_raises(control_plane, monkeypatch):
    # With no key the SDK sends no header and the api answers 401, surfaced as a RuntimeError.
    monkeypatch.delenv("MICROSANDBOX_API_KEY", raising=False)
    with pytest.raises(RuntimeError) as exc:
        Sandbox(base_url=control_plane, api_key=None)
    assert "API" in str(exc.value)


def test_team_isolation(control_plane):
    """A sandbox created by team "default" is invisible to team_b, and team_b cannot delete it."""
    sb = Sandbox(base_url=control_plane, api_key=DEV_KEY)
    try:
        # default sees its own sandbox...
        status, body = _api(control_plane, "GET", "/sandboxes", key=DEV_KEY)
        assert status == 200
        assert sb._sandbox_id in {s["id"] for s in body["sandboxes"]}

        # ...team_b does not.
        status, body = _api(control_plane, "GET", "/sandboxes", key=TEAM_B_KEY)
        assert status == 200
        assert sb._sandbox_id not in {s["id"] for s in body["sandboxes"]}

        # team_b cannot delete default's sandbox -- 404 (we don't even admit it exists).
        status, _ = _api(control_plane, "DELETE", f"/sandboxes/{sb._sandbox_id}", key=TEAM_B_KEY)
        assert status == 404

        # and the sandbox is still alive: the ownership check ran before any VM teardown.
        assert sb.run_code("print(40 + 2)").stdout.strip() == "42"
    finally:
        sb.close()
