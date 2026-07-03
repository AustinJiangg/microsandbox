"""Stage 6: named templates -- a sandbox boots from a custom (rootfs, snapshot) image.

The 'example' template (templates/example/Dockerfile) is the stock agent image plus a
marker file. These assert the named template boots and carries that marker, while the
default image does not -- proving that selecting a template actually swaps the image,
not just the name. Real VMs, so they auto-skip without go/firecracker/kvm like the
other microVM tests (the control_plane fixture behind both fixtures does the skipping).

Stage 10 adds test_build_template_via_api: the same image, but built through the api's
async TemplateService (POST /templates + status polling) instead of build-template.sh.
"""

import os
import pathlib
import subprocess

import pytest

from microsandbox import Sandbox, build_template

MARKER = "/etc/microsandbox-template"


def test_named_template_carries_its_own_image(example_sandbox):
    assert "example template" in example_sandbox.files.read(MARKER)


def test_default_image_lacks_the_template_marker(sandbox):
    # The stock image has no such file, so `cat` exits non-zero.
    assert sandbox.commands.run(f"cat {MARKER}").exit_code != 0


def test_build_template_via_api(api_template_build):
    """Stage 10: build a template through the api (async TemplateService), then boot it.

    Submits the example recipe's Dockerfile to POST /templates, polls the build to
    success, then cold-starts a sandbox from it and checks the marker -- exercising the
    full SDK -> api -> orchestrator -> pkg/build path. with_snapshot=False keeps it cheap
    (skips the 512MB snapshot; the sandbox cold-starts).
    """
    base_url = api_template_build
    dockerfile = (
        pathlib.Path(__file__).resolve().parents[1] / "templates" / "example" / "Dockerfile"
    ).read_text()

    build_template("apibuilt", dockerfile, with_snapshot=False, base_url=base_url)

    with Sandbox(template="apibuilt", base_url=base_url) as sb:
        assert "example template" in sb.files.read(MARKER)


DERIVED_MARKER = "/etc/microsandbox-derived"


def test_layered_template_via_api(api_template_build):
    """Stage 18/19: build a copy-on-write LAYERED template (base="default") through the api, boot it,
    and assert the layering win is real.

    `derived` is the default image plus a marker file, but its rootfs is stored as a DIFF over the
    default's (only its changed blocks + a flattened header). This exercises the full SDK(base=) ->
    api(from) -> orchestrator -> pkg/build layered path on a real VM: a sandbox cold-starts from the
    layered build, carries the child's added content, and runs code -- proving the layered rootfs is a
    valid, bootable image.

    Stage 19 also asserts the SIZE win directly: the layout-preserving builder (build-rootfs-layered.sh
    mutates a copy of the base's rootfs in place, rather than re-mkfs-ing it) makes `derived`'s stored
    rootfs object ~its genuine delta (measured ~28 KiB over the 576 MiB base, ~0.005%), vs Stage 18's
    re-mkfs ~48%. The e2e has no S3 client, so a small Go probe (msb-rootfs-stat) reports the bucket's
    "<stored> <full>" bytes -- the "Go probe in the e2e harness" of docs/STAGE19_DESIGN.md Decision 4.
    """
    if "--storage local-fs" in os.environ.get("MSB_ORCH_FLAGS", ""):
        pytest.skip("layered COW builds need object storage (s3 mode)")

    base_url = api_template_build
    repo_root = pathlib.Path(__file__).resolve().parents[1]
    dockerfile = f'FROM microsandbox-agent\nRUN echo "derived COW layer" > {DERIVED_MARKER}\n'

    build_template("derived", dockerfile, base="default", with_snapshot=False, base_url=base_url)

    with Sandbox(template="derived", base_url=base_url) as sb:
        assert "derived COW layer" in sb.files.read(DERIVED_MARKER)
        assert sb.run_code("print(6 * 7)").stdout.strip() == "42"

    # The COW win is now asserted, not just recorded out-of-band: `derived`'s stored rootfs must be a
    # tiny fraction of the base's full size. A regression to the Stage-18 re-mkfs path (~48%) would fail
    # this; the real layout-preserving delta is ~0.005%, so a 2% (full/50) ceiling is decisive yet loose.
    stat = subprocess.run(
        ["go", "run", "./services/cmd/msb-rootfs-stat", "--name", "derived"],
        cwd=str(repo_root), capture_output=True, text=True, check=True,
    )
    stored, full = map(int, stat.stdout.strip().splitlines()[-1].split())
    assert stored < full // 50, f"layered rootfs stored {stored}B is not << full {full}B (layout not preserved?)"
