"""Stage 6: named templates -- a sandbox boots from a custom (rootfs, snapshot) image.

The 'example' template (templates/example/Dockerfile) is the stock agent image plus a
marker file. These assert the named template boots and carries that marker, while the
default image does not -- proving that selecting a template actually swaps the image,
not just the name. Real VMs, so they auto-skip without go/firecracker/kvm like the
other microVM tests (the control_plane fixture behind both fixtures does the skipping).

Stage 10 adds test_build_template_via_api: the same image, but built through the api's
async TemplateService (POST /templates + status polling) instead of build-template.sh.
"""

import pathlib

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
    """Stage 18: build a copy-on-write LAYERED template (base="default") through the api, then boot it.

    `derived` is the default image plus a marker file, but its rootfs is stored as a DIFF over the
    default's (only its changed blocks + a flattened header), pinned to the default's size so the
    diff stays small. This exercises the full SDK(base=) -> api(from) -> orchestrator -> pkg/build
    layered path on a real VM: a sandbox cold-starts from the layered build, carries the child's
    added content, and runs code -- proving the layered rootfs is a valid, bootable image.

    The "stores only the diff" win is asserted hermetically in services/pkg/storage
    (TestPublishAndMaterializeRootfsDiff: the diff object holds only the changed blocks); the
    measured real bytes are recorded in docs/STAGE18_DESIGN.md. On one box the boot may cache-hit
    the orchestrator's local build output, so the bucket-assemble path (default + derived objects)
    is the one covered hermetically by the storage unit tests.
    """
    base_url = api_template_build
    dockerfile = f'FROM microsandbox-agent\nRUN echo "derived COW layer" > {DERIVED_MARKER}\n'

    build_template("derived", dockerfile, base="default", with_snapshot=False, base_url=base_url)

    with Sandbox(template="derived", base_url=base_url) as sb:
        assert "derived COW layer" in sb.files.read(DERIVED_MARKER)
        assert sb.run_code("print(6 * 7)").stdout.strip() == "42"
