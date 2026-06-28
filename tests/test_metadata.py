"""Stage 8c: the api persists sandbox metadata in its store, and GET /sandboxes reads
it back. This boots a real VM (so it's in the microVM group that auto-skips without
go / firecracker / kvm), then checks the api's metadata endpoint reflects create + delete.
"""

import json
import os
import urllib.request

from microsandbox import Sandbox


def _list_ids(base_url: str) -> set[str]:
    # Stage 16: GET /sandboxes now requires an X-API-Key. Use the suite's seeded dev key (the
    # conftest exports it) so this raw request is scoped to the same team the Sandbox uses.
    req = urllib.request.Request(
        base_url + "/sandboxes",
        headers={"X-API-Key": os.environ.get("MICROSANDBOX_API_KEY", "")},
    )
    with urllib.request.urlopen(req, timeout=5) as r:
        return {sb["id"] for sb in json.load(r)["sandboxes"]}


def test_metadata_store_tracks_lifecycle(control_plane: str) -> None:
    """Creating a sandbox records a row (visible via GET /sandboxes); closing it removes
    the row -- proving the api writes to / deletes from the SQLite store, not just the
    orchestrator's in-memory registry."""
    sb = Sandbox(base_url=control_plane)
    try:
        sid = sb._sandbox_id
        listed = _list_ids(control_plane)
        assert sid in listed  # create wrote a metadata row
    finally:
        sb.close()
    assert sid not in _list_ids(control_plane)  # delete removed it
